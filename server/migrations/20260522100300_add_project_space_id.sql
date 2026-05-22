-- +goose Up
-- +goose StatementBegin

ALTER TABLE projects ADD COLUMN IF NOT EXISTS space_id UUID REFERENCES spaces(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_projects_space_id ON projects(space_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_projects_space_id;
ALTER TABLE projects DROP COLUMN IF EXISTS space_id;

-- +goose StatementEnd
