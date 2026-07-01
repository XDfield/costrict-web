package adminimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/audit"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	defaultPollInterval  = 5 * time.Second
	defaultReaperTimeout = 30 * time.Minute
)

var errNoJob = errors.New("no import job")

// ImportRunner is the leader-elected background executor for capability import
// jobs. Only the leader replica runs it (main.go leader.Election), so execution
// is serialized: it claims one pending job at a time with FOR UPDATE SKIP LOCKED
// (mirrors internal/worker), runs the ingest, and finalizes with retry/terminal
// semantics. A reaper recovers jobs orphaned by a crashed/restarted leader.
type ImportRunner struct {
	db      *gorm.DB
	storage storage.Backend
	ingest  *services.CatalogIngestService

	PollInterval time.Duration
	ReaperAfter  time.Duration

	mu      sync.Mutex // guards running/stopCh/cancel against concurrent Start/Stop on leader flap
	running bool
	stopCh  chan struct{}
	cancel  context.CancelFunc // cancels the in-flight Ingest when leadership is lost
	wg      sync.WaitGroup
}

// NewImportRunner builds a runner with default poll/reaper timings.
func NewImportRunner(db *gorm.DB, backend storage.Backend, ingest *services.CatalogIngestService) *ImportRunner {
	return &ImportRunner{
		db:           db,
		storage:      backend,
		ingest:       ingest,
		PollInterval: defaultPollInterval,
		ReaperAfter:  defaultReaperTimeout,
	}
}

// Start begins the poll+reap loop. Intended for the leader.Election onStart
// callback; call Stop from onStop. Idempotent guard-free: the caller (leader
// election) guarantees a single Start/Stop pairing.
func (r *ImportRunner) Start() {
	r.mu.Lock()
	if r.running {
		// Guard against a double Start (e.g. rapid leader re-acquire) starting a
		// second loop that would double-claim jobs.
		r.mu.Unlock()
		return
	}
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	if r.ReaperAfter <= 0 {
		r.ReaperAfter = defaultReaperTimeout
	}
	r.running = true
	r.stopCh = make(chan struct{})
	stopCh := r.stopCh // hand the loop its own reference so Stop can nil the field safely
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(1)
	r.mu.Unlock()
	go r.loop(ctx, stopCh)
	logger.Info("import runner started (poll=%s reaper=%s)", r.PollInterval, r.ReaperAfter)
}

// Stop halts the loop and waits for the in-flight tick to finish. Safe to call
// when not running (no-op) and safe against a concurrent Start (leader flap).
func (r *ImportRunner) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stopCh)
	if r.cancel != nil {
		r.cancel() // interrupt an in-flight Ingest so we stop writing once leadership is lost
	}
	r.mu.Unlock()
	r.wg.Wait()
	logger.Info("import runner stopped")
}

func (r *ImportRunner) loop(ctx context.Context, stopCh chan struct{}) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.reap()
			r.expireStale()
			r.processOne(ctx)
		}
	}
}

// processOne claims a single pending job (FOR UPDATE SKIP LOCKED) and runs it.
// Claiming and execution are separate: the claim commits status=running before
// the (long) ingest runs, so a crash mid-ingest leaves a running row the reaper
// can recover rather than an unclaimed pending row two replicas could double-run.
func (r *ImportRunner) processOne(ctx context.Context) {
	var job models.CapabilityImportJob
	err := r.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Raw(`
			SELECT * FROM capability_import_jobs
			WHERE status = 'pending' AND scheduled_at <= NOW()
			ORDER BY scheduled_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		`).Scan(&job)
		if res.Error != nil {
			return res.Error
		}
		if job.ID == "" {
			return errNoJob
		}
		now := time.Now()
		return tx.Model(&models.CapabilityImportJob{}).Where("id = ?", job.ID).Updates(map[string]any{
			"status":     statusRunning,
			"started_at": now,
		}).Error
	})
	if errors.Is(err, errNoJob) || err != nil {
		return
	}
	r.runJob(ctx, &job)
}

func (r *ImportRunner) runJob(ctx context.Context, job *models.CapabilityImportJob) {
	src, cleanup, err := r.materialize(ctx, job)
	if err != nil {
		r.finalize(job, nil, fmt.Errorf("materialize bundle: %w", err))
		return
	}
	defer cleanup()

	result, ingestErr := r.ingest.Ingest(ctx, src, services.IngestOptions{
		DryRun:  job.DryRun,
		Reparse: job.Reparse,
		// NOT job.TriggerUser: imported catalog items keep "system"/public
		// ownership so they don't become the importing admin's "my uploads".
		// The operator is still recorded on the job row + audit log.
		TriggerUser: catalogImportTriggerUser,
	})
	r.finalize(job, result, ingestErr)
}

