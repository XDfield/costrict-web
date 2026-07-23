-- Git Ownership Refactor Phase 3：user_created_event_log 幂等表
--
-- cs-user outbox 是 at-least-once 投递（多副本 + 无 leader election），server 必须
-- 按 event_id 幂等去重。本表是 server 端唯一的事实来源：每个 event_id 仅处理一次。
--
-- Schema 决策：
--
--   1. PK = event_id（UUID v4，cs-user 生成）—— 重复 INSERT 走 ON CONFLICT DO NOTHING。
--   2. status：'processed' | 'soft_skipped' | 'failed' —— 应用层枚举。
--      - processed：成功调用 ProvisionUser（无论 binding 最终是 synced 还是 error）。
--      - soft_skipped：tenant 无 git_server，跳过；后续 binding 仍可能在 pending。
--      - failed：handler 自身异常（如 db 不可达），返回 5xx，cs-user 会重投。
--   3. subject_id / tenant_id 冗余存放：便于按用户/租户排查历史事件。
--   4. error_message NULL unless status='failed'。
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_created_event_log (
    event_id       UUID                     NOT NULL,
    event_type     VARCHAR(64)              NOT NULL DEFAULT 'user.created',
    subject_id     TEXT                     NOT NULL,
    tenant_id      TEXT                     NOT NULL DEFAULT 'default',
    status         VARCHAR(32)              NOT NULL,
    error_message  TEXT,
    processed_at   TIMESTAMPTZ              NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id)
);

CREATE INDEX IF NOT EXISTS idx_user_created_event_log_subject
    ON user_created_event_log (subject_id, processed_at);

COMMENT ON TABLE user_created_event_log IS 'user.created 事件幂等日志 — Git Ownership Refactor Phase 3';
COMMENT ON COLUMN user_created_event_log.event_id IS 'cs-user 生成的 UUID v4（用于去重）';
COMMENT ON COLUMN user_created_event_log.status IS 'processed | soft_skipped | failed';
COMMENT ON COLUMN user_created_event_log.processed_at IS '事件首次被 server 处理的时间';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_created_event_log_subject;
DROP TABLE IF EXISTS user_created_event_log;

-- +goose StatementEnd
