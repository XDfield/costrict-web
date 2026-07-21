-- 20260721160000_users_add_provider_columns.sql
--
-- Reconciles cs-user/internal/models.User with the DB schema.
--
-- The User struct gained 4 nullable provider-binding columns (phone,
-- auth_provider, external_key, provider_user_id) when the multi-IdP work
-- landed, but no migration ever added them to the users table — so
-- GetOrCreateUser's "query by external_key" path errored at runtime
-- (SQLSTATE 42703). This migration closes that drift.
--
-- All four columns are nullable to keep the write path idempotent: rows
-- that pre-date this migration simply have NULL until next sync.
--
-- external_key unique index is PARTIAL (WHERE IS NOT NULL) so that the
-- majority of legacy rows (which never set it) don't conflict; gorm's
-- struct tag `uniqueIndex:idx_user_external_key` is satisfied by a
-- partial unique index of the same name.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE users ADD COLUMN IF NOT EXISTS phone            VARCHAR(64);
ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_provider    VARCHAR(64);
ALTER TABLE users ADD COLUMN IF NOT EXISTS external_key     VARCHAR(255);
ALTER TABLE users ADD COLUMN IF NOT EXISTS provider_user_id VARCHAR(191);

-- Partial unique: only rows that actually set external_key must be unique.
-- This lets legacy NULL rows coexist (multiple users with NULL).
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_external_key
    ON users(external_key)
    WHERE external_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_user_phone
    ON users(phone)
    WHERE phone IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_user_auth_provider
    ON users(auth_provider)
    WHERE auth_provider IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_user_provider_user_id
    ON users(provider_user_id)
    WHERE provider_user_id IS NOT NULL;

COMMENT ON COLUMN users.phone            IS '可选手机号，多 IdP 绑定时使用';
COMMENT ON COLUMN users.auth_provider    IS '主登录 IdP 标识（casdoor / oidc / saml 等）';
COMMENT ON COLUMN users.external_key     IS '全局唯一外部身份键（issuer+provider+external subject），跨 IdP 复用同一物理用户';
COMMENT ON COLUMN users.provider_user_id IS '主 IdP 侧 user id（与 auth_provider 配对）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_provider_user_id;
DROP INDEX IF EXISTS idx_user_auth_provider;
DROP INDEX IF EXISTS idx_user_phone;
DROP INDEX IF EXISTS idx_user_external_key;

ALTER TABLE users DROP COLUMN IF EXISTS provider_user_id;
ALTER TABLE users DROP COLUMN IF EXISTS external_key;
ALTER TABLE users DROP COLUMN IF EXISTS auth_provider;
ALTER TABLE users DROP COLUMN IF EXISTS phone;

-- +goose StatementEnd
