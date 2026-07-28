-- +goose Up
-- Add explicitly_unbound column to user_auth_identities table
-- This flag tracks whether an identity was explicitly unbound by the user
-- to prevent automatic re-binding through JWT token processing

ALTER TABLE user_auth_identities ADD COLUMN explicitly_unbound BOOLEAN NOT NULL DEFAULT false;
COMMENT ON COLUMN user_auth_identities.explicitly_unbound IS 'True if user explicitly unbound this identity provider, prevents auto-rebinding';

-- +goose Down
ALTER TABLE user_auth_identities DROP COLUMN IF EXISTS explicitly_unbound;
