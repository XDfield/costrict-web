-- Partition item_tags by writer domain.
--
-- Three writers rebuild the tag set of the same item with DELETE + re-insert:
-- the user API (POST /items/:id/tags), the Git capability sync worker (every
-- push), and the system side (security scan backfill / catalog ingest / legacy
-- registry sync). Without a domain marker the last writer wins and silently
-- drops the other two writers' tags -- most visibly, tags a user set in Cloud
-- disappear on the next Git push.
--
-- After this migration every writer only deletes rows inside its own domain:
--   git    -- manifest frontmatter `tags` on Git-backed capabilities
--   system -- security scan backfill, catalog ingest, legacy registry sync
--   user   -- tags a user set through the Cloud API
--
-- Existing rows are backfilled to 'legacy' rather than guessed into one of the
-- three domains: their writer cannot be reconstructed, and mislabeling them
-- would let that writer delete a batch of real data on its next rebuild. No
-- writer ever deletes 'legacy' rows. The cost is that legacy rows are never
-- reclaimed automatically; that is accepted for this round and there is
-- deliberately no cleanup path.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE item_tags ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'legacy';

-- The old key (item_id, tag_id) makes the domains collide: when Git frontmatter
-- and the user both carry tag `auth`, the second insert raises a unique
-- violation that SetItemTags swallows, so the tag only ever exists in one
-- domain and vanishes when that domain is rebuilt. Widen the key so each domain
-- can own its own row for the same tag.
ALTER TABLE item_tags DROP CONSTRAINT IF EXISTS uq_item_tag;
ALTER TABLE item_tags DROP CONSTRAINT IF EXISTS uq_item_tag_source;
ALTER TABLE item_tags ADD CONSTRAINT uq_item_tag_source UNIQUE (item_id, tag_id, source);

CREATE INDEX IF NOT EXISTS idx_item_tags_item_source ON item_tags (item_id, source);

COMMENT ON COLUMN item_tags.source IS 'Writer domain: git | system | user | legacy. Each writer rebuilds only its own domain; legacy rows (pre-partition, unattributable) are never deleted by any writer.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_item_tags_item_source;

-- Collapsing the domains can resurrect (item_id, tag_id) duplicates that were
-- legal only while `source` existed, so drop the extra copies before restoring
-- the narrow key.
DELETE FROM item_tags a
USING item_tags b
WHERE a.item_id = b.item_id
  AND a.tag_id = b.tag_id
  AND a.ctid > b.ctid;

ALTER TABLE item_tags DROP CONSTRAINT IF EXISTS uq_item_tag_source;
ALTER TABLE item_tags ADD CONSTRAINT uq_item_tag UNIQUE (item_id, tag_id);
ALTER TABLE item_tags DROP COLUMN IF EXISTS source;

-- +goose StatementEnd
