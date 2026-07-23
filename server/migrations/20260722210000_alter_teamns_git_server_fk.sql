-- Git Ownership Refactor Phase 4：team_ns.git_server_id FK 改引用本地 git_servers
--
-- 在 P1 之前，server.team_ns.git_server_id 是语义引用 cs-user.git_servers（通过 RPC
-- 查询）。P4.1 一次性迁移脚本把 cs-user.git_servers 复制到 server.git_servers 后，
-- 这里的 FK 应该指向本地表。
--
-- **前置条件**：必须先运行 server/cmd/migrate-git-from-cs-user 把所有现有
-- git_server_id 的对应行灌进 server.git_servers，否则本迁移的 ADD CONSTRAINT
-- 会因 dangling references 失败。
--
-- +goose Up
-- +goose StatementBegin

ALTER TABLE team_ns
    ADD CONSTRAINT fk_team_ns_git_server
    FOREIGN KEY (git_server_id)
    REFERENCES git_servers(server_id)
    ON DELETE RESTRICT;

COMMENT ON COLUMN team_ns.git_server_id IS '绑定的 git_server_id（本地 git_servers 表 PK）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE team_ns
    DROP CONSTRAINT IF EXISTS fk_team_ns_git_server;

COMMENT ON COLUMN team_ns.git_server_id IS '绑定的 git_server_id（cs-user git_servers 表 PK）';

-- +goose StatementEnd
