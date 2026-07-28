-- Phase E3b.1.1：git_servers 表 + tenants.git_server_id 外键
--
-- Per-tenant Gitea 架构落地（MULTI_TENANCY_DESIGN §20.4）：
--   每个 tenant 严格绑定一个 git_servers 行（tenants.git_server_id）。
--   git_servers 持有 endpoint + config（JSONB：暂时存 admin_token，vault 集成后迁移）。
--
-- Bug fix 背景：E3a.1（cs-user giteasync）与 E3b.1（@server gitsync）原本都读全局
-- 环境变量（CS_USER_GITEA_BASE_URL / GITEA_BASE_URL），违反 per-tenant 架构。本次
-- 引入 git_servers 表后，两端都通过 ResolveAdapterForTenant(tenantID) 解析 endpoint。
--
-- 设计取舍：
--
--   1. server_id VARCHAR(64) PK —— 应用层生成（"gs-template-..." / "gs-" + uuid）。
--      与 tenant_id 同语义（应用层稳定主键），避免 DB 自增序列泄漏。
--   2. kind VARCHAR(32) —— 当前仅 'gitea'（v1 唯一 kind），保留扩展位。
--   3. config JSONB —— 存 {"admin_token": "..."}。Vault 集成前的过渡方案
--      （TODO[vault]：迁移到 vault 后，config 改为只存非敏感元数据，敏感字段走 vault）。
--   4. is_template BOOLEAN —— 标记模板行（用于新租户克隆）。partial unique index
--      保证全局最多一行 template（与设计文档 §20.4 一致）。
--   5. enabled BOOLEAN —— 软禁用开关；disabled 时 resolver 返回错误。
--   6. tenants.git_server_id nullable —— 迁移窗口期容错；应用层 bootstrap 步骤会
--      把所有现存 tenant 回填到 template 行，后续 migration 加 NOT NULL 约束。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS git_servers (
    server_id     VARCHAR(64)  PRIMARY KEY,
    kind          VARCHAR(32)  NOT NULL,
    endpoint      TEXT         NOT NULL,
    display_name  TEXT         NOT NULL,
    config        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    is_template   BOOLEAN      NOT NULL DEFAULT false,
    enabled       BOOLEAN      NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- 全局最多一行 template（MULTI_TENANCY_DESIGN §20.4）
CREATE UNIQUE INDEX IF NOT EXISTS idx_git_servers_template
    ON git_servers (is_template) WHERE is_template = true;

-- tenant → git_server 1:1 绑定
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS git_server_id VARCHAR(64) REFERENCES git_servers(server_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_git_server
    ON tenants (git_server_id);

COMMENT ON TABLE git_servers IS 'Per-tenant Git 服务器配置 — Phase E3b.1.1：MULTI_TENANCY_DESIGN §20.4';
COMMENT ON COLUMN git_servers.server_id IS '应用层稳定主键（gs-template-... / gs-<uuid>）';
COMMENT ON COLUMN git_servers.kind IS 'Git 服务器类型（v1 仅 gitea；扩展位）';
COMMENT ON COLUMN git_servers.endpoint IS 'Gitea API base URL（如 https://gitea.example.com）';
COMMENT ON COLUMN git_servers.display_name IS '人类可读名称（运维 UI 用）';
COMMENT ON COLUMN git_servers.config IS 'JSONB 配置：{"admin_token": "..."}（TODO[vault]: 敏感字段迁移）';
COMMENT ON COLUMN git_servers.is_template IS '是否为模板行（用于新租户克隆；全局唯一）';
COMMENT ON COLUMN git_servers.enabled IS '软禁用开关（false 时 resolver 报错）';
COMMENT ON COLUMN tenants.git_server_id IS '绑定的 git_servers.server_id（1:1 unique）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_tenants_git_server;
ALTER TABLE tenants DROP COLUMN IF EXISTS git_server_id;
DROP INDEX IF EXISTS idx_git_servers_template;
DROP TABLE IF EXISTS git_servers;

-- +goose StatementEnd
