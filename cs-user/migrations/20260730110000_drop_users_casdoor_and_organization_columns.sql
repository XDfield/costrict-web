-- +goose Up
-- +goose StatementBegin

-- Drop legacy Casdoor-linking columns and organization from users.
--
-- universal_id / sub / id are login-source attributes and belong on
-- user_auth_identities (external_key = `casdoor:<provider>:<universal_id>`),
-- NOT on the business-user row. Keeping a copy here led to:
--   * two sources of truth for the same identity (users.casdoor_universal_id
--     vs user_auth_identities.external_subject), drifting on rebind/unbind;
--   * multi-binding ambiguity — users.* can only hold one value while a user
--     may legitimately carry multiple Casdoor identities.
--
-- organization (Casdoor owner claim) is likewise a login-source attribute and
-- is being relocated to a dedicated module; the column on users is dropped.
--
-- Downgrading code paths that still write these columns will fail loudly on
-- the next INSERT/UPDATE; rollout order is migration-first.

DROP INDEX IF EXISTS idx_user_casdoor_sub;
DROP INDEX IF EXISTS idx_user_casdoor_universal_id;
DROP INDEX IF EXISTS idx_user_casdoor_id;
DROP INDEX IF EXISTS idx_user_organization;

ALTER TABLE users DROP COLUMN IF EXISTS casdoor_id;
ALTER TABLE users DROP COLUMN IF EXISTS casdoor_universal_id;
ALTER TABLE users DROP COLUMN IF EXISTS casdoor_sub;
ALTER TABLE users DROP COLUMN IF EXISTS organization;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restores the legacy columns for rollback. Data is not recoverable once
-- dropped — these columns are repopulated by the next OAuth callback per
-- user (GetOrCreateUser's external_key path no longer writes them, so the
-- restore is structural only).
ALTER TABLE users ADD COLUMN IF NOT EXISTS casdoor_id VARCHAR(191);
ALTER TABLE users ADD COLUMN IF NOT EXISTS casdoor_universal_id VARCHAR(191);
ALTER TABLE users ADD COLUMN IF NOT EXISTS casdoor_sub VARCHAR(191);
ALTER TABLE users ADD COLUMN IF NOT EXISTS organization VARCHAR(191);

CREATE INDEX IF NOT EXISTS idx_user_casdoor_id ON users (casdoor_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_universal_id ON users (casdoor_universal_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_sub ON users (casdoor_sub);
CREATE INDEX IF NOT EXISTS idx_user_organization ON users (organization);

-- +goose StatementEnd
