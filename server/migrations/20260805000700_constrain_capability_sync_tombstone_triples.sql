-- Review finding F-2: reason / source / lifecycle_reason were constrained
-- separately, so no constraint described a legal tombstone.
--
-- Three independent CHECKs accept the cross product minus a few cells. Two rows
-- the contract forbids passed all three, because the lifecycle CHECK keyed off
-- `source` while the reason CHECK keyed off `reason` and neither looked at the
-- other:
--
--     reason='git_archived',  source='favorite',      lifecycle_reason=NULL
--     reason='unfavorited',   source='git_lifecycle', lifecycle_reason='manifest_removed'
--
-- The first is precisely the row the lifecycle CHECK was written to prevent — a
-- Git tombstone with no Git cause — reached by lying about `source` instead of
-- about `lifecycle_reason`. The second attributes a favourite the user removed
-- to a repository event that did not happen. csc reads `reason`, `source` and
-- `lifecycleReason` as one statement about why an entitlement ended, so a row
-- whose three fields disagree is not a smaller error than a missing field: it
-- is a false one.
--
-- The three CHECKs are therefore replaced by ONE that enumerates the legal
-- triples. Not added alongside them: keeping the enum CHECKs as well would put
-- the set of valid reasons in two places, and the next reason added to one and
-- not the other is a constraint that contradicts itself. The enumeration below
-- subsumes both enum checks — a value outside it matches no branch and is
-- rejected — so nothing is weakened by removing them.
--
-- `source` is determined by `reason` here, and deliberately so. The pairing is
-- the contract's own: an entitlement ends because the Git lifecycle archived the
-- item, because the user unfavorited it, or because a distribution was revoked,
-- and each of those has exactly one producing subsystem. The snapshot payload
-- carries both fields for the client's benefit; the database's job is to make
-- them impossible to disagree.
--
-- NULL handling is spelled out for the same reason it is in the original
-- lifecycle CHECK: `NULL IN (...)` evaluates to NULL, a CHECK that evaluates to
-- NULL PASSES, and the row that slips through is exactly the one that must not.
-- Every branch tests lifecycle_reason with IS NULL / IS NOT NULL before any
-- membership test, so no branch can ever evaluate to NULL.

-- +goose Up
-- +goose StatementBegin

-- PostgreSQL validates an ADD CONSTRAINT against existing rows. Do that
-- validation explicitly before removing the older checks, so an operator gets
-- a count and a repair path instead of an opaque ALTER TABLE 23514. Goose runs
-- this migration transactionally, so the exception leaves both the data and
-- the old constraints untouched.
DO $$
DECLARE
    invalid_tombstone_count BIGINT;
BEGIN
    SELECT COUNT(*)
    INTO invalid_tombstone_count
    FROM capability_sync_tombstones
    WHERE NOT (
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

    IF invalid_tombstone_count > 0 THEN
        RAISE EXCEPTION
            'capability_sync_tombstones has % rows that do not match a legal reason/source/lifecycle_reason triple',
            invalid_tombstone_count
            USING ERRCODE = 'check_violation',
                  HINT = 'Repair each tombstone through its owning lifecycle, favorite, or distribution workflow, then rerun this migration. Do not delete tombstones to bypass this check.';
    END IF;
END $$;

ALTER TABLE capability_sync_tombstones
    DROP CONSTRAINT IF EXISTS chk_capability_sync_tombstones_reason;
ALTER TABLE capability_sync_tombstones
    DROP CONSTRAINT IF EXISTS chk_capability_sync_tombstones_source;
ALTER TABLE capability_sync_tombstones
    DROP CONSTRAINT IF EXISTS chk_capability_sync_tombstones_lifecycle_reason;

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

-- +goose Down
-- +goose StatementBegin

ALTER TABLE capability_sync_tombstones
    DROP CONSTRAINT IF EXISTS chk_capability_sync_tombstones_cause;

ALTER TABLE capability_sync_tombstones
    ADD CONSTRAINT chk_capability_sync_tombstones_reason
    CHECK (reason IN ('git_archived', 'unfavorited', 'distribution_revoked'));
ALTER TABLE capability_sync_tombstones
    ADD CONSTRAINT chk_capability_sync_tombstones_source
    CHECK (source IN ('git_lifecycle', 'favorite', 'distribution'));
ALTER TABLE capability_sync_tombstones
    ADD CONSTRAINT chk_capability_sync_tombstones_lifecycle_reason
    CHECK (
        (source = 'git_lifecycle'
            AND lifecycle_reason IS NOT NULL
            AND lifecycle_reason IN (
                'manifest_removed',
                'default_branch_missing',
                'repository_deleted'
            ))
        OR (source <> 'git_lifecycle' AND lifecycle_reason IS NULL)
    );

COMMENT ON COLUMN capability_sync_tombstones.reason IS 'git_archived | unfavorited | distribution_revoked';
COMMENT ON COLUMN capability_sync_tombstones.source IS 'git_lifecycle | favorite | distribution';

-- +goose StatementEnd
