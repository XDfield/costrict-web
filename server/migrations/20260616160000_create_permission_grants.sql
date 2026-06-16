-- +goose Up
-- +goose StatementBegin

-- 细粒度权限授予表（mentor RBAC 方案 / prd §11.2）。一行 = 一次授权：
--   (permission_code, subject)，subject ∈ {user, department}。
-- 与 resource_permissions.allowed_roles[]（粗粒度角色门禁）共存：最终鉴权 = role 路径 ∪ grant 路径。
--
-- 部门授权向子部门继承：用 dept-sync 物化路径 dept_path 做前缀匹配（U == P_D 或 U 以 P_D + '/' 开头），
-- 无需 closure table。授权 department 时冗余存其 dept_path 加速鉴权（避免每次鉴权回查 dept 树）。
--   subject_type = 'user'       → subject_id = users.subject_id，dept_path 为空
--   subject_type = 'department' → subject_id = dept-sync dept_id，dept_path = 该部门物化路径(/A/B/C)
CREATE TABLE IF NOT EXISTS permission_grants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    permission_code VARCHAR(128) NOT NULL,            -- e.g. kanban/admin、kanban/reader
    subject_type    VARCHAR(32)  NOT NULL,            -- user | department
    subject_id      VARCHAR(191) NOT NULL,            -- user: users.subject_id；department: dept_id
    dept_path       VARCHAR(1024) NOT NULL DEFAULT '', -- 部门授权时冗余存其 dept_path，加速前缀匹配
    granted_by      VARCHAR(191),                     -- 操作者 subject_id
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_permission_grant_subject_type CHECK (subject_type IN ('user', 'department')),
    CONSTRAINT uk_permission_grant UNIQUE (permission_code, subject_type, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_permission_grants_code    ON permission_grants(permission_code);
CREATE INDEX IF NOT EXISTS idx_permission_grants_subject ON permission_grants(subject_type, subject_id);

COMMENT ON TABLE permission_grants IS '细粒度权限授予（mentor RBAC：permission_code + (user|department)，部门授权按 dept_path 前缀向下继承）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS permission_grants;

-- +goose StatementEnd
