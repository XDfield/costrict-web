-- Git lifecycle ownership markers for Git-backed capability items.
--
-- Cloud keeps two independent concerns on a Git-backed row:
--   1. `status`        — marketplace availability and manual moderation.
--   2. `git_lifecycle_*` — the last authoritative Git observation.
--
-- `git_sync_status` already says "this sync hid the row" ('orphaned'), but it
-- cannot say WHY, and the recovery rules differ per cause: a removed manifest
-- or a missing default branch may be auto-reactivated when the exact bound
-- identity comes back, while a deleted repository is terminal for automatic
-- recovery. `git_lifecycle_reason` is that discriminator, and clearing it is
-- how a human moderation write revokes Git's permission to auto-reactivate the
-- row.
--
-- `git_visibility_verified_at` records the last successful re-check of the
-- remote repository's visibility/accessibility. Public browse/search may only
-- surface a Git-backed row whose verification is fresh (10-minute default), so
-- a Gitea outage or a worker backlog hides stale rows instead of extending
-- exposure. NULL means "never verified" and therefore "not publicly
-- serveable" — the fail-closed default for every existing row.
--
-- All three columns are nullable and additive: db-backed rows and every
-- existing Git-backed row keep their current behavior.
--
-- NOTE: capability_items is also covered by GORM AutoMigrate (cmd/migrate and
-- cmd/worker). The column types here are mirrored exactly by the struct tags on
-- models.CapabilityItem so whichever runs first produces the same column.
-- AutoMigrate never creates the CHECK constraint or the partial indexes below;
-- this migration owns them.

-- +goose Up

ALTER TABLE capability_items
    ADD COLUMN IF NOT EXISTS git_lifecycle_reason VARCHAR(32),
    ADD COLUMN IF NOT EXISTS git_lifecycle_changed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS git_visibility_verified_at TIMESTAMPTZ;

-- Enum + pairing invariant. A misspelled reason would silently turn a terminal
-- 'repository_deleted' into a value the recovery guard does not recognise, so
-- the enum is enforced in the database rather than only in application code
-- (unlike the free-form `git_sync_status`, whose values are not safety gates).
--
-- DROP + ADD rather than a PL/pgSQL guard: this repository's goose migrations
-- are plain SQL only (see 20260722300000), and the pair is idempotent on
-- re-run. All existing rows have NULL in the new column, so the validating scan
-- is trivially satisfied.
ALTER TABLE capability_items
    DROP CONSTRAINT IF EXISTS chk_capability_items_git_lifecycle_reason;
ALTER TABLE capability_items
    ADD CONSTRAINT chk_capability_items_git_lifecycle_reason
    CHECK (
        git_lifecycle_reason IS NULL
        OR (
            git_lifecycle_reason IN (
                'manifest_removed',
                'default_branch_missing',
                'repository_deleted'
            )
            AND git_lifecycle_changed_at IS NOT NULL
        )
    );

-- Operator view: "everything Git took down, newest transition first".
CREATE INDEX IF NOT EXISTS idx_capability_items_git_lifecycle_reason
    ON capability_items (git_lifecycle_reason, git_lifecycle_changed_at DESC)
    WHERE git_lifecycle_reason IS NOT NULL;

-- Serves both directions of the freshness rule on the Git-backed subset:
--   * public projection      — WHERE git_visibility_verified_at >= now() - '10 min'
--   * re-verification drain  — ORDER BY git_visibility_verified_at ASC NULLS FIRST
-- NULLS FIRST is declared so the never-verified rows sort at the head of the
-- drain without a separate index.
CREATE INDEX IF NOT EXISTS idx_capability_items_git_visibility_verified
    ON capability_items (git_visibility_verified_at NULLS FIRST)
    WHERE content_backend = 'git';

COMMENT ON COLUMN capability_items.git_lifecycle_reason IS 'Git-owned archive cause: manifest_removed | default_branch_missing | repository_deleted. NULL = Git holds no archive claim on this row. Recoverable: manifest_removed, default_branch_missing. Terminal: repository_deleted. A manual archived/inactive/banned write clears it in the same transaction.';
COMMENT ON COLUMN capability_items.git_lifecycle_changed_at IS 'When the transition represented by git_lifecycle_reason was observed. Required whenever a reason is present; deliberately NOT cleared with the reason so the last Git transition stays auditable.';
COMMENT ON COLUMN capability_items.git_visibility_verified_at IS 'Last successful Gitea visibility/access verification. Public browse/search requires freshness (10-minute default); NULL or stale fails closed and hides the row.';

-- +goose Down

DROP INDEX IF EXISTS idx_capability_items_git_visibility_verified;
DROP INDEX IF EXISTS idx_capability_items_git_lifecycle_reason;

ALTER TABLE capability_items
    DROP CONSTRAINT IF EXISTS chk_capability_items_git_lifecycle_reason;

ALTER TABLE capability_items
    DROP COLUMN IF EXISTS git_visibility_verified_at,
    DROP COLUMN IF EXISTS git_lifecycle_changed_at,
    DROP COLUMN IF EXISTS git_lifecycle_reason;
