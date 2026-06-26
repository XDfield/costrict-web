-- Add partial unique index on workspaces(user_id, name) for active records.
-- This prevents duplicate active workspace names per user while allowing
-- soft-deleted rows with the same name to coexist.
-- Without this index, GORM's soft-delete means the application-level
-- uniqueness check can miss race conditions on PostgreSQL.

-- +goose NO TRANSACTION

-- +goose Up
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_workspaces_user_name_active
    ON workspaces (user_id, name)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_workspaces_user_name_active;
