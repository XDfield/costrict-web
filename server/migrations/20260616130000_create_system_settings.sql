-- +goose Up
-- +goose StatementBegin

-- 系统级设置 / feature flags（平台管理员配置，全局单例 KV）。
-- key 唯一，value 存任意 JSON（feature flag 用 bool/string，维护模式等）。
-- 与 user_configs（per-user KV）区分：此表为全局系统级配置。
CREATE TABLE IF NOT EXISTS system_settings (
    key        VARCHAR(128) PRIMARY KEY,
    value      JSONB NOT NULL DEFAULT '{}',
    updated_by VARCHAR(191),                       -- 操作者 subject_id
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE system_settings IS '系统级设置 / feature flags（平台管理员配置，全局单例 KV）';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS system_settings;

-- +goose StatementEnd
