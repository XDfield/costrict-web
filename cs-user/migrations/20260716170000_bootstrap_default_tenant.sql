-- +goose Up
-- +goose StatementBegin

-- Phase A (A6): bootstrap the implicit-single-tenant default row.
--
-- Phase A runs in single-tenant mode per ROADMAP §5 ("如果暂时不需要多租户：
-- 只做阶段 A 即可，跳过 B/C。tenant_id 全部默认填 default"). A4's
-- ApplyEnterpriseMapping reads tenant_configs by tenant_id; if no row exists,
-- the OAuth callback would fail to find provider mapping. This migration
-- guarantees the default row is present from first boot.
--
-- Idempotent via ON CONFLICT (tenant_id) DO NOTHING — re-running goose Up
-- after a partial rollback, or running it on a DB where an operator has
-- already inserted the row manually, is a safe no-op. (sqlite >= 3.24
-- supports the same syntax; the integration test exercises this against
-- sqlite to confirm.)
--
-- config_yaml ships as '{}' — Phase A stores the blob verbatim; A4
-- introduces the typed reader and treats missing sections as "no providers
-- enabled" (which is the correct Phase A default). Operators who want to
-- enable an employment provider before A4's typed reader lands can UPDATE
-- the row directly with a fuller YAML blob; A4's reader will pick it up.
INSERT INTO tenant_configs (tenant_id, config_yaml)
VALUES ('default', '{}')
ON CONFLICT (tenant_id) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverting this migration removes the default row. Down is intended for
-- development rollback only — production rollback would leave A4's reader
-- with no tenant_configs row to read, breaking the OAuth callback. The
-- WHERE clause ensures we only delete the bootstrap row, preserving any
-- operator-supplied tenant_configs the operator may have added.
DELETE FROM tenant_configs WHERE tenant_id = 'default';

-- +goose StatementEnd
