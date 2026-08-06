-- Next-due reconcile scheduling for Git-backed capability repository bindings.
--
-- Reconciliation — not the webhook — is the correctness path for a lost,
-- delayed, or reordered delivery, with a 10-minute default freshness interval.
-- A fixed "one batch per interval" loop cannot honour that: with N bindings and
-- a batch of B, the effective SLA silently degrades to ceil(N/B) intervals.
-- Storing a per-binding next_due_at turns the loop into a drain — repeatedly
-- claim the oldest due rows in bounded batches until nothing is due — so the
-- SLA depends on worker throughput, which is measurable and alertable, instead
-- of on the binding count, which is not.
--
-- Backlog metrics come from this column directly: oldest-due age is
-- now() - MIN(next_due_at) over the unpaused set, and queue depth is the count
-- of rows with next_due_at <= now().
--
-- Health filtering: a permanently failing binding must not monopolise the
-- drain. reconcile_failures drives an exponential backoff that is applied by
-- pushing next_due_at out, so an unhealthy binding self-schedules away from the
-- head of the queue without a separate state machine; reconcile_paused is the
-- operator kill switch for a single binding and is excluded from the partial
-- index (and therefore from the drain) entirely. A repository that was deleted
-- upstream does not need either mechanism: its binding is removed in the same
-- transaction that archives its items.
--
-- Existing rows take now() as their default, i.e. every registered binding is
-- immediately due after this migration and gets one full reconcile pass. That
-- is intentional: it is also the backfill of the new lifecycle/visibility
-- fields' first real observation.

-- +goose Up

ALTER TABLE git_capability_repositories
    ADD COLUMN IF NOT EXISTS next_due_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS reconcile_paused BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS reconcile_failures INTEGER NOT NULL DEFAULT 0;

-- Drain claim: WHERE reconcile_paused = false AND next_due_at <= now()
--              ORDER BY next_due_at LIMIT <batch>
-- The predicate matches the partial index exactly so the planner can use it,
-- and paused bindings cost nothing to skip because they are not in the index.
CREATE INDEX IF NOT EXISTS idx_git_capability_repositories_due
    ON git_capability_repositories (next_due_at)
    WHERE reconcile_paused = false;

COMMENT ON COLUMN git_capability_repositories.next_due_at IS 'When this binding must next be re-read from Gitea; the drain claims due rows oldest-first. Default 10-minute freshness interval, pushed out by backoff on failure';
COMMENT ON COLUMN git_capability_repositories.reconcile_paused IS 'Operator kill switch for one binding; paused rows leave the drain index and are never claimed';
COMMENT ON COLUMN git_capability_repositories.reconcile_failures IS 'Consecutive reconcile failures; drives exponential backoff on next_due_at and the failure alert. Reset to 0 on success';

-- +goose Down

DROP INDEX IF EXISTS idx_git_capability_repositories_due;

ALTER TABLE git_capability_repositories
    DROP COLUMN IF EXISTS reconcile_failures,
    DROP COLUMN IF EXISTS reconcile_paused,
    DROP COLUMN IF EXISTS next_due_at;
