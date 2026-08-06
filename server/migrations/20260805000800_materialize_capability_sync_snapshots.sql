-- Review finding F-1 (P0): a paged snapshot stored only its manifest, so page N
-- had to be recomputed from live tables.
--
-- The contract requires every page to repeat one immutable id/generation/count/
-- digest and requires the client to reassemble all pages and verify that digest
-- before applying anything. Recomputing page N cannot honour that. The measured
-- failure: build snapshot S (10 pages, digest D); the client fetches page 0; a
-- user unfavorites one item; the client fetches page 1 from data that no longer
-- matches; the reassembled set differs from the built set; digest != D; the
-- client correctly discards the whole candidate. Convergence then needs a quiet
-- window at least as long as a full pagination pass, every retry burns a
-- generation and a full O(N) rebuild, and the odds get worse the bigger the
-- account — a livelock with no liveness guarantee. Worse still, the same
-- generation could serve an item as active on page 1 and as a tombstone on page
-- 3, which is precisely the state the contract calls invalid.
--
-- This migration stores the snapshot instead. `capability_sync_snapshot_payloads`
-- holds the whole RFC 8785 canonical document as bytes; `snapshot_digest` is the
-- SHA-256 of exactly those bytes; a page is a deterministic slice of the stored
-- artifact. Nothing about a served page is derived from data that can move.
--
-- content_digest — why a SECOND digest
-- ------------------------------------
-- snapshot_digest covers snapshotId, generation and generatedAt, so two builds
-- of identical content produce different values and it cannot answer "did
-- anything actually change?". content_digest covers the observable content and
-- the page size, and nothing else. The build compares it against the principal's
-- newest complete snapshot and, when equal, re-serves that snapshot unchanged
-- rather than allocating a new generation.
--
-- That is not an optimization. csc refuses any generation that is not strictly
-- greater than the one it applied, so a generation allocated per REQUEST turns a
-- polling fleet into a fleet doing full re-verification forever. Generation has
-- to mean "version of the observable final state".
--
-- page_size is inside content_digest because page_count is inside
-- snapshot_digest: a snapshot is frozen together with its pagination, so a
-- differently paginated snapshot is a different snapshot and must not reuse the
-- artifact built for the old shape.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE capability_sync_snapshots
    ADD COLUMN IF NOT EXISTS content_digest VARCHAR(64),
    ADD COLUMN IF NOT EXISTS page_size      INTEGER NOT NULL DEFAULT 0;

ALTER TABLE capability_sync_snapshots
    DROP CONSTRAINT IF EXISTS chk_capability_sync_snapshots_content_digest_format;
ALTER TABLE capability_sync_snapshots
    ADD CONSTRAINT chk_capability_sync_snapshots_content_digest_format
    CHECK (content_digest IS NULL OR content_digest ~ '^[0-9a-f]{64}$');

-- A complete snapshot must be fully described: both digests, a real page count,
-- and the page size those pages were cut at. Without page_size a later reader
-- would have to guess how to slice the stored artifact, and a wrong guess
-- produces pages that reassemble to a different digest.
ALTER TABLE capability_sync_snapshots
    DROP CONSTRAINT IF EXISTS chk_capability_sync_snapshots_complete;
ALTER TABLE capability_sync_snapshots
    ADD CONSTRAINT chk_capability_sync_snapshots_complete
    CHECK (
        NOT complete
        OR (snapshot_digest IS NOT NULL
            AND content_digest IS NOT NULL
            AND page_count >= 1
            AND page_size  >= 1)
    );

-- "the principal's newest complete snapshot" is the hot read on every sync:
-- it is what the content digest is compared against and what a reuse re-serves.
CREATE INDEX IF NOT EXISTS idx_capability_sync_snapshots_current
    ON capability_sync_snapshots (principal_id, generation DESC)
    WHERE complete;

