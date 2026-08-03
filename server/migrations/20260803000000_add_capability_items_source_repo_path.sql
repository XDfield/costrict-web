-- +goose Up
-- +goose StatementBegin

-- Main file of a git-backed capability item, repo-relative.
--
-- The "edit in Gitea" hand-off links straight at the plugin manifest's edit
-- page (<repo>/_edit/<ref>/<path>), and Gitea 404s on a path that doesn't
-- exist — so the path has to be the one actually probed on the repo at fork
-- time, not a guess. source_path is no substitute: it carries the CATALOG
-- layout (.plugin.json) while the mirrored repo usually uses
-- .claude-plugin/plugin.json.
--
-- Empty means "not detected" and callers fall back to the repo home page;
-- that is also the default every existing row gets, so nothing changes for
-- db-backed items.
ALTER TABLE capability_items
    ADD COLUMN IF NOT EXISTS source_repo_path TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN capability_items.source_repo_path IS 'git-backed item：主文件在仓库中的相对路径（如 .claude-plugin/plugin.json），fork 时探测得到；探测不到为空，跳转回落仓库首页';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE capability_items DROP COLUMN IF EXISTS source_repo_path;

-- +goose StatementEnd
