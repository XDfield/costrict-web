-- Stable repository-level binding and first-discovery state for Git-backed capabilities.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS git_capability_repositories (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    git_server_id         VARCHAR(64) NOT NULL REFERENCES git_servers(server_id) ON DELETE CASCADE,
    git_repo_id           BIGINT      NOT NULL,
    repository_id         UUID        NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    registry_id           UUID        NOT NULL REFERENCES capability_registries(id) ON DELETE CASCADE,
    full_name             TEXT        NOT NULL,
    repo_kind             VARCHAR(32) NOT NULL DEFAULT 'standalone',
    identification_status VARCHAR(32) NOT NULL DEFAULT 'unknown',
    visibility            VARCHAR(16) NOT NULL DEFAULT 'public',
    git_remote_url        TEXT        NOT NULL,
    default_branch        TEXT        NOT NULL,
    last_synced_commit    VARCHAR(40) NOT NULL DEFAULT '',
    last_synced_at        TIMESTAMPTZ,
    last_error            TEXT        NOT NULL DEFAULT '',
    created_by            VARCHAR(191) NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_git_capability_repositories_identity UNIQUE (git_server_id, git_repo_id),
    CONSTRAINT uq_git_capability_repositories_repository UNIQUE (repository_id),
    CONSTRAINT uq_git_capability_repositories_registry UNIQUE (registry_id)
);

CREATE INDEX IF NOT EXISTS idx_git_capability_repositories_status
    ON git_capability_repositories (identification_status, updated_at DESC);

COMMENT ON TABLE git_capability_repositories IS 'Stable Gitea repository binding and first capability discovery state';
COMMENT ON COLUMN git_capability_repositories.identification_status IS 'clean | warning | polluted | unknown; per-item type is locked in capability_items.item_type';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS git_capability_repositories;

-- +goose StatementEnd
