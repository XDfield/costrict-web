-- Per-owner / per-repository push quota rules pushed to the CoStrict Gitea fork
-- (Gitea fork integration FI-4).
--
-- The column set is dictated by the fork's modules/costrict/quota.Rule, which is
-- the JSON shape POST /api/internal/costrict/quota-invalidate accepts. Anything
-- not in that struct cannot be enforced, so it is deliberately not stored here.
--
-- repo is NOT NULL DEFAULT '' rather than nullable so the primary key stays
-- simple AND so the empty string carries the fork's own sentinel meaning
-- verbatim: Rule.Repo == "" is the owner-level default. A nullable column would
-- introduce a NULL/'' ambiguity that has no counterpart on the wire.
--
-- The global default tier is NOT here: it lives in Gitea's app.ini
-- ([costrict] QUOTA_DEFAULT_MAX_FILE_SIZE_MB / QUOTA_DEFAULT_REPO_QUOTA_MB) and
-- the fork exposes no way to push it. Lookup priority on the fork side is
-- repo row > owner row > app.ini default.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS git_quota_rules (
    git_server_id    VARCHAR(64)  NOT NULL REFERENCES git_servers(server_id) ON DELETE CASCADE,
    owner            VARCHAR(255) NOT NULL,
    repo             VARCHAR(255) NOT NULL DEFAULT '',
    max_file_size_mb BIGINT       NOT NULL DEFAULT 0,
    repo_quota_mb    BIGINT       NOT NULL DEFAULT 0,
    updated_by       VARCHAR(191) NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT pk_git_quota_rules PRIMARY KEY (git_server_id, owner, repo),
    CONSTRAINT ck_git_quota_rules_owner_not_blank CHECK (owner <> ''),
    CONSTRAINT ck_git_quota_rules_max_file_size_mb CHECK (max_file_size_mb >= 0),
    CONSTRAINT ck_git_quota_rules_repo_quota_mb CHECK (repo_quota_mb >= 0)
);

COMMENT ON TABLE git_quota_rules IS 'Push quota rules pushed to the CoStrict Gitea fork via /api/internal/costrict/quota-invalidate';
COMMENT ON COLUMN git_quota_rules.repo IS 'Empty string = owner-level default, matching the fork Rule.Repo == "" sentinel';
COMMENT ON COLUMN git_quota_rules.max_file_size_mb IS 'Per-file limit in MB; 0 = unlimited (fork semantics)';
COMMENT ON COLUMN git_quota_rules.repo_quota_mb IS 'Repository total limit in MB; 0 = unlimited (fork semantics)';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS git_quota_rules;

-- +goose StatementEnd
