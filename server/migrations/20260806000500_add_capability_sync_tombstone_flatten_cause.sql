-- `migrate flatten-plugins` gets its own removal cause.
--
-- The command archives package-derived Plugin child rows under the flat
-- capability model, and (since the F-27 fix) tombstones every holder of a row it
-- archived. It had to borrow `admin_archived`/`moderation` to say so, because
-- that was the only existing triple whose every OTHER claim was true. One claim
-- was not: no moderator looked at anything. Nobody judged the capability's
-- content, and nobody decided it should not be on the shelf. A data migration
-- retired a row that was a duplicate projection of a file inside a package.
--
-- The contract's rule is that reason and effect must be truthful — a
-- legal-but-false row is the same disease as an internally inconsistent one, one
-- layer up — and the reason travels to the device, where csc logs it verbatim
-- and shows the user wording derived from it. "An administrator archived this"
-- points a user asking why their capability vanished at a moderation decision
-- that never happened, and at a moderator who never saw it.
--
-- Why this costs nothing to add
-- -----------------------------
-- The reason set is OPEN by contract (safe-lifecycle-contract.md): the
-- tombstone's presence IS the removal instruction and `reason` only explains it,
-- so csc removes on a reason it does not recognise, reports the string verbatim
-- in diagnostics, and falls back to generic user-facing wording. Verified on the
-- deployed client at csc 7552786d7: `client.ts` parses `reason` as a non-empty
-- string and never compares its value, `reconcileCloudPlugins.ts` and `sync.ts`
-- log it without testing it, and `contract.ts:describeTombstoneReason` has a
-- `default` branch. No allowlist exists on either side. So this migration needs
-- no client release, no legacy-drain window and no minimum-version gate — which
-- is exactly the debt the open-set decision was made to avoid, now being spent
-- for the first time.
--
-- Why `data_migration` and not `migration`
-- ----------------------------------------
-- This table already carries `moderation`, and `migration` differs from it by
-- three letters in the middle of a nine-character word. A source read at a
-- glance in a log line, an artifact or a psql column is exactly what this
-- constraint work exists to keep unambiguous, so the two are kept visibly apart.
-- It also matches the existing two-word `git_lifecycle` shape.
--
-- NULL handling, for the third time on this table
-- -----------------------------------------------
-- `NULL IN (...)` evaluates to NULL and a CHECK that evaluates to NULL PASSES.
-- The new branch therefore tests `lifecycle_reason IS NULL` and nothing else:
-- a `package_flattened` row carrying ANY lifecycle reason must be REJECTED, and
-- a branch written as a membership test would either accept it outright or
-- accept it by way of an unknown truth value. This trap has already been hit
-- once on this table (20260805000700).
--
-- Rollback
-- --------
-- The Down block restores the five-triple constraint, and ADD CONSTRAINT
-- validates existing rows — so it FAILS while any `package_flattened` row
-- exists. That is deliberate, and identical to 20260806000300's reasoning:
-- deleting those rows to make the rollback succeed would tell every device that
-- has not yet polled that the removal never happened, and the capability would
-- stay installed forever. An operator rolling back must decide what to do with
-- them explicitly. Note that `migrate flatten-plugins rollback-apply` is the
-- supported way to undo the archive itself; it reactivates the item, and an
-- active item supersedes its own older tombstone at materialization time
-- without the row having to be deleted.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE capability_sync_tombstones
    DROP CONSTRAINT IF EXISTS chk_capability_sync_tombstones_cause;
ALTER TABLE capability_sync_tombstones
    ADD CONSTRAINT chk_capability_sync_tombstones_cause
    CHECK (
        (reason = 'git_archived'
            AND source = 'git_lifecycle'
            AND lifecycle_reason IS NOT NULL
            AND lifecycle_reason IN (
                'manifest_removed',
                'default_branch_missing',
                'repository_deleted'
            ))
        OR (reason = 'unfavorited'
            AND source = 'favorite'
            AND lifecycle_reason IS NULL)
        OR (reason = 'distribution_revoked'
            AND source = 'distribution'
            AND lifecycle_reason IS NULL)
        OR (reason = 'admin_archived'
            AND source = 'moderation'
            AND lifecycle_reason IS NULL)
        OR (reason = 'item_deleted'
            AND source = 'catalog'
            AND lifecycle_reason IS NULL)
        OR (reason = 'package_flattened'
            AND source = 'data_migration'
            AND lifecycle_reason IS NULL)
    );

COMMENT ON COLUMN capability_sync_tombstones.reason IS 'git_archived | unfavorited | distribution_revoked | admin_archived | item_deleted | package_flattened; paired with source and lifecycle_reason by chk_capability_sync_tombstones_cause. The client-facing reason set is open: an unrecognised reason still removes.';
COMMENT ON COLUMN capability_sync_tombstones.source IS 'git_lifecycle | favorite | distribution | moderation | catalog | data_migration; determined by reason, enforced as a triple';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE capability_sync_tombstones
    DROP CONSTRAINT IF EXISTS chk_capability_sync_tombstones_cause;
ALTER TABLE capability_sync_tombstones
    ADD CONSTRAINT chk_capability_sync_tombstones_cause
    CHECK (
        (reason = 'git_archived'
            AND source = 'git_lifecycle'
            AND lifecycle_reason IS NOT NULL
            AND lifecycle_reason IN (
                'manifest_removed',
                'default_branch_missing',
                'repository_deleted'
            ))
        OR (reason = 'unfavorited'
            AND source = 'favorite'
            AND lifecycle_reason IS NULL)
        OR (reason = 'distribution_revoked'
            AND source = 'distribution'
            AND lifecycle_reason IS NULL)
        OR (reason = 'admin_archived'
            AND source = 'moderation'
            AND lifecycle_reason IS NULL)
        OR (reason = 'item_deleted'
            AND source = 'catalog'
            AND lifecycle_reason IS NULL)
    );

COMMENT ON COLUMN capability_sync_tombstones.reason IS 'git_archived | unfavorited | distribution_revoked | admin_archived | item_deleted; paired with source and lifecycle_reason by chk_capability_sync_tombstones_cause. The client-facing reason set is open: an unrecognised reason still removes.';
COMMENT ON COLUMN capability_sync_tombstones.source IS 'git_lifecycle | favorite | distribution | moderation | catalog; determined by reason, enforced as a triple';

-- +goose StatementEnd
