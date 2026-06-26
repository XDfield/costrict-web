package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// setupBundleJobTestDB creates an in-memory SQLite DB with the bundle_jobs schema,
// including the partial unique index that prevents duplicate in-flight jobs per item.
func setupBundleJobTestDB(t *testing.T) *gorm.DB {
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
	// Partial unique index: at most one in-flight (pending|running) job per item.
	if err := db.Exec(`CREATE UNIQUE INDEX idx_bundle_jobs_active_item ON bundle_jobs (item_id) WHERE status IN ('pending','running')`).Error; err != nil {
		t.Fatalf("create partial unique index: %v", err)
	}
	return db
}

func TestBundleJobService_EnqueueCreatesPendingJob(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	job, err := svc.Enqueue("item-1", BundleEnqueueOptions{TriggerType: "subscribe"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job == nil {
		t.Fatal("expected a job, got nil")
	}
	if job.Status != "pending" {
		t.Errorf("status = %q, want pending", job.Status)
	}
	if job.MaxAttempts != 3 {
		t.Errorf("maxAttempts = %d, want default 3", job.MaxAttempts)
	}

	var count int64
	db.Model(&models.BundleJob{}).Where("item_id = ?", "item-1").Count(&count)
	if count != 1 {
		t.Errorf("expected 1 job row, got %d", count)
	}
}

func TestBundleJobService_DedupSyncReturnsNoOp(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	if _, err := svc.Enqueue("item-1", BundleEnqueueOptions{TriggerType: "sync"}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Second sync enqueue for the same item: in-flight job exists -> no-op (nil, nil).
	job, err := svc.Enqueue("item-1", BundleEnqueueOptions{TriggerType: "sync"})
	if err != nil {
		t.Fatalf("second enqueue returned error: %v", err)
	}
	if job != nil {
		t.Errorf("expected no-op (nil job) on dedup, got %+v", job)
	}

	var count int64
	db.Model(&models.BundleJob{}).Where("item_id = ?", "item-1").Count(&count)
	if count != 1 {
		t.Errorf("dedup should keep a single job, got %d", count)
	}
}

func TestBundleJobService_DedupManualReturnsError(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	if _, err := svc.Enqueue("item-1", BundleEnqueueOptions{TriggerType: "sync"}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// A manual enqueue while a job is in flight should surface the queued error.
	if _, err := svc.Enqueue("item-1", BundleEnqueueOptions{TriggerType: "manual"}); err != ErrBundleJobAlreadyQueued {
		t.Errorf("expected ErrBundleJobAlreadyQueued, got %v", err)
	}
}

func TestBundleJobService_ReEnqueueAfterCompletion(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	first, err := svc.Enqueue("item-1", BundleEnqueueOptions{})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Mark the first job done; the partial unique index only covers pending|running,
	// so a fresh refresh enqueue must succeed.
	db.Model(&models.BundleJob{}).Where("id = ?", first.ID).Update("status", "success")

	second, err := svc.Enqueue("item-1", BundleEnqueueOptions{})
	if err != nil {
		t.Fatalf("re-enqueue after completion: %v", err)
	}
	if second == nil {
		t.Fatal("expected a new job after the prior one completed")
	}
}

func TestBundleJobService_EmptyItemIDErrors(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}
	if _, err := svc.Enqueue("", BundleEnqueueOptions{}); err == nil {
		t.Fatal("expected error for empty itemID")
	}
}

func TestBundleJobService_GetActiveCount(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	if n, _ := svc.GetActiveCount("item-1"); n != 0 {
		t.Errorf("active count before enqueue = %d, want 0", n)
	}
	if _, err := svc.Enqueue("item-1", BundleEnqueueOptions{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, _ := svc.GetActiveCount("item-1"); n != 1 {
		t.Errorf("active count after enqueue = %d, want 1", n)
	}
}

func TestBundleJobService_LatestJobNoneReturnsNil(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}
	job, err := svc.LatestJob("ghost")
	if err != nil {
		t.Fatalf("LatestJob: %v", err)
	}
	if job != nil {
		t.Errorf("expected nil job for unknown item, got %+v", job)
	}
}

// insertFailedJob inserts a failed bundle job whose finished_at is `ago` in the
// past (negative ago => future). created_at is set so LatestJob picks it.
func insertFailedJob(t *testing.T, db *gorm.DB, id, itemID, lastErr string, finishedAgo time.Duration) {
	t.Helper()
	finished := time.Now().Add(-finishedAgo)
	job := &models.BundleJob{
		ID: id, ItemID: itemID, Status: "failed", TriggerType: "subscribe",
		MaxAttempts: 3, RetryCount: 3, LastError: lastErr,
		ScheduledAt: time.Now().Add(-time.Hour), FinishedAt: &finished,
		CreatedAt: time.Now(),
	}
	if err := db.Create(job).Error; err != nil {
		t.Fatalf("insert failed job: %v", err)
	}
}

func TestBundleJobService_FailedInCooldown_RecentFailureSuppresses(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	insertFailedJob(t, db, "j-fail", "item-cool", "clone refused", 1*time.Minute)

	suppressed, job, err := svc.FailedInCooldown("item-cool", 10*time.Minute)
	if err != nil {
		t.Fatalf("FailedInCooldown: %v", err)
	}
	if !suppressed {
		t.Error("expected suppression for a failure 1m ago within a 10m cooldown")
	}
	if job == nil || job.LastError != "clone refused" {
		t.Errorf("expected the failed job with its last_error, got %+v", job)
	}
}

func TestBundleJobService_FailedInCooldown_OldFailureAllowsRetry(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	insertFailedJob(t, db, "j-old", "item-old", "transient", 20*time.Minute)

	suppressed, _, err := svc.FailedInCooldown("item-old", 10*time.Minute)
	if err != nil {
		t.Fatalf("FailedInCooldown: %v", err)
	}
	if suppressed {
		t.Error("expected NO suppression for a failure 20m ago past a 10m cooldown")
	}
}

func TestBundleJobService_FailedInCooldown_NonFailedLatestNotSuppressed(t *testing.T) {
	db := setupBundleJobTestDB(t)
	svc := &BundleJobService{DB: db}

	// Latest job is a success -> never suppress.
	now := time.Now()
	db.Create(&models.BundleJob{
		ID: "j-ok", ItemID: "item-ok", Status: "success", MaxAttempts: 3,
		ScheduledAt: now, FinishedAt: &now, CreatedAt: now,
	})

	suppressed, _, err := svc.FailedInCooldown("item-ok", 10*time.Minute)
	if err != nil {
		t.Fatalf("FailedInCooldown: %v", err)
	}
	if suppressed {
		t.Error("a successful latest job must never suppress enqueue")
	}
}
