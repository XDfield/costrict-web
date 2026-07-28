-- Git Ownership Refactor Phase 4 (destructive):
-- Drop cs-user's git-server / user_gitea_binding tables and the
-- tenants.git_server_id column. After Phase 4, git ownership lives
-- exclusively on the @server side (server.user_git_binding + git_servers +
-- tenant_git_server_binding). cs-user only emits user.created events.
--
-- Plain DROP IF EXISTS forms (no DO blocks; this goose version's SQL parser
-- rejects PL/pgSQL). goose tracks applied versions so re-runs don't happen;
-- IF EXISTS guards protect dev environments that ran Phase 1 migrations.

-- +goose Up

-- Drop legacy tenant → git_server pointer + its unique index.
DROP INDEX IF EXISTS idx_tenants_git_server;
ALTER TABLE IF EXISTS tenants DROP COLUMN IF EXISTS git_server_id;

-- Drop the Gitea binding mirror (server-side user_git_binding is canonical).
DROP INDEX IF EXISTS idx_user_gitea_binding_tenant;
DROP INDEX IF EXISTS uq_user_gitea_binding_gitea_username;
DROP TABLE IF EXISTS user_gitea_binding;

-- Drop git_servers + the (server-side-only) tenant_git_server_binding clone
-- if a dev env happened to create it. Production server-side table lives in
-- the @server database, not here.
DROP INDEX IF EXISTS idx_git_servers_template;
DROP TABLE IF EXISTS tenant_git_server_binding;
DROP TABLE IF EXISTS git_servers;

-- +goose Down

-- Down is best-effort: reconstructs the empty schema matching the
-- 20260721150010 / 20260721160000 originals so a rollback at least leaves
-- the tables queryable. Data is NOT restored (cs-user had no production
-- data at cutover).

CREATE TABLE IF NOT EXISTS git_servers (
    server_id    TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    endpoint     TEXT NOT NULL,
    display_name TEXT NOT NULL,
    config       TEXT NOT NULL DEFAULT '{}',
    is_template  BOOLEAN NOT NULL DEFAULT FALSE,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenant_git_server_binding (
    tenant_id     TEXT PRIMARY KEY,
    git_server_id TEXT NOT NULL REFERENCES git_servers(server_id) ON DELETE CASCADE,
    bound_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_gitea_binding (
    user_subject_id TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    gitea_uid       BIGINT,
    gitea_username  VARCHAR(64) NOT NULL,
    sync_status     VARCHAR(32) NOT NULL DEFAULT 'pending',
    last_synced_at  TIMESTAMPTZ,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_subject_id, tenant_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_gitea_binding_gitea_username
    ON user_gitea_binding (gitea_username);
CREATE INDEX IF NOT EXISTS idx_user_gitea_binding_tenant
    ON user_gitea_binding (tenant_id, sync_status);

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS git_server_id VARCHAR(64);
