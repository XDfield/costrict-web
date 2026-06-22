-- Admin 后台菜单权限种子
--
-- 为独立 /admin 管理后台注册菜单资源码 'admin'，仅 platform_admin 可见。
-- 与 enterprise 写接口（POST/PUT/DELETE /api/admin/enterprise-customers，硬卡 platform_admin）
-- 权限口径保持一致，避免 business_admin 看得见菜单却一写就 403。
-- 不需改 main.go：authz registry 启动时从本表加载，迁移执行 + 重启即生效。

-- +goose Up
-- +goose StatementBegin

INSERT INTO resource_permissions (resource_code, resource_type, allowed_roles) VALUES
    ('admin', 'menu', ARRAY['platform_admin']::text[])
ON CONFLICT (resource_code) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM resource_permissions WHERE resource_code = 'admin' AND resource_type = 'menu';

-- +goose StatementEnd
