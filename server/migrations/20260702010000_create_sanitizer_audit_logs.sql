-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS sanitizer_audit_logs (
    id                  BIGSERIAL    PRIMARY KEY,
    trace_id            VARCHAR(128) NOT NULL,
    user_id             VARCHAR(255) NOT NULL,
    scene_id            VARCHAR(64)  NOT NULL,
    sanitize_phase      VARCHAR(32)  NOT NULL,
    raw_content         TEXT         NOT NULL,
    input_chars         INTEGER      NOT NULL DEFAULT 0,
    truncated_at_input  BOOLEAN      NOT NULL DEFAULT FALSE,
    sanitized_content   TEXT,
    output_chars        INTEGER      NOT NULL DEFAULT 0,
    truncated_at_output BOOLEAN      NOT NULL DEFAULT FALSE,
    llm_model           VARCHAR(64),
    llm_latency_ms      BIGINT       NOT NULL DEFAULT 0,
    failure_reason      VARCHAR(32),
    created_at          TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sanitizer_audit_created ON sanitizer_audit_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_sanitizer_audit_user ON sanitizer_audit_logs(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sanitizer_audit_trace ON sanitizer_audit_logs(trace_id);

COMMENT ON TABLE sanitizer_audit_logs IS 'AI sanitizer audit trail (input_received + delivered|failed rows per call)';
COMMENT ON COLUMN sanitizer_audit_logs.sanitize_phase IS 'input_received | delivered | failed';
COMMENT ON COLUMN sanitizer_audit_logs.failure_reason IS 'timeout | rate_limited | model_error | output_invalid | audit_write_failed (NULL when phase=delivered)';
COMMENT ON COLUMN sanitizer_audit_logs.trace_id IS 'correlates the input_received + delivered/failed pair from one Sanitize() call';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS sanitizer_audit_logs;

-- +goose StatementEnd
