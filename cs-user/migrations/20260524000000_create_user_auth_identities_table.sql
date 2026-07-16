-- +goose Up
-- Create user_auth_identities table to store external login identities bound to local users.
-- Each row represents one external identity (e.g. Casdoor/OIDC) linked to a user subject.

CREATE TABLE IF NOT EXISTS user_auth_identities (
    id BIGSERIAL PRIMARY KEY,
    user_subject_id text NOT NULL,
    provider text NOT NULL,
    issuer text,
    external_key text NOT NULL,
    external_subject text,
    external_user_id text,
    provider_user_id text,
    display_name text,
    email text,
    phone text,
    avatar_url text,
    organization text,
    is_primary boolean NOT NULL DEFAULT false,
    last_login_at timestamptz,
    created_at timestamptz,
    updated_at timestamptz,
    deleted_at timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_auth_identities_external_key ON user_auth_identities(external_key);
CREATE INDEX IF NOT EXISTS idx_user_auth_identities_user_subject_id ON user_auth_identities(user_subject_id);

COMMENT ON TABLE user_auth_identities IS '外部登录身份绑定表，每行代表绑定到本地用户的一个外部身份（如 Casdoor/OIDC）';
COMMENT ON COLUMN user_auth_identities.user_subject_id IS '本地用户 subject_id';
COMMENT ON COLUMN user_auth_identities.provider IS '身份提供者';
COMMENT ON COLUMN user_auth_identities.issuer IS 'OIDC issuer';
COMMENT ON COLUMN user_auth_identities.external_key IS '外部身份唯一键，全局唯一';
COMMENT ON COLUMN user_auth_identities.external_subject IS '外部 subject';
COMMENT ON COLUMN user_auth_identities.external_user_id IS '外部用户 ID';
COMMENT ON COLUMN user_auth_identities.provider_user_id IS '提供者侧用户 ID';
COMMENT ON COLUMN user_auth_identities.display_name IS '显示名称';
COMMENT ON COLUMN user_auth_identities.email IS '邮箱';
COMMENT ON COLUMN user_auth_identities.phone IS '手机号';
COMMENT ON COLUMN user_auth_identities.avatar_url IS '头像 URL';
COMMENT ON COLUMN user_auth_identities.organization IS '所属组织';
COMMENT ON COLUMN user_auth_identities.is_primary IS '是否为主身份';
COMMENT ON COLUMN user_auth_identities.last_login_at IS '最后登录时间';
COMMENT ON COLUMN user_auth_identities.created_at IS '创建时间';
COMMENT ON COLUMN user_auth_identities.updated_at IS '更新时间';
COMMENT ON COLUMN user_auth_identities.deleted_at IS '软删除时间戳';

-- +goose Down
DROP INDEX IF EXISTS idx_user_auth_identities_user_subject_id;
DROP INDEX IF EXISTS idx_user_auth_identities_external_key;
DROP TABLE IF EXISTS user_auth_identities;
