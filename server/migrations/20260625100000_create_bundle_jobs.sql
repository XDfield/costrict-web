-- +goose Up
-- +goose StatementBegin

-- bundle_jobs drives the async lazy clone-and-pack pipeline for the DB+HTTP plugin
-- distribution channel: a worker picks up a pending row, clones the plugin's upstream
-- source_url, packs a lossless ZIP, and upserts a clone_pack CapabilityArtifact.
-- Shape mirrors scan_jobs (FOR UPDATE SKIP LOCKED + backoff retry).
CREATE TABLE IF NOT EXISTS bundle_jobs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id      TEXT NOT NULL,
    trigger_type TEXT NOT NULL DEFAULT 'sync',
    trigger_user TEXT,
    status       TEXT NOT NULL DEFAULT 'pending',
    retry_count  INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    last_error   TEXT,
    artifact_id  UUID,
    scheduled_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at   TIMESTAMP,
    finished_at  TIMESTAMP,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_bundle_jobs_item ON bundle_jobs (item_id);
CREATE INDEX IF NOT EXISTS idx_bundle_jobs_status ON bundle_jobs (status);
CREATE INDEX IF NOT EXISTS idx_bundle_jobs_scheduled_at ON bundle_jobs (scheduled_at);

-- Duplicate-clone guard: at most one in-flight (pending|running) bundle job per item.
-- Partial unique index, so completed/failed history does not block re-enqueue.
CREATE UNIQUE INDEX IF NOT EXISTS idx_bundle_jobs_active_item
    ON bundle_jobs (item_id)
    WHERE status IN ('pending', 'running');

COMMENT ON TABLE bundle_jobs IS 'Async lazy clone-and-pack queue for the DB+HTTP plugin bundle distribution channel';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_bundle_jobs_active_item;
DROP INDEX IF EXISTS idx_bundle_jobs_scheduled_at;
DROP INDEX IF EXISTS idx_bundle_jobs_status;
DROP INDEX IF EXISTS idx_bundle_jobs_item;
DROP TABLE IF EXISTS bundle_jobs;

-- +goose StatementEnd
