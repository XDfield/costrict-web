-- 统一项目模块中的 project_id 外键列为 uuid，避免与 projects.id(uuid) 比较时报 text = uuid。

-- +goose Up
-- +goose StatementBegin

ALTER TABLE project_members
    ALTER COLUMN project_id TYPE uuid USING project_id::uuid;

ALTER TABLE project_invitations
    ALTER COLUMN project_id TYPE uuid USING project_id::uuid;

ALTER TABLE project_repositories
    ALTER COLUMN project_id TYPE uuid USING project_id::uuid;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

SELECT 1;

-- +goose StatementEnd
