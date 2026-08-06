-- Per-item Git revision history for Git-backed capabilities.
--
-- History is an ordered sequence of SUCCESSFUL item projection changes, not a
-- unique set of SHAs and not a delivery log. git_capability_sync_jobs already
-- records deliveries/attempts; it must not be overloaded into user-facing
-- history (a retry, a failure, and a no-op reconcile all produce job rows but
-- no revision).
--
-- `git_sha` is the repository DEFAULT-BRANCH COMMIT SHA that produced the
-- projection — not a manifest blob hash. A row is appended only when that head
-- SHA differs from the item's current authoritative `capability_items.git_sha`,
-- so a duplicate delivery or a same-SHA reconcile appends nothing while a
-- force-push/revert back to a previously observed SHA does append (it is a new
-- transition).
--
-- No content snapshot is stored. The content for any revision lives in Git.
--
-- revision_no allocation
-- ----------------------
-- revision_no is a BIGINT that is strictly increasing and unique WITHIN an
-- item. It is allocated as MAX(revision_no)+1 for the item inside the same
-- transaction that updates capability_items.git_sha, and that transaction must
-- already hold the row lock on capability_items for the item (SELECT ... FOR
-- UPDATE) because it is updating the item's current SHA. That item row lock is
-- the serialization point: every writer of a revision is also a writer of the
-- item's current SHA, so two allocations for the same item cannot interleave.
-- uq_capability_item_git_revisions_no is the backstop — a lost race becomes a
-- unique violation and a transaction rollback/retry, never a duplicate or a
-- reused revision number. Gaps are permitted (a rolled-back projection consumes
-- nothing, but a manual repair may); only strict monotonicity is contractual.
--
-- The unique constraint's btree (item_id, revision_no) also serves the only two
-- read paths — `WHERE item_id = ? ORDER BY revision_no DESC LIMIT 5` (backward
-- index scan) and the `before_revision` cursor `AND revision_no < ?` — so no
-- additional index is created.
--
-- Foreign keys
-- ------------
-- item_id CASCADEs: an item that is genuinely hard-deleted by an operator has
-- no history to show. Archiving is a `status` change, not a delete, so Git
-- archive/restore never touches these rows — history survives archival, as the
-- contract requires.
--
-- git_server_id deliberately has NO foreign key, matching
-- capability_items.source_git_server_id. These columns are recorded coordinates
-- of a past observation, not a live reference; deregistering a Git server must
-- not erase an item's history.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS capability_item_git_revisions (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id       UUID        NOT NULL REFERENCES capability_items(id) ON DELETE CASCADE,
    revision_no   BIGINT      NOT NULL,
    git_server_id VARCHAR(64) NOT NULL,
    git_repo_id   BIGINT      NOT NULL,
    git_ref       TEXT        NOT NULL DEFAULT '',
    manifest_path TEXT        NOT NULL DEFAULT '',
    entry_key     TEXT        NOT NULL DEFAULT '',
    git_sha       VARCHAR(40) NOT NULL,
    version_label TEXT        NOT NULL DEFAULT '',
    source        VARCHAR(16) NOT NULL,
    observed_at   TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_capability_item_git_revisions_no UNIQUE (item_id, revision_no),
    CONSTRAINT chk_capability_item_git_revisions_no CHECK (revision_no > 0),
    CONSTRAINT chk_capability_item_git_revisions_sha CHECK (git_sha <> ''),
    CONSTRAINT chk_capability_item_git_revisions_source
        CHECK (source IN ('backfill', 'provision', 'push', 'reconcile', 'restore'))
);

COMMENT ON TABLE capability_item_git_revisions IS 'Ordered successful Git projection transitions per capability item; append-only, no content snapshot';
COMMENT ON COLUMN capability_item_git_revisions.revision_no IS 'Strictly increasing within item_id; allocated under the capability_items row lock, uniqueness enforced as the backstop';
COMMENT ON COLUMN capability_item_git_revisions.git_sha IS 'Repository default-branch commit SHA used for this projection, not a manifest blob hash';
COMMENT ON COLUMN capability_item_git_revisions.git_ref IS 'Default-branch ref observed at projection time (branch name or refs/heads/<branch>)';
COMMENT ON COLUMN capability_item_git_revisions.entry_key IS 'Stable entry identity within one manifest; empty for single-entry manifests (mirrors capability_items.source_git_entry_key)';
COMMENT ON COLUMN capability_item_git_revisions.version_label IS 'Item-visible version text at this revision; may be empty when the manifest declares none';
COMMENT ON COLUMN capability_item_git_revisions.observed_at IS 'Server time of the successful observation. Backfill rows reuse capability_items.git_last_synced_at';
COMMENT ON COLUMN capability_item_git_revisions.source IS 'backfill | provision | push | reconcile | restore';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS capability_item_git_revisions;

-- +goose StatementEnd
