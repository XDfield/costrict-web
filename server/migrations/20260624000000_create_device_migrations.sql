-- +goose Up
-- Tracks device_id migrations from hash-based legacy IDs to random v2 IDs.
-- Each row records: old hash-based device_id → new random device_id, scoped by user_id.
-- Used by migrateFromLegacyDeviceID to offer recovery when device_v2.json is lost.
CREATE TABLE IF NOT EXISTS device_migrations (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  old_device_id VARCHAR(191) NOT NULL,
  new_device_id VARCHAR(191) NOT NULL,
  user_id       VARCHAR(191) NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Composite index for user-scoped lookup: WHERE old_device_id = ? AND user_id = ?
CREATE INDEX IF NOT EXISTS idx_migration_old_user
  ON device_migrations (old_device_id, user_id);

-- Index for reverse lookup by new_device_id
CREATE INDEX IF NOT EXISTS idx_migration_new_device_id
  ON device_migrations (new_device_id);

-- +goose Down
DROP INDEX IF EXISTS idx_migration_new_device_id;
DROP INDEX IF EXISTS idx_migration_old_user;
DROP TABLE IF EXISTS device_migrations;
