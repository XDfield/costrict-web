-- 资源权限表
--
-- 将菜单/API 资源权限映射从代码迁移到数据库，支持动态配置。

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS resource_permissions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_code VARCHAR(128) NOT NULL,
    resource_type VARCHAR(32) NOT NULL,
    allowed_roles TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uk_resource_permission_code UNIQUE (resource_code),
    CONSTRAINT chk_resource_type CHECK (resource_type IN ('menu', 'api'))
);

CREATE INDEX IF NOT EXISTS idx_resource_permissions_type ON resource_permissions(resource_type);

COMMENT ON TABLE resource_permissions IS '资源权限映射表';
COMMENT ON COLUMN resource_permissions.resource_code IS '资源码，如 console.projects';
COMMENT ON COLUMN resource_permissions.resource_type IS '资源类型：menu | api';
COMMENT ON COLUMN resource_permissions.allowed_roles IS '允许访问的角色列表，空数组表示任何登录用户';

-- Seed existing permissions (same as previous hard-coded values)
INSERT INTO resource_permissions (resource_code, resource_type, allowed_roles) VALUES
    ('console.repositories', 'menu', '{}'),
    ('console.projects', 'menu', '{}'),
    ('console.capabilities', 'menu', ARRAY['platform_admin']::text[]),
    ('console.devices', 'menu', '{}'),
    ('console.notifications', 'menu', ARRAY['platform_admin']::text[]),
    ('console.kanban', 'menu', ARRAY['business_admin', 'platform_admin']::text[]),
    ('admin.system-roles', 'api', ARRAY['platform_admin']::text[]),
    ('admin.notification-channels', 'api', ARRAY['platform_admin']::text[]),
    ('api.kanban.overview', 'api', ARRAY['business_admin', 'platform_admin']::text[])
ON CONFLICT (resource_code) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_resource_permissions_type;
DROP TABLE IF EXISTS resource_permissions;

-- +goose StatementEnd
