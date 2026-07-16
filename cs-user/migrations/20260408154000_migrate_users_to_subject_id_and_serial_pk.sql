-- 将 users 表从“字符串业务主键”迁移为“自增主键 + subject_id 业务标识”
--
-- 迁移目标：
-- 1. users.id 改为 BIGSERIAL 主键，仅作为本地数据库内部主键使用
-- 2. 新增 users.subject_id，作为系统内部稳定业务用户标识
-- 3. 迁移并回填历史用户数据，subject_id 永远使用本地生成的 usr_<uuid>

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS users_new (
    id BIGSERIAL PRIMARY KEY,
    subject_id VARCHAR(191) NOT NULL,
    username VARCHAR(191) NOT NULL,
    display_name VARCHAR(191),
    email VARCHAR(191),
    avatar_url TEXT,
    casdoor_id VARCHAR(191),
    casdoor_universal_id VARCHAR(191),
    casdoor_sub VARCHAR(191),
    organization VARCHAR(191),
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    last_login_at TIMESTAMP,
    last_sync_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);

INSERT INTO users_new (
    subject_id,
    username,
    display_name,
    email,
    avatar_url,
    casdoor_id,
    casdoor_universal_id,
    casdoor_sub,
    organization,
    is_active,
    last_login_at,
    last_sync_at,
    created_at,
    updated_at,
    deleted_at
)
SELECT
    'usr_' || gen_random_uuid()::text AS subject_id,
    username,
    display_name,
    email,
    avatar_url,
    casdoor_id,
    casdoor_universal_id,
    casdoor_sub,
    organization,
    COALESCE(is_active, TRUE),
    last_login_at,
    last_sync_at,
    COALESCE(created_at, CURRENT_TIMESTAMP),
    COALESCE(updated_at, CURRENT_TIMESTAMP),
    deleted_at
FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

ALTER TABLE users
    ADD CONSTRAINT idx_user_subject_id UNIQUE (subject_id);

ALTER TABLE users
    ADD CONSTRAINT idx_user_username UNIQUE (username);

CREATE INDEX IF NOT EXISTS idx_user_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_id ON users(casdoor_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_universal_id ON users(casdoor_universal_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_sub ON users(casdoor_sub);
CREATE INDEX IF NOT EXISTS idx_user_organization ON users(organization);
CREATE INDEX IF NOT EXISTS idx_user_deleted_at ON users(deleted_at);

COMMENT ON TABLE users IS '用户表 - 本地存储从 Casdoor 同步的用户信息';
COMMENT ON COLUMN users.id IS '本地数据库主键（自增），仅用于内部关联';
COMMENT ON COLUMN users.subject_id IS '系统内部稳定业务用户标识，用于业务表和请求上下文';
COMMENT ON COLUMN users.username IS '用户名 (Casdoor name)';
COMMENT ON COLUMN users.display_name IS '显示名称 (Casdoor preferred_username)';
COMMENT ON COLUMN users.email IS '邮箱';
COMMENT ON COLUMN users.avatar_url IS '头像 URL';
COMMENT ON COLUMN users.casdoor_id IS 'Casdoor id claim，仅用于兼容查找';
COMMENT ON COLUMN users.casdoor_universal_id IS 'Casdoor stable universal_id，用于身份绑定';
COMMENT ON COLUMN users.casdoor_sub IS 'Casdoor sub claim，用于兼容查找和迁移';
COMMENT ON COLUMN users.organization IS '所属组织 (Casdoor owner)';
COMMENT ON COLUMN users.is_active IS '是否激活';
COMMENT ON COLUMN users.last_login_at IS '最后登录时间';
COMMENT ON COLUMN users.last_sync_at IS '最后同步时间';
COMMENT ON COLUMN users.deleted_at IS '软删除时间戳';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS users_old (
    id VARCHAR(191) NOT NULL PRIMARY KEY,
    username VARCHAR(191) NOT NULL,
    display_name VARCHAR(191),
    email VARCHAR(191),
    avatar_url TEXT,
    casdoor_id VARCHAR(191),
    casdoor_universal_id VARCHAR(191),
    casdoor_sub VARCHAR(191),
    organization VARCHAR(191),
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    last_login_at TIMESTAMP,
    last_sync_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);

INSERT INTO users_old (
    id,
    username,
    display_name,
    email,
    avatar_url,
    casdoor_id,
    casdoor_universal_id,
    casdoor_sub,
    organization,
    is_active,
    last_login_at,
    last_sync_at,
    created_at,
    updated_at,
    deleted_at
)
SELECT
    subject_id,
    username,
    display_name,
    email,
    avatar_url,
    casdoor_id,
    casdoor_universal_id,
    casdoor_sub,
    organization,
    is_active,
    last_login_at,
    last_sync_at,
    created_at,
    updated_at,
    deleted_at
FROM users;

DROP TABLE users;
ALTER TABLE users_old RENAME TO users;

ALTER TABLE users
    ADD CONSTRAINT idx_user_username UNIQUE (username);

CREATE INDEX IF NOT EXISTS idx_user_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_id ON users(casdoor_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_universal_id ON users(casdoor_universal_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_sub ON users(casdoor_sub);
CREATE INDEX IF NOT EXISTS idx_user_organization ON users(organization);
CREATE INDEX IF NOT EXISTS idx_user_deleted_at ON users(deleted_at);

COMMENT ON TABLE users IS '用户表 - 本地存储从 Casdoor 同步的用户信息';
COMMENT ON COLUMN users.id IS '用户唯一标识 (回滚后使用 subject_id 恢复)';
COMMENT ON COLUMN users.username IS '用户名 (Casdoor name)';
COMMENT ON COLUMN users.display_name IS '显示名称 (Casdoor preferred_username)';
COMMENT ON COLUMN users.email IS '邮箱';
COMMENT ON COLUMN users.avatar_url IS '头像 URL';
COMMENT ON COLUMN users.casdoor_id IS 'Casdoor 用户 ID (UUID)';
COMMENT ON COLUMN users.casdoor_universal_id IS 'Casdoor 通用唯一 ID (UUID)';
COMMENT ON COLUMN users.casdoor_sub IS 'Casdoor OIDC sub (可能为 owner/name 格式)';
COMMENT ON COLUMN users.organization IS '所属组织 (Casdoor owner)';
COMMENT ON COLUMN users.is_active IS '是否激活';
COMMENT ON COLUMN users.last_login_at IS '最后登录时间';
COMMENT ON COLUMN users.last_sync_at IS '最后同步时间';
COMMENT ON COLUMN users.deleted_at IS '软删除时间戳';

-- +goose StatementEnd
