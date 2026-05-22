-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS squads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT,
    leader_id TEXT,
    instructions TEXT,
    avatar_url TEXT,
    archived_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_squads_space_id ON squads(space_id);
CREATE INDEX idx_squads_archived_at ON squads(archived_at) WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS squad_members (
    squad_id UUID NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'member',
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (squad_id, user_id)
);

CREATE INDEX idx_squad_members_squad_id ON squad_members(squad_id);
CREATE INDEX idx_squad_members_user_id ON squad_members(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS squad_members;
DROP TABLE IF EXISTS squads;

-- +goose StatementEnd
