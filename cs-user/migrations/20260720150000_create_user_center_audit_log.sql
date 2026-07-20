-- Phase C4.1：user_center_audit_log 表迁移
--
-- 越权防护与审计基础设施（MULTI_TENANCY_DESIGN §16.1）：记录所有 admin 写操作
-- 的 actor + action + target，供合规审计 / 跨 tenant 操作追溯 / 越权检测（C4.2
-- 中间件采样）使用。
--
-- 与 platform_admins / tenant_admins 平级存在，独立一张表：
--   - 不耦合到 user/tenant/admin 行级数据 — 即使源行 hard-delete，审计行保留
--     （regulator-visible action history per §16.2）
--   - 不加 FK 到 tenants/users — 删 tenant / 删 user 不级联清空审计
--
-- 设计取舍：
--
--   1. tenant_id 用 TEXT（UUID 字符串）而非 UUID 列类型 — 与 tenants.tenant_id
--      / tenant_configs.tenant_id 对齐（沿用 cs-user 现有约定，见
--      20260717100000_create_tenants_and_tenant_admins.sql 决策 1）。
--   2. actor_subject_id 用 TEXT — 与 users.id (VARCHAR(191)) 兼容，但保留更宽
--      类型以便将来记录非 users 表的外部 actor（如 webhook 系统账户）。
--   3. action 不加 CHECK 约束 — 应用层枚举校验（auditlog.Action* 常量集），
--      新事件加类型不需要迁移，匹配 platform_admins.scope / tenant_admins.role
--      的同样决策。
--   4. payload 用 JSONB — 自由形态（before/after / diff / metadata），匹配
--      tenant_configs.features 的 JSONB 模式。
--   5. 没有 updated_at — 审计行 immutable，不允许 UPDATE（应用层强制）。
--   6. 没有 deleted_at — 不软删；retention 由独立 cron 处理（C4.x out-of-scope）。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_center_audit_log (
    -- 自增主键（ BIGSERIAL：审计行写多读少，BIGINT 容量充足）
    id                     BIGSERIAL                PRIMARY KEY,

    -- 租户上下文（NULL = 平台级事件，如 platform_admin 创建 tenant）
    tenant_id              TEXT,

    -- 操作发起方（NULL = 系统动作，如未来 hard-delete cron）
    actor_subject_id       TEXT,
    actor_tenant_role      VARCHAR(32),
    actor_platform_scope   VARCHAR(32),

    -- 操作语义（NOT NULL：每行必须有 action；target_type/target_id 可空用于
    -- 平台级事件没有明确 target 的边界场景）
    action                 VARCHAR(64)              NOT NULL,
    target_type            VARCHAR(32),
    target_id              TEXT,

    -- 自由形态负载（before/after diff / extra metadata）
    payload                JSONB,

    -- 网络上下文（来自 gin c.ClientIP() / User-Agent header）
    ip                     VARCHAR(45),
    user_agent             TEXT,

    created_at             TIMESTAMPTZ              NOT NULL DEFAULT now()
);

-- 跨 tenant 审计：platform_admin 全局查询 + tenant_admin 本租户列表
CREATE INDEX IF NOT EXISTS idx_audit_log_tenant
    ON user_center_audit_log (tenant_id, created_at DESC);

-- 单 actor 行为追溯：给定 user_id 找所有 admin 动作
CREATE INDEX IF NOT EXISTS idx_audit_log_actor
    ON user_center_audit_log (actor_subject_id, created_at DESC);

-- 按 action 类型筛选：所有 tenant.create / 所有 tenant_config.update 等
CREATE INDEX IF NOT EXISTS idx_audit_log_action
    ON user_center_audit_log (action, created_at DESC);

COMMENT ON TABLE user_center_audit_log IS '审计日志 — Phase C4.1：admin 写操作 actor/action/target 审计（MULTI_TENANCY §16.1）';
COMMENT ON COLUMN user_center_audit_log.tenant_id IS '租户上下文（NULL = 平台级事件；TEXT UUID 字符串与 tenants.tenant_id 同语义）';
COMMENT ON COLUMN user_center_audit_log.actor_subject_id IS '操作发起方 users.id（NULL = 系统动作，如 cron）';
COMMENT ON COLUMN user_center_audit_log.actor_tenant_role IS 'actor 在本 tenant 的角色：owner / tenant_admin / tenant_member（NULL = 平台级 actor）';
COMMENT ON COLUMN user_center_audit_log.actor_platform_scope IS 'actor 平台权限范围：full / support / read_only（NULL = tenant 级 actor）';
COMMENT ON COLUMN user_center_audit_log.action IS '操作语义常量（应用层枚举，无 CHECK）：tenant.create / tenant.suspend / tenant_config.update 等';
COMMENT ON COLUMN user_center_audit_log.target_type IS '目标资源类型：tenant / tenant_config / provider_mapping 等';
COMMENT ON COLUMN user_center_audit_log.target_id IS '目标资源标识（如 tenant_id 或 "tenant_config:<id>"）';
COMMENT ON COLUMN user_center_audit_log.payload IS '自由形态 JSONB 负载（before/after diff / extra metadata）';
COMMENT ON COLUMN user_center_audit_log.ip IS '请求来源 IP（gin c.ClientIP()）';
COMMENT ON COLUMN user_center_audit_log.user_agent IS '请求 User-Agent header';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_audit_log_action;
DROP INDEX IF EXISTS idx_audit_log_actor;
DROP INDEX IF EXISTS idx_audit_log_tenant;
DROP TABLE IF EXISTS user_center_audit_log;

-- +goose StatementEnd
