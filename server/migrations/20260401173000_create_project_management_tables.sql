-- 项目管理模块基础表
--
-- 创建 projects / project_members / project_invitations 三张表及相关索引。
-- 通知策略说明：
-- 1. system.notification 作为通用系统通知事件
-- 2. 通知模板由具体业务模块自行组装
-- 3. 接受/拒绝邀请当前无需额外发送通知

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS projects (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    description text DEFAULT '',
    creator_id text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    enabled_at timestamptz,
    archived_at timestamptz,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_project_creator_name ON projects(creator_id, name);
CREATE INDEX IF NOT EXISTS idx_projects_enabled ON projects(enabled);
CREATE INDEX IF NOT EXISTS idx_projects_enabled_at ON projects(enabled_at);
CREATE INDEX IF NOT EXISTS idx_projects_archived_at ON projects(archived_at);
CREATE INDEX IF NOT EXISTS idx_projects_deleted_at ON projects(deleted_at);

CREATE TABLE IF NOT EXISTS project_members (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id text NOT NULL,
    role text NOT NULL DEFAULT 'member',
    joined_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    CONSTRAINT chk_project_members_role CHECK (role IN ('admin', 'member'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_project_user ON project_members(project_id, user_id);
CREATE INDEX IF NOT EXISTS idx_project_members_user_id ON project_members(user_id);
CREATE INDEX IF NOT EXISTS idx_project_members_deleted_at ON project_members(deleted_at);

CREATE TABLE IF NOT EXISTS project_invitations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    inviter_id text NOT NULL,
    invitee_id text NOT NULL,
    role text NOT NULL DEFAULT 'member',
    status text NOT NULL DEFAULT 'pending',
    message text DEFAULT '',
    responded_at timestamptz,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_project_invitations_role CHECK (role IN ('admin', 'member')),
    CONSTRAINT chk_project_invitations_status CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled'))
);

CREATE INDEX IF NOT EXISTS idx_project_invitee ON project_invitations(project_id, invitee_id);
CREATE INDEX IF NOT EXISTS idx_invitee_status ON project_invitations(invitee_id, status);
CREATE INDEX IF NOT EXISTS idx_project_invitations_status ON project_invitations(status);

COMMENT ON TABLE projects IS '项目表';
COMMENT ON TABLE project_members IS '项目成员表';
COMMENT ON TABLE project_invitations IS '项目邀请表';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_project_invitations_status;
DROP INDEX IF EXISTS idx_invitee_status;
DROP INDEX IF EXISTS idx_project_invitee;
DROP TABLE IF EXISTS project_invitations;

DROP INDEX IF EXISTS idx_project_members_deleted_at;
DROP INDEX IF EXISTS idx_project_members_user_id;
DROP INDEX IF EXISTS idx_project_user;
DROP TABLE IF EXISTS project_members;

DROP INDEX IF EXISTS idx_projects_deleted_at;
DROP INDEX IF EXISTS idx_projects_archived_at;
DROP INDEX IF EXISTS idx_projects_enabled_at;
DROP INDEX IF EXISTS idx_projects_enabled;
DROP INDEX IF EXISTS idx_project_creator_name;
DROP TABLE IF EXISTS projects;

-- +goose StatementEnd
