-- Stable identity and sync state for the incremental Git-backed capability index.
-- DB-backed rows retain their defaults and are excluded from the partial unique index.

-- +goose Up
ALTER TABLE capability_items
    ADD COLUMN IF NOT EXISTS source_git_server_id VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_git_repo_id BIGINT NOT NULL DEFAULT 0,
	ADD COLUMN IF NOT EXISTS source_git_entry_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS git_sha VARCHAR(40) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS git_last_synced_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS git_sync_status VARCHAR(16) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS git_sync_error TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_capability_items_git_repo
    ON capability_items (source_git_server_id, source_git_repo_id)
    WHERE content_backend = 'git';

CREATE UNIQUE INDEX IF NOT EXISTS uq_capability_items_git_manifest
    ON capability_items (source_git_server_id, source_git_repo_id, source_repo_path, source_git_entry_key)
    WHERE content_backend = 'git'
      AND source_git_server_id <> ''
      AND source_git_repo_id > 0
      AND source_repo_path <> '';

-- +goose Down
DROP INDEX IF EXISTS uq_capability_items_git_manifest;
DROP INDEX IF EXISTS idx_capability_items_git_repo;

ALTER TABLE capability_items
    DROP COLUMN IF EXISTS git_sync_error,
    DROP COLUMN IF EXISTS git_sync_status,
    DROP COLUMN IF EXISTS git_last_synced_at,
    DROP COLUMN IF EXISTS git_sha,
	DROP COLUMN IF EXISTS source_git_entry_key,
    DROP COLUMN IF EXISTS source_git_repo_id,
    DROP COLUMN IF EXISTS source_git_server_id;
