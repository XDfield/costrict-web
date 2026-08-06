package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNoGitCapabilityJob     = errors.New("no Git capability sync job available")
	ErrGitCapabilityLeaseLost = errors.New("git capability job lease lost")
)

type GitCapabilityConfigResolver interface {
	ResolveByServerID(ctx context.Context, serverID string) (*gitserver.Config, error)
}

type GitCapabilitySyncer interface {
	SyncRepository(ctx context.Context, cfg *gitserver.Config, repoID int64, repoFullName, defaultBranch string, defaultBranchDeleted bool, lease services.GitCapabilitySyncLease) (*services.GitCapabilitySyncResult, error)
}

type GitCapabilityWorkerPool struct {
	DB           *gorm.DB
	Resolver     GitCapabilityConfigResolver
	SyncService  GitCapabilitySyncer
	Concurrency  int
	PollInterval time.Duration
	LeaseTimeout time.Duration
	JobTimeout   time.Duration
	// ReconcileInterval is the freshness SLA, not a sweep period: every healthy
	// registered binding must be re-read from Gitea within this window. It is the
	// convergence bound this platform actually ships, because rename, transfer,
	// visibility change, default-branch change and archive emit NO webhook on the
	// deployed Gitea 1.24.6 — reconcile is their only correctness path.
	ReconcileInterval time.Duration
	// ReconcileBatchSize bounds ONE claim round, not one interval. The drain
	// repeats until nothing is due, so the SLA depends on worker throughput
	// (measurable, alertable) instead of on binding count.
	ReconcileBatchSize int
	// ReconcileDrainBudget bounds how long a single drain pass may run before it
	// yields to job processing. Exceeding it is reported: it means the enqueue
	// side alone cannot keep up, which is the first symptom of an SLA breach.
	ReconcileDrainBudget time.Duration
	lastDrain            time.Time
	reconcileMu          sync.Mutex
	stopCh               chan struct{}
	wg                   sync.WaitGroup
}

func (p *GitCapabilityWorkerPool) Start() {
	if p == nil || p.DB == nil || p.Resolver == nil || p.SyncService == nil {
		return
	}
	if p.Concurrency <= 0 {
		p.Concurrency = 2
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 5 * time.Second
	}
	if p.LeaseTimeout <= 0 {
		p.LeaseTimeout = 15 * time.Minute
	}
	if p.JobTimeout <= 0 {
		p.JobTimeout = 10 * time.Minute
	}
	if p.ReconcileInterval <= 0 {
		p.ReconcileInterval = 10 * time.Minute
	}
	// Nothing is served by a freshness window shorter than a second, and a
	// pathological value would spend the whole worker on re-reads.
	if p.ReconcileInterval < time.Second {
		logger.Warn("Git capability reconcile interval %s is below one second; using 1s", p.ReconcileInterval)
		p.ReconcileInterval = time.Second
	}
	if p.ReconcileBatchSize <= 0 {
		p.ReconcileBatchSize = 50
	}
	if p.ReconcileDrainBudget <= 0 {
		p.ReconcileDrainBudget = 30 * time.Second
	}
	p.stopCh = make(chan struct{})
	for i := 0; i < p.Concurrency; i++ {
		p.wg.Add(1)
		go p.runWorker()
	}
}

func (p *GitCapabilityWorkerPool) Stop() {
	if p == nil || p.stopCh == nil {
		return
	}
	close(p.stopCh)
	p.wg.Wait()
}

func (p *GitCapabilityWorkerPool) runWorker() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.drainDueReconciles()
			p.reclaimExpiredLeases()
			// Process jobs back to back while any remain. One job per tick made
			// throughput a function of PollInterval, which the reconcile SLA cannot
			// tolerate: with 5s ticks and 2 workers the whole platform could apply
			// at most 240 convergences per 10-minute freshness window, no matter how
			// many bindings were due.
			//
			// Bounded by one poll interval, because an unbounded loop trades the old
			// starvation for a new one: under sustained webhook load every worker
			// would stay inside it and the drain — which is the ONLY correctness
			// path for the five transitions Gitea emits nothing for — would never
			// run again. Yielding here costs a tick; not yielding costs convergence.
			deadline := time.Now().Add(p.PollInterval)
			for p.processOne() == nil {
				select {
				case <-p.stopCh:
					return
				default:
				}
				if !time.Now().Before(deadline) {
					break
				}
			}
		}
	}
}

