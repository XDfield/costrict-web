-- Phase B2：为 user-domain 表添加 tenant_id 列
--
-- 这是 Phase B（tenant 维度落地）使多租户隔离真正生效的第一步：在每张 user 相关
-- 表上加入 tenant_id 列、回填 'default'、建立到 tenants(tenant_id) 的 FK 与索引。
--
-- 涉及的表（user_profile 表尚未存在，跳过 — 其迁移会随该表首次落地一并加入）：
--
--   users                  — 本地用户主表
--   user_auth_identities   — 外部登录身份
--   employment_identities  — 企业雇佣身份快照（A1 落地）
--
-- Schema 决策（与 cs-user 现有约定保持一致；偏离 MULTI_TENANCY_DESIGN §7 在 B1
-- 迁移头里已说明）：
--
--   1. tenant_id 用 TEXT（UUID 字符串）而非 UUID 列类型 — 与 tenants.tenant_id
--      / users.id / tenant_configs.tenant_id 对齐，避免 cs-user 全仓升级 UUID 列类型。
--   2. NOT NULL DEFAULT 'default' — 让回填和后续 INSERT 在未显式指定 tenant_id 时
--      自动落到 default 租户；与 A6 的 default tenant bootstrap 对齐。
--   3. FK ON DELETE RESTRICT（三张表统一）— 不允许删除仍持有用户/身份的 tenant；
--      tenant 的生命周期走 status='deleted' 路径（见 tenant.go 注释），物理删除是
--      极少见的运维操作，RESTRICT 让运维必须先迁移用户再删 tenant。
--   4. 索引：每张表加 idx_<table>_tenant_id 覆盖 "给定 tenant 找全部 user" 的热路径；
--      employment_identities 额外加 (tenant_id, user_subject_id) 复合索引，覆盖
--      跨租户查 "给定 tenant + subject_id" 的反向解析（MULTI_TENANCY §8.3）。
--   5. (tenant_id, enterprise_uid) 唯一索引 NOT 在 B2 落地 — enterprise_uid 列本身
--      要等后续 Phase B 子任务引入（A1 注释里已说明），届时一并迁移。
--
-- 与 A1/A2/B1 一致，本迁移只动 schema，不动应用层；service/handler 改造由 B3
-- （tenant 解析 / context 注入）起逐子任务落地。
--
-- +goose Up
-- +goose StatementBegin

-- ------------------------------------------------------------------
-- users
-- ------------------------------------------------------------------
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';

ALTER TABLE users
    ADD CONSTRAINT fk_users_tenant
        FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id)
        ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_users_tenant_id ON users(tenant_id);

COMMENT ON COLUMN users.tenant_id IS '所属租户 ID（默认 default；FK 到 tenants.tenant_id，RESTRICT 删除）';

-- ------------------------------------------------------------------
-- user_auth_identities
-- ------------------------------------------------------------------
ALTER TABLE user_auth_identities
    ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';

ALTER TABLE user_auth_identities
    ADD CONSTRAINT fk_user_auth_identities_tenant
        FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id)
        ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_user_auth_identities_tenant_id ON user_auth_identities(tenant_id);

COMMENT ON COLUMN user_auth_identities.tenant_id IS '所属租户 ID（默认 default；FK 到 tenants.tenant_id，RESTRICT 删除）';

-- ------------------------------------------------------------------
-- employment_identities
-- ------------------------------------------------------------------
ALTER TABLE employment_identities
    ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default';

ALTER TABLE employment_identities
    ADD CONSTRAINT fk_employment_identities_tenant
        FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id)
        ON DELETE RESTRICT;

-- 单列索引覆盖 "给定 tenant 找所有雇佣身份"
CREATE INDEX IF NOT EXISTS idx_employment_identities_tenant_id
    ON employment_identities(tenant_id);

-- 复合索引覆盖 "给定 tenant + subject_id" 反向解析（MULTI_TENANCY §8.3）
CREATE INDEX IF NOT EXISTS idx_employment_identities_tenant_user
    ON employment_identities(tenant_id, user_subject_id);

COMMENT ON COLUMN employment_identities.tenant_id IS '所属租户 ID（默认 default；FK 到 tenants.tenant_id，RESTRICT 删除；与 user_subject_id 组成复合索引支持跨租户反向解析）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- users
DROP INDEX IF EXISTS idx_users_tenant_id;
ALTER TABLE users DROP CONSTRAINT IF EXISTS fk_users_tenant;
ALTER TABLE users DROP COLUMN IF EXISTS tenant_id;

-- user_auth_identities
DROP INDEX IF EXISTS idx_user_auth_identities_tenant_id;
ALTER TABLE user_auth_identities DROP CONSTRAINT IF EXISTS fk_user_auth_identities_tenant;
ALTER TABLE user_auth_identities DROP COLUMN IF EXISTS tenant_id;

-- employment_identities
DROP INDEX IF EXISTS idx_employment_identities_tenant_user;
DROP INDEX IF EXISTS idx_employment_identities_tenant_id;
ALTER TABLE employment_identities DROP CONSTRAINT IF EXISTS fk_employment_identities_tenant;
ALTER TABLE employment_identities DROP COLUMN IF EXISTS tenant_id;

-- +goose StatementEnd
