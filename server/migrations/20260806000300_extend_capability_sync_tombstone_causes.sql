-- Review finding F-27: the two moderation/catalog removal paths had no legal
-- way to say what they had done, so they said nothing at all.
--
-- An admin taking an item off the shelf (adminitem.SetStatus / PUT /items/:id)
-- and an operator hard-deleting one both end every holder's entitlement, but
-- neither wrote a tombstone. Under snapshot v1 that was survivable because csc
-- inferred removal from absence. Under v2 absence is a no-op by construction —
-- so the same two actions would leave the capability installed on every device
-- forever, with nothing anywhere reporting a fault.
--
-- Why two NEW reasons rather than reusing an existing one
-- ------------------------------------------------------
-- Every existing triple is a claim about a specific subsystem, and each of the
-- three available lies is worse than it looks:
--
--   git_archived         requires a lifecycle_reason, i.e. a manifest/branch/
--                        repository event that did not happen. It is also the
--                        one reason the snapshot service suppresses while the
--                        Git rollout flag is off, so a moderation take-down
--                        wearing it would be silently swallowed on every
--                        deployment that has not enabled Git yet — the failure
--                        this finding is about, reintroduced by the fix.
--   unfavorited          claims the user removed it themselves. They did not,
--                        and the client reports the reason to them.
--   distribution_revoked points a user who merely favorited the item at a
--                        distribution that never existed.
--
-- The contract's rule is that reason and effect must be truthful: a
-- legal-but-false row is the same disease as an internally inconsistent one,
-- one layer up. So moderation and catalog removal get their own names.
--
-- The reason set is open by contract (see safe-lifecycle-contract.md): a
-- tombstone's presence IS the removal instruction and `reason` only explains
-- it, so a client that does not recognise `admin_archived` still removes the
-- capability and reports the string verbatim. Adding a reason therefore needs
-- no client release and no minimum-version gate.
--
-- NULL handling, again
-- --------------------
-- `NULL IN (...)` evaluates to NULL and a CHECK that evaluates to NULL PASSES.
-- Every branch below therefore tests lifecycle_reason with IS NULL / IS NOT
-- NULL *before* any membership test, so no branch can evaluate to NULL and the
-- disjunction is always a definite TRUE or FALSE. This exact trap was hit once
-- already on this table (20260805000700) — the two new branches are written the
-- same way for the same reason: `admin_archived` carrying a lifecycle_reason
-- must be REJECTED, not accepted by way of an unknown truth value.
--
-- Rollback
-- --------
-- The Down block restores the three-triple constraint, and ADD CONSTRAINT
-- validates existing rows — so it FAILS while any admin_archived / item_deleted
-- row exists. That is deliberate. Deleting those rows to make the rollback
-- succeed would tell every device that had not yet polled that the removal
-- never happened, and the capability would stay installed. An operator rolling
-- back must decide what to do with them explicitly.

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
    );

COMMENT ON COLUMN capability_sync_tombstones.reason IS 'git_archived | unfavorited | distribution_revoked | admin_archived | item_deleted; paired with source and lifecycle_reason by chk_capability_sync_tombstones_cause. The client-facing reason set is open: an unrecognised reason still removes.';
COMMENT ON COLUMN capability_sync_tombstones.source IS 'git_lifecycle | favorite | distribution | moderation | catalog; determined by reason, enforced as a triple';

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
    );

COMMENT ON COLUMN capability_sync_tombstones.reason IS 'git_archived | unfavorited | distribution_revoked; paired with source and lifecycle_reason by chk_capability_sync_tombstones_cause';
COMMENT ON COLUMN capability_sync_tombstones.source IS 'git_lifecycle | favorite | distribution; determined by reason, enforced as a triple';

-- +goose StatementEnd
