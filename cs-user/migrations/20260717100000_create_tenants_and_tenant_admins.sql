-- Phase B1：tenants + tenant_admins 表迁移
--
-- 这是 Phase B（tenant 维度落地）的第一张表。配合 A2 的 tenant_configs（已存在，
-- tenant_id TEXT PRIMARY KEY），构成多租户三表骨架：
--
--   tenants           — 租户主表（canonical tenant entity）
--   tenant_admins     — 租户管理员（user × tenant 多对多）
--   tenant_configs    — 每租户 YAML 配置（A2 已落地，B1 加 FK）
--
-- Schema 决策（与 cs-user 现有约定保持一致，而非理想化的 MULTI_TENANCY_DESIGN §7）：
--
--   1. tenant_id 用 TEXT（UUID 字符串）而非 UUID 列类型 — 与 tenant_configs
--      / users(id) 对齐，避免 cs-user 全仓升级 UUID 列类型。
--   2. email_domains / features / limits / settings 用 TEXT 持有 JSON 文本
--      （非 JSONB / TEXT[]）— 与 EmploymentIdentity.Attributes 同约定
--      （B2/B3 引入 typed reader 时再按需迁移）。
--   3. timestamp 列统一 TIMESTAMPTZ（cs-user 现有 users 表用 TIMESTAMP 而非
--      TIMESTAMPTZ 是历史包袱；新表遵循 RFC 3339 best practice 用带时区的类型）。
--   4. tenant_admins 主键用复合 (tenant_id, user_id) — 防止同租户内同 user 重复
--      授权；全局范围一个 user 仍可在多个租户里担任管理员（多租户成员资格）。
--   5. role 取值集合 owner | admin | billing — 见 MULTI_TENANCY_DESIGN §7。
--      B1 不加 CHECK 约束（应用层校验），避免后续枚举扩展需要迁移。
--
-- Bootstrap：插入 tenant_id='default' 行，与 A6 的 tenant_configs('default')
-- 对齐 — Phase B 全部代码默认在 default 租户上下文中运行，真实多租户从 B5 起
-- 才动态选择 tenant_id。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS tenants (
    -- 主键：tenant_id（UUID 字符串，与 tenant_configs.tenant_id 同语义）
    tenant_id              TEXT                     NOT NULL PRIMARY KEY,

    -- URL-safe slug：全局唯一，用于 URL 路径 / 子域 / API 引用
    -- 约束 [a-z0-9-]{3,32} 在应用层校验（B2 引入 Slug validate）
    slug                   VARCHAR(32)              NOT NULL,

    -- 显示名（人类可读，UTF-8，无格式约束）
    display_name           VARCHAR(191)             NOT NULL,

    -- 租户生命周期状态：active | suspended | deleted
    status                 VARCHAR(32)              NOT NULL DEFAULT 'active',

    -- 计费版本：team | enterprise | self-hosted | ...
    edition                VARCHAR(32)              NOT NULL DEFAULT 'team',

    -- 邮箱域白名单（JSON array of strings，应用层 marshal/unmarshal）
    -- 例：'["example.com","example.cn"]' — 用于 B5 邮箱域路由
    email_domains          TEXT                     NOT NULL DEFAULT '[]',

    -- 功能开关（JSON object）— 例：'{"ai_assistant":true,"sso":false}'
    features               TEXT                     NOT NULL DEFAULT '{}',

    -- 配额上限（JSON object）— 例：'{"max_users":100,"max_seats":50}'
    limits                 TEXT                     NOT NULL DEFAULT '{}',

    -- 通用设置（JSON object）— 例：'{"locale":"zh-CN","timezone":"Asia/Shanghai"}'
    settings               TEXT                     NOT NULL DEFAULT '{}',

    -- 软删除 / 注销请求时间戳（NULL 表示活跃）
    deletion_requested_at  TIMESTAMPTZ,
    deleted_at             TIMESTAMPTZ,

    -- 审计字段
    created_at             TIMESTAMPTZ              NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ              NOT NULL DEFAULT now(),

    CONSTRAINT uq_tenants_slug UNIQUE (slug)
);

