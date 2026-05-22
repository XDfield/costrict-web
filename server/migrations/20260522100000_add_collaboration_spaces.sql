-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS spaces (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    slug TEXT NOT NULL UNIQUE,
    description TEXT,
    settings JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_spaces_slug ON spaces(slug);

CREATE TABLE IF NOT EXISTS space_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(space_id, user_id)
);

CREATE INDEX idx_space_members_space_id ON space_members(space_id);
CREATE INDEX idx_space_members_user_id ON space_members(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS space_members;
DROP TABLE IF EXISTS spaces;

-- +goose StatementEnd