// materialize turns the job's source into an IngestSource. URL sources are
// handed to the ingest service directly (it downloads); uploaded sources are
// pulled from storage into a temp file (cleaned up by the returned func).
func (r *ImportRunner) materialize(ctx context.Context, job *models.CapabilityImportJob) (services.IngestSource, func(), error) {
	noop := func() {}
	switch job.SourceKind {
	case sourceKindURL:
		return services.IngestSource{URL: job.SourceURL}, noop, nil
	case sourceKindUpload:
		rc, _, err := r.storage.Get(ctx, job.StorageKey)
		if err != nil {
			return services.IngestSource{}, noop, err
		}
		defer rc.Close()
		tmp, err := os.CreateTemp("", "admin-import-*.tar.gz")
		if err != nil {
			return services.IngestSource{}, noop, err
		}
		if _, err := io.Copy(tmp, rc); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return services.IngestSource{}, noop, err
		}
		tmp.Close()
		return services.IngestSource{Tarball: tmp.Name()}, func() { os.Remove(tmp.Name()) }, nil
	default:
		return services.IngestSource{}, noop, fmt.Errorf("unknown source kind %q", job.SourceKind)
	}
}

// finalize records the terminal (or retry) state for a job after execution.
//
//   - Ingest-side error (infra: download/materialize/ingest crashed):
//     dry-run is an idempotent read → retry with backoff; real import is
//     non-idempotent (may have partially written) → fail terminally and flag for
//     human review, never auto-retry.
//   - Ingest success: the result may still carry data-quality Failed/Incomplete
//     counts — that is surfaced to the admin, not treated as a job failure.
func (r *ImportRunner) finalize(job *models.CapabilityImportJob, result *services.IngestResult, err error) {
	now := time.Now()
	updates := map[string]any{"finished_at": now}

	if err != nil {
		if job.DryRun && job.RetryCount+1 < job.MaxAttempts {
			backoff := time.Duration(math.Pow(10, float64(job.RetryCount+1))) * 3 * time.Second
			updates["status"] = statusPending
			updates["retry_count"] = job.RetryCount + 1
			updates["scheduled_at"] = now.Add(backoff)
			updates["started_at"] = nil
			updates["finished_at"] = nil // requeued, not finished — don't leave a stale finished_at
			updates["error_message"] = err.Error()
			logger.Warn("import dry-run will retry jobID=%s attempt=%d/%d err=%v", job.ID, job.RetryCount+1, job.MaxAttempts, err)
			r.db.Model(&models.CapabilityImportJob{}).Where("id = ?", job.ID).Updates(updates)
			return
		}
		updates["status"] = statusFailed
		if job.DryRun {
			updates["error_message"] = err.Error()
		} else {
			updates["error_message"] = "疑似进程中断或导入错误，请人工核对 DB 后重新发起：" + err.Error()
		}
		r.db.Model(&models.CapabilityImportJob{}).Where("id = ?", job.ID).Updates(updates)
		r.cleanupBundle(job)
		if !job.DryRun {
			audit.Record(job.TriggerUser, audit.ActionBundleImport, audit.TargetImportJob, job.ID, map[string]any{
				"status": "failed",
				"error":  err.Error(),
			})
		}
		logger.Error("import job failed jobID=%s dryRun=%v err=%v", job.ID, job.DryRun, err)
		return
	}

	imp := fromIngestResult(result)
	raw, _ := json.Marshal(imp)
	updates["result"] = datatypes.JSON(raw)

	if job.DryRun {
		updates["status"] = statusPreviewed
		r.db.Model(&models.CapabilityImportJob{}).Where("id = ?", job.ID).Updates(updates)
		logger.Info("import dry-run previewed jobID=%s added=%d updated=%d deleted=%d failed=%d", job.ID, imp.Added, imp.Updated, imp.Deleted, imp.Failed)
		return
	}

	// Real import terminal status. A nil Ingest error still isn't necessarily a
	// clean import — two cases must NOT be reported as success:
	//   - imp.Failed > 0: per-entry writes/archives errored (Ingest returns nil
	//     but the import partially failed; a dry-run can be clean because it
	//     skips those DB writes).
	//   - manifest sha drift: a URL job's confirmed import re-downloads, and the
	//     content may differ from what dry-run previewed/gated. job.Result here
	//     still holds the dry-run result, so compare the sha the real import saw
	//     against the previewed one.
	statusFinal := statusSuccess
	var prev ImportResult
	_ = json.Unmarshal(job.Result, &prev)
	switch {
	case imp.Failed > 0:
		statusFinal = statusFailed
		updates["error_message"] = fmt.Sprintf("导入完成但有 %d 个条目写入失败，详见错误日志", imp.Failed)
	case prev.ManifestSHA256 != "" && imp.ManifestSHA256 != "" && imp.ManifestSHA256 != prev.ManifestSHA256:
		statusFinal = statusFailed
		updates["error_message"] = fmt.Sprintf("包内容在预览后发生变化（预览 sha=%s → 导入 sha=%s），未经预览门禁校验，请人工核对", prev.ManifestSHA256, imp.ManifestSHA256)
	}
	updates["status"] = statusFinal
	r.db.Model(&models.CapabilityImportJob{}).Where("id = ?", job.ID).Updates(updates)
	r.cleanupBundle(job)
	audit.Record(job.TriggerUser, audit.ActionBundleImport, audit.TargetImportJob, job.ID, map[string]any{
		"status":     statusFinal,
		"filename":   job.Filename,
		"sourceKind": job.SourceKind,
		"added":      imp.Added,
		"updated":    imp.Updated,
		"deleted":    imp.Deleted,
		"failed":     imp.Failed,
	})
	logger.Info("import finished jobID=%s status=%s added=%d updated=%d deleted=%d failed=%d", job.ID, statusFinal, imp.Added, imp.Updated, imp.Deleted, imp.Failed)
}