// drainDueReconciles claims every binding whose re-read is due, in bounded
// rounds, until nothing is due or the budget for this pass is spent.
//
// The loop is the whole point. A fixed "one batch per interval" schedule
// silently degrades the freshness SLA to ceil(bindings/batch) intervals — with
// 88 bindings and a batch of 50 that is already 20 minutes for a 10-minute
// promise, and nothing in the system reports it. Draining makes the SLA depend
// on throughput instead, and throughput is measured and alerted below.
func (p *GitCapabilityWorkerPool) drainDueReconciles() {
	if p == nil || p.DB == nil {
		return
	}
	interval := p.ReconcileInterval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	batchSize := p.ReconcileBatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	budget := p.ReconcileDrainBudget
	if budget <= 0 {
		budget = 30 * time.Second
	}
	pollInterval := p.PollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	// One drain per poll interval across the whole pool: every worker goroutine
	// calls this, and N concurrent drains would just contend on the same due
	// rows. This is a throttle on redundant work, NOT the schedule — the schedule
	// lives in next_due_at, in the database, where a restart cannot lose it.
	start := time.Now()
	p.reconcileMu.Lock()
	if !p.lastDrain.IsZero() && start.Sub(p.lastDrain) < pollInterval {
		p.reconcileMu.Unlock()
		return
	}
	p.lastDrain = start
	p.reconcileMu.Unlock()

	deadline := start.Add(budget)
	enqueued := 0
	for {
		selected, claimed, err := p.claimDueReconcileBatch(interval, batchSize)
		if err != nil {
			logger.Warn("Git capability reconcile drain failed after %d binding(s): %v", enqueued, err)
			break
		}
		enqueued += claimed
		if selected < batchSize {
			break
		}
		if !time.Now().Before(deadline) {
			logger.Warn("Git capability reconcile drain budget %s exhausted after %d binding(s); backlog remains",
				budget, enqueued)
			break
		}
	}
	p.reportReconcileBacklog(interval, enqueued, time.Since(start))
}

// claimDueReconcileBatch enqueues one bounded round of due bindings and returns
// how many rows were selected and how many were actually claimed.
//
// The two numbers differ when another replica won a row. The caller uses
// `selected` to decide whether the queue is drained (a short read means nothing
// is left due) and `claimed` only for reporting.
func (p *GitCapabilityWorkerPool) claimDueReconcileBatch(interval time.Duration, batchSize int) (int, int, error) {
	selected, claimed := 0, 0
	err := p.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		query := tx.Where("reconcile_paused = ? AND next_due_at <= ?", false, now).
			Order("next_due_at ASC").Limit(batchSize)
		if tx.Dialector.Name() == "postgres" {
			// SKIP LOCKED is what lets several replicas drain the same queue
			// concurrently without one waiting on another's batch.
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		var repos []models.GitCapabilityRepository
		if err := query.Find(&repos).Error; err != nil {
			return err
		}
		selected = len(repos)
		for _, repo := range repos {
			// Compare-and-set on "still due". A replica that lost the race finds
			// next_due_at already pushed out and skips the row. The predicate is
			// `<= now` rather than an equality on the exact timestamp so it does not
			// depend on a driver round-tripping sub-second precision unchanged.
			advanced := tx.Model(&models.GitCapabilityRepository{}).
				Where("id = ? AND next_due_at <= ?", repo.ID, now).
				Updates(map[string]any{"next_due_at": now.Add(interval), "updated_at": now})
			if advanced.Error != nil {
				return advanced.Error
			}
			if advanced.RowsAffected == 0 {
				continue
			}
			// The delivery id is derived from the schedule slot this claim consumed,
			// so it is identical on every replica that could have claimed it and
			// distinct from the next slot's. The old whole-interval bucket could not
			// do the second half: a binding re-scheduled early by backoff produced a
			// delivery id that already existed in a terminal state, and its retry
			// was silently dropped.
			delivery := reconcileDeliveryID(repo.GitRepoID, repo.NextDueAt)
			job := models.GitCapabilitySyncJob{
				ID: uuid.NewString(), GitServerID: repo.GitServerID, DeliveryID: delivery,
				RepoID: repo.GitRepoID, RepoFullName: repo.FullName, DefaultBranch: repo.DefaultBranch,
				Ref: repo.DefaultBranch, AfterSHA: repo.LastSyncedCommit,
				Status: models.GitCapabilitySyncJobStatusPending, MaxAttempts: 3,
				ScheduledAt: now, CreatedAt: now,
			}
			result := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "git_server_id"}, {Name: "delivery_id"}},
				DoNothing: true,
			}).Create(&job)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				claimed++
			}
		}
		return nil
	})
	return selected, claimed, err
}

