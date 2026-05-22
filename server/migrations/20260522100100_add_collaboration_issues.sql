-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS issues (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    number INTEGER NOT NULL,
    title TEXT NOT NULL,
    description TEXT,
    status TEXT NOT NULL DEFAULT 'backlog' CHECK (status IN ('backlog', 'todo', 'in_progress', 'in_review', 'done', 'blocked', 'cancelled')),
    priority TEXT NOT NULL DEFAULT 'none' CHECK (priority IN ('urgent', 'high', 'medium', 'low', 'none')),
    assignee_type TEXT CHECK (assignee_type IN ('member', 'squad')),
    assignee_id UUID,
    creator_id TEXT NOT NULL,
    parent_issue_id UUID REFERENCES issues(id) ON DELETE SET NULL,
    position DOUBLE PRECISION NOT NULL DEFAULT 0,
    due_date TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX idx_issues_space_id ON issues(space_id);
CREATE INDEX idx_issues_status ON issues(status);
CREATE INDEX idx_issues_assignee ON issues(assignee_id);
CREATE INDEX idx_issues_creator ON issues(creator_id);
CREATE INDEX idx_issues_deleted_at ON issues(deleted_at) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_issues_space_number ON issues(space_id, number);

CREATE TABLE IF NOT EXISTS issue_comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_issue_comments_issue_id ON issue_comments(issue_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS issue_comments;
DROP TABLE IF EXISTS issues;

-- +goose StatementEnd
