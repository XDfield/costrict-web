package services

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupBehaviorFavoriteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE item_favorites (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			invoke_mode TEXT NOT NULL DEFAULT 'auto',
			created_at DATETIME
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY,
			favorite_count INTEGER DEFAULT 0
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	if err := db.Exec(`INSERT INTO capability_items (id, favorite_count) VALUES (?, 0)`, "item-1").Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return db
}

func favoriteRow(t *testing.T, db *gorm.DB, itemID, userID string) models.ItemFavorite {
	t.Helper()
	var fav models.ItemFavorite
	if err := db.Where("item_id = ? AND user_id = ?", itemID, userID).First(&fav).Error; err != nil {
		t.Fatalf("load favorite: %v", err)
	}
	return fav
}

func TestFavoriteItem_CreatesWithMode(t *testing.T) {
	db := setupBehaviorFavoriteDB(t)
	svc := NewBehaviorService(db)
	ctx := context.Background()

	count, created, err := svc.FavoriteItem(ctx, "item-1", "u1", "manual")
	if err != nil {
		t.Fatalf("favorite: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true")
	}
	if count != 1 {
		t.Fatalf("expected favorite_count=1, got %d", count)
	}
	if got := favoriteRow(t, db, "item-1", "u1").InvokeMode; got != "manual" {
		t.Fatalf("expected invoke_mode=manual, got %q", got)
	}
}

func TestFavoriteItem_EmptyModeDefaultsAuto(t *testing.T) {
	db := setupBehaviorFavoriteDB(t)
	svc := NewBehaviorService(db)

	if _, _, err := svc.FavoriteItem(context.Background(), "item-1", "u1", ""); err != nil {
		t.Fatalf("favorite: %v", err)
	}
	if got := favoriteRow(t, db, "item-1", "u1").InvokeMode; got != "auto" {
		t.Fatalf("expected invoke_mode=auto, got %q", got)
	}
}

func TestFavoriteItem_InvalidModeFallsBackAuto(t *testing.T) {
	db := setupBehaviorFavoriteDB(t)
	svc := NewBehaviorService(db)

	// Defensive: service normalizes anything not "manual" to "auto".
	if _, _, err := svc.FavoriteItem(context.Background(), "item-1", "u1", "bogus"); err != nil {
		t.Fatalf("favorite: %v", err)
	}
	if got := favoriteRow(t, db, "item-1", "u1").InvokeMode; got != "auto" {
		t.Fatalf("expected invoke_mode=auto, got %q", got)
	}
}

func TestFavoriteItem_IdempotentUpsertMode(t *testing.T) {
	db := setupBehaviorFavoriteDB(t)
	svc := NewBehaviorService(db)
	ctx := context.Background()

	// First favorite as auto.
	if _, created, err := svc.FavoriteItem(ctx, "item-1", "u1", "auto"); err != nil || !created {
		t.Fatalf("first favorite: created=%v err=%v", created, err)
	}
	// Re-favorite as manual: idempotent (no new row, count stays 1) but mode flips.
	count, created, err := svc.FavoriteItem(ctx, "item-1", "u1", "manual")
	if err != nil {
		t.Fatalf("second favorite: %v", err)
	}
	if created {
		t.Fatalf("expected created=false on re-favorite")
	}
	if count != 1 {
		t.Fatalf("expected favorite_count stays 1, got %d", count)
	}
	if got := favoriteRow(t, db, "item-1", "u1").InvokeMode; got != "manual" {
		t.Fatalf("expected invoke_mode flipped to manual, got %q", got)
	}

	// Exactly one row exists.
	var n int64
	db.Model(&models.ItemFavorite{}).Where("item_id = ? AND user_id = ?", "item-1", "u1").Count(&n)
	if n != 1 {
		t.Fatalf("expected exactly 1 favorite row, got %d", n)
	}
}
