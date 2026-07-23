-- Git Ownership Refactor Phase 1：tenant_git_server_binding 表
--
-- Server 没有 tenants 主表（租户表在 cs-user）。本表是 server 侧的租户 → git_server
-- 1:1 绑定记录，供 server 端 gitsync.Resolver 本地解析，不再 RPC 回 cs-user。
--
-- Schema 决策：
--
--   1. tenant_id TEXT PK —— 一个 tenant 最多绑定一个 git_server。
--   2. git_server_id REFERENCES git_servers(server_id) —— 强 FK，绑定时校验存在。
--   3. 不存软删除列 —— unbind 即 DELETE；server 视角下绑定关系没有历史价值
--      （审计走 server 的 behavior_log）。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS tenant_git_server_binding (
    tenant_id      VARCHAR(191) NOT NULL PRIMARY KEY,
    git_server_id  VARCHAR(64)  NOT NULL REFERENCES git_servers(server_id),
    bound_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMENT ON TABLE tenant_git_server_binding IS 'Tenant → git_server 1:1 绑定 — Git Ownership Refactor Phase 1（server 无 tenants 主表）';
COMMENT ON COLUMN tenant_git_server_binding.tenant_id IS '租户 ID（与 cs-user tenants.tenant_id 同语义；server 不维护 tenants 主表）';
COMMENT ON COLUMN tenant_git_server_binding.git_server_id IS '绑定的 git_servers.server_id';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS tenant_git_server_binding;

-- +goose StatementEnd
