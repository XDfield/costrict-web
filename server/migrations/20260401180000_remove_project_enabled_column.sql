-- 项目管理语义收敛
--
-- 启用时间 enabled_at 与归档时间 archived_at 仅作为可选生命周期时间字段，
-- 不再使用 enabled 布尔字段表达状态机，因此移除 projects.enabled 列及相关索引。

-- +goose Up
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_projects_enabled;
ALTER TABLE projects DROP COLUMN IF EXISTS enabled;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE projects ADD COLUMN IF NOT EXISTS enabled boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_projects_enabled ON projects(enabled);

-- +goose StatementEnd
