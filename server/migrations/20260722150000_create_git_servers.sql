-- Git Ownership Refactor Phase 1（迁移自 cs-user）
-- 关联提案：docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md
--
-- 把 cs-user 的 git_servers 表搬到 server：
--   - Git 适配层主体本来就在 server（gitsync/）
--   - 配合 user_gitea_binding + tenant_git_server_binding 的本地化
--   - cs-user 后续通过 outbox 事件通知 server 异步开户
--
-- Schema 决策（与 cs-user 原 git_servers 1:1 镜像，仅去掉对 tenants 表的 FK —
-- server 没有 tenants 主表，租户绑定走独立的 tenant_git_server_binding 表）：
--
--   1. server_id VARCHAR(64) PK —— 应用层生成（"gs-template-..." / "gs-" + uuid）。
--   2. kind VARCHAR(32) —— v1 仅 'gitea'，保留扩展位。
--   3. config JSONB —— {"admin_token": "...", "admin_user": "...", "admin_password": "..."}。
--      敏感字段过渡方案；TODO[vault] 后续迁移。
--   4. is_template BOOLEAN partial unique —— 全局最多一行 template。
--   5. enabled BOOLEAN —— 软禁用开关。
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

-- 全局最多一行 template
CREATE UNIQUE INDEX IF NOT EXISTS idx_git_servers_template
    ON git_servers (is_template) WHERE is_template = true;

COMMENT ON TABLE git_servers IS 'Per-tenant Git 服务器配置 — Phase 1 of Git Ownership Refactor';
COMMENT ON COLUMN git_servers.server_id IS '应用层稳定主键（gs-template-... / gs-<uuid>）';
COMMENT ON COLUMN git_servers.kind IS 'Git 服务器类型（v1 仅 gitea；扩展位）';
COMMENT ON COLUMN git_servers.endpoint IS 'Gitea API base URL（如 https://gitea.example.com）';
COMMENT ON COLUMN git_servers.display_name IS '人类可读名称（运维 UI 用）';
COMMENT ON COLUMN git_servers.config IS 'JSONB 配置：{"admin_token": "...", "admin_user": "...", "admin_password": "..."}';
COMMENT ON COLUMN git_servers.is_template IS '是否为模板行（用于新租户克隆；全局唯一）';
COMMENT ON COLUMN git_servers.enabled IS '软禁用开关（false 时 resolver 报错）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_git_servers_template;
DROP TABLE IF EXISTS git_servers;

-- +goose StatementEnd
