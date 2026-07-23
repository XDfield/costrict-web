-- Git Ownership Refactor Phase 1：user_gitea_binding 表（迁移自 cs-user）
--
-- Server 端的 Gitea 用户绑定记录。Phase 3 起，cs-user 创建用户 → 发 user.created
-- 事件 → server 消费 → 调 gitsync.ProvisionUser 写入本表。
--
-- Schema 决策（与 cs-user 原 user_gitea_binding 1:1 镜像）：
--
--   1. PK = (user_subject_id, tenant_id) —— 一个用户在一个 tenant 内只有一行。
--   2. 不加 FK 到 users —— binding 行需要撑过用户 hard-delete，便于检测孤儿 Gitea 账号。
--   3. gitea_uid NULL while pending —— POST /admin/users 成功后回填。
--   4. gitea_username 全局唯一 —— 与 Gitea 自身约束对齐。
--   5. sync_status 无 CHECK 约束 —— 应用层枚举校验。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_gitea_binding (
    user_subject_id   TEXT                     NOT NULL,
    tenant_id         TEXT                     NOT NULL DEFAULT 'default',

    gitea_uid         BIGINT,
    gitea_username    VARCHAR(64)              NOT NULL,

    sync_status       VARCHAR(32)              NOT NULL DEFAULT 'pending',

    last_synced_at    TIMESTAMPTZ,
    last_error        TEXT,

    created_at        TIMESTAMPTZ              NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ              NOT NULL DEFAULT now(),

    PRIMARY KEY (user_subject_id, tenant_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_user_gitea_binding_gitea_username
    ON user_gitea_binding (gitea_username);

CREATE INDEX IF NOT EXISTS idx_user_gitea_binding_tenant
    ON user_gitea_binding (tenant_id, sync_status);

COMMENT ON TABLE user_gitea_binding IS 'Gitea 用户绑定 — Git Ownership Refactor Phase 1';
COMMENT ON COLUMN user_gitea_binding.user_subject_id IS 'cs-user users.subject_id';
COMMENT ON COLUMN user_gitea_binding.tenant_id IS '租户分片键（与 cs-user users.tenant_id 同语义）';
COMMENT ON COLUMN user_gitea_binding.gitea_uid IS 'Gitea 内部 user.id（pending 期间 NULL）';
COMMENT ON COLUMN user_gitea_binding.gitea_username IS 'Gitea 登录账号名（全局唯一）';
COMMENT ON COLUMN user_gitea_binding.sync_status IS '状态机：pending | synced | error（应用层枚举）';
COMMENT ON COLUMN user_gitea_binding.last_synced_at IS '最后一次成功同步时间';
COMMENT ON COLUMN user_gitea_binding.last_error IS '最后一次失败原因';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_gitea_binding_tenant;
DROP INDEX IF EXISTS uq_user_gitea_binding_gitea_username;
DROP TABLE IF EXISTS user_gitea_binding;

-- +goose StatementEnd
