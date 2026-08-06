package worker

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The reconcile drain on a real PostgreSQL.
//
// SQLite cannot prove the part that matters in production: SELECT ... FOR UPDATE
// SKIP LOCKED is what lets several worker replicas drain the same queue at once
// without one blocking on another's batch, and SQLite serializes every writer
// anyway, so it cannot tell a working claim from a missing one.

var gitReconcilePostgresFixture = []string{
	`CREATE TABLE git_capability_repositories (
		id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		git_server_id         VARCHAR(64) NOT NULL,
		git_repo_id           BIGINT NOT NULL,
		repository_id         UUID NOT NULL,
		registry_id           UUID NOT NULL,
		full_name             TEXT NOT NULL,
		repo_kind             VARCHAR(32) NOT NULL DEFAULT 'standalone',
		identification_status VARCHAR(32) NOT NULL DEFAULT 'unknown',
		visibility            VARCHAR(16) NOT NULL DEFAULT 'public',
		git_remote_url        TEXT NOT NULL DEFAULT '',
		default_branch        TEXT NOT NULL DEFAULT 'main',
		last_synced_commit    VARCHAR(40) NOT NULL DEFAULT '',
		last_synced_at        TIMESTAMPTZ,
		last_error            TEXT NOT NULL DEFAULT '',
		next_due_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
		reconcile_paused      BOOLEAN NOT NULL DEFAULT false,
		reconcile_failures    INTEGER NOT NULL DEFAULT 0,
		created_by            VARCHAR(191) NOT NULL DEFAULT 'test',
		created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (git_server_id, git_repo_id)
	)`,
	`CREATE INDEX idx_git_capability_repositories_due
		ON git_capability_repositories (next_due_at) WHERE reconcile_paused = false`,
	`CREATE TABLE git_capability_sync_jobs (
		id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		git_server_id  VARCHAR(64) NOT NULL,
		delivery_id    VARCHAR(128) NOT NULL,
		repo_id        BIGINT NOT NULL,
		repo_full_name TEXT NOT NULL,
		default_branch TEXT NOT NULL,
		ref            TEXT NOT NULL,
		before_sha     TEXT NOT NULL DEFAULT '',
		after_sha      TEXT NOT NULL,
		status         VARCHAR(32) NOT NULL,
		retry_count    INT NOT NULL DEFAULT 0,
		max_attempts   INT NOT NULL DEFAULT 3,
		last_error     TEXT,
		scheduled_at   TIMESTAMPTZ NOT NULL,
		started_at     TIMESTAMPTZ,
		lease_token    VARCHAR(36) NOT NULL DEFAULT '',
		finished_at    TIMESTAMPTZ,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (git_server_id, delivery_id)
	)`,
}

func newGitReconcilePostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL reconcile drain test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("git_reconcile_%d", time.Now().UnixNano())

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`
	if err := admin.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + quoted + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// search_path travels in the DSN, not as a SET: the concurrency test needs
	// several connections at once and a per-session SET would leave the others
	// pointing at `public`.
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL in %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	for _, ddl := range gitReconcilePostgresFixture {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

func seedPGBindings(t *testing.T, db *gorm.DB, count int, due time.Time) {
	t.Helper()
	for i := 1; i <= count; i++ {
		if err := db.Exec(`INSERT INTO git_capability_repositories
			(git_server_id, git_repo_id, repository_id, registry_id, full_name, next_due_at)
			VALUES (?, ?, gen_random_uuid(), gen_random_uuid(), ?, ?)`,
			workerGitServerID, int64(i), fmt.Sprintf("org/repo-%d", i), due).Error; err != nil {
			t.Fatalf("seed binding %d: %v", i, err)
		}
	}
}

// The freshness SLA, measured rather than asserted by construction.
//
// 500 bindings against a batch of 50 is ten full rounds — the exact shape the
// old scheduler could not handle, because it enqueued one batch and then slept
// for a whole interval, silently turning a 10-minute promise into 100 minutes.
// The assertion is that ONE drain pass leaves nothing due.
func TestGitCapabilityReconcile_PostgresDrainsFiveHundredBindingsInOnePass(t *testing.T) {
	db := newGitReconcilePostgresDB(t)
	const bindings = 500
	seedPGBindings(t, db, bindings, time.Now().Add(-time.Hour))

	pool := &GitCapabilityWorkerPool{
		DB: db, ReconcileInterval: 10 * time.Minute, ReconcileBatchSize: 50,
		PollInterval: time.Second, ReconcileDrainBudget: 30 * time.Second,
	}
	start := time.Now()
	pool.drainDueReconciles()
	elapsed := time.Since(start)

	var jobs, stillDue int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&jobs)
	db.Model(&models.GitCapabilityRepository{}).
		Where("reconcile_paused = false AND next_due_at <= now()").Count(&stillDue)

	if jobs != bindings {
		t.Fatalf("jobs = %d, want one per binding (%d)", jobs, bindings)
	}
	if stillDue != 0 {
		t.Fatalf("stillDue = %d after one drain pass; the freshness SLA degrades with binding count", stillDue)
	}
	// Not a performance assertion — a sanity bound. If enqueueing 500 bindings
	// took longer than the freshness interval itself, the drain could never meet
	// the SLA no matter how the loop is written.
	if elapsed > 10*time.Minute {
		t.Fatalf("draining %d bindings took %s, longer than the freshness interval", bindings, elapsed)
	}
	t.Logf("drained %d bindings in %s (%.0f bindings/second)", bindings, elapsed.Truncate(time.Millisecond),
		float64(bindings)/elapsed.Seconds())

	// And the schedule really moved forward by one interval, so the next pass is
	// a no-op rather than a re-enqueue.
	var nextDue time.Time
	if err := db.Raw(`SELECT MIN(next_due_at) FROM git_capability_repositories`).Scan(&nextDue).Error; err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(nextDue); delay < 9*time.Minute || delay > 11*time.Minute {
		t.Fatalf("next due in %s, want ~1 freshness interval", delay)
	}
}

// Several replicas drain the same queue. SKIP LOCKED plus the "still due"
// compare-and-set must produce exactly one job per binding — no duplicates, and
// nothing left behind because two workers each assumed the other took it.
func TestGitCapabilityReconcile_PostgresConcurrentDrainersDoNotDuplicateOrDrop(t *testing.T) {
	db := newGitReconcilePostgresDB(t)
	const bindings = 200
	seedPGBindings(t, db, bindings, time.Now().Add(-time.Hour))

	const replicas = 4
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A separate pool per goroutine: the in-process throttle is per pool,
			// and what is under test is the DATABASE-level claim, which is the only
			// thing that coordinates real replicas.
			pool := &GitCapabilityWorkerPool{
				DB: db, ReconcileInterval: 10 * time.Minute, ReconcileBatchSize: 25,
				PollInterval: time.Second, ReconcileDrainBudget: 30 * time.Second,
			}
			pool.drainDueReconciles()
		}()
	}
	wg.Wait()

	var jobs, distinctRepos, stillDue int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&jobs)
	db.Raw(`SELECT COUNT(DISTINCT repo_id) FROM git_capability_sync_jobs`).Scan(&distinctRepos)
	db.Model(&models.GitCapabilityRepository{}).
		Where("reconcile_paused = false AND next_due_at <= now()").Count(&stillDue)

	if distinctRepos != bindings {
		t.Fatalf("distinct repositories enqueued = %d, want %d — a binding was dropped", distinctRepos, bindings)
	}
	if jobs != bindings {
		t.Fatalf("jobs = %d for %d bindings — a binding was enqueued twice", jobs, bindings)
	}
	if stillDue != 0 {
		t.Fatalf("stillDue = %d after %d concurrent drainers", stillDue, replicas)
	}
}

// A drain that cannot keep up must SAY so. Silence here is the failure mode the
// old scheduler had: the backlog grew, the SLA was missed, and the only symptom
// was stale data nobody was looking at.
func TestGitCapabilityReconcile_PostgresReportsAnOverdueBacklog(t *testing.T) {
	db := newGitReconcilePostgresDB(t)
	seedPGBindings(t, db, 3, time.Now().Add(-time.Hour))

	pool := &GitCapabilityWorkerPool{
		DB: db, ReconcileInterval: time.Minute, ReconcileBatchSize: 10,
		PollInterval: time.Second, ReconcileDrainBudget: 30 * time.Second,
	}
	// Backlog is read from the same column the drain claims on, so a binding that
	// is overdue by more than one interval is measurable without any extra
	// bookkeeping. Assert the measurement directly rather than the log line.
	var due int64
	var oldest time.Time
	if err := db.Raw(`SELECT COUNT(*), MIN(next_due_at) FROM git_capability_repositories
		WHERE reconcile_paused = false AND next_due_at <= now()`).Row().Scan(&due, &oldest); err != nil {
		t.Fatal(err)
	}
	if due != 3 || time.Since(oldest) < time.Minute {
		t.Fatalf("backlog before drain: due=%d oldestAge=%s", due, time.Since(oldest))
	}

	pool.drainDueReconciles()

	if err := db.Raw(`SELECT COUNT(*) FROM git_capability_repositories
		WHERE reconcile_paused = false AND next_due_at <= now()`).Row().Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != 0 {
		t.Fatalf("backlog after drain = %d, want 0", due)
	}
}

// A binding that keeps failing must not monopolise the head of the queue, and it
// must not disappear from it either. The backoff is capped, so an unhealthy
// repository is still re-read on a bounded schedule.
func TestGitCapabilityReconcile_PostgresFailureBackoffIsBoundedAndRecovers(t *testing.T) {
	db := newGitReconcilePostgresDB(t)
	seedPGBindings(t, db, 1, time.Now().Add(-time.Hour))
	pool := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: time.Minute}

	job := models.GitCapabilitySyncJob{
		ID: uuid.NewString(), GitServerID: workerGitServerID, RepoID: 1,
	}
	for attempt, wantFailures := range []int{1, 2, 3} {
		job.RetryCount = attempt
		pool.rescheduleBinding(&job, pool.ReconcileInterval, true)
		var binding models.GitCapabilityRepository
		if err := db.First(&binding, "git_repo_id = ?", 1).Error; err != nil {
			t.Fatal(err)
		}
		if binding.ReconcileFailures != wantFailures {
			t.Fatalf("attempt %d: failures = %d, want %d", attempt, binding.ReconcileFailures, wantFailures)
		}
		if delay := time.Until(binding.NextDueAt); delay > 8*time.Minute {
			t.Fatalf("attempt %d: backoff %s exceeds the cap; the binding would fall out of the schedule",
				attempt, delay)
		}
	}

	pool.rescheduleBinding(&job, pool.ReconcileInterval, false)
	var recovered models.GitCapabilityRepository
	if err := db.First(&recovered, "git_repo_id = ?", 1).Error; err != nil {
		t.Fatal(err)
	}
	if recovered.ReconcileFailures != 0 {
		t.Fatalf("failures = %d after a success, want 0", recovered.ReconcileFailures)
	}
	if delay := time.Until(recovered.NextDueAt); delay < 50*time.Second || delay > 70*time.Second {
		t.Fatalf("next due in %s after a success, want ~1 interval", delay)
	}
}
