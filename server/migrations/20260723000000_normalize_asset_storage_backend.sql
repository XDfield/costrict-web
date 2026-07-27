-- +goose Up
-- Text assets live entirely in PostgreSQL. Keep storage backend/key empty so
-- callers can distinguish them from externally stored binary assets.

ALTER TABLE capability_assets
    ALTER COLUMN storage_backend SET DEFAULT '';
ALTER TABLE capability_version_assets
    ALTER COLUMN storage_backend SET DEFAULT '';

UPDATE capability_assets
SET storage_backend = '', storage_key = ''
WHERE text_content IS NOT NULL;

UPDATE capability_version_assets
SET storage_backend = '', storage_key = ''
WHERE text_content IS NOT NULL;

-- Import bundles are also stored objects. Record their backend so queued and
-- previewed jobs cannot be read from a different deployment mode.
ALTER TABLE capability_import_jobs
    ADD COLUMN IF NOT EXISTS storage_backend TEXT NOT NULL DEFAULT '';

UPDATE capability_import_jobs
SET storage_backend = 'local'
WHERE source_kind = 'upload'
  AND storage_key <> ''
  AND storage_backend = '';

-- +goose Down

ALTER TABLE capability_import_jobs
    DROP COLUMN IF EXISTS storage_backend;

ALTER TABLE capability_assets
    ALTER COLUMN storage_backend SET DEFAULT 'local';
ALTER TABLE capability_version_assets
    ALTER COLUMN storage_backend SET DEFAULT 'local';