-- The frozen artifact.
--
-- It lives in its own table rather than as a column on the manifest because the
-- manifest is read constantly (generation comparison, expiry sweep, page
-- header) and the payload is read only when a page is actually served. A BYTEA
-- column on the manifest would drag TOAST pointers through every one of those
-- reads for no benefit.
--
-- ON DELETE CASCADE is correct here and deliberately unlike the tombstone
-- table's missing FK: a payload without its manifest is unservable, whereas a
-- tombstone without its item is still a valid instruction to remove it.
CREATE TABLE IF NOT EXISTS capability_sync_snapshot_payloads (
    snapshot_id UUID        PRIMARY KEY
        REFERENCES capability_sync_snapshots (id) ON DELETE CASCADE,
    byte_size   INTEGER     NOT NULL,
    payload     BYTEA       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- byte_size is stored so a size query never has to detoast the payload, and
    -- checked against it so the two can never disagree.
    CONSTRAINT chk_capability_sync_snapshot_payloads_size
        CHECK (byte_size = octet_length(payload) AND byte_size > 0)
);

COMMENT ON TABLE capability_sync_snapshot_payloads IS 'Frozen RFC 8785 canonical bytes of one snapshot; pages are deterministic slices of this artifact, never a re-query';
COMMENT ON COLUMN capability_sync_snapshot_payloads.payload IS 'The exact bytes capability_sync_snapshots.snapshot_digest is the SHA-256 of';
COMMENT ON COLUMN capability_sync_snapshots.content_digest IS 'Change-detection key over content + page size only; equal digests re-serve the existing snapshot instead of allocating a generation';
COMMENT ON COLUMN capability_sync_snapshots.page_size IS 'Page size this snapshot was frozen at; page_count is derived from it and is inside snapshot_digest';

-- +goose StatementEnd

-- Review finding F-5 (P1): the generation counter could be bypassed, and ONE
-- bypass causes a real inversion.
--
-- uq_capability_sync_snapshots_generation stops a number being REUSED. It does
-- nothing about a number being INVENTED. A repair script inserting generation
-- 42 while the counter sits at 5 makes csc apply 42 and then permanently reject
-- 6..41 as not strictly greater — 36 generations of silent paralysis, visible
-- only as "stale generation" in a log nobody reads.
--
-- Two triggers close it. The counter may only move by exactly one, and a
-- manifest row may only carry the number the counter currently holds. Together
-- they make "the caller brought its own generation" unrepresentable, in the
-- database, for every writer — application, migration, psql session.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capability_sync_snapshot_generation_guard()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- A principal starts at 0 (row created ahead of use) or 1 (created by
        -- its first allocation). Anything else is someone seeding a number.
        IF NEW.generation NOT IN (0, 1) THEN
            RAISE EXCEPTION
                'capability sync snapshot generation for principal % must start at 0 or 1, got %',
                NEW.principal_id, NEW.generation
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.principal_id <> OLD.principal_id THEN
        RAISE EXCEPTION 'capability sync snapshot generation rows may not change principal (% -> %)',
            OLD.principal_id, NEW.principal_id
            USING ERRCODE = 'check_violation';
    END IF;

    -- Unchanged is allowed so metadata-only updates (last_snapshot_id) do not
    -- have to burn a generation; anything other than +1 is rejected.
    IF NEW.generation <> OLD.generation AND NEW.generation <> OLD.generation + 1 THEN
        RAISE EXCEPTION
            'capability sync snapshot generation may only advance by one (principal %, % -> %)',
            NEW.principal_id, OLD.generation, NEW.generation
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS capability_sync_snapshot_generation_guard
    ON capability_sync_snapshot_generations;
CREATE TRIGGER capability_sync_snapshot_generation_guard
    BEFORE INSERT OR UPDATE ON capability_sync_snapshot_generations
    FOR EACH ROW EXECUTE FUNCTION capability_sync_snapshot_generation_guard();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capability_sync_snapshot_manifest_guard()
