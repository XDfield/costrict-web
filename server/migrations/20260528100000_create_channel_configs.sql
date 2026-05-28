-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS channel_configs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    channel_type TEXT NOT NULL,
    name TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    config JSONB DEFAULT '{}',
    webhook_verified BOOLEAN NOT NULL DEFAULT FALSE,
    last_active_at TIMESTAMP,
    last_error TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channel_configs_user_id ON channel_configs(user_id);
CREATE INDEX IF NOT EXISTS idx_channel_configs_channel_type ON channel_configs(channel_type);
CREATE INDEX IF NOT EXISTS idx_channel_configs_deleted_at ON channel_configs(deleted_at);

COMMENT ON TABLE channel_configs IS '双向 Channel 渠道配置（用户配置）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS channel_configs;

-- +goose StatementEnd
