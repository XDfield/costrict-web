-- 创建用户表
--
-- 本地用户数据表，用于存储从 Casdoor 同步的用户信息
-- 主键使用 JWT Token 中的 id 或 sub 字段（UUID 格式）
--
-- 设计要点：
-- 1. id 字段作为主键，存储 JWT 中的 id/sub（UUID），与现有数据库表结构一致
-- 2. 同时存储 id、sub、universal_id，确保与各种数据源的兼容性
-- 3. 支持登录时的 get_or_create 操作
-- 4. 提供用户信息查询缓存，减少对 Casdoor API 的依赖

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS users (
    -- 主键（使用 JWT Token 中的 id 或 sub 字段，UUID 格式）
    id VARCHAR(191) NOT NULL PRIMARY KEY,

    -- 基本信息
    username VARCHAR(191) NOT NULL,
    display_name VARCHAR(191),
    email VARCHAR(191),
    avatar_url TEXT,

    -- Casdoor 相关字段
    casdoor_id VARCHAR(191),
    casdoor_universal_id VARCHAR(191),
    casdoor_sub VARCHAR(191),
    organization VARCHAR(191),

    -- 状态字段
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    last_login_at TIMESTAMP,
    last_sync_at TIMESTAMP,

    -- 审计字段
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP,

    -- 索引
    CONSTRAINT idx_user_username UNIQUE (username)
);

-- 创建索引
CREATE INDEX IF NOT EXISTS idx_user_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_id ON users(casdoor_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_universal_id ON users(casdoor_universal_id);
CREATE INDEX IF NOT EXISTS idx_user_casdoor_sub ON users(casdoor_sub);
CREATE INDEX IF NOT EXISTS idx_user_organization ON users(organization);
CREATE INDEX IF NOT EXISTS idx_user_deleted_at ON users(deleted_at);

-- 添加表注释
COMMENT ON TABLE users IS '用户表 - 本地存储从 Casdoor 同步的用户信息';
COMMENT ON COLUMN users.id IS '用户唯一标识 (JWT id/sub, UUID)';
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

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_deleted_at;
DROP INDEX IF EXISTS idx_user_organization;
DROP INDEX IF EXISTS idx_user_casdoor_sub;
DROP INDEX IF EXISTS idx_user_casdoor_universal_id;
DROP INDEX IF EXISTS idx_user_casdoor_id;
DROP INDEX IF EXISTS idx_user_email;
DROP TABLE IF EXISTS users;

-- +goose StatementEnd
