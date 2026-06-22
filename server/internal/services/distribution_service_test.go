package services

import (
	"context"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupDistributionServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE item_distributions (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			distributor_id TEXT NOT NULL,
			permission_mode TEXT DEFAULT 'readonly',
			status TEXT DEFAULT 'active',
			scope_type TEXT DEFAULT 'user',
			target_id TEXT NOT NULL,
			message TEXT,
			revoked_at DATETIME,
			expires_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE item_distribution_receipts (
			id TEXT PRIMARY KEY,
			distribution_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			receipt_status TEXT DEFAULT 'unread',
			forked_item_id TEXT,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY,
			name TEXT
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func seedDistributions(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	items := [][2]string{
		{"item-1", "Alpha Plugin"},
		{"item-2", "Beta Skill"},
	}
	for _, it := range items {
		if err := db.Exec(`INSERT INTO capability_items (id, name) VALUES (?, ?)`, it[0], it[1]).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	dists := []models.ItemDistribution{
		{ID: "d1", ItemID: "item-1", DistributorID: "admin-a", Status: "active", ScopeType: "user", TargetID: "u1", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "d2", ItemID: "item-1", DistributorID: "admin-b", Status: "paused", ScopeType: "user", TargetID: "u2", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "d3", ItemID: "item-2", DistributorID: "admin-a", Status: "active", ScopeType: "organization", TargetID: "org-x", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "d4", ItemID: "item-2", DistributorID: "admin-c", Status: "revoked", ScopeType: "user", TargetID: "u3", CreatedAt: now},
	}
	for _, d := range dists {
		if err := db.Create(&d).Error; err != nil {
			t.Fatalf("seed distribution: %v", err)
		}
	}
}

func TestListAllDistributions_AcrossDistributors(t *testing.T) {
	db := setupDistributionServiceDB(t)
	seedDistributions(t, db)
	svc := NewDistributionService(db, nil)

	list, total, err := svc.ListAllDistributions(context.Background(), DistributionListFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected total 4, got %d", total)
	}
	if len(list) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(list))
	}
	// Default order: created_at DESC -> d4, d3, d2, d1
	if list[0].ID != "d4" {
		t.Fatalf("expected newest first (d4), got %s", list[0].ID)
	}
	// Item preloaded
	if list[0].Item == nil || list[0].Item.Name != "Beta Skill" {
		t.Fatalf("expected Item preloaded with name Beta Skill, got %+v", list[0].Item)
	}
}

func TestListAllDistributions_FiltersAndPagination(t *testing.T) {
	db := setupDistributionServiceDB(t)
	seedDistributions(t, db)
	svc := NewDistributionService(db, nil)
	ctx := context.Background()

	// status filter
	list, total, err := svc.ListAllDistributions(ctx, DistributionListFilter{Status: "active"})
	if err != nil {
		t.Fatalf("status filter: %v", err)
	}
	if total != 2 || len(list) != 2 {
		t.Fatalf("expected 2 active, got total=%d len=%d", total, len(list))
	}

	// scope filter
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{ScopeType: "organization"})
	if err != nil {
		t.Fatalf("scope filter: %v", err)
	}
	if total != 1 || list[0].ID != "d3" {
		t.Fatalf("expected only d3 for org scope, got total=%d", total)
	}

	// search by item name
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{Search: "Alpha"})
	if err != nil {
		t.Fatalf("search item name: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 distributions for item Alpha Plugin, got %d", total)
	}

	// search by distributor id
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{Search: "admin-c"})
	if err != nil {
		t.Fatalf("search distributor: %v", err)
	}
	if total != 1 || list[0].ID != "d4" {
		t.Fatalf("expected d4 for distributor admin-c, got total=%d", total)
	}

	// pagination: page size 2
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("pagination page1: %v", err)
	}
	if total != 4 || len(list) != 2 {
		t.Fatalf("expected total=4 len=2 on page1, got total=%d len=%d", total, len(list))
	}
	page2, _, err := svc.ListAllDistributions(ctx, DistributionListFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("pagination page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID == list[0].ID {
		t.Fatalf("expected distinct page2 rows, got %v vs %v", page2[0].ID, list[0].ID)
	}
}

