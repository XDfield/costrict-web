package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const workerGitServerID = "git-server-1"

type fakeGitCapabilityResolver struct {
	cfg *gitserver.Config
	err error
}

func (r *fakeGitCapabilityResolver) ResolveByServerID(context.Context, string) (*gitserver.Config, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.cfg, nil
}

type fakeGitCapabilitySyncer struct {
	result               *services.GitCapabilitySyncResult
	err                  error
	lease                services.GitCapabilitySyncLease
	jobID                string
	defaultBranchDeleted bool
}

func (s *fakeGitCapabilitySyncer) SyncRepository(
	_ context.Context,
	_ *gitserver.Config,
	_ int64,
	_ string,
	_ string,
	defaultBranchDeleted bool,
	lease services.GitCapabilitySyncLease,
) (*services.GitCapabilitySyncResult, error) {
	s.lease = lease
	s.jobID = lease.JobID
	s.defaultBranchDeleted = defaultBranchDeleted
	return s.result, s.err
}

func setupGitCapabilityWorkerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql database: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec(`CREATE TABLE git_capability_sync_jobs (
		id TEXT PRIMARY KEY, git_server_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
		repo_id INTEGER NOT NULL, repo_full_name TEXT NOT NULL, default_branch TEXT NOT NULL,
		ref TEXT NOT NULL, before_sha TEXT NOT NULL DEFAULT '', after_sha TEXT NOT NULL,
		status TEXT NOT NULL, retry_count INTEGER NOT NULL, max_attempts INTEGER NOT NULL,
		last_error TEXT, scheduled_at DATETIME NOT NULL, started_at DATETIME,
		lease_token TEXT NOT NULL DEFAULT '', finished_at DATETIME, created_at DATETIME NOT NULL,
		UNIQUE(git_server_id, delivery_id)
	)`).Error; err != nil {
		t.Fatalf("create jobs table: %v", err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX uq_git_capability_sync_jobs_running_repo
			ON git_capability_sync_jobs (git_server_id, repo_id) WHERE status = 'running'`).Error; err != nil {
		t.Fatalf("create running repo index: %v", err)
	}
	if err := db.Exec(`CREATE TABLE git_capability_repositories (
		id TEXT PRIMARY KEY, git_server_id TEXT NOT NULL, git_repo_id INTEGER NOT NULL,
		repository_id TEXT NOT NULL, registry_id TEXT NOT NULL, full_name TEXT NOT NULL,
		repo_kind TEXT NOT NULL, identification_status TEXT NOT NULL, visibility TEXT NOT NULL,
		git_remote_url TEXT NOT NULL, default_branch TEXT NOT NULL, last_synced_commit TEXT NOT NULL DEFAULT '',
		last_synced_at DATETIME, last_error TEXT NOT NULL DEFAULT '', next_due_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, reconcile_paused INTEGER NOT NULL DEFAULT 0, reconcile_failures INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL,
		created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create repositories table: %v", err)
	}
	return db
}

func newGitCapabilityWorkerJob(id, deliveryID string, repoID int64, status string) models.GitCapabilitySyncJob {
	now := time.Now()
	return models.GitCapabilitySyncJob{
		ID:            id,
		GitServerID:   workerGitServerID,
		DeliveryID:    deliveryID,
		RepoID:        repoID,
		RepoFullName:  "alice/repository",
		DefaultBranch: "main",
		Ref:           "refs/heads/main",
		AfterSHA:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:        status,
		MaxAttempts:   3,
		ScheduledAt:   now.Add(-time.Minute),
		CreatedAt:     now,
	}
}

func createGitCapabilityWorkerJob(t *testing.T, db *gorm.DB, job models.GitCapabilitySyncJob) {
	t.Helper()
	if err := db.Create(&job).Error; err != nil {
		t.Fatalf("create job %s: %v", job.ID, err)
	}
}

func loadGitCapabilityWorkerJob(t *testing.T, db *gorm.DB, id string) models.GitCapabilitySyncJob {
	t.Helper()
	var job models.GitCapabilitySyncJob
	if err := db.First(&job, "id = ?", id).Error; err != nil {
		t.Fatalf("load job %s: %v", id, err)
	}
	return job
}

func TestGitCapabilityWorkerClaimOneDoesNotRunTwoJobsForSameRepository(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	running := newGitCapabilityWorkerJob("running-job", "delivery-running", 101, models.GitCapabilitySyncJobStatusRunning)
	running.StartedAt = ptrWorkerTime(time.Now())
	running.LeaseToken = "running-lease"
	pending := newGitCapabilityWorkerJob("pending-job", "delivery-pending", 101, models.GitCapabilitySyncJobStatusPending)
	createGitCapabilityWorkerJob(t, db, running)
	createGitCapabilityWorkerJob(t, db, pending)

	pool := &GitCapabilityWorkerPool{DB: db}
	_, err := pool.claimOne()
	if !errors.Is(err, ErrNoGitCapabilityJob) {
		t.Fatalf("claim error = %v, want no job while same repository is running", err)
	}
	if after := loadGitCapabilityWorkerJob(t, db, pending.ID); after.Status != models.GitCapabilitySyncJobStatusPending || after.LeaseToken != "" {
		t.Errorf("pending job was claimed despite running job: %+v", after)
	}
}

func TestGitCapabilityWorkerClaimOneSetsLeaseToken(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	pending := newGitCapabilityWorkerJob("pending-job", "delivery-pending", 101, models.GitCapabilitySyncJobStatusPending)
	createGitCapabilityWorkerJob(t, db, pending)

	claimed, err := (&GitCapabilityWorkerPool{DB: db}).claimOne()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Status != models.GitCapabilitySyncJobStatusRunning || claimed.StartedAt == nil || claimed.LeaseToken == "" {
		t.Fatalf("claimed job has no active lease: %+v", claimed)
	}
	if _, err := uuid.Parse(claimed.LeaseToken); err != nil {
		t.Errorf("lease token = %q, want UUID: %v", claimed.LeaseToken, err)
	}
	persisted := loadGitCapabilityWorkerJob(t, db, pending.ID)
	if persisted.Status != models.GitCapabilitySyncJobStatusRunning || persisted.LeaseToken != claimed.LeaseToken || persisted.StartedAt == nil {
		t.Errorf("claim was not persisted: %+v", persisted)
	}
}

func TestGitCapabilityWorkerReclaimExpiredLeaseRetriesOrFailsAndClearsToken(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	retry := newGitCapabilityWorkerJob("retry-job", "delivery-retry", 101, models.GitCapabilitySyncJobStatusRunning)
	retry.StartedAt = ptrWorkerTime(time.Now().Add(-2 * time.Hour))
	retry.LeaseToken = "retry-token"
	retry.RetryCount = 0
	exhausted := newGitCapabilityWorkerJob("exhausted-job", "delivery-exhausted", 102, models.GitCapabilitySyncJobStatusRunning)
	exhausted.StartedAt = ptrWorkerTime(time.Now().Add(-2 * time.Hour))
	exhausted.LeaseToken = "exhausted-token"
	exhausted.RetryCount = 2
	createGitCapabilityWorkerJob(t, db, retry)
	createGitCapabilityWorkerJob(t, db, exhausted)

	pool := &GitCapabilityWorkerPool{DB: db, LeaseTimeout: time.Minute}
	pool.reclaimExpiredLeases()

	retried := loadGitCapabilityWorkerJob(t, db, retry.ID)
	if retried.Status != models.GitCapabilitySyncJobStatusPending || retried.RetryCount != 1 || retried.LeaseToken != "" || retried.StartedAt != nil || retried.FinishedAt != nil {
		t.Errorf("reclaimed retry job has wrong state: %+v", retried)
	}
	if retried.LastError == nil || *retried.LastError == "" {
		t.Error("reclaimed retry job has no failure reason")
	}
	failed := loadGitCapabilityWorkerJob(t, db, exhausted.ID)
	if failed.Status != models.GitCapabilitySyncJobStatusFailed || failed.RetryCount != 3 || failed.LeaseToken != "" || failed.FinishedAt == nil {
		t.Errorf("exhausted job has wrong terminal state: %+v", failed)
	}
}

func TestGitCapabilityWorkerFinalizeRejectsStaleLeaseToken(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	job := newGitCapabilityWorkerJob("running-job", "delivery-running", 101, models.GitCapabilitySyncJobStatusRunning)
	job.StartedAt = ptrWorkerTime(time.Now())
	job.LeaseToken = "current-token"
	createGitCapabilityWorkerJob(t, db, job)

	stale := job
	stale.LeaseToken = "stale-token"
	err := (&GitCapabilityWorkerPool{DB: db}).finalizeJob(&stale, &services.GitCapabilitySyncResult{}, nil)
	if !errors.Is(err, ErrGitCapabilityLeaseLost) {
		t.Fatalf("finalize error = %v, want lease lost", err)
	}
	after := loadGitCapabilityWorkerJob(t, db, job.ID)
	if after.Status != models.GitCapabilitySyncJobStatusRunning || after.LeaseToken != "current-token" || after.FinishedAt != nil {
		t.Errorf("stale worker changed claimed job: %+v", after)
	}
}

func TestGitCapabilityWorkerProcessOnePassesLeaseToSyncer(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	job := newGitCapabilityWorkerJob("pending-job", "delivery-pending", 101, models.GitCapabilitySyncJobStatusPending)
	createGitCapabilityWorkerJob(t, db, job)
	syncer := &fakeGitCapabilitySyncer{result: &services.GitCapabilitySyncResult{CommitSHA: job.AfterSHA}}
	pool := &GitCapabilityWorkerPool{
		DB:          db,
		Resolver:    &fakeGitCapabilityResolver{cfg: &gitserver.Config{ServerID: workerGitServerID}},
		SyncService: syncer,
	}
	if err := pool.processOne(); err != nil {
		t.Fatalf("process job: %v", err)
	}
	if syncer.jobID != job.ID || syncer.lease.Token == "" {
		t.Fatalf("syncer received no claim lease: %+v", syncer.lease)
	}
	if _, err := uuid.Parse(syncer.lease.Token); err != nil {
		t.Errorf("syncer lease token = %q, want UUID: %v", syncer.lease.Token, err)
	}
	completed := loadGitCapabilityWorkerJob(t, db, job.ID)
	if completed.Status != models.GitCapabilitySyncJobStatusSuccess || completed.LeaseToken != "" || completed.FinishedAt == nil {
		t.Errorf("job was not completed with compare-and-swap: %+v", completed)
	}
}

func TestGitCapabilityWorkerProcessOneMarksDeletedDefaultBranch(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	job := newGitCapabilityWorkerJob("deleted-default-job", "delivery-deleted-default", 101, models.GitCapabilitySyncJobStatusPending)
	job.AfterSHA = strings.Repeat("0", 40)
	createGitCapabilityWorkerJob(t, db, job)
	syncer := &fakeGitCapabilitySyncer{result: &services.GitCapabilitySyncResult{}}
	pool := &GitCapabilityWorkerPool{
		DB:          db,
		Resolver:    &fakeGitCapabilityResolver{cfg: &gitserver.Config{ServerID: workerGitServerID}},
		SyncService: syncer,
	}
	if err := pool.processOne(); err != nil {
		t.Fatalf("process deletion job: %v", err)
	}
	if !syncer.defaultBranchDeleted {
		t.Fatal("worker did not identify the zero-SHA default-branch deletion")
	}
}

func ptrWorkerTime(value time.Time) *time.Time { return &value }

func seedGitCapabilityBinding(t *testing.T, db *gorm.DB, repoID int64, due time.Time, paused bool) models.GitCapabilityRepository {
	t.Helper()
	now := time.Now()
	repo := models.GitCapabilityRepository{
		ID: uuid.NewString(), GitServerID: workerGitServerID, GitRepoID: repoID,
		RepositoryID: uuid.NewString(), RegistryID: uuid.NewString(), FullName: "org/repo",
		RepoKind: "standalone", IdentificationStatus: "unknown", Visibility: "public",
		GitRemoteURL: "https://git/repo", DefaultBranch: "main", NextDueAt: due,
		ReconcilePaused: paused, CreatedBy: "test", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&repo).Error; err != nil {
		t.Fatalf("seed binding %d: %v", repoID, err)
	}
	return repo
}

// The freshness SLA is the reason this scheduler exists, so this is the test
// that has to hold: with more bindings than fit in one batch, ONE drain pass
// must still enqueue every one of them. The old fixed "one batch per interval"
// loop enqueued exactly ReconcileBatchSize rows and then waited a full interval,
// silently turning a 10-minute promise into ceil(bindings/batch) intervals.
func TestGitCapabilityWorkerReconcileDrainsEveryDueBindingInOnePass(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	overdue := time.Now().Add(-time.Hour)
	const bindings = 7
	for i := int64(1); i <= bindings; i++ {
		seedGitCapabilityBinding(t, db, i, overdue, false)
	}

	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: time.Hour, ReconcileBatchSize: 2, PollInterval: time.Second}
	p.drainDueReconciles()

	var count int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != bindings {
		t.Fatalf("jobs=%d, want every due binding drained (%d)", count, bindings)
	}

	// And every binding's schedule moved, so nothing is due immediately after.
	var stillDue int64
	db.Model(&models.GitCapabilityRepository{}).
		Where("reconcile_paused = ? AND next_due_at <= ?", false, time.Now()).Count(&stillDue)
	if stillDue != 0 {
		t.Fatalf("stillDue=%d after a complete drain, want 0", stillDue)
	}
}

