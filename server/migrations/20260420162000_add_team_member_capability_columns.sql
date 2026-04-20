-- Add capability-related columns for CloudTeam member scoring/heartbeat.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE team_session_members
    ADD COLUMN IF NOT EXISTS cpu_idle_percent DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS memory_free_mb DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS rtt_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS heartbeat_success_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS reported_repo_urls TEXT[];

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE team_session_members
    DROP COLUMN IF EXISTS reported_repo_urls,
    DROP COLUMN IF EXISTS heartbeat_success_rate,
    DROP COLUMN IF EXISTS rtt_ms,
    DROP COLUMN IF EXISTS memory_free_mb,
    DROP COLUMN IF EXISTS cpu_idle_percent;

-- +goose StatementEnd
