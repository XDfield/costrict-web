-- Personal space：user_credentials 表
--
-- 配合 personal-space KB ensure API (Phase E3c extension)。
-- 为每个用户的个人 Gitea namespace 存储 PAT 凭据。
--
-- Schema 决策：
--
--   1. user_subject_id 为主键（JWT subject claim），映射到 cs-user 的用户。
--
--   2. 结构与 team_bot_credentials 1:1 对齐，仅将 team_id 替换为 user_subject_id。
--      加密、sha256 指纹、软删除模式完全一致。
--
--   3. gitea_username 有 partial unique index（revoked_at IS NULL 时唯一），
--      和 team_bot_credentials 的模式一致。
--
--   4. token_sha256 有 index，用于泄露检测时按 sha256 查行撤销。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_credentials (
    user_subject_id      VARCHAR(191) NOT NULL PRIMARY KEY,
    tenant_id            TEXT         NOT NULL,
    git_server_id        VARCHAR(64)  NOT NULL,
    gitea_username       VARCHAR(191) NOT NULL,             -- u-<sanitized_username>
    gitea_user_id        BIGINT       NOT NULL,
    gitea_token_id       BIGINT       NOT NULL,
    token_encrypted      TEXT         NOT NULL,             -- AES-GCM ciphertext (base64)
    token_sha256         CHAR(64)     NOT NULL,             -- 检索泄露 token
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    rotated_at           TIMESTAMPTZ,
    revoked_at           TIMESTAMPTZ
);

-- 反查：gitea username 全局唯一（Gitea 内必须唯一，server 视角下也唯一）
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_credentials_gitea_username
    ON user_credentials(gitea_username)
    WHERE revoked_at IS NULL;

-- 反查：泄露检测（外部 token 被怀疑泄露时，按 sha256 查行撤销）
CREATE INDEX IF NOT EXISTS idx_user_credentials_sha256
    ON user_credentials(token_sha256);

COMMENT ON TABLE user_credentials IS 'Phase E3c extension：用户个人空间凭据 — Gitea PAT + 加密存储（PK user_subject_id）';
COMMENT ON COLUMN user_credentials.user_subject_id IS 'JWT subject_id — cs-user 的用户主键';
COMMENT ON COLUMN user_credentials.gitea_username IS 'Gitea 用户名（u-<sanitized>），revoked_at IS NULL 时全局唯一';
COMMENT ON COLUMN user_credentials.token_encrypted IS 'AES-GCM ciphertext (base64)；明文仅在 ProvisionUser 时一次性返回';
COMMENT ON COLUMN user_credentials.token_sha256 IS 'PAT 明文 sha256；泄露检测用索引';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_credentials_sha256;
DROP INDEX IF EXISTS uq_user_credentials_gitea_username;
DROP TABLE IF EXISTS user_credentials;

-- +goose StatementEnd
