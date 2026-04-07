-- 为项目成员增加个人置顶字段，用于每个用户独立置顶自己加入的项目

-- +goose Up
-- +goose StatementBegin
ALTER TABLE project_members
    ADD COLUMN IF NOT EXISTS pinned_at timestamptz;

CREATE INDEX IF NOT EXISTS idx_project_members_pinned_at ON project_members(pinned_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_members_pinned_at;

ALTER TABLE project_members
    DROP COLUMN IF EXISTS pinned_at;
-- +goose StatementEnd