func TestGitCapabilityWorkerReconcileSkipsPausedAndNotYetDueBindings(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	overdue := time.Now().Add(-time.Hour)
	seedGitCapabilityBinding(t, db, 1, overdue, false)
	seedGitCapabilityBinding(t, db, 2, overdue, true)                    // operator kill switch
	seedGitCapabilityBinding(t, db, 3, time.Now().Add(time.Hour), false) // not due yet

	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: time.Hour, ReconcileBatchSize: 10, PollInterval: time.Second}
	p.drainDueReconciles()

	var jobs []models.GitCapabilitySyncJob
	if err := db.Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].RepoID != 1 {
		t.Fatalf("jobs=%+v, want only the due, unpaused binding", jobs)
	}
}

// A second drain within the same schedule slot must not re-enqueue: next_due_at
// was already pushed out, and the delivery id is derived from the slot that was
// consumed.
func TestGitCapabilityWorkerReconcileDrainIsIdempotentWithinASlot(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	seedGitCapabilityBinding(t, db, 1, time.Now().Add(-time.Hour), false)

	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: time.Hour, ReconcileBatchSize: 10, PollInterval: time.Second}
	p.drainDueReconciles()
	p.lastDrain = time.Time{}
	p.drainDueReconciles()

	var count int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 1 {
		t.Fatalf("jobs=%d, want idempotent 1", count)
	}
}

