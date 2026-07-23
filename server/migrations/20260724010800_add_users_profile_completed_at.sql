-- +goose Up
-- +goose StatementBegin

-- Adds profile_completed_at to server's local users mirror (REGISTRATION_PROFILE_DESIGN §4.2).
-- Server is single-tenant; cs-user already holds the (tenant_id, username)
-- composite unique. This column is the gate signal consumed by the new
-- RequireProfileComplete middleware on the OAuth-authenticated API surface.
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
