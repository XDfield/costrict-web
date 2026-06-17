-- +goose Up
-- catalog_entry_dir holds the synthetic "<type-dir>/<entry-id>" key used to
-- match a catalog-ingested row back to its upstream entry across re-ingests.
-- It is decoupled from source_path so source_path can store the faithful,
-- repo-relative path that the plugin "work tree" mirrors.
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS catalog_entry_dir TEXT NOT NULL DEFAULT '';

-- Indexed for the per-entry row lookup during ingest reconciliation.
CREATE INDEX IF NOT EXISTS idx_item_catalog_entry_dir
  ON capability_items (catalog_entry_dir);

-- +goose Down
DROP INDEX IF EXISTS idx_item_catalog_entry_dir;

ALTER TABLE capability_items
  DROP COLUMN IF EXISTS catalog_entry_dir;
