-- Gitea capability webhook ingress queue.
--
-- A Gitea delivery is at-least-once. The unique delivery identity is the
-- stable configured Git server plus Gitea's delivery ID. Commit SHAs are
-- processing inputs, not idempotency keys: a force-push can legitimately
-- revisit a prior SHA with a new delivery. The HTTP handler inserts pending
-- rows only; the worker later claims and processes them.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS git_capability_sync_jobs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    git_server_id  VARCHAR(64) NOT NULL REFERENCES git_servers(server_id) ON DELETE CASCADE,
    delivery_id    VARCHAR(128) NOT NULL,
    repo_id        BIGINT      NOT NULL,
    repo_full_name TEXT        NOT NULL,
    default_branch TEXT        NOT NULL,
    ref            TEXT        NOT NULL,
    before_sha     TEXT        NOT NULL DEFAULT '',
    after_sha      TEXT        NOT NULL,
    status         VARCHAR(32) NOT NULL DEFAULT 'pending',
    retry_count    INTEGER     NOT NULL DEFAULT 0,
    max_attempts   INTEGER     NOT NULL DEFAULT 3,
    last_error     TEXT,
    scheduled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    lease_token    VARCHAR(36) NOT NULL DEFAULT '',
    finished_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_git_capability_sync_jobs_delivery
        UNIQUE (git_server_id, delivery_id)
);

CREATE INDEX IF NOT EXISTS idx_git_capability_sync_jobs_pending
    ON git_capability_sync_jobs (scheduled_at, created_at)
    WHERE status = 'pending';

-- One repository is processed by at most one worker at a time across all
-- replicas. started_at is the worker lease timestamp; a future/recovering
-- worker can reclaim a stale running row by moving it back to pending before
-- claiming the next delivery.
CREATE UNIQUE INDEX IF NOT EXISTS uq_git_capability_sync_jobs_running_repo
    ON git_capability_sync_jobs (git_server_id, repo_id)
    WHERE status = 'running';

COMMENT ON TABLE git_capability_sync_jobs IS 'Gitea push webhook durable sync queue for Git-backed capability index';
COMMENT ON COLUMN git_capability_sync_jobs.repo_id IS 'Gitea repository numeric ID; stable identity within git_server_id';
COMMENT ON COLUMN git_capability_sync_jobs.delivery_id IS 'Gitea X-Gitea-Delivery ID; durable at-least-once delivery idempotency key within a git server';
COMMENT ON COLUMN git_capability_sync_jobs.after_sha IS 'Push target commit SHA; processing input only, not an idempotency key';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_git_capability_sync_jobs_pending;
DROP INDEX IF EXISTS uq_git_capability_sync_jobs_running_repo;
DROP TABLE IF EXISTS git_capability_sync_jobs;

-- +goose StatementEnd
