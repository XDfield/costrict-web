-- Phase C1：platform_admins 表迁移
--
-- 三级权限模型（MULTI_TENANCY_DESIGN §14）的最顶层：platform_admin 是 CoStrict
-- 内部跨租户管理员。与 tenant_admins（§7.2，B1 已落地）平级存在，互不混入，
-- 避免"哪个 user 是什么角色"在一张表里堆 N 个 nullable 列。
--
--   tenant_admins   — B1：tenant 范围内 user × tenant 多对多
--   platform_admins — C1：跨 tenant 平台级权限，每 user 至多一行（PK = user_id）
--
-- 设计取舍（与 tenant_admins 一致 + 偏离 MULTI_TENANCY §7.3）：
--
--   1. user_id 用 VARCHAR(191) 而非 UUID 列类型 — 与 tenant_admins.user_id
--      / users.id 对齐，沿用 cs-user 现有约定。MULTI_TENANCY §7.3 用 UUID 是
--      理想化方案；cs-user 实际 users.id 是 VARCHAR(191)（看
--      20260401100000_create_users_table.sql）。
--   2. scope 取值集合 full | support | read_only — 见 §14.3。B1 风格延续：
--      不加 CHECK 约束，应用层校验，未来加新 scope 不需要迁移。
--   3. granted_by RESTRICT 删除（防误删授权人）；user_id CASCADE 删除
--      （删 user 自动清掉对应 platform_admin 记录）。
--   4. 没有 revoked_at 列 — platform_admin 撤销走 DELETE 而非软删，与 §7.3
--      设计一致（"platform_admins 是当前活跃的管理员名单，历史审计走审计日志，
--      不沉淀在本表"）。tenant_admins 有 revoked_at 是因为 tenant 内角色变化
--      频繁、需要保留行级历史；platform_admin 调整低频，审计日志足够。
--
-- Bootstrap：本迁移不插入任何初始 platform_admin。operator 部署后通过手写
-- SQL 给首位 platform_admin 授权（典型路径：首位用户登录产生 users 行后，
-- operator 直连库 INSERT INTO platform_admins (user_id, granted_by, scope)
-- VALUES ('usr_xxx', 'usr_xxx', 'full');）。首位 platform_admin 之后的授予
-- 走 C2 platform_admin API（待 Phase C2 落地）。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS platform_admins (
    -- 主键：user_id（一个 user 至多一行 platform_admin 记录）
    user_id                VARCHAR(191)             NOT NULL PRIMARY KEY,

    -- 授权人 user_id（初始 bootstrap 时通常 = user_id，表示 self-bootstrap）
    granted_by             VARCHAR(191)             NOT NULL,
    granted_at             TIMESTAMPTZ              NOT NULL DEFAULT now(),

    -- scope：full | support | read_only（应用层枚举校验，无 CHECK 约束）
    scope                  VARCHAR(32)              NOT NULL DEFAULT 'full',

    CONSTRAINT fk_platform_admins_user
        FOREIGN KEY (user_id)
        REFERENCES users(subject_id)
        ON DELETE CASCADE,

    CONSTRAINT fk_platform_admins_granted_by
        FOREIGN KEY (granted_by)
        REFERENCES users(subject_id)
        ON DELETE RESTRICT
);

-- scope 反查：给定 scope 找所有该 scope 的 platform_admin（如列出所有 full 权限管理员）
CREATE INDEX IF NOT EXISTS idx_platform_admins_scope
    ON platform_admins (scope);

COMMENT ON TABLE platform_admins IS '平台管理员授权 — Phase C1：跨租户平台级权限（user_id PK，每 user 至多一行）';
COMMENT ON COLUMN platform_admins.user_id IS '被授权用户 user_id（FK users.id CASCADE）';
COMMENT ON COLUMN platform_admins.granted_by IS '授权人 user_id（FK users.id RESTRICT；初始 bootstrap 时通常 = user_id）';
COMMENT ON COLUMN platform_admins.scope IS '权限范围枚举：full | support | read_only（应用层校验）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_platform_admins_scope;
DROP TABLE IF EXISTS platform_admins;

-- +goose StatementEnd
