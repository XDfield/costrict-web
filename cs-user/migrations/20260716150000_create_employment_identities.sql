-- +goose Up
-- Phase A (A1): employment_identities table — per-user enterprise-identity
-- snapshot written by ApplyEnterpriseMapping in the OAuth callback (lands in
-- A4). Source schema: docs/identity-tenant/CS_USER_SERVICE_DESIGN.md §9.2
-- (lines 674-705). Translated to cs-user's SQL conventions (BIGSERIAL PK,
-- text columns, timestamptz, no cs_user. schema prefix, no SQL FK — same
-- style as 20260524000000_create_user_auth_identities_table.sql).
--
-- Phase A scope: ONE snapshot row per user (partial unique index on
-- user_subject_id WHERE deleted_at IS NULL). Single-tenant mode — the
-- tenant_id column + (tenant_id, enterprise_uid) unique index land in Phase B
-- (MULTI_TENANCY §6.5.1, §8.3) along with tenant_id on every other table.

CREATE TABLE IF NOT EXISTS employment_identities (
    id BIGSERIAL PRIMARY KEY,
    user_subject_id text NOT NULL,
    provider text NOT NULL,
    employee_number text,
    cost_center text,
    org_path text,
    direct_manager_subject_id text,
    direct_manager_external_ref text,
    job_title text,
    job_level text,
    employment_type text,
    hire_date date,
    regular_date date,
    work_location text,
    attributes jsonb NOT NULL DEFAULT '{}',
    sync_status text NOT NULL DEFAULT 'fresh',
    last_synced_at timestamptz NOT NULL DEFAULT now(),
    next_sync_due_at timestamptz NOT NULL DEFAULT now(),
    raw_payload_hash text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_employment_identities_user_subject_id ON employment_identities(user_subject_id);
CREATE INDEX IF NOT EXISTS idx_employment_identities_provider ON employment_identities(provider);
CREATE INDEX IF NOT EXISTS idx_employment_identities_cost_center ON employment_identities(cost_center);
CREATE INDEX IF NOT EXISTS idx_employment_identities_manager ON employment_identities(direct_manager_subject_id);

-- One active snapshot per user. Soft-deleted rows are excluded so a user can
-- be re-onboarded after offboarding (deleted_at gets a timestamp, new row
-- inserts cleanly). Postgres partial unique index; sqlite (test boundary)
-- approximates with a non-partial unique index via AutoMigrate — see
-- employment_identity_test.go for the contract this protects.
CREATE UNIQUE INDEX IF NOT EXISTS uq_employment_identities_user_subject_id
    ON employment_identities(user_subject_id)
    WHERE deleted_at IS NULL;

COMMENT ON TABLE employment_identities IS '用户雇佣身份快照表，每个用户一行，记录 employee_number / cost_center / org_path 等企业身份字段，由 OAuth callback 的 ApplyEnterpriseMapping 写入';
COMMENT ON COLUMN employment_identities.user_subject_id IS '本地用户 subject_id（应用层引用 users.subject_id，不做 SQL FK）';
COMMENT ON COLUMN employment_identities.provider IS '最近一次同步来源 provider（idtrust / aad / ldap 等 employment provider）';
COMMENT ON COLUMN employment_identities.employee_number IS '员工工号（IdP employeeNumber / employeeId 映射）';
COMMENT ON COLUMN employment_identities.cost_center IS '成本中心';
COMMENT ON COLUMN employment_identities.org_path IS '组织路径（/总部/研发/平台组 形式）';
COMMENT ON COLUMN employment_identities.direct_manager_subject_id IS '直属上级 subject_id（解析成功时填，要求上级也是 cs-user 用户）';
COMMENT ON COLUMN employment_identities.direct_manager_external_ref IS '直属上级原始引用（解析失败时保留 DN / UPN / email）';
COMMENT ON COLUMN employment_identities.job_title IS '岗位名称';
COMMENT ON COLUMN employment_identities.job_level IS '岗位级别（P7 / T6 等）';
COMMENT ON COLUMN employment_identities.employment_type IS '雇佣类型（full_time / contractor / intern）';
COMMENT ON COLUMN employment_identities.hire_date IS '入职日期';
COMMENT ON COLUMN employment_identities.regular_date IS '转正日期';
COMMENT ON COLUMN employment_identities.work_location IS '工作地点';
COMMENT ON COLUMN employment_identities.attributes IS '租户自定义扩展字段（jsonb，由 provider-mapping.yaml 配置）';
COMMENT ON COLUMN employment_identities.sync_status IS '同步状态：fresh（最新）/ stale（待刷新）/ error（同步失败）';
COMMENT ON COLUMN employment_identities.last_synced_at IS '最近成功同步时间';
COMMENT ON COLUMN employment_identities.next_sync_due_at IS '下次同步到期时间（按 provider TTL）';
COMMENT ON COLUMN employment_identities.raw_payload_hash IS 'IdP 原始 payload hash，用于跨同步运行的变更检测';
COMMENT ON COLUMN employment_identities.deleted_at IS '软删除时间戳（gorm.DeletedAt）';

-- +goose Down
DROP INDEX IF EXISTS uq_employment_identities_user_subject_id;
DROP INDEX IF EXISTS idx_employment_identities_manager;
DROP INDEX IF EXISTS idx_employment_identities_cost_center;
DROP INDEX IF EXISTS idx_employment_identities_provider;
DROP INDEX IF EXISTS idx_employment_identities_user_subject_id;
DROP TABLE IF EXISTS employment_identities;
