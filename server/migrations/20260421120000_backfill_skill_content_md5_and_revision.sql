-- +goose Up
ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS content_md5 VARCHAR(32) NOT NULL DEFAULT '';

ALTER TABLE capability_items
  ADD COLUMN IF NOT EXISTS current_revision INTEGER NOT NULL DEFAULT 1;

ALTER TABLE capability_versions
  ADD COLUMN IF NOT EXISTS content_md5 VARCHAR(32) NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE capability_versions
  DROP COLUMN IF EXISTS content_md5;

ALTER TABLE capability_items
  DROP COLUMN IF EXISTS current_revision;

ALTER TABLE capability_items
  DROP COLUMN IF EXISTS content_md5;
