-- Dev seed for E2E: tenant + git_server + binding + sample user.
--
-- Apply:
--   psql -h <host> -U costrict -d cs_user -f scripts/dev-seed.sql
--
-- After applying, REPLACE the Gitea admin token below (search for <REPLACE_WITH_LOCAL_PAT>)
-- with a fresh PAT minted from your local fork Gitea (admin user → Settings → Applications
-- → Generate New Token, scope: `write:admin` + `write:organization`). The PAT is what
-- server's gitsync.GitServerResolver hands to *gitsync.Client for provisioning orgs,
-- bot users, repos and branch protection during E2E runs.
--
-- This seed is IDEMPOTENT (ON CONFLICT DO NOTHING / DO UPDATE) so re-running
-- after a partial apply is safe. It targets the cs_user database (cs-user owns
-- git_servers + tenants + users + employment_identities). server's own DB
-- (team_ns + team_bot_credentials) is created on-the-fly by the test setup.
--
-- See docs/repo-management/E2E_TESTING.md for the full runbook.

BEGIN;

-- 1. tenant row (E2E uses its own tenant_id; tests pass this in via E2E_TENANT_ID).
INSERT INTO tenants (tenant_id, slug, display_name, status, edition)
VALUES ('tenant-e2e', 'tenant-e2e', 'E2E Test Tenant', 'active', 'enterprise')
ON CONFLICT (tenant_id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    status       = EXCLUDED.status;

-- 2. tenant_configs row (A2 — required by tenants FK in B1).
INSERT INTO tenant_configs (tenant_id, config_yaml)
VALUES ('tenant-e2e', '{}')
ON CONFLICT (tenant_id) DO NOTHING;

-- 3. git_servers row — REPLACE token + admin_user/admin_password placeholders
--    with values from your local fork Gitea. admin_user/admin_password are
--    REQUIRED for token-mint endpoints (POST /users/{name}/tokens sits behind
--    Gitea's reqBasicOrRevProxyAuth middleware, which 401s admin PAT auth).
INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled)
VALUES (
    'gitea-local',
    'gitea',
    'http://127.0.0.1:3000',
    'Local fork Gitea (E2E)',
    '{
        "admin_token": "<REPLACE_WITH_LOCAL_PAT>",
        "admin_user": "<REPLACE_WITH_GITEA_ADMIN_USER>",
        "admin_password": "<REPLACE_WITH_GITEA_ADMIN_PASSWORD>"
    }'::jsonb,
    false,
    true
)
ON CONFLICT (server_id) DO UPDATE SET
    endpoint     = EXCLUDED.endpoint,
    config       = EXCLUDED.config,
    enabled      = EXCLUDED.enabled,
    updated_at   = now();

-- 4. Bind tenant to git_server (1:1 unique per idx_tenants_git_server).
UPDATE tenants
SET git_server_id = 'gitea-local'
WHERE tenant_id = 'tenant-e2e';

-- 5. Sample user + employment identity. The E2E members:sync flow resolves
-- UserRef{employee_number:"E001"} → this user's gitea_username. If the user
-- has no gitea_username yet, the test will first drive cs-user's apply-enterprise-
-- mapping / giteasync lazy-create before running the sync assertions.
INSERT INTO users (subject_id, tenant_id, username, display_name, email, is_active, created_at, updated_at)
VALUES (
    'usr_e2e_1',
    'tenant-e2e',
    'e2e-user-1',
    'E2E User One',
    'e2e-user-1@example.com',
    TRUE,
    now(),
    now()
)
ON CONFLICT (subject_id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    email        = EXCLUDED.email,
    is_active    = EXCLUDED.is_active,
    updated_at   = now();

-- 6. Employment identity — drives SearchByEmployeeNumber("E001") resolution.
INSERT INTO employment_identities (
    user_subject_id, tenant_id, provider, employee_number, job_title,
    sync_status, last_synced_at, next_sync_due_at, created_at, updated_at
)
VALUES (
    'usr_e2e_1',
    'tenant-e2e',
    'manual',
    'E001',
    'Software Engineer',
    'fresh',
    now(),
    now() + interval '1 day',
    now(),
    now()
)
ON CONFLICT DO NOTHING;

COMMIT;

-- Sanity check (run manually after apply to confirm seed took):
--   SELECT tenant_id, git_server_id FROM tenants WHERE tenant_id = 'tenant-e2e';
--   SELECT server_id, kind, endpoint, enabled FROM git_servers WHERE server_id = 'gitea-local';
--   SELECT subject_id, tenant_id, display_name FROM users WHERE subject_id = 'usr_e2e_1';
--   SELECT user_subject_id, employee_number FROM employment_identities WHERE user_subject_id = 'usr_e2e_1';

-- To tear down after E2E session (preserves git_server if you want to keep it):
--   DELETE FROM employment_identities WHERE user_subject_id = 'usr_e2e_1';
--   DELETE FROM users         WHERE subject_id = 'usr_e2e_1';
--   UPDATE tenants SET git_server_id = NULL WHERE tenant_id = 'tenant-e2e';
--   DELETE FROM tenant_configs WHERE tenant_id = 'tenant-e2e';
--   DELETE FROM tenants        WHERE tenant_id = 'tenant-e2e';
--   DELETE FROM git_servers    WHERE server_id = 'gitea-local';
