-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS integration_events (
    id VARCHAR(36) PRIMARY KEY,
    source VARCHAR(64) NOT NULL,
    event_id VARCHAR(64) NOT NULL,
    event_type VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency for inbound integration envelopes: sender retries carry the
-- same (source, event_id) and are ACKed without re-delivery.
CREATE UNIQUE INDEX IF NOT EXISTS idx_integration_events_source_event
    ON integration_events(source, event_id);

COMMENT ON TABLE integration_events IS '外部集成事件接收记录（幂等去重），当前来源为 multica 通知桥。表只增不删：事件量与通知量同阶，如需控制体积可定期清理 30 天前的记录（older rows 仅用于去重，不影响投递）。';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS integration_events;

-- +goose StatementEnd
