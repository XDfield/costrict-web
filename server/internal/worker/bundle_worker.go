package worker

import (
	"context"
	"errors"
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
type BundleWorkerPool struct {
	DB           *gorm.DB
	PackService  *services.BundlePackService
	Concurrency  int
	PollInterval time.Duration
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

func (p *BundleWorkerPool) Start() {
	if p.Concurrency <= 0 {
		p.Concurrency = 2
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 5 * time.Second
	}
	p.stopCh = make(chan struct{})

	for i := 0; i < p.Concurrency; i++ {
		p.wg.Add(1)
		go p.runWorker()
	}
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

	var item models.CapabilityItem
	if loadErr := p.DB.Preload("Registry").First(&item, "id = ?", job.ItemID).Error; loadErr != nil {
		logger.Error("bundle job item load failed jobID=%s itemID=%s err=%v", job.ID, job.ItemID, loadErr)
		p.finalizeJob(&job, nil, loadErr)
		return
	}

	artifact, packErr := p.PackService.PackItemBundle(context.Background(), &item)
	if packErr != nil {
		logger.Error("bundle job pack failed jobID=%s itemID=%s err=%v", job.ID, job.ItemID, packErr)
	}
	p.finalizeJob(&job, artifact, packErr)
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
