package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
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
