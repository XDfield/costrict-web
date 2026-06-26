package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func setupBundleJobDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmt := `CREATE TABLE bundle_jobs (
		id TEXT PRIMARY KEY,
		item_id TEXT NOT NULL,
		trigger_type TEXT NOT NULL DEFAULT 'sync',
		trigger_user TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		retry_count INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		last_error TEXT,
		artifact_id TEXT,
		scheduled_at DATETIME NOT NULL,
		started_at DATETIME,
		finished_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME
	)`
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create bundle_jobs: %v", err)
	}
	return db
}

func TestBundleWorker_FinalizeSuccess(t *testing.T) {
	db := setupBundleJobDB(t)
	pool := &BundleWorkerPool{DB: db}

	job := &models.BundleJob{ID: "j1", ItemID: "i1", Status: "running", MaxAttempts: 3, ScheduledAt: time.Now()}
	db.Create(job)

	pool.finalizeJob(job, &models.CapabilityArtifact{ID: "art-1", ArtifactVersion: "sha"}, nil)

	var got models.BundleJob
	db.First(&got, "id = ?", "j1")
	if got.Status != "success" {
		t.Errorf("status = %q, want success", got.Status)
	}
	if got.ArtifactID == nil || *got.ArtifactID != "art-1" {
		t.Errorf("artifactID not recorded, got %v", got.ArtifactID)
	}
}

func TestBundleWorker_FinalizeRetryBackoff(t *testing.T) {
	db := setupBundleJobDB(t)
	pool := &BundleWorkerPool{DB: db}

	// retry_count 0, max 3 -> first failure goes back to pending with backoff.
	job := &models.BundleJob{ID: "j2", ItemID: "i2", Status: "running", RetryCount: 0, MaxAttempts: 3, ScheduledAt: time.Now()}
	db.Create(job)

	pool.finalizeJob(job, nil, errors.New("clone timeout"))

	var got models.BundleJob
	db.First(&got, "id = ?", "j2")
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending (retry)", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("retryCount = %d, want 1", got.RetryCount)
	}
	if got.LastError != "clone timeout" {
		t.Errorf("lastError = %q, want 'clone timeout'", got.LastError)
	}
	if !got.ScheduledAt.After(time.Now()) {
		t.Errorf("scheduledAt = %v, want a future backoff time", got.ScheduledAt)
	}
}

func TestBundleWorker_FinalizePermanentFailure(t *testing.T) {
	db := setupBundleJobDB(t)
	pool := &BundleWorkerPool{DB: db}

	// retry_count 2, max 3 -> next failure exhausts attempts -> failed.
	job := &models.BundleJob{ID: "j3", ItemID: "i3", Status: "running", RetryCount: 2, MaxAttempts: 3, ScheduledAt: time.Now()}
	db.Create(job)

	pool.finalizeJob(job, nil, errors.New("repo gone"))

	var got models.BundleJob
	db.First(&got, "id = ?", "j3")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.LastError != "repo gone" {
		t.Errorf("lastError = %q, want 'repo gone'", got.LastError)
	}
}

// TestBundleWorker_ReclaimStaleRunning verifies that a job stuck in 'running' past
// the lease TTL is reset back to 'pending' (retries left) so the per-item partial
// unique index no longer wedges future enqueues for that item after a crash.
func TestBundleWorker_ReclaimStaleRunning(t *testing.T) {
	db := setupBundleJobDB(t)
	pool := &BundleWorkerPool{DB: db, JobTimeout: time.Minute} // lease TTL = 2m

	old := time.Now().Add(-10 * time.Minute)
	db.Create(&models.BundleJob{
		ID: "stale", ItemID: "i-stale", Status: "running", RetryCount: 0, MaxAttempts: 3,
		ScheduledAt: old, StartedAt: &old,
	})

	n := pool.reclaimStaleRunning()
	if n != 1 {
		t.Fatalf("reclaimed = %d, want 1", n)
	}

	var got models.BundleJob
	db.First(&got, "id = ?", "stale")
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending (retries left)", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("retryCount = %d, want 1", got.RetryCount)
	}
	if got.StartedAt != nil {
		t.Errorf("started_at should be cleared, got %v", got.StartedAt)
	}
}

