-- Durable per-user removal records for the csc snapshot v2 sync contract.
--
-- Core safety invariant: an absent item is NOT a deletion signal. csc may
-- unload/disable a managed local capability only when a complete, versioned
-- snapshot carries an explicit tombstone for it. This table is the durable
-- source of those tombstones; without it, removal could only be expressed by
-- absence, which a partial page, an auth failure, or an empty error response
-- would forge.
--
-- One row per (user_id, item_id)
-- ------------------------------
-- A tombstone means "this user's entitlement to this item ended", not "one
-- particular favorite row was deleted". A user who both favorited and received
-- a distribution keeps access until the LAST relationship ends, so exactly one
-- terminal record per user/item is the correct shape. It also makes the
-- snapshot-v2 rule "multiple non-identical tombstones for one user/item make
-- the snapshot invalid" unrepresentable in storage rather than merely
-- forbidden at serialization time. `reason` and `source` describe the last
-- cause of the removal.
--
-- event_id
-- --------
-- event_id is the durable, client-facing dedup key. It is globally unique and
-- must be ROTATED on every new removal transition (re-removal upserts this row
-- with a fresh event_id and a new removed_at). This is not cosmetic: with a
-- stable event_id, the sequence unfavorite -> refavorite -> unfavorite would
-- dedupe against the tombstone csc already applied, and the capability would
-- stay installed forever. Conversely, event_id must never be regenerated for a
-- tombstone that has not transitioned, or every snapshot would look like a new
-- removal.
--
-- No foreign key on item_id — deliberate
-- --------------------------------------
-- If item_id CASCADEd, an operator hard-deleting a capability would erase the
-- only record that tells csc to remove it, and every device that already
-- installed it would keep it forever. The tombstone must outlive the item, so
-- item_id is an unenforced stable identifier, exactly like the itemId csc
-- stores locally. Orphan tombstones are harmless: they instruct removal.
--
-- Invariant ownership (DB vs application)
-- ---------------------------------------
-- Enforced by this schema:
--   * at most one tombstone per (user_id, item_id)         — unique constraint
--   * globally unique event_id                             — unique constraint
--   * reason / source / lifecycle_reason enum membership    — check constraints
--   * lifecycle_reason present iff source = 'git_lifecycle' — check constraint
-- Enforced by the application (snapshot materialization), because it spans
-- tables and cannot be expressed as a row predicate:
--   * "at most one FINAL state per user/item/generation": a currently active
--     item supersedes its older tombstone, so the materializer emits the item
--     in `items` OR in `tombstones`, never both. Supersession is computed at
--     materialization time; the tombstone row is NOT deleted (V4 does not
--     automatically hard-delete tombstones).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS capability_sync_tombstones (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          VARCHAR(191) NOT NULL,
    item_id          UUID         NOT NULL,
    reason           VARCHAR(32)  NOT NULL,
    lifecycle_reason VARCHAR(32),
    source           VARCHAR(32)  NOT NULL,
    event_id         VARCHAR(64)  NOT NULL,
    removed_at       TIMESTAMPTZ  NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uq_capability_sync_tombstones_user_item UNIQUE (user_id, item_id),
    CONSTRAINT uq_capability_sync_tombstones_event UNIQUE (event_id),
    CONSTRAINT chk_capability_sync_tombstones_reason
        CHECK (reason IN ('git_archived', 'unfavorited', 'distribution_revoked')),
    CONSTRAINT chk_capability_sync_tombstones_source
        CHECK (source IN ('git_lifecycle', 'favorite', 'distribution')),
    -- The explicit IS NOT NULL is load-bearing, not redundant. Without it, a
    -- git_lifecycle row with a NULL lifecycle_reason evaluates the first branch
    -- to NULL ("NULL IN (...)" is NULL, not false) and the whole CHECK to NULL
    -- — and a CHECK that is NULL PASSES. The exact tombstone that must carry a
    -- Git cause would slip through with none.
    CONSTRAINT chk_capability_sync_tombstones_lifecycle_reason
        CHECK (
            (source = 'git_lifecycle'
                AND lifecycle_reason IS NOT NULL
                AND lifecycle_reason IN (
                    'manifest_removed',
                    'default_branch_missing',
                    'repository_deleted'
                ))
            OR (source <> 'git_lifecycle' AND lifecycle_reason IS NULL)
        )
);

-- Operator/debug path: every user affected by one item's lifecycle change.
-- The snapshot read path (WHERE user_id = ?) is already served by
-- uq_capability_sync_tombstones_user_item.
CREATE INDEX IF NOT EXISTS idx_capability_sync_tombstones_item
    ON capability_sync_tombstones (item_id);

COMMENT ON TABLE capability_sync_tombstones IS 'Durable explicit per-user removal records for csc snapshot v2; absence is never a removal signal';
COMMENT ON COLUMN capability_sync_tombstones.user_id IS 'Owning principal subject_id, matching item_favorites.user_id';
COMMENT ON COLUMN capability_sync_tombstones.item_id IS 'Stable capability item id; intentionally unconstrained so the tombstone survives item hard deletion';
COMMENT ON COLUMN capability_sync_tombstones.reason IS 'git_archived | unfavorited | distribution_revoked';
COMMENT ON COLUMN capability_sync_tombstones.source IS 'git_lifecycle | favorite | distribution';
COMMENT ON COLUMN capability_sync_tombstones.lifecycle_reason IS 'Git archive cause, present only when source = git_lifecycle';
COMMENT ON COLUMN capability_sync_tombstones.event_id IS 'Durable client-facing dedup key; rotated on every new removal transition, never regenerated otherwise';
COMMENT ON COLUMN capability_sync_tombstones.removed_at IS 'Server time of the removal transition this tombstone represents';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS capability_sync_tombstones;

-- +goose StatementEnd