func reconcileDeliveryID(repoID int64, due time.Time) string {
	return fmt.Sprintf("%s%d:%d", models.GitCapabilitySyncDeliveryPrefixReconcile, repoID, due.UTC().UnixNano())
}

// reportReconcileBacklog publishes the four numbers the freshness SLA is
// actually judged on: oldest-due age, queue depth, drain completion latency and
// enqueue count.
//
// Oldest-due age is the one that matters. It answers "how stale is the most
// stale binding right now" directly, which is the SLA statement, whereas a
// throughput number only answers it after arithmetic that nobody does during an
// incident.
func (p *GitCapabilityWorkerPool) reportReconcileBacklog(interval time.Duration, enqueued int, elapsed time.Duration) {
	var backlog struct {
		Due    int64
		Oldest *time.Time
	}
	if err := p.DB.Model(&models.GitCapabilityRepository{}).
		Select("COUNT(*) AS due, MIN(next_due_at) AS oldest").
		Where("reconcile_paused = ? AND next_due_at <= ?", false, time.Now()).
		Scan(&backlog).Error; err != nil {
		logger.Warn("Git capability reconcile backlog query failed: %v", err)
		return
	}
	oldestAge := time.Duration(0)
	if backlog.Oldest != nil {
		oldestAge = time.Since(*backlog.Oldest)
	}
	if backlog.Due == 0 && enqueued == 0 {
		return
	}
	// Overdue by a whole freshness window means the promise is already broken for
	// at least one binding, whatever the averages say.
	if oldestAge > interval {
		logger.Warn("Git capability reconcile backlog: due=%d oldestDueAge=%s exceeds freshness interval %s (enqueued=%d in %s)",
			backlog.Due, oldestAge.Truncate(time.Second), interval, enqueued, elapsed.Truncate(time.Millisecond))
		return
	}
	logger.Info("Git capability reconcile drained: enqueued=%d due=%d oldestDueAge=%s elapsed=%s",
		enqueued, backlog.Due, oldestAge.Truncate(time.Second), elapsed.Truncate(time.Millisecond))
}

