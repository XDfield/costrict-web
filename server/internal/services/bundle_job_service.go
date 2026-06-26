package services

import (
	"errors"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrBundleJobAlreadyQueued is returned when a manual enqueue races an in-flight
// bundle job for the same item. The sync/ingest callers treat a duplicate as a
// no-op (nil, nil) rather than an error, mirroring ScanJobService.Enqueue.
var ErrBundleJobAlreadyQueued = errors.New("a bundle job is already pending or running for this item")

// BundleJobService manages the async lazy clone-and-pack queue (bundle_jobs).
// It only writes rows; the actual clone+pack runs in the worker process
// (internal/worker.BundleWorkerPool), so the API process can enqueue without
// pulling the git/storage machinery into its address space.
type BundleJobService struct {
	DB *gorm.DB
}

// BundleEnqueueOptions controls per-call enqueue behavior.
type BundleEnqueueOptions struct {
	TriggerType string // sync | manual | subscribe; defaults to "sync"
	TriggerUser string
	MaxAttempts int
	ScheduledAt time.Time
}

// Enqueue inserts a pending bundle job for the given item, de-duplicating against
// any in-flight (pending|running) job for the same item.
//
// Return contract (mirrors ScanJobService.Enqueue):
//   - (job, nil)  — a new job was queued.
//   - (nil, nil)  — skipped because an in-flight job already exists (and the
//     trigger is a non-interactive sync/subscribe), so the caller should treat
//     it as a benign no-op.
//   - (nil, ErrBundleJobAlreadyQueued) — a *manual* enqueue lost the de-dup race.
//
// The pre-count below is a best-effort fast path; the partial unique index
// idx_bundle_jobs_active_item is the authoritative guard and turns a concurrent
// double-insert into a unique-violation, which is folded back into the same
// no-op / already-queued contract.
func (s *BundleJobService) Enqueue(itemID string, opts BundleEnqueueOptions) (*models.BundleJob, error) {
	if itemID == "" {
		return nil, errors.New("bundle enqueue: empty itemID")
	}

	triggerType := opts.TriggerType
	if triggerType == "" {
		triggerType = "sync"
	}

	var count int64
	s.DB.Model(&models.BundleJob{}).
		Where("item_id = ? AND status IN ('pending', 'running')", itemID).
		Count(&count)
	if count > 0 {
		return nil, s.dedupResult(triggerType)
	}

	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 3
	}
	if opts.ScheduledAt.IsZero() {
		opts.ScheduledAt = time.Now()
	}

	job := &models.BundleJob{
		ID:          uuid.New().String(),
		ItemID:      itemID,
		TriggerType: triggerType,
		TriggerUser: opts.TriggerUser,
		Status:      "pending",
		MaxAttempts: opts.MaxAttempts,
		ScheduledAt: opts.ScheduledAt,
	}

	if err := s.DB.Create(job).Error; err != nil {
		// A concurrent enqueue beat us to the partial unique index.
		if isUniqueViolationErr(err) {
			return nil, s.dedupResult(triggerType)
		}
		return nil, err
	}
	return job, nil
}

// dedupResult maps a "job already in flight" situation onto the Enqueue contract:
// silent no-op for non-interactive triggers, explicit error for manual ones.
func (s *BundleJobService) dedupResult(triggerType string) error {
	if strings.EqualFold(triggerType, "manual") {
		return ErrBundleJobAlreadyQueued
	}
	return nil
}

// GetActiveCount reports how many in-flight (pending|running) bundle jobs exist
// for an item. Used by the bundle handler to surface a "still packing" status.
func (s *BundleJobService) GetActiveCount(itemID string) (int64, error) {
	var count int64
	err := s.DB.Model(&models.BundleJob{}).
		Where("item_id = ? AND status IN ('pending', 'running')", itemID).
		Count(&count).Error
	return count, err
}

// LatestJob returns the most recent bundle job (by created_at) for an item, or
// (nil, nil) when none exists. The handler uses it to decide whether a fresh
// enqueue is warranted: a *permanently failed* job inside the cooldown window
// must NOT be re-enqueued (otherwise every client request spawns a new doomed
// clone and the client 202-polls forever), so the handler returns a terminal
// failure response instead.
func (s *BundleJobService) LatestJob(itemID string) (*models.BundleJob, error) {
	var job models.BundleJob
	err := s.DB.Model(&models.BundleJob{}).
		Where("item_id = ?", itemID).
		Order("created_at DESC").
		First(&job).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// FailedInCooldown reports whether the item's most recent job is a permanent
// failure whose finished_at is within the cooldown window (so the handler should
// return a terminal failure response rather than enqueue another doomed clone).
// It also returns that job so the caller can surface the recorded last_error.
//
// A failed job whose finished_at is older than the cooldown — or a missing
// finished_at — is treated as eligible for retry (returns false), so a transient
// failure is automatically retried once enough time has passed.
func (s *BundleJobService) FailedInCooldown(itemID string, cooldown time.Duration) (bool, *models.BundleJob, error) {
	job, err := s.LatestJob(itemID)
	if err != nil {
		return false, nil, err
	}
	if job == nil || job.Status != "failed" {
		return false, job, nil
	}
	if job.FinishedAt == nil {
		// No finish timestamp recorded; allow a retry rather than wedge the item.
		return false, job, nil
	}
	if time.Since(*job.FinishedAt) < cooldown {
		return true, job, nil
	}
	return false, job, nil
}
