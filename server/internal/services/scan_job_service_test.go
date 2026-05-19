package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newScanJobTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE scan_jobs (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			item_revision INTEGER NOT NULL DEFAULT 0,
			trigger_type TEXT NOT NULL,
			trigger_user TEXT,
			priority INTEGER NOT NULL DEFAULT 5,
			status TEXT NOT NULL DEFAULT 'pending',
			retry_count INTEGER DEFAULT 0,
			max_attempts INTEGER DEFAULT 2,
			last_error TEXT,
			scheduled_at DATETIME NOT NULL,
			started_at DATETIME,
			finished_at DATETIME,
			scan_result_id TEXT,
			created_at DATETIME
		)`,
		`CREATE TABLE security_scans (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			item_revision INTEGER NOT NULL DEFAULT 0,
			trigger_type TEXT NOT NULL,
			scan_model TEXT,
			category TEXT DEFAULT '',
			builtin_tags TEXT DEFAULT '[]',
			risk_level TEXT DEFAULT '',
			verdict TEXT DEFAULT '',
			red_flags TEXT DEFAULT '[]',
			permissions TEXT DEFAULT '{}',
			summary TEXT,
			recommendations TEXT DEFAULT '[]',
			raw_output TEXT,
			duration_ms INTEGER DEFAULT 0,
			created_at DATETIME,
			finished_at DATETIME
		)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func seedSecurityScan(t *testing.T, db *gorm.DB, itemID string, revision int) {
	t.Helper()
	now := time.Now()
	row := &models.SecurityScan{
		ID:           "scan-existing-" + itemID,
		ItemID:       itemID,
		ItemRevision: revision,
		TriggerType:  "sync",
		ScanModel:    "deepseek-v4-flash",
		RiskLevel:    "low",
		Verdict:      "safe",
		CreatedAt:    now,
		FinishedAt:   &now,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed security_scan: %v", err)
	}
}

func TestEnqueueShortCircuit_SyncTriggerSkipsWhenSecurityScanExists(t *testing.T) {
	db := newScanJobTestDB(t)
	seedSecurityScan(t, db, "item-1", 1)
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-1", 1, "sync", "system", ScanEnqueueOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job != nil {
		t.Fatalf("expected nil job (short-circuited), got %+v", job)
	}
	var count int64
	db.Model(&models.ScanJob{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 scan_jobs, got %d", count)
	}
}

func TestEnqueueShortCircuit_CreateTriggerSkipsWhenSecurityScanExists(t *testing.T) {
	db := newScanJobTestDB(t)
	seedSecurityScan(t, db, "item-2", 1)
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-2", 1, "create", "system", ScanEnqueueOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job != nil {
		t.Fatalf("expected nil job for create trigger short-circuit, got %+v", job)
	}
}

func TestEnqueueShortCircuit_UpdateTriggerSkipsWhenSecurityScanExists(t *testing.T) {
	db := newScanJobTestDB(t)
	seedSecurityScan(t, db, "item-3", 2)
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-3", 2, "update", "system", ScanEnqueueOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job != nil {
		t.Fatalf("expected nil job for update trigger short-circuit, got %+v", job)
	}
}

func TestEnqueueShortCircuit_ManualTriggerNeverSkips(t *testing.T) {
	db := newScanJobTestDB(t)
	seedSecurityScan(t, db, "item-4", 1)
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-4", 1, "manual", "admin", ScanEnqueueOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job == nil {
		t.Fatalf("manual trigger must not short-circuit even when SecurityScan exists")
	}
	var count int64
	db.Model(&models.ScanJob{}).Where("trigger_type = ?", "manual").Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 manual scan_job, got %d", count)
	}
}

func TestEnqueueShortCircuit_SyncTriggerEnqueuesWhenNoSecurityScan(t *testing.T) {
	db := newScanJobTestDB(t)
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-5", 1, "sync", "system", ScanEnqueueOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job == nil {
		t.Fatalf("expected normal enqueue when no SecurityScan exists")
	}
}

func TestEnqueueShortCircuit_RevisionMismatchEnqueues(t *testing.T) {
	db := newScanJobTestDB(t)
	seedSecurityScan(t, db, "item-6", 1) // existing scan for revision 1
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-6", 2, "sync", "system", ScanEnqueueOptions{}) // revision 2 → not covered
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job == nil {
		t.Fatalf("expected enqueue when SecurityScan revision differs")
	}
}

func TestEnqueueShortCircuit_FeatureFlagDisablesShortCircuit(t *testing.T) {
	t.Setenv("SECURITY_SCAN_SHORT_CIRCUIT_DISABLED", "true")
	db := newScanJobTestDB(t)
	seedSecurityScan(t, db, "item-7", 1)
	svc := &ScanJobService{DB: db}

	job, err := svc.Enqueue("item-7", 1, "sync", "system", ScanEnqueueOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if job == nil {
		t.Fatalf("feature flag should disable short-circuit, expected enqueue")
	}
}
