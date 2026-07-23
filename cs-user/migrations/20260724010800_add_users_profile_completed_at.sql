-- +goose Up
-- +goose StatementBegin

-- Adds profile_completed_at to users (REGISTRATION_PROFILE_DESIGN §4.2).
-- Nullable: NULL means the user has not yet finished the first-time
-- registration flow (custom username + display_name per RegFormComplete).
-- Existing rows are backfilled to created_at so the rollout treats every
--存量 user as already registered — the gate middleware (server side) only
-- fires for users created AFTER this migration.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS profile_completed_at TIMESTAMPTZ;

UPDATE users
   SET profile_completed_at = COALESCE(created_at, NOW())
 WHERE profile_completed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_users_profile_completed_at
    ON users (profile_completed_at)
 WHERE profile_completed_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_users_profile_completed_at;
ALTER TABLE users DROP COLUMN IF EXISTS profile_completed_at;

-- +goose StatementEnd
