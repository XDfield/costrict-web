-- +goose Up
-- +goose StatementBegin

-- Git-backed capability items (Fork → Gitea user namespace).
--
-- A plugin forked into the user's own Gitea namespace stores only metadata in
-- the DB; the real content lives in the forked repository. These three columns
-- carry the repo coordinate and mark which backend is authoritative.
--
-- Defaults are chosen so every existing row (and every non-git code path)
-- keeps its current behavior: content_backend = 'db' means "content column +
-- capability_assets are the truth", which is what all rows are today.
ALTER TABLE capability_items
    ADD COLUMN IF NOT EXISTS source_repo_url TEXT NOT NULL DEFAULT '';

ALTER TABLE capability_items
    ADD COLUMN IF NOT EXISTS source_repo_ref VARCHAR(64) NOT NULL DEFAULT 'main';

ALTER TABLE capability_items
    ADD COLUMN IF NOT EXISTS content_backend VARCHAR(16) NOT NULL DEFAULT 'db';

COMMENT ON COLUMN capability_items.source_repo_url IS 'git-backed item：规范化的仓库地址（由 git_server endpoint + owner/repo 拼出，不采信 Gitea 返回的 html_url）；db-backed 为空';
COMMENT ON COLUMN capability_items.source_repo_ref IS 'git-backed item：仓库 ref（分支名），默认 main';
COMMENT ON COLUMN capability_items.content_backend IS '内容真相源：db（content + capability_assets）| git（source_repo_url 指向的仓库）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE capability_items DROP COLUMN IF EXISTS content_backend;
ALTER TABLE capability_items DROP COLUMN IF EXISTS source_repo_ref;
ALTER TABLE capability_items DROP COLUMN IF EXISTS source_repo_url;

-- +goose StatementEnd