// TestBundleWorker_ReclaimStaleRunning_ExhaustedFails verifies a stale job that is
// already out of attempts is marked 'failed' rather than re-queued forever.
func TestBundleWorker_ReclaimStaleRunning_ExhaustedFails(t *testing.T) {
	db := setupBundleJobDB(t)
	pool := &BundleWorkerPool{DB: db, JobTimeout: time.Minute}

	old := time.Now().Add(-10 * time.Minute)
	db.Create(&models.BundleJob{
		ID: "stale2", ItemID: "i-stale2", Status: "running", RetryCount: 2, MaxAttempts: 3,
		ScheduledAt: old, StartedAt: &old,
	})

	if n := pool.reclaimStaleRunning(); n != 1 {
		t.Fatalf("reclaimed = %d, want 1", n)
	}

	var got models.BundleJob
	db.First(&got, "id = ?", "stale2")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed (attempts exhausted)", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("failed reclaimed job should record finished_at")
	}
}

// TestBundleWorker_ReclaimStaleRunning_FreshRunningUntouched verifies a job that
// only just started (within the lease TTL) is NOT reclaimed.
func TestBundleWorker_ReclaimStaleRunning_FreshRunningUntouched(t *testing.T) {
	db := setupBundleJobDB(t)
	pool := &BundleWorkerPool{DB: db, JobTimeout: time.Minute}

	now := time.Now()
	db.Create(&models.BundleJob{
		ID: "fresh", ItemID: "i-fresh", Status: "running", MaxAttempts: 3,
		ScheduledAt: now, StartedAt: &now,
	})

	if n := pool.reclaimStaleRunning(); n != 0 {
		t.Errorf("reclaimed = %d, want 0 (fresh running job within lease)", n)
	}
	var got models.BundleJob
	db.First(&got, "id = ?", "fresh")
	if got.Status != "running" {
		t.Errorf("fresh job status = %q, want running (untouched)", got.Status)
	}
}

// TestBundleWorker_RunJobPanicFinalizesFailed verifies the panic-recovery guard:
// runJob recovers from a panic in the pack (here a nil PackService deref), finalizes
// the job as failed with a last_error, and does NOT propagate the panic (the worker
// goroutine survives). The job must not be left stuck 'running'.
//
// runJob is the pack+finalize tail split out of processOne; the dequeue step uses
// Postgres-only FOR UPDATE SKIP LOCKED so it can't run under SQLite, but runJob is
// pure DB + pack and tests the recovery path directly with an already-claimed job.
func TestBundleWorker_RunJobPanicFinalizesFailed(t *testing.T) {
	db := setupBundleJobDB(t)
	// capability_items so the item Preload succeeds; the item carries a valid http
	// source_url so PackItemBundle proceeds past its guards into the (nil) Git clone.
	if err := db.Exec(`CREATE TABLE capability_items (id TEXT PRIMARY KEY, registry_id TEXT, source_url TEXT)`).Error; err != nil {
		t.Fatalf("create capability_items: %v", err)
	}
	db.Exec(`INSERT INTO capability_items (id, registry_id, source_url) VALUES ('i-panic','','https://github.com/owner/repo/tree/main')`)

	// PackService is non-nil but its Git collaborator is nil, so PackItemBundle's
	// s.Git.CloneContext(...) nil-derefs -> a real panic inside the pack, exactly the
	// shape runJob's recovery must contain.
	pool := &BundleWorkerPool{DB: db, PackService: &services.BundlePackService{DB: db, AllowLocalClone: false}}

	job := models.BundleJob{ID: "panic-job", ItemID: "i-panic", Status: "running", MaxAttempts: 3, ScheduledAt: time.Now()}
	db.Create(&job)

	// runJob must not panic out (recover swallows it).
	pool.runJob(&job)

	var got models.BundleJob
	db.First(&got, "id = ?", "panic-job")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed after panic recovery", got.Status)
	}
	if got.LastError == "" {
		t.Error("expected last_error recorded after panic recovery")
	}
}
