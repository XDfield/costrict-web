package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/gorm"
)

// BundleWorkerPool drains the bundle_jobs queue: it picks up a pending job with
// FOR UPDATE SKIP LOCKED, lazily clones the item's upstream source and packs a
// lossless ZIP via BundlePackService, then finalizes the job (success or backoff
// retry). It mirrors ScanWorkerPool — the per-item background-task template — so
// the queue semantics stay identical across the two pipelines.
//
// Hardening for production:
//   - JobTimeout bounds each pack (clone + zip). The context is propagated through
//     BundlePackService -> go-git PlainCloneContext, so a hung upstream aborts the
//     job instead of pinning a worker goroutine forever.
//   - processOne recovers from panics and finalizes the job as failed, so one bad
//     plugin cannot take down a worker goroutine.
//   - reclaimStaleRunning resets jobs stuck in 'running' past the lease TTL back to
//     'pending'. A crash mid-pack would otherwise leave a 'running' row that the
//     partial unique index idx_bundle_jobs_active_item keeps blocking every future
//     enqueue for that item.
type BundleWorkerPool struct {
	DB          *gorm.DB
	PackService *services.BundlePackService
	Concurrency int
	// PollInterval is the per-worker dequeue tick.
	PollInterval time.Duration
	// JobTimeout bounds a single pack (clone + zip). Zero falls back to a sane
	// default in Start.
	JobTimeout time.Duration
	// StaleReclaimInterval is how often a janitor goroutine sweeps stuck 'running'
	// jobs. Zero falls back to a default in Start.
	StaleReclaimInterval time.Duration
	stopCh               chan struct{}
	wg                   sync.WaitGroup
}

func (p *BundleWorkerPool) Start() {
	if p.Concurrency <= 0 {
		p.Concurrency = 2
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 5 * time.Second
	}
	if p.JobTimeout <= 0 {
		p.JobTimeout = 5 * time.Minute
	}
	if p.StaleReclaimInterval <= 0 {
		p.StaleReclaimInterval = p.staleLeaseTTL()
	}
	p.stopCh = make(chan struct{})

	// Reclaim any 'running' rows left over from a previous crash before workers
	// start, so a wedged item is unblocked immediately on boot.
	if n := p.reclaimStaleRunning(); n > 0 {
		logger.Warn("bundle worker reclaimed %d stale running job(s) on startup", n)
	}

	for i := 0; i < p.Concurrency; i++ {
		p.wg.Add(1)
		go p.runWorker()
	}

	p.wg.Add(1)
	go p.runStaleReclaimer()
}

func (p *BundleWorkerPool) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}

func (p *BundleWorkerPool) runWorker() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.processOne()
		}
	}
}

// runStaleReclaimer periodically resets jobs stuck in 'running' beyond the lease
// TTL back to 'pending' (or failed when attempts are exhausted), unblocking the
// per-item partial unique index after a worker crash.
func (p *BundleWorkerPool) runStaleReclaimer() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.StaleReclaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if n := p.reclaimStaleRunning(); n > 0 {
				logger.Warn("bundle worker reclaimed %d stale running job(s)", n)
			}
		}
	}
}

// staleLeaseTTL is how long a 'running' job may live before it is presumed dead.
// It is 2x the job timeout so a job that legitimately runs to the timeout (and is
// finalized) is never mistaken for stale.
func (p *BundleWorkerPool) staleLeaseTTL() time.Duration {
	if p.JobTimeout > 0 {
		return 2 * p.JobTimeout
	}
	return 10 * time.Minute
}

// reclaimStaleRunning resets 'running' jobs whose started_at is older than the
// lease TTL: those with retries left go back to 'pending' (scheduled now) so they
// re-run; those out of attempts are marked 'failed'. Returns the number reclaimed.
func (p *BundleWorkerPool) reclaimStaleRunning() int {
	cutoff := time.Now().Add(-p.staleLeaseTTL())

	var stale []models.BundleJob
	if err := p.DB.Where("status = ? AND started_at IS NOT NULL AND started_at < ?", "running", cutoff).
		Find(&stale).Error; err != nil {
		logger.Error("bundle worker stale reclaim query failed err=%v", err)
		return 0
	}

	reclaimed := 0
	now := time.Now()
	for i := range stale {
		job := &stale[i]
		updates := map[string]any{"updated_at": now, "last_error": "reclaimed: worker lease expired (stale running)"}
		if job.RetryCount+1 < job.MaxAttempts {
			updates["status"] = "pending"
			updates["retry_count"] = job.RetryCount + 1
			updates["scheduled_at"] = now
			updates["started_at"] = nil
		} else {
			updates["status"] = "failed"
			updates["finished_at"] = now
		}
		// Scope the write to the still-'running' row so we never clobber a job a
		// worker just finalized between our SELECT and UPDATE.
		res := p.DB.Model(&models.BundleJob{}).
			Where("id = ? AND status = ?", job.ID, "running").
			Updates(updates)
		if res.Error != nil {
			logger.Error("bundle worker stale reclaim update failed jobID=%s err=%v", job.ID, res.Error)
			continue
		}
		reclaimed += int(res.RowsAffected)
	}
	return reclaimed
}