// rescheduleBinding moves a binding's next re-read after a job that targeted it
// reached a terminal state.
//
// Success restarts the freshness clock — a webhook-triggered convergence is a
// re-read too, so it should postpone the periodic one rather than run beside it.
// Terminal failure backs the binding off exponentially, which is what keeps a
// permanently broken repository from monopolising every drain round while still
// letting it retry; the failure counter is the alerting signal.
func (p *GitCapabilityWorkerPool) rescheduleBinding(job *models.GitCapabilitySyncJob, interval time.Duration, failed bool) {
	if p == nil || p.DB == nil || job == nil || job.RepoID <= 0 {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	now := time.Now()
	updates := map[string]any{"updated_at": now}
	if failed {
		updates["reconcile_failures"] = gorm.Expr("reconcile_failures + 1")
		updates["next_due_at"] = now.Add(gitCapabilityReconcileBackoff(interval, job.RetryCount))
	} else {
		updates["reconcile_failures"] = 0
		updates["next_due_at"] = now.Add(interval)
	}
	if err := p.DB.Model(&models.GitCapabilityRepository{}).
		Where("git_server_id = ? AND git_repo_id = ?", job.GitServerID, job.RepoID).
		Updates(updates).Error; err != nil {
		logger.Warn("Git capability reconcile reschedule failed serverID=%s repoID=%d err=%v",
			job.GitServerID, job.RepoID, err)
	}
}

// gitCapabilityReconcileBackoff caps at eight intervals so an unhealthy binding
// still gets re-read roughly hourly on the default configuration, rather than
// disappearing from the schedule.
func gitCapabilityReconcileBackoff(interval time.Duration, failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures > 3 {
		failures = 3
	}
	return interval * time.Duration(1<<uint(failures))
}

func (p *GitCapabilityWorkerPool) processOne() error {
	if p == nil || p.DB == nil || p.Resolver == nil || p.SyncService == nil {
		return errors.New("Git capability worker is not configured")
	}

	job, err := p.claimOne()
	if err != nil {
		return err
	}

	timeout := p.JobTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cfg, resolveErr := p.Resolver.ResolveByServerID(ctx, job.GitServerID)
	var result *services.GitCapabilitySyncResult
	if resolveErr == nil {
		result, resolveErr = p.SyncService.SyncRepository(ctx, cfg, job.RepoID, job.RepoFullName, job.DefaultBranch, isDefaultGitBranchDeletion(job.AfterSHA), services.GitCapabilitySyncLease{
			JobID: job.ID,
			Token: job.LeaseToken,
		})
	}
	if resolveErr != nil {
		logger.Error("Git capability sync failed jobID=%s serverID=%s repoID=%d err=%v",
			job.ID, job.GitServerID, job.RepoID, resolveErr)
	}
	return p.finalizeJob(job, result, resolveErr)
}

func isDefaultGitBranchDeletion(afterSHA string) bool {
	return afterSHA == strings.Repeat("0", 40)
}

func (p *GitCapabilityWorkerPool) reclaimExpiredLeases() {
	lease := p.LeaseTimeout
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	cutoff := time.Now().Add(-lease)
	msg := "worker lease expired; retrying"
	now := time.Now()
	failed := p.DB.Model(&models.GitCapabilitySyncJob{}).
		Where("status = ? AND started_at < ? AND retry_count + 1 >= max_attempts",
			models.GitCapabilitySyncJobStatusRunning, cutoff).
		Updates(map[string]any{
			"status":      models.GitCapabilitySyncJobStatusFailed,
			"retry_count": gorm.Expr("retry_count + 1"),
			"finished_at": now,
			"last_error":  msg,
			"lease_token": "",
		})
	if failed.Error != nil {
		logger.Warn("Git capability expired lease failure update failed: %v", failed.Error)
		return
	}

	result := p.DB.Model(&models.GitCapabilitySyncJob{}).
		Where("status = ? AND started_at < ? AND retry_count + 1 < max_attempts",
			models.GitCapabilitySyncJobStatusRunning, cutoff).
		Updates(map[string]any{
			"status":       models.GitCapabilitySyncJobStatusPending,
			"retry_count":  gorm.Expr("retry_count + 1"),
			"started_at":   nil,
			"scheduled_at": now,
			"last_error":   msg,
			"lease_token":  "",
		})
	if result.Error != nil {
		logger.Warn("Git capability lease recovery failed: %v", result.Error)
	} else if result.RowsAffected > 0 || failed.RowsAffected > 0 {
		logger.Warn("Git capability lease recovery reclaimed %d job(s), failed %d exhausted job(s)",
			result.RowsAffected, failed.RowsAffected)
	}
}

func (p *GitCapabilityWorkerPool) claimOne() (*models.GitCapabilitySyncJob, error) {
	var job models.GitCapabilitySyncJob
	err := p.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		query := tx.
			Where(`git_capability_sync_jobs.status = ?
				AND git_capability_sync_jobs.scheduled_at <= ?
				AND NOT EXISTS (
					SELECT 1 FROM git_capability_sync_jobs running
					WHERE running.git_server_id = git_capability_sync_jobs.git_server_id
					  AND running.repo_id = git_capability_sync_jobs.repo_id
					  AND running.status = ?
				)`, models.GitCapabilitySyncJobStatusPending, now, models.GitCapabilitySyncJobStatusRunning).
			Order("git_capability_sync_jobs.scheduled_at ASC, git_capability_sync_jobs.created_at ASC")
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		query = query.First(&job)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrNoGitCapabilityJob
		}
		if query.Error != nil {
			return query.Error
		}

		startedAt := time.Now()
		leaseToken := uuid.NewString()
		updated := tx.Model(&models.GitCapabilitySyncJob{}).
			Where("id = ? AND status = ?", job.ID, models.GitCapabilitySyncJobStatusPending).
			Updates(map[string]any{
				"status":      models.GitCapabilitySyncJobStatusRunning,
				"started_at":  startedAt,
				"lease_token": leaseToken,
			})
		if updated.Error != nil {
			// The partial unique running-repo index closes the race between two
			// replicas that selected different pending jobs for the same repo.
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrNoGitCapabilityJob
		}
		job.Status = models.GitCapabilitySyncJobStatusRunning
		job.StartedAt = &startedAt
		job.LeaseToken = leaseToken
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (p *GitCapabilityWorkerPool) finalizeJob(job *models.GitCapabilitySyncJob, result *services.GitCapabilitySyncResult, syncErr error) error {
	now := time.Now()
	terminalFailure := false
	updates := map[string]any{"finished_at": now, "lease_token": ""}
	if syncErr == nil {
		updates["status"] = models.GitCapabilitySyncJobStatusSuccess
		updates["last_error"] = nil
		logger.Info("Git capability sync succeeded jobID=%s serverID=%s repoID=%d sha=%s created=%d updated=%d archived=%d",
			job.ID, job.GitServerID, job.RepoID, resultSHA(result), resultCreated(result), resultUpdated(result), resultArchived(result))
	} else if job.RetryCount+1 < job.MaxAttempts {
		backoff := time.Duration(math.Pow(10, float64(job.RetryCount+1))) * 3 * time.Second
		updates["status"] = models.GitCapabilitySyncJobStatusPending
		updates["retry_count"] = job.RetryCount + 1
		updates["scheduled_at"] = now.Add(backoff)
		updates["started_at"] = nil
		updates["finished_at"] = nil
		updates["last_error"] = syncErr.Error()
	} else {
		updates["status"] = models.GitCapabilitySyncJobStatusFailed
		updates["retry_count"] = job.RetryCount + 1
		updates["last_error"] = syncErr.Error()
		terminalFailure = true
	}
	updated := p.DB.Model(&models.GitCapabilitySyncJob{}).
		Where("id = ? AND status = ? AND lease_token = ?", job.ID, models.GitCapabilitySyncJobStatusRunning, job.LeaseToken).
		Updates(updates)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrGitCapabilityLeaseLost
	}
	// Only a terminal outcome moves the schedule. A job that is going to retry
	// has not answered "is this binding fresh?" yet, and pushing next_due_at out
	// for it would hide the staleness the retry is still trying to fix.
	if syncErr == nil || terminalFailure {
		p.rescheduleBinding(job, p.ReconcileInterval, terminalFailure)
	}
	return nil
}

func resultSHA(result *services.GitCapabilitySyncResult) string {
	if result == nil {
		return ""
	}
	return result.CommitSHA
}

func resultUpdated(result *services.GitCapabilitySyncResult) int {
	if result == nil {
		return 0
	}
	return result.Updated
}

func resultCreated(result *services.GitCapabilitySyncResult) int {
	if result == nil {
		return 0
	}
	return result.Created
}

func resultArchived(result *services.GitCapabilitySyncResult) int {
	if result == nil {
		return 0
	}
	return result.Archived
}