// cleanupBundle removes an uploaded bundle's stored object once the job is done.
func (r *ImportRunner) cleanupBundle(job *models.CapabilityImportJob) {
	if job.SourceKind == sourceKindUpload && job.StorageKey != "" {
		_ = r.storage.Delete(context.Background(), job.StorageKey)
	}
}

// reap recovers jobs stuck in running past ReaperAfter (leader crash/restart mid
// execution). Dry-run (idempotent read) is requeued; real import (non-idempotent)
// is failed and flagged for human review — never auto-retried.
func (r *ImportRunner) reap() {
	cutoff := time.Now().Add(-r.ReaperAfter)
	var stale []models.CapabilityImportJob
	if err := r.db.Where("status = ? AND started_at IS NOT NULL AND started_at < ?", statusRunning, cutoff).Find(&stale).Error; err != nil {
		return
	}
	for i := range stale {
		job := &stale[i]
		if job.DryRun {
			// Bump retry_count so a job that keeps getting reaped (e.g. an ingest
			// that legitimately runs longer than ReaperAfter and keeps being
			// requeued) eventually gives up instead of looping / double-running
			// forever.
			if job.RetryCount+1 >= job.MaxAttempts {
				r.db.Model(&models.CapabilityImportJob{}).Where("id = ? AND status = ?", job.ID, statusRunning).Updates(map[string]any{
					"status":        statusFailed,
					"finished_at":   time.Now(),
					"error_message": "dry-run 反复中断/超时，超过重试上限",
				})
				logger.Warn("reaped stuck dry-run import job=%s → failed (max attempts)", job.ID)
			} else {
				r.db.Model(&models.CapabilityImportJob{}).Where("id = ? AND status = ?", job.ID, statusRunning).Updates(map[string]any{
					"status":        statusPending,
					"started_at":    nil,
					"scheduled_at":  time.Now(),
					"retry_count":   job.RetryCount + 1,
					"error_message": "dry-run 疑似中断，已自动重排",
				})
				logger.Warn("reaped stuck dry-run import job=%s → pending (retry %d/%d)", job.ID, job.RetryCount+1, job.MaxAttempts)
			}
		} else {
			r.db.Model(&models.CapabilityImportJob{}).Where("id = ? AND status = ?", job.ID, statusRunning).Updates(map[string]any{
				"status":        statusFailed,
				"finished_at":   time.Now(),
				"error_message": "疑似进程中断，请人工核对 DB 后重新发起",
			})
			r.cleanupBundle(job)
			logger.Warn("reaped stuck real import job=%s → failed", job.ID)
		}
	}
}

// expireStale transitions previewed jobs past their TTL to expired and clears
// their stored bundle (mirrors Service.maybeExpire but proactively under leader).
func (r *ImportRunner) expireStale() {
	cutoff := time.Now().Add(-previewTTL)
	var stale []models.CapabilityImportJob
	if err := r.db.Where("status = ? AND updated_at < ?", statusPreviewed, cutoff).Find(&stale).Error; err != nil {
		return
	}
	for i := range stale {
		job := &stale[i]
		// CAS the status FIRST; only delete the bundle if this call actually won
		// the previewed→expired transition. A concurrent ConfirmJob may have
		// already flipped previewed→pending (bundle still needed for the real
		// import) — in that case the guarded update affects 0 rows and we must
		// NOT delete the bundle.
		res := r.db.Model(&models.CapabilityImportJob{}).Where("id = ? AND status = ?", job.ID, statusPreviewed).Update("status", statusExpired)
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		r.cleanupBundle(job)
	}
}