// The old bucketed delivery id keyed on now/interval, so a binding rescheduled
// early by backoff produced an id that already existed in a terminal state and
// its retry was silently dropped. Deriving the id from the schedule slot fixes
// it: a new slot is a new delivery.
func TestGitCapabilityWorkerReconcileEnqueuesAgainInTheNextSlot(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	binding := seedGitCapabilityBinding(t, db, 1, time.Now().Add(-time.Hour), false)

	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: time.Hour, ReconcileBatchSize: 10, PollInterval: time.Second}
	p.drainDueReconciles()

	// Simulate a failure backoff bringing the binding due again inside the same
	// wall-clock interval bucket.
	if err := db.Model(&models.GitCapabilityRepository{}).Where("id = ?", binding.ID).
		Update("next_due_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	p.lastDrain = time.Time{}
	p.drainDueReconciles()

	var count int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 2 {
		t.Fatalf("jobs=%d, want a second delivery for the new schedule slot", count)
	}
}

func TestGitCapabilityWorkerRescheduleBindingResetsOnSuccessAndBacksOffOnFailure(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	binding := seedGitCapabilityBinding(t, db, 1, time.Now().Add(-time.Hour), false)
	if err := db.Model(&models.GitCapabilityRepository{}).Where("id = ?", binding.ID).
		Update("reconcile_failures", 2).Error; err != nil {
		t.Fatal(err)
	}
	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: 10 * time.Minute}
	job := newGitCapabilityWorkerJob("job-1", "delivery-1", 1, models.GitCapabilitySyncJobStatusRunning)

	p.rescheduleBinding(&job, p.ReconcileInterval, false)
	var after models.GitCapabilityRepository
	if err := db.First(&after, "id = ?", binding.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ReconcileFailures != 0 {
		t.Errorf("reconcileFailures = %d after success, want 0", after.ReconcileFailures)
	}
	if delay := time.Until(after.NextDueAt); delay < 9*time.Minute || delay > 11*time.Minute {
		t.Errorf("next due in %s after success, want ~1 interval", delay)
	}

	job.RetryCount = 2
	p.rescheduleBinding(&job, p.ReconcileInterval, true)
	if err := db.First(&after, "id = ?", binding.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ReconcileFailures != 1 {
		t.Errorf("reconcileFailures = %d after failure, want 1", after.ReconcileFailures)
	}
	if delay := time.Until(after.NextDueAt); delay < 35*time.Minute {
		t.Errorf("next due in %s after failure, want an exponential backoff", delay)
	}
}

// A sub-second reconcile interval used to take the whole worker down: the old
// bucket divided by whole seconds, so anything under 1s truncated to zero and
// panicked with a division by zero. The value is reachable from configuration —
// GIT_CAPABILITY_RECONCILE_INTERVAL parses any positive Duration — so this is a
// deployment away, not a programming error. Kept after the rewrite so the
// arithmetic stays safe independently of Start()'s clamp.
func TestGitCapabilityWorkerReconcileSurvivesSubSecondInterval(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	seedGitCapabilityBinding(t, db, 91, time.Now().Add(-time.Second), false)

	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: 500 * time.Millisecond, ReconcileBatchSize: 2, PollInterval: time.Second}
	p.drainDueReconciles()

	var count int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 1 {
		t.Fatalf("jobs=%d, want the due repository enqueued once", count)
	}
}
