-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS gateway_registry (
    id              TEXT PRIMARY KEY,
    endpoint        TEXT NOT NULL,
    internal_url    TEXT NOT NULL,
    region          TEXT NOT NULL,
    capacity        INT  NOT NULL DEFAULT 0,
    current_conns   INT  NOT NULL DEFAULT 0,
    last_heartbeat  BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_gateway_registry_region_heartbeat
    ON gateway_registry(region, last_heartbeat);
CREATE INDEX IF NOT EXISTS idx_gateway_registry_heartbeat
    ON gateway_registry(last_heartbeat);

CREATE TABLE IF NOT EXISTS gateway_device_bindings (
    device_id   TEXT PRIMARY KEY,
    gateway_id  TEXT NOT NULL REFERENCES gateway_registry(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_gateway_device_bindings_gateway_id
    ON gateway_device_bindings(gateway_id);

CREATE TABLE IF NOT EXISTS server_epoch (
    singleton_key TEXT PRIMARY KEY DEFAULT 'singleton' CHECK (singleton_key = 'singleton'),
    epoch_value   BIGINT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS channel_reply_contexts (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_config_id TEXT NOT NULL,
    external_user_id  TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    channel_type      TEXT NOT NULL,
    external_chat_id  TEXT NOT NULL,
    context_token     TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_channel_reply_contexts_config_user
        UNIQUE (channel_config_id, external_user_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_reply_contexts_user_id
    ON channel_reply_contexts(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_channel_reply_contexts_user_id;
DROP TABLE IF EXISTS channel_reply_contexts;
DROP TABLE IF EXISTS server_epoch;
DROP INDEX IF EXISTS idx_gateway_device_bindings_gateway_id;
DROP TABLE IF EXISTS gateway_device_bindings;
DROP INDEX IF EXISTS idx_gateway_registry_heartbeat;
DROP INDEX IF EXISTS idx_gateway_registry_region_heartbeat;
DROP TABLE IF EXISTS gateway_registry;

-- +goose StatementEnd
