-- +goose Up
-- +goose StatementBegin

-- 订阅调用模式：per-user 的 skill 系订阅偏好。
-- "auto"   = 允许 AI 自动调用（skill discovery 命中）。
-- "manual" = 仅手动 /name 调用（csc 写盘时注入 disable-model-invocation:true）。
-- 仅对 skill/command/subagent 有语义，但所有类型一律存储，便于统一回显。
-- 旧订阅行由 DEFAULT 'auto' 自动回填，行为与改动前一致（向后兼容）。
ALTER TABLE item_favorites
    ADD COLUMN IF NOT EXISTS invoke_mode VARCHAR(16) NOT NULL DEFAULT 'auto';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE item_favorites DROP COLUMN IF EXISTS invoke_mode;

-- +goose StatementEnd
