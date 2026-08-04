package worker

import (
	"context"
	"errors"
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
	stopCh       chan struct{}
	wg           sync.WaitGroup
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
			_ = p.processOne()
		}
	}
}

func (p *GitCapabilityWorkerPool) processOne() error {
	if p == nil || p.DB == nil || p.Resolver == nil || p.SyncService == nil {
		return errors.New("Git capability worker is not configured")
	}
	p.reclaimExpiredLeases()

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
	updates := map[string]any{"finished_at": now, "lease_token": ""}
	if syncErr == nil {
		updates["status"] = models.GitCapabilitySyncJobStatusSuccess
		updates["last_error"] = nil
		logger.Info("Git capability sync succeeded jobID=%s serverID=%s repoID=%d sha=%s updated=%d archived=%d",
			job.ID, job.GitServerID, job.RepoID, resultSHA(result), resultUpdated(result), resultArchived(result))
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

func resultArchived(result *services.GitCapabilitySyncResult) int {
	if result == nil {
		return 0
	}
	return result.Archived
}
