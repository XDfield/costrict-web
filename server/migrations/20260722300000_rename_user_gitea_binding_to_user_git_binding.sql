-- 20260722300000_rename_user_gitea_binding_to_user_git_binding.sql
--
-- Adapter pattern refactor: user_gitea_binding → user_git_binding so the
-- table is not Gitea-specific. Future providers (gitlab, gitea-enterprise,
-- ...) reuse the same row schema; provider_kind denormalizes git_servers.kind
-- so single-table queries can dispatch without JOIN.
--
-- Plain ALTER IF EXISTS forms (no DO blocks; this goose version's SQL parser
-- rejects PL/pgSQL). goose tracks applied versions so re-runs don't happen;
-- IF EXISTS guards only protect dev environments that ran Phase 1
-- (20260722150010) before this rename existed.

-- +goose Up

ALTER TABLE IF EXISTS user_gitea_binding RENAME TO user_git_binding;
ALTER INDEX IF EXISTS uq_user_gitea_binding_gitea_username RENAME TO uq_user_git_binding_git_username;

-- PostgreSQL has no IF EXISTS for RENAME COLUMN. These ran in 20260722150010
-- as gitea_uid / gitea_username; we rename unconditionally. The migration is
-- one-shot per goose version tracking — a re-run never reaches this stanza.
ALTER TABLE user_git_binding RENAME COLUMN gitea_uid TO git_uid;
ALTER TABLE user_git_binding RENAME COLUMN gitea_username TO git_username;

ALTER TABLE user_git_binding
    ADD COLUMN IF NOT EXISTS provider_kind VARCHAR(32) NOT NULL DEFAULT 'gitea';

CREATE INDEX IF NOT EXISTS idx_user_git_binding_provider_kind
    ON user_git_binding(provider_kind);

COMMENT ON TABLE user_git_binding IS 'cs-user 用户与 Git server 账号 1:1 绑定（多 provider 适配）';
COMMENT ON COLUMN user_git_binding.git_uid IS 'Git server 内部 user.id（Gitea: int64；其它 provider 视情况）';
COMMENT ON COLUMN user_git_binding.git_username IS 'Git server 登录账号名';
COMMENT ON COLUMN user_git_binding.provider_kind IS '冗余 git_servers.kind，方便不 join 直接判断 provider';

-- +goose Down

ALTER TABLE user_git_binding RENAME COLUMN git_username TO gitea_username;
ALTER TABLE user_git_binding RENAME COLUMN git_uid TO gitea_uid;

ALTER INDEX IF EXISTS uq_user_git_binding_git_username RENAME TO uq_user_gitea_binding_gitea_username;

ALTER TABLE user_git_binding
    DROP COLUMN IF EXISTS provider_kind;

DROP INDEX IF EXISTS idx_user_git_binding_provider_kind;

ALTER TABLE user_git_binding RENAME TO user_gitea_binding;

COMMENT ON TABLE user_gitea_binding IS 'cs-user 用户与 Gitea 账号 1:1 绑定';
COMMENT ON COLUMN user_gitea_binding.gitea_uid IS 'Gitea 内部 user.id';
COMMENT ON COLUMN user_gitea_binding.gitea_username IS 'Gitea 登录账号名';
