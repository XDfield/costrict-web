-- +goose Up
ALTER TABLE security_scans
  ADD COLUMN IF NOT EXISTS builtin_tags JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down
ALTER TABLE security_scans
  DROP COLUMN IF EXISTS builtin_tags;
