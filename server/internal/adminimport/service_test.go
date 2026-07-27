package adminimport

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupTestDB builds an in-memory sqlite schema for the two tables the import
// service touches. Hand-rolled (not AutoMigrate) because the postgres jsonb/uuid
// column types don't map onto sqlite cleanly; this mirrors the migration closely
// enough for the create/confirm/stats logic under test. FOR UPDATE SKIP LOCKED
// (runner.processOne) is postgres-only and is exercised by the live E2E instead.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	stmts := []string{
		`CREATE TABLE capability_import_jobs (
			id TEXT PRIMARY KEY,
			source_kind TEXT NOT NULL DEFAULT 'url',
			source_url TEXT,
			filename TEXT,
			storage_backend TEXT NOT NULL DEFAULT '',
			storage_key TEXT,
			file_size INTEGER DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			dry_run INTEGER NOT NULL DEFAULT 1,
			reparse INTEGER NOT NULL DEFAULT 0,
			trigger_user TEXT NOT NULL DEFAULT '',
			result TEXT DEFAULT '{}',
			error_message TEXT,
			retry_count INTEGER DEFAULT 0,
			max_attempts INTEGER DEFAULT 3,
			scheduled_at DATETIME,
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY,
			item_type TEXT,
			status TEXT DEFAULT 'active',
			experience_score REAL DEFAULT 0
		)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// seedPreviewedJob inserts a previewed job with the given serialized ImportResult.
func seedPreviewedJob(t *testing.T, db *gorm.DB, id string, res ImportResult) {
	t.Helper()
	raw, _ := json.Marshal(res)
	now := time.Now()
	if err := db.Exec(
		`INSERT INTO capability_import_jobs
			(id, source_kind, source_url, status, dry_run, trigger_user, result, max_attempts, scheduled_at, created_at, updated_at)
		 VALUES (?, 'url', 'https://x/b.tar.gz', 'previewed', 1, 'admin', ?, 3, ?, ?, ?)`,
		id, string(raw), now, now, now,
	).Error; err != nil {
		t.Fatalf("seed job %s: %v", id, err)
	}
}

func TestCreateURLJob_Validation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, nil)

	if _, err := svc.CreateURLJob("  ", false, "admin"); !errors.Is(err, ErrEmptySource) {
		t.Fatalf("empty url: expected ErrEmptySource, got %v", err)
	}
	if _, err := svc.CreateURLJob("ftp://x/y", false, "admin"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("ftp url: expected ErrInvalidURL, got %v", err)
	}
	job, err := svc.CreateURLJob("https://example.com/catalog-bundle.tar.gz", true, "admin")
	if err != nil {
		t.Fatalf("valid url: %v", err)
	}
	if job.ID == "" || job.Status != statusPending || !job.DryRun || !job.Reparse {
		t.Fatalf("unexpected job: %+v", job)
	}
	if job.Filename != "catalog-bundle.tar.gz" {
		t.Fatalf("expected filename from url tail, got %q", job.Filename)
	}
}

func TestConfirmJob_NotPreviewed(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, nil)
	// A freshly created job is pending, not previewed.
	job, _ := svc.CreateURLJob("https://x/b.tar.gz", false, "admin")
	if err := svc.ConfirmJob(job.ID, false); !errors.Is(err, ErrNotPreviewed) {
		t.Fatalf("expected ErrNotPreviewed, got %v", err)
	}
}

func TestConfirmJob_FailedGate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, nil)
	seedPreviewedJob(t, db, "j-failed", ImportResult{Added: 10, Failed: 1})
	if err := svc.ConfirmJob("j-failed", false); !errors.Is(err, ErrPreviewHasFailures) {
		t.Fatalf("expected ErrPreviewHasFailures, got %v", err)
	}
}

func TestConfirmJob_LargeDeleteGate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, nil)
	// 3 active items; a dry-run that would delete 2 is 66% > 20% threshold.
	for _, id := range []string{"i1", "i2", "i3"} {
		if err := db.Exec(`INSERT INTO capability_items (id, item_type, status, experience_score) VALUES (?, 'skill', 'active', 1)`, id).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	seedPreviewedJob(t, db, "j-del", ImportResult{Added: 1, Deleted: 2})

	// Without confirmLargeDelete → blocked.
	if err := svc.ConfirmJob("j-del", false); !errors.Is(err, ErrLargeDeleteUnconfirmed) {
		t.Fatalf("expected ErrLargeDeleteUnconfirmed, got %v", err)
	}
	// With confirmLargeDelete → allowed, flips to pending real-import.
	if err := svc.ConfirmJob("j-del", true); err != nil {
		t.Fatalf("confirm with flag: %v", err)
	}
	assertPendingRealImport(t, db, "j-del")
}

func TestConfirmJob_Success(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, nil)
	seedPreviewedJob(t, db, "j-ok", ImportResult{Added: 100, Updated: 3, Failed: 0, Deleted: 0})
	if err := svc.ConfirmJob("j-ok", false); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	assertPendingRealImport(t, db, "j-ok")
}

func assertPendingRealImport(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	var job models.CapabilityImportJob
	if err := db.First(&job, "id = ?", id).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Status != statusPending {
		t.Fatalf("expected status pending after confirm, got %q", job.Status)
	}
	if job.DryRun {
		t.Fatalf("expected dry_run=false after confirm")
	}
}

func TestStats(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, nil)
	seed := func(id, typ, status string) {
		if err := db.Exec(`INSERT INTO capability_items (id, item_type, status, experience_score) VALUES (?, ?, ?, 1)`, id, typ, status).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed("1", "skill", "active")
	seed("2", "skill", "active")
	seed("3", "plugin", "active")
	seed("4", "skill", "archived") // excluded (not active)

	rows, total, err := svc.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if total != 3 {
		t.Fatalf("expected active total 3, got %d", total)
	}
	byType := map[string]int64{}
	for _, r := range rows {
		byType[r.ItemType] = r.Count
	}
	if byType["skill"] != 2 || byType["plugin"] != 1 {
		t.Fatalf("unexpected byType: %+v", byType)
	}
}

func TestFromIngestResult(t *testing.T) {
	src := &services.IngestResult{
		BundleEntries:  10,
		Added:          7,
		Deleted:        1,
		Duration:       2 * time.Second,
		ManifestSHA256: "abc",
		GeneratedAt:    "2026-07-01T00:00:00Z",
	}
	imp := fromIngestResult(src)
	if imp.Added != 7 || imp.Deleted != 1 || imp.BundleEntries != 10 {
		t.Fatalf("mapping mismatch: %+v", imp)
	}
	if imp.DurationMs != 2000 {
		t.Fatalf("expected DurationMs 2000, got %d", imp.DurationMs)
	}
	if imp.ManifestSHA256 != "abc" || imp.GeneratedAt != "2026-07-01T00:00:00Z" {
		t.Fatalf("manifest/generatedAt mismatch: %+v", imp)
	}
	if fromIngestResult(nil).Added != 0 {
		t.Fatalf("nil result should map to zero value")
	}
}