-- tenant_admins：租户管理员授权记录
-- 复合主键 (tenant_id, user_id) — 同租户内一个 user 只能有一条 active 授权
CREATE TABLE IF NOT EXISTS tenant_admins (
    tenant_id              TEXT                     NOT NULL,
    user_id                VARCHAR(191)             NOT NULL,
    -- 角色：owner | admin | billing（应用层枚举校验）
    role                   VARCHAR(32)              NOT NULL,
    -- 授权人 user_id（必须存在 users 表；自引用场景下可等于 user_id）
    granted_by             VARCHAR(191)             NOT NULL,
    granted_at             TIMESTAMPTZ              NOT NULL DEFAULT now(),
    -- 撤销时间戳（NULL = active；非 NULL = 历史记录，主键冲突时复用历史行需先撤销）
    revoked_at             TIMESTAMPTZ,

    PRIMARY KEY (tenant_id, user_id),

    CONSTRAINT fk_tenant_admins_tenant
        FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id)
        ON DELETE CASCADE,

    CONSTRAINT fk_tenant_admins_user
        FOREIGN KEY (user_id)
        REFERENCES users(subject_id)
        ON DELETE CASCADE,

    CONSTRAINT fk_tenant_admins_granted_by
        FOREIGN KEY (granted_by)
        REFERENCES users(subject_id)
        ON DELETE RESTRICT
);

-- 全局反查：给定 user_id 找到所有他/她拥有 active 授权的租户
-- 部分索引：revoked_at IS NULL 排除已撤销记录（active-only 索引扫描更高效）
CREATE INDEX IF NOT EXISTS idx_tenant_admins_user_active
    ON tenant_admins (user_id)
    WHERE revoked_at IS NULL;

-- 注释（供 psql \d+ 与 DB 工具展示）
COMMENT ON TABLE tenants IS '租户主表 — Phase B1 canonical tenant entity（配合 tenant_configs / tenant_admins）';
COMMENT ON COLUMN tenants.tenant_id IS '租户唯一标识（UUID 字符串，与 tenant_configs.tenant_id 同语义）';
COMMENT ON COLUMN tenants.slug IS '全局唯一 URL-safe slug [a-z0-9-]{3,32}（应用层校验）';
COMMENT ON COLUMN tenants.status IS '租户生命周期状态：active | suspended | deleted';
COMMENT ON COLUMN tenants.edition IS '计费版本：team | enterprise | self-hosted';
COMMENT ON COLUMN tenants.email_domains IS '邮箱域白名单 JSON 数组（B5 邮箱路由用）';
COMMENT ON COLUMN tenants.features IS '功能开关 JSON 对象';
COMMENT ON COLUMN tenants.limits IS '配额上限 JSON 对象';
COMMENT ON COLUMN tenants.settings IS '通用设置 JSON 对象（locale/timezone/...）';

COMMENT ON TABLE tenant_admins IS '租户管理员授权 — user × tenant 多对多';
COMMENT ON COLUMN tenant_admins.role IS '角色枚举：owner | admin | billing';
COMMENT ON COLUMN tenant_admins.granted_by IS '授权人 user_id（必须存在 users 表）';
COMMENT ON COLUMN tenant_admins.revoked_at IS '撤销时间戳；NULL = active';

-- Bootstrap default tenant — 与 A6 的 tenant_configs('default') 对齐
-- display_name='Default Tenant' / slug='default' / status='active' / edition='enterprise'
-- （Phase B 全部代码默认在 default 租户上下文中运行）
INSERT INTO tenants (tenant_id, slug, display_name, status, edition)
VALUES ('default', 'default', 'Default Tenant', 'active', 'enterprise')
ON CONFLICT (tenant_id) DO NOTHING;

-- 给 tenant_configs 加 FK — A2 的 TODO 注释里承诺的 "Phase B 升级为 tenants(tenant_id) FK"
-- 已 bootstrap default tenant，所以 default 行的 FK 校验通过
ALTER TABLE tenant_configs
    ADD CONSTRAINT fk_tenant_configs_tenant
    FOREIGN KEY (tenant_id)
    REFERENCES tenants(tenant_id)
    ON DELETE RESTRICT;  -- 不允许删除有 config 引用的 tenant（先删 config 再删 tenant）

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- 先删 FK 再删 tenants，避免 CASCADE 把 tenant_configs 行带走
ALTER TABLE tenant_configs
    DROP CONSTRAINT IF EXISTS fk_tenant_configs_tenant;

DROP INDEX IF EXISTS idx_tenant_admins_user_active;
DROP TABLE IF EXISTS tenant_admins;
DROP TABLE IF EXISTS tenants;

-- +goose StatementEnd
