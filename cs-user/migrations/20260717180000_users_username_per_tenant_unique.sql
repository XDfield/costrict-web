-- Phase B7：users.username 从全局唯一改为 (tenant_id, username) 复合唯一
--
-- 这是 Phase B 的最后一项 schema 变更：随着 tenant_id 列在 B2 落地，"username 全局
-- 唯一" 这个约束变得过严 —— 两个不同 tenant 各自都应该能有一个叫 "alice" 的用户。
-- 本迁移把 1D unique 降级为 2D 复合 unique。
--
-- 设计决策（与 MULTI_TENANCY_DESIGN §7 / §8 对齐）：
--
--   1. 仅改 users.username —— email 全局唯一保留（用户用 email 登录，跨 tenant 仍然
--      是同一个人；SSO 桥接、密码找回、邮件通知都依赖 email 全局唯一）。
--   2. user_auth_identities.external_key 全局唯一保留 —— external_key 形如
--      `{provider}:{provider_user_id}`，是跨 tenant 的 SSO 去重锚点（同一 GitHub
--      账号跨 tenant 仍指向同一个人）。如果改成 (tenant_id, external_key) 复合唯一，
--      会破坏 B3b 后续的跨 tenant 身份合并（identity transfer）链路。
--   3. employment_identities 当前无 unique 约束，本迁移不动。
--   4. 复合索引用 CREATE UNIQUE INDEX 而非 ALTER TABLE ADD CONSTRAINT —— cs-user
--      仓库历史上有两种风格混用（idx_user_subject_id 是 CONSTRAINT，
--      idx_user_auth_identities_external_key 是 INDEX）；这里用 INDEX 风格是为了
--      方便 B8+ 如果需要 partial unique（WHERE deleted_at IS NULL）能直接修改索引
--      而不必先 DROP CONSTRAINT。
--   5. 数据兼容：当前所有用户的 tenant_id 都是 'default'（B2 回填），所以现有
--      "alice/default" 的数据在迁移后仍然唯一，不会冲突。新建 tenant 后才有真正的
--      多 tenant 用户重叠场景。
--
-- 不动应用层：GetOrCreateUser / SyncUser 当前都是按 (sub, universal_id) 找用户，
-- 不依赖 username 唯一性；service 层不需要随本迁移改动。
--
-- +goose Up
-- +goose StatementBegin

-- 1. 删除全局唯一约束（两种 DDL 风格都覆盖：CONSTRAINT 形式来自
--    20260408154000_migrate_users_to_subject_id_and_serial_pk.sql 第 72 行，
--    不同 PostgreSQL 版本对 "constraint or index" 的元数据处理略有差异，
--    两条语句都跑一遍 + IF EXISTS 保证幂等）。
ALTER TABLE users DROP CONSTRAINT IF EXISTS idx_user_username;
DROP INDEX IF EXISTS idx_user_username;

-- 2. 加复合唯一索引。INCLUDE tenant_id 让 "alice" 可以在多个 tenant 下并存。
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_username
    ON users(tenant_id, username);

COMMENT ON INDEX idx_users_tenant_username IS
    '复合唯一索引：(tenant_id, username) — 同一 tenant 内 username 唯一，跨 tenant 可重复（Phase B7）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- 回滚：恢复全局唯一约束。注意若已有跨 tenant 同名用户，回滚会失败 —— 这是预期，
-- 因为回滚到 Phase B 前的多租户状态本身就不一致。
DROP INDEX IF EXISTS idx_users_tenant_username;

ALTER TABLE users
    ADD CONSTRAINT idx_user_username UNIQUE (username);

-- +goose StatementEnd
