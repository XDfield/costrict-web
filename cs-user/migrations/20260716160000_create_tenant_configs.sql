-- +goose Up
-- Phase A (A2): tenant_configs table — per-tenant YAML configuration store.
-- Ships the minimal Phase A shape: tenant_id PK + a single YAML blob column.
-- Phase B (MULTI_TENANCY_DESIGN.md §9.2, lines 807-823) expands this into
-- typed subsections and adds the tenants(tenant_id) FK once the tenants table
-- lands (B1).
--
-- Phase A does NOT parse config_yaml — it stores the blob verbatim. A4
-- (ApplyEnterpriseMapping) introduces the first typed reader via
-- gopkg.in/yaml.v3 for the employment_providers section. Splitting into
-- per-section YAML columns (provider_mapping / username_strategy /
-- display_name_strategy / employment_providers / features /
-- enterprise_schema_ext) is deferred to Phase B's final shape.
--
-- tenant_id is text (not UUID) in Phase A so we don't lock the project to
-- UUID-before-Phase-B1-lands. The default bootstrap row written by A6 uses
-- tenant_id="default" per ROADMAP §5 ("tenant_id 全部默认填 default").

CREATE TABLE IF NOT EXISTS tenant_configs (
    tenant_id text PRIMARY KEY,
    config_yaml text NOT NULL DEFAULT '{}',
    updated_by text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE tenant_configs IS '租户配置表（最小 schema）：每个租户一行 YAML 配置，Phase A 仅存储原始 blob，由 A4 引入类型化读取';
COMMENT ON COLUMN tenant_configs.tenant_id IS '租户 ID（Phase A 文本占位；Phase B 升级为 tenants(tenant_id) UUID FK）';
COMMENT ON COLUMN tenant_configs.config_yaml IS 'YAML 配置原文（Phase A：单 blob；Phase B：拆分为 provider_mapping/username_strategy/employment_providers/features 等列）';
COMMENT ON COLUMN tenant_configs.updated_by IS '最后写入者 subject_id（Phase A 仅内部调用，无管理端 endpoint）';
COMMENT ON COLUMN tenant_configs.updated_at IS '最近写入时间';
COMMENT ON COLUMN tenant_configs.created_at IS '行创建时间';

-- +goose Down
DROP TABLE IF EXISTS tenant_configs;
