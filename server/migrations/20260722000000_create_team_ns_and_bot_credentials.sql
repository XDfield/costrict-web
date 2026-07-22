-- Phase E3c：team_ns + team_bot_credentials 表
--
-- 配合 team-namespace API v1.1（doc/repo-management/TEAM_NAMESPACE_API_REFERENCE.md）
-- 落地 server 侧的两张新表：
--
--   team_ns                 — 每个 team 的 Gitea namespace 元数据（t-<team_short>
--                             org + git_server 绑定 + 状态）
--   team_bot_credentials    — 每个 team 的 bot 账号凭据（gitea_username + 加密
--                             PAT + sha256 指纹），是 server 控制的 Gitea 资产
--
-- Schema 决策：
--
--   1. 两表都落 @server：bot 凭据是 server 控制的 Gitea 资产，server 才有
--      AES-GCM key；team_ns 元数据虽然可以放 cs-user，但 server 是唯一写入者
--      且每个调用都跨进程读 → 留在 server 减少跨进程往返。
--
--   2. tenant_id 用 TEXT 而非 FK：server 端没有 tenants 表（租户主表在
--      cs-user 库）。应用层校验存在性，与 server 现有 tenant_id 列约定一致。
--
--   3. team_ns.team_ns_org 全局唯一（CREATE UNIQUE INDEX）：t-<team_short>
--      在单 Gitea 内必须唯一；跨 git_server 时也按字符串去重（不同 server
--      的 team_short 命名空间是隔离的，但 org_name 字符串本身一致仍会冲突
--      —— 在 server 视角下，用全局字符串唯一最简单）。
--
--   4. team_bot_credentials.token_encrypted 用 TEXT（AES-GCM ciphertext base64），
--      不存明文；token_sha256 是辅助索引，便于「泄露检测」时按 sha256 撤销。
--
--   5. 软删除走 revoked_at / dissolved_at；不引入 gorm.DeletedAt，因为两张表
--      都需要保留历史用于审计。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS team_ns (
    team_id              VARCHAR(191) NOT NULL PRIMARY KEY,
    tenant_id            TEXT         NOT NULL,
    team_display_name    VARCHAR(191) NOT NULL,
    team_ns_org          VARCHAR(64)  NOT NULL,             -- t-<team_short>
    team_short           VARCHAR(32)  NOT NULL,             -- team_short_id (UUID first 8 hex)
    git_server_id        VARCHAR(64)  NOT NULL,
    status               VARCHAR(32)  NOT NULL DEFAULT 'active',  -- active | archived | dissolved
    dissolved_at         TIMESTAMPTZ,
    dissolution_reason   VARCHAR(64),                        -- dissolved | migrated | abuse
    retention_until      TIMESTAMPTZ,                        -- dissolved 行的物理删除窗口截止
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- team_ns_org 全局唯一（一个 Gitea org_name 不允许映射到多个 team）
CREATE UNIQUE INDEX IF NOT EXISTS uq_team_ns_org
    ON team_ns(team_ns_org);

-- 反查：tenant 维度列活跃 team（按 tenant 列出 team 用）
CREATE INDEX IF NOT EXISTS idx_team_ns_tenant_active
    ON team_ns(tenant_id)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS team_bot_credentials (
    team_id              VARCHAR(191) NOT NULL PRIMARY KEY,
    tenant_id            TEXT         NOT NULL,
    git_server_id        VARCHAR(64)  NOT NULL,
    gitea_username       VARCHAR(191) NOT NULL,             -- bot-t-<team_short>
    gitea_user_id        BIGINT       NOT NULL,
    gitea_token_id       BIGINT       NOT NULL,
    token_encrypted      TEXT         NOT NULL,             -- AES-GCM ciphertext (base64)
    token_sha256         CHAR(64)     NOT NULL,             -- 检索泄露 token
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    rotated_at           TIMESTAMPTZ,
    revoked_at           TIMESTAMPTZ
);

-- 反查：bot username 全局唯一（Gitea 内必须唯一，server 视角下也唯一）
CREATE UNIQUE INDEX IF NOT EXISTS uq_team_bot_credentials_gitea_username
    ON team_bot_credentials(gitea_username)
    WHERE revoked_at IS NULL;

-- 反查：泄露检测（外部 token 被怀疑泄露时，按 sha256 查行撤销）
CREATE INDEX IF NOT EXISTS idx_team_bot_credentials_sha256
    ON team_bot_credentials(token_sha256);

COMMENT ON TABLE team_ns IS 'Phase E3c：team namespace 元数据 — 每个 team 的 Gitea org 绑定 + 状态（PK team_id）';
COMMENT ON COLUMN team_ns.team_ns_org IS 'Gitea org_name（t-<team_short>），全局唯一';
COMMENT ON COLUMN team_ns.team_short IS 'team_short_id（UUID first 8 hex）— team_ns_org / bot 命名共用的短键';
COMMENT ON COLUMN team_ns.git_server_id IS '绑定的 git_server_id（cs-user git_servers 表 PK）';
COMMENT ON COLUMN team_ns.status IS '状态枚举：active | archived | dissolved（应用层校验）';
COMMENT ON COLUMN team_ns.retention_until IS 'dissolved 行的物理删除截止（90 天保留窗口）';

COMMENT ON TABLE team_bot_credentials IS 'Phase E3c：team bot 凭据 — Gitea bot 账号 + 加密 PAT（PK team_id）';
COMMENT ON COLUMN team_bot_credentials.gitea_username IS 'Gitea bot 用户名（bot-t-<team_short>），revoked_at IS NULL 时全局唯一';
COMMENT ON COLUMN team_bot_credentials.token_encrypted IS 'AES-GCM ciphertext (base64)；明文仅在 Provision/Rotate 时一次性返回';
COMMENT ON COLUMN team_bot_credentials.token_sha256 IS 'PAT 明文 sha256；泄露检测用索引';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_team_bot_credentials_sha256;
DROP INDEX IF EXISTS uq_team_bot_credentials_gitea_username;
DROP TABLE IF EXISTS team_bot_credentials;

DROP INDEX IF EXISTS idx_team_ns_tenant_active;
DROP INDEX IF EXISTS uq_team_ns_org;
DROP TABLE IF EXISTS team_ns;

-- +goose StatementEnd
