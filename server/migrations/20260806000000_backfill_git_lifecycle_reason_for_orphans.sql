-- Give every row Git had ALREADY archived a lifecycle reason, before the
-- recovery rule starts requiring one.
--
-- Why this is not optional
-- ------------------------
-- 20260805000000 added git_lifecycle_reason NULL for every existing row. Phase C
-- then tightened the auto-reactivation predicate in
-- services.gitCapabilityActivateStatus from
--
--     git_sync_status = 'orphaned'
--
-- to
--
--     git_sync_status = 'orphaned' AND git_lifecycle_reason IN (recoverable)
--
-- because the reason is the half a human moderation write clears, and clearing
-- it is how "I hid this deliberately" revokes Git's standing permission to put
-- the row back. Without that half, an admin archiving an already-orphaned row
-- could not revoke anything and the next returning manifest republished it.
--
-- The cost of the tightening is this migration: a row orphaned by an EARLIER
-- deployment carries NULL, so under the new predicate it would never be
-- reactivated — its manifest could come back and the capability would stay dark
-- forever, silently. Backfilling the reason restores exactly the behaviour those
-- rows had before.
--
-- Why 'manifest_removed' specifically
-- ----------------------------------
-- git_sync_status='orphaned' is written by exactly two archive paths: a manifest
-- missing from HEAD, and a default branch that could not be resolved. Both are
-- recoverable and both recover under the same condition (the manifest is present
-- at HEAD of the current default branch), so the distinction changes no
-- behaviour here. 'manifest_removed' is the overwhelmingly more common of the
-- two, and the first sync that observes such a row rewrites the reason with the
-- one it actually measured — so a mislabelled row is self-correcting within one
-- reconcile interval.
--
-- Scope, stated so the predicate is not mistaken for a broader claim:
--   * content_backend = 'git'    — db-backed rows have no Git lifecycle at all.
--   * git_sync_status='orphaned' — ONLY rows this sync itself hid. A row a human
--     archived has 'synced' and is deliberately left with NULL: Git holds no
--     claim on it, and inventing one would block the moderator from ever
--     re-activating their own decision.
--   * git_lifecycle_reason IS NULL — never overwrite an observed reason,
--     including the terminal 'repository_deleted'.
--
-- Idempotent by that last predicate: a second run matches nothing.
--
-- git_lifecycle_changed_at is required by chk_capability_items_git_lifecycle_reason
-- whenever a reason is present. The true transition time is not recoverable
-- (nothing recorded it), so the best available approximation is used and the
-- fallback to now() covers rows that were never successfully synced.

-- +goose Up

UPDATE capability_items
SET git_lifecycle_reason     = 'manifest_removed',
    git_lifecycle_changed_at = COALESCE(git_last_synced_at, updated_at, now())
WHERE content_backend = 'git'
  AND git_sync_status = 'orphaned'
  AND git_lifecycle_reason IS NULL;

-- +goose Down

-- Reverting means putting those rows back into the state where the OLD predicate
-- is the one in force. Narrowed to the value this migration writes so a reason
-- observed by a real sync afterwards is not erased; a row that genuinely became
-- manifest_removed after this ran is indistinguishable and is accepted as the
-- cost of a rollback (the next sync re-establishes it).
UPDATE capability_items
SET git_lifecycle_reason     = NULL,
    git_lifecycle_changed_at = NULL
WHERE content_backend = 'git'
  AND git_sync_status = 'orphaned'
  AND git_lifecycle_reason = 'manifest_removed';
