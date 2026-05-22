-- +goose Up
-- +goose StatementBegin

ALTER TABLE spaces ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_spaces_deleted_at ON spaces(deleted_at) WHERE deleted_at IS NOT NULL;

ALTER TABLE space_members ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_space_members_deleted_at ON space_members(deleted_at) WHERE deleted_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_space_members_deleted_at;
ALTER TABLE space_members DROP COLUMN IF EXISTS deleted_at;

DROP INDEX IF EXISTS idx_spaces_deleted_at;
ALTER TABLE spaces DROP COLUMN IF EXISTS deleted_at;

-- +goose StatementEnd
