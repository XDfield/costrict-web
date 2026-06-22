-- +goose Up
-- +goose StatementBegin

-- 后台管理写操作审计日志。每行 = 一次管理员写操作（大客户/角色/权限/下发/渠道/设置等）。
-- actor_id 为操作者 subject_id；payload 存请求体子集 / 变更摘要（jsonb）。
-- 仅追加写入（fire-and-forget），平台管理员可按 actor/action/类型/时间过滤查询。
CREATE TABLE IF NOT EXISTS admin_audit_logs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id    VARCHAR(191) NOT NULL,            -- 操作者 subject_id
    action      VARCHAR(128) NOT NULL,            -- e.g. enterprise.create / system_role.grant
    target_type VARCHAR(64),                      -- e.g. enterprise_customer / user / distribution
    target_id   VARCHAR(191),
    payload     JSONB NOT NULL DEFAULT '{}',      -- 变更详情（请求体子集 / before-after 摘要）
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_created_at ON admin_audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_actor      ON admin_audit_logs(actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_action     ON admin_audit_logs(action, created_at DESC);

COMMENT ON TABLE admin_audit_logs IS '后台管理写操作审计日志';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS admin_audit_logs;

-- +goose StatementEnd
