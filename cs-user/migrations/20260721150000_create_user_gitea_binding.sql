-- Phase E3a.1：user_gitea_binding 表迁移
--
-- Gitea 用户自动开户桥接表（USER_CENTER_DESIGN §4.4 + §11.1）：
--   cs-user users.subject_id  ↔  Gitea user.username 的 1:1 映射。
--   sync_status 状态机：pending → synced | error。
--
-- 设计取舍：
--
--   1. PK 是 (user_subject_id, tenant_id) —— 遵循 cs-user 现有 users /
--      user_auth_identities 模式（subject_id 是 TEXT 应用层主键；tenant_id
--      是 B5 多租户分片键）。一个用户在一个 tenant 内只有一行 binding。
--   2. 不加 FK 到 users —— 与 user_center_audit_log 同决策：binding 行必须
--      能撑过 users 的 hard-delete，否则 reconciliation cron 无法检测孤儿
--      Gitea 账号（§11.3 兜底）。
--   3. gitea_uid NULL while pending —— POST /admin/users 成功后回填。
--      BIGINT 与 Gitea 内部自增 ID 类型对齐。
--   4. gitea_username 全局唯一 —— Gitea usernames 自身全局唯一，跨 tenant
--      同名会冲突（设计上正确：fork JWT middleware 用 username 路由）。
--   5. sync_status 不加 CHECK 约束 —— 应用层枚举校验（giteasync.Status*
--      常量集），新状态加类型不需要迁移，匹配 audit_log 的同样决策。
--   6. last_error TEXT —— 失败原因（HTTP body / sentinel error string），
--      ops 排障用。NULL when sync_status='synced'。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_gitea_binding (
    user_subject_id   TEXT                     NOT NULL,
    tenant_id         TEXT                     NOT NULL DEFAULT 'default',

    -- Gitea 内部 user ID（pending 期间 NULL；POST /admin/users 成功后回填）
    gitea_uid         BIGINT,

    -- Gitea 登录账号名（POST /admin/users body.username；全局唯一）
    gitea_username    VARCHAR(64)              NOT NULL,

    -- 状态机：pending | synced | error（无 dead_letter — E3a.1 不实现重试队列）
    sync_status       VARCHAR(32)              NOT NULL DEFAULT 'pending',

    last_synced_at    TIMESTAMPTZ,
    last_error        TEXT,

    created_at        TIMESTAMPTZ              NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ              NOT NULL DEFAULT now(),

    PRIMARY KEY (user_subject_id, tenant_id)
);

-- Gitea usernames 全局唯一（与 Gitea 自身约束对齐）
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_gitea_binding_gitea_username
    ON user_gitea_binding (gitea_username);

-- 列出 tenant 内 pending/error 的 binding（reconciliation cron 用，E3a.2）
CREATE INDEX IF NOT EXISTS idx_user_gitea_binding_tenant
    ON user_gitea_binding (tenant_id, sync_status);

COMMENT ON TABLE user_gitea_binding IS 'Gitea 用户绑定 — Phase E3a.1：cs-user user ↔ Gitea user 1:1 映射（USER_CENTER §4.4 + §11）';
COMMENT ON COLUMN user_gitea_binding.user_subject_id IS 'cs-user users.subject_id（应用层稳定主键）';
COMMENT ON COLUMN user_gitea_binding.tenant_id IS '租户分片键（B5 多租户；与 users.tenant_id 同语义）';
COMMENT ON COLUMN user_gitea_binding.gitea_uid IS 'Gitea 内部 user.id（pending 期间 NULL；POST /admin/users 成功后回填）';
COMMENT ON COLUMN user_gitea_binding.gitea_username IS 'Gitea 登录账号名（全局唯一，与 Gitea 自身约束对齐）';
COMMENT ON COLUMN user_gitea_binding.sync_status IS '同步状态机：pending | synced | error（应用层枚举，无 CHECK）';
COMMENT ON COLUMN user_gitea_binding.last_synced_at IS '最后一次成功同步时间（synced 状态下非空）';
COMMENT ON COLUMN user_gitea_binding.last_error IS '最后一次失败原因（HTTP body / sentinel error string）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_gitea_binding_tenant;
DROP INDEX IF EXISTS uq_user_gitea_binding_gitea_username;
DROP TABLE IF EXISTS user_gitea_binding;

-- +goose StatementEnd
