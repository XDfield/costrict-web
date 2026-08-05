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
		last_synced_at DATETIME, last_error TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL,
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

func TestGitCapabilityWorkerReconcileIsBoundedStaleAndBucketIdempotent(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	now := time.Now()
	for i := int64(1); i <= 4; i++ {
		repo := models.GitCapabilityRepository{ID: uuid.NewString(), GitServerID: workerGitServerID, GitRepoID: i, RepositoryID: uuid.NewString(), RegistryID: uuid.NewString(), FullName: "org/repo", RepoKind: "standalone", IdentificationStatus: "unknown", Visibility: "public", GitRemoteURL: "https://git/repo", DefaultBranch: "main", CreatedBy: "test", CreatedAt: now, UpdatedAt: now}
		if i == 1 {
			fresh := now
			repo.LastSyncedAt = &fresh
		}
		if err := db.Create(&repo).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: time.Hour, ReconcileBatchSize: 2}
	p.reconcileIfDue()
	var count int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 2 {
		t.Fatalf("jobs=%d, want bounded batch 2", count)
	}
	p.lastReconcile = time.Time{}
	p.reconcileIfDue()
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 2 {
		t.Fatalf("jobs=%d after same bucket, want idempotent 2", count)
	}
}

// A sub-second reconcile interval used to take the whole worker down: the
// bucket divided by whole seconds, so anything under 1s truncated to zero and
// panicked with a division by zero the moment a repository came due. The value
// is reachable from configuration — GIT_CAPABILITY_RECONCILE_INTERVAL parses
// any positive Duration — so this is a deployment away, not a programming error.
func TestGitCapabilityWorkerReconcileSurvivesSubSecondInterval(t *testing.T) {
	db := setupGitCapabilityWorkerDB(t)
	now := time.Now()
	repo := models.GitCapabilityRepository{ID: uuid.NewString(), GitServerID: workerGitServerID, GitRepoID: 91, RepositoryID: uuid.NewString(), RegistryID: uuid.NewString(), FullName: "org/subsecond", RepoKind: "standalone", IdentificationStatus: "unknown", Visibility: "public", GitRemoteURL: "https://git/repo", DefaultBranch: "main", CreatedBy: "test", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}

	// Deliberately bypass Start()'s clamp and hand reconcileIfDue the raw
	// sub-second value: this pins the bucket arithmetic itself, so the guard and
	// the arithmetic are independently safe rather than one covering for the other.
	p := &GitCapabilityWorkerPool{DB: db, ReconcileInterval: 500 * time.Millisecond, ReconcileBatchSize: 2}

	// The real assertion is that this returns at all — before the fix it panicked
	// with an integer divide by zero.
	p.reconcileIfDue()

	var count int64
	db.Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 1 {
		t.Fatalf("jobs=%d, want the due repository enqueued once", count)
	}
}
