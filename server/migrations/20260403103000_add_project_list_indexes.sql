-- 优化 /api/projects 列表查询：
-- 1. project_members 先按 user_id + deleted_at 过滤，再用 project_id 回表 join projects
-- 2. pinned 查询场景可复用 pinned_at 列
-- 3. projects.created_at 用于最终排序

-- +goose Up
-- +goose StatementBegin

CREATE INDEX IF NOT EXISTS idx_project_members_user_deleted_pinned_project
    ON project_members(user_id, deleted_at, pinned_at, project_id);

CREATE INDEX IF NOT EXISTS idx_projects_created_at
    ON projects(created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_projects_created_at;
DROP INDEX IF EXISTS idx_project_members_user_deleted_pinned_project;

-- +goose StatementEnd
