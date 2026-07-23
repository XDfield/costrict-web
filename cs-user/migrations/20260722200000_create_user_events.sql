-- Git Ownership Refactor Phase 2: cs-user outbox table.
--
-- Holds user lifecycle events (user.created, user.updated, ...) until the
-- outbox worker delivers them to server. Decouples user writes from
-- downstream consumer availability (at-least-once + idempotent consumer).
--
-- Schema decisions:
--
--   1. event_id UUID PK — caller-generated; survives retries.
--   2. event_type VARCHAR(64) — 'user.created' today; extensible.
--   3. subject_id TEXT — the user the event is about.
--   4. payload JSONB — full event body delivered verbatim to the consumer.
--   5. status VARCHAR(16) — 'pending' | 'delivered' | 'failed'
--      (no DB-level CHECK, app-level enum).
--   6. attempts INTEGER — failure counter (drives backoff).
--   7. last_error TEXT — most recent failure reason (cleared on success).
--   8. available_at TIMESTAMPTZ — next eligible delivery time (backoff).
--   9. delivered_at TIMESTAMPTZ NULL — success marker + audit.
--  10. created_at TIMESTAMPTZ — queue order for FairLoad.
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS user_events (
    event_id      UUID         PRIMARY KEY,
    event_type    VARCHAR(64)  NOT NULL,
    subject_id    TEXT         NOT NULL,
    tenant_id     TEXT         NOT NULL DEFAULT 'default',
    payload       JSONB        NOT NULL,
    status        VARCHAR(16)  NOT NULL DEFAULT 'pending',
    attempts      INTEGER      NOT NULL DEFAULT 0,
    last_error    TEXT,
    available_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    delivered_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Worker scan: rows WHERE status='pending' AND available_at <= now()
-- ordered by created_at, limited to batch_size.
CREATE INDEX IF NOT EXISTS idx_user_events_pending
    ON user_events (available_at, created_at)
    WHERE status = 'pending';

-- Audit / operator query: "what happened to event X".
CREATE INDEX IF NOT EXISTS idx_user_events_subject
    ON user_events (subject_id, created_at DESC);

COMMENT ON TABLE user_events IS 'cs-user outbox — Git Ownership Refactor Phase 2';
COMMENT ON COLUMN user_events.event_id IS 'caller-generated UUID; idempotency key for downstream consumer';
COMMENT ON COLUMN user_events.event_type IS 'enum: user.created | user.updated | user.deleted (today: user.created)';
COMMENT ON COLUMN user_events.subject_id IS 'users.subject_id the event refers to';
COMMENT ON COLUMN user_events.tenant_id IS 'tenant shard key (mirrors users.tenant_id)';
COMMENT ON COLUMN user_events.payload IS 'JSONB body delivered verbatim to consumer';
COMMENT ON COLUMN user_events.status IS 'pending | delivered | failed (app-level enum, no DB CHECK)';
COMMENT ON COLUMN user_events.attempts IS 'failure counter; drives exponential backoff';
COMMENT ON COLUMN user_events.available_at IS 'next eligible delivery time (now() + backoff after failure)';
COMMENT ON COLUMN user_events.delivered_at IS 'success timestamp; consumer idempotency relies on event_id, not this column';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_events_subject;
DROP INDEX IF EXISTS idx_user_events_pending;
DROP TABLE IF EXISTS user_events;

-- +goose StatementEnd
