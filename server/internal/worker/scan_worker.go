package worker

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/gorm"
)

type ScanWorkerPool struct {
	DB           *gorm.DB
	ScanService  *services.ScanService
	Concurrency  int
	PollInterval time.Duration
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

func (p *ScanWorkerPool) Start() {
	if p.Concurrency <= 0 {
		p.Concurrency = 2
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 3 * time.Second
	}
	p.stopCh = make(chan struct{})

	for i := 0; i < p.Concurrency; i++ {
		p.wg.Add(1)
		go p.runWorker()
	}
}

func (p *ScanWorkerPool) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}

func (p *ScanWorkerPool) runWorker() {
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

func (p *ScanWorkerPool) processOne() {
	var job models.ScanJob

	err := p.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Raw(`
			SELECT * FROM scan_jobs
			WHERE status = 'pending'
			  AND scheduled_at <= NOW()
			ORDER BY priority ASC, scheduled_at ASC
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
		}).Error
	})

	if errors.Is(err, ErrNoJob) || (err == nil && job.ID == "") {
		return
	}
	if err != nil {
		return
	}

	scanResult, scanErr := p.ScanService.ScanItem(
		context.Background(),
		job.ItemID,
		job.ItemRevision,
		job.TriggerType,
	)
	p.finalizeJob(&job, scanResult, scanErr)
}

func (p *ScanWorkerPool) finalizeJob(job *models.ScanJob, result *models.SecurityScan, err error) {
	updates := map[string]any{"finished_at": time.Now()}

	if err != nil && job.RetryCount+1 < job.MaxAttempts {
		backoff := time.Duration(math.Pow(2, float64(job.RetryCount+1))) * time.Minute
		updates["status"] = "pending"
		updates["retry_count"] = job.RetryCount + 1
		updates["scheduled_at"] = time.Now().Add(backoff)
		updates["last_error"] = err.Error()
	} else if err != nil {
		updates["status"] = "failed"
		updates["last_error"] = err.Error()
	} else {
		updates["status"] = "success"
		if result != nil {
			updates["scan_result_id"] = result.ID
		}
	}

	p.DB.Model(job).Updates(updates)
}
