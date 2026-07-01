-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS capability_import_jobs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_kind   TEXT NOT NULL DEFAULT 'url',
    source_url    TEXT NOT NULL DEFAULT '',
    filename      TEXT NOT NULL DEFAULT '',
    storage_key   TEXT NOT NULL DEFAULT '',
    file_size     BIGINT NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'pending',
    dry_run       BOOLEAN NOT NULL DEFAULT true,
    reparse       BOOLEAN NOT NULL DEFAULT false,
    trigger_user  TEXT NOT NULL DEFAULT '',
    result        JSONB NOT NULL DEFAULT '{}',
    error_message TEXT NOT NULL DEFAULT '',
    retry_count   INT NOT NULL DEFAULT 0,
    max_attempts  INT NOT NULL DEFAULT 3,
    scheduled_at  TIMESTAMPTZ,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_capability_import_jobs_status
    ON capability_import_jobs(status);
CREATE INDEX IF NOT EXISTS idx_capability_import_jobs_scheduled_at
    ON capability_import_jobs(scheduled_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_capability_import_jobs_scheduled_at;
DROP INDEX IF EXISTS idx_capability_import_jobs_status;
DROP TABLE IF EXISTS capability_import_jobs;

-- +goose StatementEnd
