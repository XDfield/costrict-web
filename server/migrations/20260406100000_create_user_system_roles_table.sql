-- 系统级角色表
--
-- 为本地用户增加系统级授权能力，用于平台管理员与业务管理成员等角色控制。

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_system_roles (
    id VARCHAR(36) NOT NULL PRIMARY KEY,
    user_id VARCHAR(191) NOT NULL,
    role VARCHAR(64) NOT NULL,
    granted_by VARCHAR(191),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP,

    CONSTRAINT uk_user_system_role UNIQUE (user_id, role),
    CONSTRAINT chk_user_system_roles_role CHECK (role IN ('platform_admin', 'business_admin'))
);

CREATE INDEX IF NOT EXISTS idx_user_system_roles_user_id ON user_system_roles(user_id);
CREATE INDEX IF NOT EXISTS idx_user_system_roles_role ON user_system_roles(role);
CREATE INDEX IF NOT EXISTS idx_user_system_roles_granted_by ON user_system_roles(granted_by);
CREATE INDEX IF NOT EXISTS idx_user_system_roles_deleted_at ON user_system_roles(deleted_at);

COMMENT ON TABLE user_system_roles IS '用户系统级角色表';
COMMENT ON COLUMN user_system_roles.id IS '主键 UUID';
COMMENT ON COLUMN user_system_roles.user_id IS '关联 users.id';
COMMENT ON COLUMN user_system_roles.role IS '系统角色 platform_admin | business_admin';
COMMENT ON COLUMN user_system_roles.granted_by IS '授予者 user_id';
COMMENT ON COLUMN user_system_roles.created_at IS '授予时间';
COMMENT ON COLUMN user_system_roles.updated_at IS '更新时间';
COMMENT ON COLUMN user_system_roles.deleted_at IS '软删除时间';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_system_roles_deleted_at;
DROP INDEX IF EXISTS idx_user_system_roles_granted_by;
DROP INDEX IF EXISTS idx_user_system_roles_role;
DROP INDEX IF EXISTS idx_user_system_roles_user_id;
DROP TABLE IF EXISTS user_system_roles;

-- +goose StatementEnd
