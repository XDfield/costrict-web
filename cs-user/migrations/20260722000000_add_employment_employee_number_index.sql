-- Phase B：employment_identities.employee_number 反查索引
--
-- 配合 team-namespace workflow 的 UserRef 解析路径（doc v1.1 §5.2）：
-- 上游 server 拿到 JWT 里的 employee_number 后，通过 cs-user RPC
-- `GET /api/internal/users/search?employee_number=...&limit=1` 反查物理用户，
-- 服务端 SQL 形如：
--   SELECT u.* FROM users u
--   JOIN employment_identities e ON e.user_subject_id = u.subject_id
--                                  AND e.tenant_id = u.tenant_id
--   WHERE u.tenant_id = ?
--     AND u.is_active = true
--     AND e.employee_number = ?
--     AND e.deleted_at IS NULL
--   ORDER BY e.last_synced_at DESC
--   LIMIT 1;
--
-- 索引设计：
--   * 复合 (tenant_id, employee_number) — 与上面 SQL 的等值前缀对齐，按
--     tenant 维度切片扫描。
--   * 部分 WHERE deleted_at IS NULL — 排除软删行，active-only 扫描更高效，
--     同时匹配 gorm.DeletedAt 的隐式过滤。
--   * **非唯一**：本期 (tenant_id, employee_number) 唯一性尚未强制（需要
--     enterprise_uid 字段先落地，见 EmploymentIdentity 模型注释）；
--     命中多行时应用层按 last_synced_at DESC 取一条。Phase B 严格化后
--     再换成 UNIQUE 部分索引。
--
-- +goose Up
-- +goose StatementBegin

CREATE INDEX IF NOT EXISTS idx_employment_identities_tenant_emp_no
    ON employment_identities (tenant_id, employee_number)
    WHERE deleted_at IS NULL AND employee_number IS NOT NULL;

COMMENT ON INDEX idx_employment_identities_tenant_emp_no IS
    'Phase B：按 (tenant_id, employee_number) 反查用户；非唯一，命中多行时应用层按 last_synced_at DESC 取一条。Phase B 严格化后改为 UNIQUE 部分索引。';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_employment_identities_tenant_emp_no;

-- +goose StatementEnd