func TestListReceipts(t *testing.T) {
	db := setupDistributionServiceDB(t)
	now := time.Now()
	receipts := []models.ItemDistributionReceipt{
		{ID: "r1", DistributionID: "d1", UserID: "u1", ReceiptStatus: "unread", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "r2", DistributionID: "d1", UserID: "u2", ReceiptStatus: "read", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "r3", DistributionID: "d2", UserID: "u3", ReceiptStatus: "dismissed", CreatedAt: now},
	}
	for _, r := range receipts {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("seed receipt: %v", err)
		}
	}
	svc := NewDistributionService(db, nil)

	got, err := svc.ListReceipts(context.Background(), "d1")
	if err != nil {
		t.Fatalf("list receipts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 receipts for d1, got %d", len(got))
	}
	// created_at DESC -> r2 then r1
	if got[0].ID != "r2" {
		t.Fatalf("expected r2 first, got %s", got[0].ID)
	}
}

func TestGetEffectivePermission(t *testing.T) {
	db := setupDistributionServiceDB(t)
	now := time.Now()
	// item-1: active distribution (dismissible) with a read receipt for u1.
	// item-2: active distribution (readonly) but u1's receipt is dismissed.
	// item-3: a paused distribution (should not count).
	dists := []models.ItemDistribution{
		{ID: "d1", ItemID: "item-1", DistributorID: "admin-a", PermissionMode: "dismissible", Status: "active", ScopeType: "user", TargetID: "u1", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "d2", ItemID: "item-2", DistributorID: "admin-a", PermissionMode: "readonly", Status: "active", ScopeType: "user", TargetID: "u1", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "d3", ItemID: "item-3", DistributorID: "admin-a", PermissionMode: "readonly", Status: "paused", ScopeType: "user", TargetID: "u1", CreatedAt: now},
	}
	for _, d := range dists {
		if err := db.Create(&d).Error; err != nil {
			t.Fatalf("seed distribution: %v", err)
		}
	}
	receipts := []models.ItemDistributionReceipt{
		{ID: "r1", DistributionID: "d1", UserID: "u1", ReceiptStatus: "read", CreatedAt: now},
		{ID: "r2", DistributionID: "d2", UserID: "u1", ReceiptStatus: "dismissed", CreatedAt: now},
		{ID: "r3", DistributionID: "d3", UserID: "u1", ReceiptStatus: "read", CreatedAt: now},
	}
	for _, r := range receipts {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("seed receipt: %v", err)
		}
	}
	svc := NewDistributionService(db, nil)
	ctx := context.Background()

	// item-1: active + non-dismissed -> returns the actual permission_mode.
	mode, ok := svc.GetEffectivePermission(ctx, "item-1", "u1")
	if !ok || mode != "dismissible" {
		t.Fatalf("item-1: expected (dismissible,true), got (%q,%v)", mode, ok)
	}

	// item-2: receipt dismissed -> no effective permission.
	if mode, ok := svc.GetEffectivePermission(ctx, "item-2", "u1"); ok || mode != "" {
		t.Fatalf("item-2 (dismissed): expected (\"\",false), got (%q,%v)", mode, ok)
	}

	// item-3: distribution paused -> no effective permission.
	if mode, ok := svc.GetEffectivePermission(ctx, "item-3", "u1"); ok || mode != "" {
		t.Fatalf("item-3 (paused): expected (\"\",false), got (%q,%v)", mode, ok)
	}

	// unknown item -> no effective permission.
	if mode, ok := svc.GetEffectivePermission(ctx, "item-none", "u1"); ok || mode != "" {
		t.Fatalf("item-none: expected (\"\",false), got (%q,%v)", mode, ok)
	}
}
