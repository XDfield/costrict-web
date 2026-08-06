-- Per-principal snapshot generation counter and snapshot manifest for the csc
-- snapshot v2 sync contract.
--
-- csc orders snapshots by `generation` alone — never by wall clock and never by
-- the opaque snapshot id — and refuses anything that is not strictly greater
-- than the generation it last applied. That makes the allocator a correctness
-- component, not a convenience.
--
-- Allocation scheme: one counter row per principal, incremented by a single
-- upsert
-- ---------------------------------------------------------------------------
--   INSERT INTO capability_sync_snapshot_generations
--       (principal_id, generation, last_allocated_at)
--   VALUES ($1, 1, now())
--   ON CONFLICT (principal_id) DO UPDATE
--       SET generation        = capability_sync_snapshot_generations.generation + 1,
--           last_allocated_at = now(),
--           updated_at        = now()
--   RETURNING generation;
--
-- Why this and not the alternatives:
--
--   * A PostgreSQL sequence per principal is unbounded DDL, and one SHARED
--     sequence is not per-principal: nextval is non-transactional, so a
--     rolled-back build permanently burns a number, and — more importantly — a
--     sequence gives no lock to serialize one principal's materialization,
--     which is the property we actually need.
--   * MAX(generation)+1 over the manifest table races: under READ COMMITTED
--     there is no row to lock, so two builds read the same MAX and only the
--     unique constraint catches it, after both have already materialized.
--
-- Concurrency protocol (the reason the counter row matters):
--   1. Materialization opens a REPEATABLE READ transaction.
--   2. Its FIRST statement is the upsert above. In REPEATABLE READ the
--      transaction snapshot is taken by that statement, so the data view is
--      established at (not before) the moment the generation is allocated, and
--      the row lock is held for the rest of the build.
--   3. A concurrent build for the same principal blocks on that row and, when
--      the winner commits, fails with serialization_failure (40001). It retries
--      and allocates a strictly newer generation against a strictly newer data
--      view. It never silently serves older data under a newer number.
--   4. Consequence: for one principal, generation order == data-view order. csc
--      can therefore reject "not strictly greater" without ever discarding a
--      genuinely newer snapshot or accepting an older one.
--
-- Gaps are permitted and safe. A rolled-back build rolls the counter back with
-- it (the increment is transactional); a build that commits its allocation but
-- is never served leaves a hole. csc requires strict increase, not density.
-- uq_capability_sync_snapshots_generation guarantees a generation that was
-- actually served is never reused for different content, because the manifest
-- row is written in the same transaction as the allocation.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS capability_sync_snapshot_generations (
    principal_id      VARCHAR(191) PRIMARY KEY,
    generation        BIGINT       NOT NULL DEFAULT 0,
    last_snapshot_id  UUID,
    last_allocated_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT chk_capability_sync_snapshot_generations_value CHECK (generation >= 0)
);

COMMENT ON TABLE capability_sync_snapshot_generations IS 'Per-principal strictly increasing snapshot generation allocator for csc snapshot v2; the row lock also single-flights one principal materialization';
COMMENT ON COLUMN capability_sync_snapshot_generations.principal_id IS 'Owning principal subject_id; principal rather than user so non-user sync principals can reuse the allocator';
COMMENT ON COLUMN capability_sync_snapshot_generations.generation IS 'Last allocated generation; 0 means never allocated. Allocated by upsert increment, strictly increasing, gaps permitted';
COMMENT ON COLUMN capability_sync_snapshot_generations.last_snapshot_id IS 'Diagnostic back-pointer to the most recently allocated snapshot manifest';

-- Snapshot manifest: the header every page of a paged snapshot must repeat
-- identically (contract version, immutable id, generation, page count, total
-- item/tombstone counts, canonical digest, completeness). Persisting it is what
-- makes paging honest: page 3 cannot recompute counts or a whole-snapshot
-- digest from live data, and an expired cursor must be rejected rather than
-- silently answered from changed data.
--
-- snapshot_digest is the lowercase SHA-256 hex of the RFC 8785 canonical
-- serialization of the reassembled snapshot (shared manifest + items sorted by
-- item id + tombstones sorted by (item_id, event_id); page-local cursor/index
-- and `complete` excluded). The CHECK enforces the encoding, not the value.
--
-- `complete` may only be true once the whole snapshot is materialized, which is
-- why digest and page_count are nullable/zero until then: a two-phase build
-- (reserve -> materialize -> finalize) stays representable, and an
-- interrupted build can never present itself as authoritative.
CREATE TABLE IF NOT EXISTS capability_sync_snapshots (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id     VARCHAR(191) NOT NULL,
    generation       BIGINT       NOT NULL,
    contract_version INTEGER      NOT NULL DEFAULT 2,
    page_count       INTEGER      NOT NULL DEFAULT 0,
    item_count       INTEGER      NOT NULL DEFAULT 0,
    tombstone_count  INTEGER      NOT NULL DEFAULT 0,
    snapshot_digest  VARCHAR(64),
    complete         BOOLEAN      NOT NULL DEFAULT false,
    generated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ  NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uq_capability_sync_snapshots_generation UNIQUE (principal_id, generation),
    CONSTRAINT chk_capability_sync_snapshots_counts
        CHECK (page_count >= 0 AND item_count >= 0 AND tombstone_count >= 0),
    CONSTRAINT chk_capability_sync_snapshots_digest_format
        CHECK (snapshot_digest IS NULL OR snapshot_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT chk_capability_sync_snapshots_complete
        CHECK (NOT complete OR (snapshot_digest IS NOT NULL AND page_count >= 1)),
    CONSTRAINT chk_capability_sync_snapshots_generation
        CHECK (generation > 0)
);

-- Cursor expiry sweep.
CREATE INDEX IF NOT EXISTS idx_capability_sync_snapshots_expiry
    ON capability_sync_snapshots (expires_at);

COMMENT ON TABLE capability_sync_snapshots IS 'Immutable snapshot manifest repeated verbatim on every page of a csc snapshot v2 response';
COMMENT ON COLUMN capability_sync_snapshots.generation IS 'Per-principal generation allocated from capability_sync_snapshot_generations; unique per principal so a served generation is never reused';
COMMENT ON COLUMN capability_sync_snapshots.snapshot_digest IS 'Lowercase SHA-256 hex over the RFC 8785 canonicalization of the whole reassembled snapshot; NULL until the build finalizes';
COMMENT ON COLUMN capability_sync_snapshots.complete IS 'True only for a fully materialized snapshot; a false row can never authorize client removal';
COMMENT ON COLUMN capability_sync_snapshots.expires_at IS 'After this time the snapshot id and its cursors are rejected; expiry never mutates the snapshot content';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS capability_sync_snapshots;
DROP TABLE IF EXISTS capability_sync_snapshot_generations;

-- +goose StatementEnd
