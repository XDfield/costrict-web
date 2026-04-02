-- 统一项目模块中与用户相关列的数据类型为 text，避免 text/uuid 比较导致 PostgreSQL 报错。

-- +goose Up
-- +goose StatementBegin

ALTER TABLE projects
    ALTER COLUMN creator_id TYPE text USING creator_id::text;

ALTER TABLE project_members
    ALTER COLUMN user_id TYPE text USING user_id::text;

ALTER TABLE project_invitations
    ALTER COLUMN inviter_id TYPE text USING inviter_id::text,
    ALTER COLUMN invitee_id TYPE text USING invitee_id::text;

ALTER TABLE project_repositories
    ALTER COLUMN bound_by_user_id TYPE text USING bound_by_user_id::text;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

SELECT 1;

-- +goose StatementEnd
