-- +goose Up
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS source VARCHAR(255) NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE capability_items
  DROP COLUMN IF EXISTS source;
