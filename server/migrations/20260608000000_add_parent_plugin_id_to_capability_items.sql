-- +goose Up
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS parent_plugin_id UUID;

-- Plain index serves the "list sub-skills of a plugin" (parent_plugin_id = ?) lookup
-- and the "exclude sub-skills" (parent_plugin_id IS NULL) public-browse filter.
CREATE INDEX IF NOT EXISTS idx_item_parent_plugin
  ON capability_items (parent_plugin_id);

-- +goose Down
DROP INDEX IF EXISTS idx_item_parent_plugin;

ALTER TABLE capability_items
  DROP COLUMN IF EXISTS parent_plugin_id;