func (p *BundleWorkerPool) processOne() {
	var job models.BundleJob

	err := p.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Raw(`
			SELECT * FROM bundle_jobs
			WHERE status = 'pending'
			  AND scheduled_at <= NOW()
			ORDER BY scheduled_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		`).Scan(&job)

		if result.Error != nil {
			return result.Error
		}
		if job.ID == "" {
			return ErrNoJob
		}

		now := time.Now()
		return tx.Model(&job).Updates(map[string]any{
			"status":     "running",
			"started_at": now,
			"updated_at": now,
		}).Error
	})

	if errors.Is(err, ErrNoJob) || (err == nil && job.ID == "") {
		return
	}
	if err != nil {
		return
	}

	p.runJob(&job)
}

// runJob loads the item for an already-claimed ('running') job, packs its bundle
// under a timeout, and finalizes it. It is split out from processOne so the
// claim-from-queue step (Postgres-only FOR UPDATE SKIP LOCKED) and the pack step
// can be tested independently.
//
// It recovers from any panic in the pack so one bad plugin cannot take down the
// worker goroutine: on panic the job is finalized as failed (last_error recorded)
// rather than left stuck 'running' to wedge the per-item active-job index.
func (p *BundleWorkerPool) runJob(job *models.BundleJob) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("bundle job panicked jobID=%s itemID=%s panic=%v", job.ID, job.ItemID, r)
			// Force a terminal failure on panic (a panicking pack is not a retryable
			// transient): max out attempts so finalizeJob marks it failed.
			job.RetryCount = job.MaxAttempts
			p.finalizeJob(job, nil, fmt.Errorf("bundle pack panicked: %v", r))
		}
	}()

	var item models.CapabilityItem
	if loadErr := p.DB.Preload("Registry").First(&item, "id = ?", job.ItemID).Error; loadErr != nil {
		logger.Error("bundle job item load failed jobID=%s itemID=%s err=%v", job.ID, job.ItemID, loadErr)
		p.finalizeJob(job, nil, loadErr)
		return
	}

	ctx := context.Background()
	if p.JobTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.JobTimeout)
		defer cancel()
	}

	artifact, packErr := p.PackService.PackItemBundle(ctx, &item)
	if packErr != nil {
		logger.Error("bundle job pack failed jobID=%s itemID=%s err=%v", job.ID, job.ItemID, packErr)
	}
	p.finalizeJob(job, artifact, packErr)
}

func (p *BundleWorkerPool) finalizeJob(job *models.BundleJob, artifact *models.CapabilityArtifact, err error) {
	now := time.Now()
	updates := map[string]any{"finished_at": now, "updated_at": now}

	if err != nil && job.RetryCount+1 < job.MaxAttempts {
		backoff := time.Duration(math.Pow(2, float64(job.RetryCount+1))) * time.Minute
		updates["status"] = "pending"
		updates["retry_count"] = job.RetryCount + 1
		updates["scheduled_at"] = now.Add(backoff)
		updates["last_error"] = err.Error()
		// Clear finished_at since the job is going back to pending.
		updates["finished_at"] = nil
		logger.Warn("bundle job will retry jobID=%s itemID=%s attempt=%d/%d backoff=%s err=%v",
			job.ID, job.ItemID, job.RetryCount+1, job.MaxAttempts, backoff, err)
	} else if err != nil {
		updates["status"] = "failed"
		updates["last_error"] = err.Error()
		logger.Error("bundle job permanently failed jobID=%s itemID=%s err=%v", job.ID, job.ItemID, err)
	} else {
		updates["status"] = "success"
		if artifact != nil {
			updates["artifact_id"] = artifact.ID
			logger.Info("bundle job succeeded jobID=%s itemID=%s artifactID=%s version=%s",
				job.ID, job.ItemID, artifact.ID, artifact.ArtifactVersion)
		}
	}

	p.DB.Model(job).Updates(updates)
}
