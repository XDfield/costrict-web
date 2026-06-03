-- +goose Up
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS forked_from_item_id  UUID,
  ADD COLUMN IF NOT EXISTS forked_from_owner_id TEXT;

-- Plain index serves the "forked_from_item_id IS NULL" public-browse filter and forkCount aggregation.
CREATE INDEX IF NOT EXISTS idx_items_forked_from_item
  ON capability_items (forked_from_item_id);

-- Enforce "each user may fork a given source item at most once" at the DB level
-- (partial: only fork rows participate, so normal items are unaffected).
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_unique_user_fork
  ON capability_items (forked_from_item_id, created_by)
  WHERE forked_from_item_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_items_unique_user_fork;
DROP INDEX IF EXISTS idx_items_forked_from_item;

ALTER TABLE capability_items
  DROP COLUMN IF EXISTS forked_from_owner_id,
  DROP COLUMN IF EXISTS forked_from_item_id;
