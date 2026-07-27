// Package adminimport exposes the platform-admin catalog-bundle import surface:
// upload/URL a bundle, dry-run preview, confirm, poll, history, and per-type
// inventory stats. It moves the previously manual `migrate ingest-upstream`
// ops flow (SSH + kubectl cp + CLI) into the admin console while reusing the
// exact same services.CatalogIngestService — so slug-collision protection and
// CatalogEntryDir matching keep user-uploaded items untouched.
//
// Execution is asynchronous via a DB queue drained by a leader-elected runner
// (runner.go), NOT in the HTTP request goroutine, so a pod restart recovers
// in-flight work and concurrent confirms are serialized. The HTTP handlers live
// in handlers.go; the platform-admin guard is applied by the caller (main.go
// mounts RegisterRoutes onto an already-guarded /admin group).
package adminimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// MaxCatalogBundleUploadSize bounds an uploaded catalog bundle. It is
	// deliberately distinct from services.MaxArchiveUploadSize (50MB, a single
	// skill/mcp/plugin zip): a catalog bundle is the multi-entry tar.gz (~33MB
	// today and growing).
	MaxCatalogBundleUploadSize = 200 << 20

	// deleteWarnRatio: when a dry-run would archive more than this fraction of the
	// current active inventory, confirm requires an explicit confirmLargeDelete.
	deleteWarnRatio = 0.2

	// previewTTL: a previewed job left unconfirmed this long is expired and its
	// stored bundle cleaned up.
	previewTTL = 2 * time.Hour

	statusPending   = "pending"
	statusRunning   = "running"
	statusPreviewed = "previewed"
	statusSuccess   = "success"
	statusFailed    = "failed"
	statusExpired   = "expired"

	sourceKindURL    = "url"
	sourceKindUpload = "upload"

	// catalogImportTriggerUser is recorded as created_by/updated_by on imported
	// catalog items. Deliberately "system" (matching the CLI ingest-upstream),
	// NOT the operating admin — catalog content must stay public/unowned rather
	// than showing up as the admin's "my uploads". Who triggered the import is
	// still auditable via CapabilityImportJob.TriggerUser + audit.Record.
	catalogImportTriggerUser = "system"
)

var (
	ErrEmptySource            = errors.New("source is empty")
	ErrInvalidURL             = errors.New("source url must be http(s)")
	ErrJobNotFound            = errors.New("import job not found")
	ErrNotPreviewed           = errors.New("import job is not awaiting confirmation")
	ErrPreviewHasFailures     = errors.New("dry-run reported failed entries; cannot confirm")
	ErrLargeDeleteUnconfirmed = errors.New("delete count exceeds threshold; confirmLargeDelete required")
)

// ImportResult is the camelCase, frontend-facing projection of
// services.IngestResult (which carries no json tags). It is what gets marshalled
// into CapabilityImportJob.Result and read back by confirm/errors-log.
type ImportResult struct {
	BundleEntries    int      `json:"bundleEntries"`
	Added            int      `json:"added"`
	Updated          int      `json:"updated"`
	MetadataUpdated  int      `json:"metadataUpdated"`
	Skipped          int      `json:"skipped"`
	Deleted          int      `json:"deleted"`
	Failed           int      `json:"failed"`
	Incomplete       int      `json:"incomplete"`
	Errors           []string `json:"errors"`
	IncompleteErrors []string `json:"incompleteErrors"`
	ManifestSHA256   string   `json:"manifestSha256"`
	GeneratedAt      string   `json:"generatedAt"`
	DurationMs       int64    `json:"durationMs"`
}

func fromIngestResult(r *services.IngestResult) ImportResult {
	if r == nil {
		return ImportResult{}
	}
	return ImportResult{
		BundleEntries:    r.BundleEntries,
		Added:            r.Added,
		Updated:          r.Updated,
		MetadataUpdated:  r.MetadataUpdated,
		Skipped:          r.Skipped,
		Deleted:          r.Deleted,
		Failed:           r.Failed,
		Incomplete:       r.Incomplete,
		Errors:           r.Errors,
		IncompleteErrors: r.IncompleteErrors,
		ManifestSHA256:   r.ManifestSHA256,
		GeneratedAt:      r.GeneratedAt,
		DurationMs:       r.Duration.Milliseconds(),
	}
}

// TypeCount is one row of the per-item-type inventory stats.
type TypeCount struct {
	ItemType string `json:"itemType"`
	Count    int64  `json:"count"`
}

// Service owns the import-job data logic against a shared DB handle + storage.
type Service struct {
	db      *gorm.DB
	storage storage.Backend
}

// NewService constructs the service around an existing DB handle and storage backend.
func NewService(db *gorm.DB, backend storage.Backend) *Service {
	return &Service{db: db, storage: backend}
}

// CreateURLJob records a pending dry-run job for a URL bundle source.
func (s *Service) CreateURLJob(rawURL string, reparse bool, user string) (*models.CapabilityImportJob, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, ErrEmptySource
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil, ErrInvalidURL
	}
	job := &models.CapabilityImportJob{
		ID:          uuid.New().String(),
		SourceKind:  sourceKindURL,
		SourceURL:   rawURL,
		Filename:    urlTail(rawURL),
		Status:      statusPending,
		DryRun:      true,
		Reparse:     reparse,
		TriggerUser: user,
		MaxAttempts: 3,
		ScheduledAt: time.Now(),
	}
	if err := s.db.Create(job).Error; err != nil {
		return nil, err
	}
	return job, nil
}

