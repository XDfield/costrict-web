-- +goose Up
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS descriptions JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE capability_versions
  ADD COLUMN IF NOT EXISTS descriptions JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
ALTER TABLE capability_versions
  DROP COLUMN IF EXISTS descriptions;

ALTER TABLE capability_items
  DROP COLUMN IF EXISTS descriptions;