RETURNS trigger AS $$
DECLARE
    allocated BIGINT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT generation INTO allocated
        FROM capability_sync_snapshot_generations
        WHERE principal_id = NEW.principal_id;

        -- The allocation and the manifest insert happen in one transaction, so
        -- this sees the number this transaction just took. A caller that did
        -- not allocate sees NULL or a mismatch and is rejected.
        IF allocated IS NULL OR allocated <> NEW.generation THEN
            RAISE EXCEPTION
                'snapshot generation % was not allocated for principal % (allocator holds %)',
                NEW.generation, NEW.principal_id, COALESCE(allocated::text, 'no row')
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    -- A complete snapshot is the artifact a client verifies a digest against.
    -- Everything that participates in that verification is frozen; expires_at
    -- is not part of it and may be extended, which is what lets an unchanged
    -- snapshot be re-served under its original generation.
    IF OLD.complete AND (
           NEW.id               IS DISTINCT FROM OLD.id
        OR NEW.principal_id     IS DISTINCT FROM OLD.principal_id
        OR NEW.generation       IS DISTINCT FROM OLD.generation
        OR NEW.contract_version IS DISTINCT FROM OLD.contract_version
        OR NEW.page_count       IS DISTINCT FROM OLD.page_count
        OR NEW.page_size        IS DISTINCT FROM OLD.page_size
        OR NEW.item_count       IS DISTINCT FROM OLD.item_count
        OR NEW.tombstone_count  IS DISTINCT FROM OLD.tombstone_count
        OR NEW.snapshot_digest  IS DISTINCT FROM OLD.snapshot_digest
        OR NEW.content_digest   IS DISTINCT FROM OLD.content_digest
        OR NEW.generated_at     IS DISTINCT FROM OLD.generated_at
        OR NEW.complete         IS DISTINCT FROM OLD.complete
    ) THEN
        RAISE EXCEPTION
            'a complete capability sync snapshot is immutable except for expires_at (snapshot %)',
            OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS capability_sync_snapshot_manifest_guard
    ON capability_sync_snapshots;
CREATE TRIGGER capability_sync_snapshot_manifest_guard
    BEFORE INSERT OR UPDATE ON capability_sync_snapshots
    FOR EACH ROW EXECUTE FUNCTION capability_sync_snapshot_manifest_guard();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capability_sync_snapshot_payload_guard()
RETURNS trigger AS $$
BEGIN
    -- The digest was computed over these bytes. Rewriting them in place would
    -- leave a manifest whose digest describes content that no longer exists,
    -- and every client would discard every page of it forever without any
    -- server-side symptom. Replacement means a new snapshot.
    RAISE EXCEPTION 'capability sync snapshot payloads are immutable (snapshot %)', OLD.snapshot_id
        USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS capability_sync_snapshot_payload_guard
    ON capability_sync_snapshot_payloads;
CREATE TRIGGER capability_sync_snapshot_payload_guard
    BEFORE UPDATE ON capability_sync_snapshot_payloads
    FOR EACH ROW EXECUTE FUNCTION capability_sync_snapshot_payload_guard();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS capability_sync_snapshot_payload_guard ON capability_sync_snapshot_payloads;
DROP FUNCTION IF EXISTS capability_sync_snapshot_payload_guard();
DROP TRIGGER IF EXISTS capability_sync_snapshot_manifest_guard ON capability_sync_snapshots;
DROP FUNCTION IF EXISTS capability_sync_snapshot_manifest_guard();
DROP TRIGGER IF EXISTS capability_sync_snapshot_generation_guard ON capability_sync_snapshot_generations;
DROP FUNCTION IF EXISTS capability_sync_snapshot_generation_guard();

DROP TABLE IF EXISTS capability_sync_snapshot_payloads;

DROP INDEX IF EXISTS idx_capability_sync_snapshots_current;

ALTER TABLE capability_sync_snapshots
    DROP CONSTRAINT IF EXISTS chk_capability_sync_snapshots_content_digest_format;
ALTER TABLE capability_sync_snapshots
    DROP CONSTRAINT IF EXISTS chk_capability_sync_snapshots_complete;
ALTER TABLE capability_sync_snapshots
    ADD CONSTRAINT chk_capability_sync_snapshots_complete
    CHECK (NOT complete OR (snapshot_digest IS NOT NULL AND page_count >= 1));

ALTER TABLE capability_sync_snapshots
    DROP COLUMN IF EXISTS page_size,
    DROP COLUMN IF EXISTS content_digest;

-- +goose StatementEnd
