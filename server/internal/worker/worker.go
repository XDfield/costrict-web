package worker

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/gorm"
)

var ErrNoJob = errors.New("no job available")

type WorkerPool struct {
	DB           *gorm.DB
	SyncService  *services.SyncService
	Concurrency  int
	PollInterval time.Duration
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

func (p *WorkerPool) Start() {
	if p.Concurrency <= 0 {
		p.Concurrency = 3
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

func (p *WorkerPool) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}

func (p *WorkerPool) runWorker() {
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

func (p *WorkerPool) processOne() {
	var job models.SyncJob

	err := p.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Raw(`
			SELECT * FROM sync_jobs
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

	if errors.Is(err, ErrNoJob) || err == nil && job.ID == "" {
		return
	}
	if err != nil {
		return
	}

	var payload struct {
		DryRun bool `json:"dryRun"`
	}
	_ = json.Unmarshal(job.Payload, &payload)

	opts := services.SyncOptions{
		TriggerType: job.TriggerType,
		TriggerUser: job.TriggerUser,
		DryRun:      payload.DryRun,
	}

	result, syncErr := p.SyncService.SyncRegistry(context.Background(), job.RegistryID, opts)
	p.finalizeJob(&job, result, syncErr)
}

func (p *WorkerPool) finalizeJob(job *models.SyncJob, result *services.SyncResult, err error) {
	updates := map[string]any{"finished_at": time.Now()}

	if err != nil && job.RetryCount+1 < job.MaxAttempts {
		backoff := time.Duration(math.Pow(10, float64(job.RetryCount+1))) * 3 * time.Second
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
			updates["sync_log_id"] = result.LogID
		}
	}

	p.DB.Model(job).Updates(updates)
}