// CreateUploadJob stores the uploaded bundle then records a pending dry-run job.
// A DB failure can leave an unreferenced object; physical garbage collection is
// intentionally outside the minimal Put/Get storage contract.
func (s *Service) CreateUploadJob(ctx context.Context, reader io.Reader, size int64, filename string, reparse bool, user string) (*models.CapabilityImportJob, error) {
	jobID := uuid.New().String()
	key := fmt.Sprintf("import-jobs/%s/bundle.tar.gz", jobID)
	if err := s.storage.Put(ctx, key, reader, size); err != nil {
		return nil, fmt.Errorf("store uploaded bundle: %w", err)
	}
	job := &models.CapabilityImportJob{
		ID:             jobID,
		SourceKind:     sourceKindUpload,
		Filename:       filename,
		StorageBackend: storage.KindOf(s.storage),
		StorageKey:     key,
		FileSize:       size,
		Status:         statusPending,
		DryRun:         true,
		Reparse:        reparse,
		TriggerUser:    user,
		MaxAttempts:    3,
		ScheduledAt:    time.Now(),
	}
	if err := s.db.Create(job).Error; err != nil {
		return nil, err
	}
	return job, nil
}

// GetJob loads a job by id, lazily expiring a stale previewed job on read.
func (s *Service) GetJob(id string) (*models.CapabilityImportJob, error) {
	var job models.CapabilityImportJob
	if err := s.db.First(&job, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	s.maybeExpire(&job)
	return &job, nil
}

// maybeExpire lazily transitions a previewed job to expired once its TTL
// passes. The object mapping remains in DB for audit and future offline GC.
func (s *Service) maybeExpire(job *models.CapabilityImportJob) {
	if job.Status != statusPreviewed || time.Since(job.UpdatedAt) < previewTTL {
		return
	}
	res := s.db.Model(&models.CapabilityImportJob{}).
		Where("id = ? AND status = ?", job.ID, statusPreviewed).
		Update("status", statusExpired)
	if res.Error != nil || res.RowsAffected == 0 {
		return
	}
	job.Status = statusExpired
}

// ListJobs returns a page of import jobs, newest first (import history).
func (s *Service) ListJobs(page, pageSize int) ([]models.CapabilityImportJob, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var total int64
	if err := s.db.Model(&models.CapabilityImportJob{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var jobs []models.CapabilityImportJob
	if err := s.db.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&jobs).Error; err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

// ConfirmJob promotes a previewed job to the real-import phase by flipping it
// back to pending with dry_run=false. The leader poller then executes it, so no
// application-level "is anything running" check is needed — serialization is the
// runner's job. Rejects when the job is not previewed, when the dry-run reported
// ingest failures, or when a large delete is not explicitly confirmed.
func (s *Service) ConfirmJob(id string, confirmLargeDelete bool) error {
	var job models.CapabilityImportJob
	if err := s.db.First(&job, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrJobNotFound
		}
		return err
	}
	if job.Status != statusPreviewed {
		return ErrNotPreviewed
	}
	var res ImportResult
	_ = json.Unmarshal(job.Result, &res)
	if res.Failed > 0 {
		return ErrPreviewHasFailures
	}
	if s.isLargeDelete(res.Deleted) && !confirmLargeDelete {
		return ErrLargeDeleteUnconfirmed
	}
	return s.db.Model(&models.CapabilityImportJob{}).Where("id = ? AND status = ?", job.ID, statusPreviewed).Updates(map[string]any{
		"status":        statusPending,
		"dry_run":       false,
		"scheduled_at":  time.Now(),
		"retry_count":   0,
		"started_at":    nil,
		"finished_at":   nil,
		"error_message": "",
	}).Error
}

// isLargeDelete reports whether archiving `deleted` items would exceed
// deleteWarnRatio of the current active inventory (empty inventory never warns).
func (s *Service) isLargeDelete(deleted int) bool {
	if deleted <= 0 {
		return false
	}
	var total int64
	s.db.Model(&models.CapabilityItem{}).Where("status = ?", "active").Count(&total)
	if total == 0 {
		return false
	}
	return float64(deleted)/float64(total) > deleteWarnRatio
}

// Stats returns the current active inventory grouped by item_type, plus the total.
func (s *Service) Stats() ([]TypeCount, int64, error) {
	var rows []TypeCount
	if err := s.db.Model(&models.CapabilityItem{}).
		Select("item_type, COUNT(*) AS count").
		Where("status = ?", "active").
		Group("item_type").
		Order("count DESC").
		Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	var total int64
	for _, r := range rows {
		total += r.Count
	}
	return rows, total, nil
}

// ErrorsLog renders the job's errors + incompleteErrors as a plain-text log.
func (s *Service) ErrorsLog(id string) (string, error) {
	job, err := s.GetJob(id)
	if err != nil {
		return "", err
	}
	var res ImportResult
	_ = json.Unmarshal(job.Result, &res)
	var b strings.Builder
	fmt.Fprintf(&b, "Import job %s (%s)\n", job.ID, job.Filename)
	fmt.Fprintf(&b, "manifestSha256: %s\ngeneratedAt: %s\n\n", res.ManifestSHA256, res.GeneratedAt)
	fmt.Fprintf(&b, "== Errors (%d) ==\n", len(res.Errors))
	for _, e := range res.Errors {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n== Incomplete (%d) ==\n", len(res.IncompleteErrors))
	for _, e := range res.IncompleteErrors {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// urlTail returns the last path segment of a URL for display.
func urlTail(u string) string {
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 && i < len(u)-1 {
		return u[i+1:]
	}
	return u
}
