-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS system_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(191) NOT NULL,
    type VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    title TEXT NOT NULL,
    content TEXT,
    session_id VARCHAR(255),
    device_id VARCHAR(255),
    workspace_id UUID,
    action_type VARCHAR(64),
    action_data JSONB DEFAULT '{}',
    action_token VARCHAR(128),
    action_result JSONB,
    acted_at TIMESTAMP,
    expires_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at TIMESTAMP,
    deleted_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_system_notifications_user_status ON system_notifications(user_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_system_notifications_user_type ON system_notifications(user_id, type, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_system_notifications_action_token ON system_notifications(action_token) WHERE action_token IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_system_notifications_session ON system_notifications(session_id) WHERE session_id IS NOT NULL;

COMMENT ON TABLE system_notifications IS '系统通知记录表，持久化所有待处理和已处理的通知';
COMMENT ON COLUMN system_notifications.type IS '通知类型: permission | question | session.completed | session.failed | ...';
COMMENT ON COLUMN system_notifications.status IS '通知状态: pending | read | acted | expired';
COMMENT ON COLUMN system_notifications.action_type IS '操作类型: permission.approve | permission.reject | question.reply | null';
COMMENT ON COLUMN system_notifications.action_data IS '操作数据: {requestId, toolName, patterns, options, ...}';
COMMENT ON COLUMN system_notifications.action_token IS '一次性操作令牌，用于无认证操作页面';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS system_notifications;

-- +goose StatementEnd
