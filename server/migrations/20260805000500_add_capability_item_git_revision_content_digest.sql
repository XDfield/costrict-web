-- The trigger for a revision row: the item's OWN projected content digest.
--
-- Why this column has to exist at all
-- -----------------------------------
-- The first cut of this table appended a revision whenever the repository's
-- default-branch head SHA moved. Measured against production-shaped data on
-- 2026-08-06, that is wrong for almost every capability there is:
--
--     538 git-backed items: 66 repositories hold more than one bound
--     capability, covering 507 items (94%); the largest holds 55.
--
-- A push that edits one manifest moves the head of the repository, not of an
-- item. Under a head-SHA trigger every sibling in that repository would receive
-- a revision for a commit that never touched it, so an item's "5 most recent
-- revisions" would mostly be other capabilities' work, and one push to the
-- largest repository would write 55 rows. The head SHA is therefore demoted to
-- a recorded coordinate, and the trigger becomes this digest.
--
-- What the digest covers
-- ----------------------
-- Exactly what the item projects to a reader, and nothing a sibling shares:
-- the item's projected content plus the manifest-derived display fields
-- (name / description / category / version / manifest metadata). Repository
-- level facts — head SHA, remote URL, default branch, full name — are excluded
-- BY CONSTRUCTION, because including any of them would reintroduce exactly the
-- sibling coupling described above. See services.gitCapabilityProjectionDigest
-- for the canonical serialization; this column stores its lowercase SHA-256.
--
-- The comparison is against the item's CURRENT digest, never against the set of
-- digests it has ever had. A revert back to earlier content is a new transition
-- and appends a row; a duplicate delivery, a same-content reconcile and a
-- sibling's commit all append nothing.
--
-- Why it is nullable, and what NULL means
-- ---------------------------------------
-- NULL means "synthesized baseline whose digest was never observed". It exists
-- for exactly one producer: `migrate backfill-git-revisions`, which seeds
-- revision 1 for rows that were already synced before this writer existed. That
-- command works from the database alone, and a Git-backed row does not store
-- its content — the content lives in the repository — so the baseline digest is
-- not computable at backfill time and inventing one would be a lie that the
-- next sync would then diff against.
--
-- The second CHECK confines that hole to its one producer: any other source
-- must carry a digest, so no future writer can quietly append a digest-less row
-- and leave the trigger unable to compare.
--
-- A NULL baseline is completed, not appended to: the first successful
-- projection adopts the digest it observes into that row (a compare-and-set on
-- `content_digest IS NULL`, see services.adoptGitCapabilityBaselineDigest) and
-- appends nothing. The alternative — treating "unknown" as "different" — would
-- write one spurious revision for every backfilled item on the first sync after
-- deployment, which is the noise this whole change exists to remove. The
-- accepted cost is stated rather than hidden: if an item's content changed
-- between the backfill and its first post-deployment projection, that single
-- change is folded into the baseline instead of being recorded. Running the
-- backfill immediately before enabling revision writes keeps that window to
-- minutes, and reconcile visits every healthy binding within its freshness
-- interval.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE capability_item_git_revisions
    ADD COLUMN IF NOT EXISTS content_digest VARCHAR(64);

ALTER TABLE capability_item_git_revisions
    DROP CONSTRAINT IF EXISTS chk_capability_item_git_revisions_digest_format;
ALTER TABLE capability_item_git_revisions
    ADD CONSTRAINT chk_capability_item_git_revisions_digest_format
    CHECK (content_digest IS NULL OR content_digest ~ '^[0-9a-f]{64}$');

-- Only the backfill may omit the digest. Spelled as "present OR backfill" so a
-- NULL never reaches an IN-list: `NULL IN (...)` is NULL, and a CHECK that
-- evaluates to NULL PASSES — the same trap the tombstone lifecycle CHECK had to
-- be written around.
ALTER TABLE capability_item_git_revisions
    DROP CONSTRAINT IF EXISTS chk_capability_item_git_revisions_digest_source;
ALTER TABLE capability_item_git_revisions
    ADD CONSTRAINT chk_capability_item_git_revisions_digest_source
    CHECK (content_digest IS NOT NULL OR source = 'backfill');

COMMENT ON COLUMN capability_item_git_revisions.content_digest IS 'Lowercase SHA-256 over this item''s own projected content and manifest-derived display fields; the append trigger. NULL only on a backfilled baseline whose digest was never observed';
COMMENT ON COLUMN capability_item_git_revisions.git_sha IS 'Repository default-branch commit observed when this change was detected. A recorded coordinate, NOT the trigger and not necessarily the commit that made the change';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE capability_item_git_revisions
    DROP CONSTRAINT IF EXISTS chk_capability_item_git_revisions_digest_source;
ALTER TABLE capability_item_git_revisions
    DROP CONSTRAINT IF EXISTS chk_capability_item_git_revisions_digest_format;
ALTER TABLE capability_item_git_revisions
    DROP COLUMN IF EXISTS content_digest;

COMMENT ON COLUMN capability_item_git_revisions.git_sha IS 'Repository default-branch commit SHA used for this projection, not a manifest blob hash';

-- +goose StatementEnd
