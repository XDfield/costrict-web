package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTagServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE item_tag_dicts (id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, tag_class TEXT NOT NULL DEFAULT 'custom', created_by TEXT NOT NULL, created_at DATETIME)`,
		`CREATE TABLE item_tags (id TEXT PRIMARY KEY, item_id TEXT NOT NULL, tag_id TEXT NOT NULL, created_at DATETIME, UNIQUE(item_id, tag_id))`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table failed: %v", err)
		}
	}
	database.DB = db
	t.Cleanup(func() { database.DB = nil })
	return db
}

func TestValidateTagSlug(t *testing.T) {
	valid := []string{"abc", "abc-123", "abc_def", "a1-b2_c3"}
	for _, slug := range valid {
		if err := ValidateTagSlug(slug); err != nil {
			t.Fatalf("expected valid slug %q, got %v", slug, err)
		}
	}
	invalid := []string{"", "hello world", "中文", "abc.", "a/b"}
	for _, slug := range invalid {
		if err := ValidateTagSlug(slug); err == nil {
			t.Fatalf("expected invalid slug %q", slug)
		}
	}
	if got := normalizeTagSlug("A-Upper"); got != "a-upper" {
		t.Fatalf("expected normalized uppercase slug to become a-upper, got %q", got)
	}
}

func TestTagServiceList_QueryAndPagination(t *testing.T) {
	db := setupTagServiceDB(t)
	svc := &TagService{DB: db}
	seed := []models.ItemTagDict{
		{ID: "t1", Slug: "auth", TagClass: TagClassCustom, CreatedBy: "u1", CreatedAt: time.Now()},
		{ID: "t2", Slug: "auth-client", TagClass: TagClassCustom, CreatedBy: "u1", CreatedAt: time.Now()},
		{ID: "t3", Slug: "official", TagClass: TagClassSystem, CreatedBy: "system", CreatedAt: time.Now()},
		{ID: "t4", Slug: "planning", TagClass: TagClassBuiltin, CreatedBy: "system", CreatedAt: time.Now()},
	}
	for _, item := range seed {
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	tags, total, err := svc.List(ListTagsOptions{Query: "auth", Page: 1, PageSize: 1})
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
	if len(tags) != 1 {
		t.Fatalf("expected 1 result due to page size, got %d", len(tags))
	}
	if tags[0].Slug != "auth" {
		t.Fatalf("unexpected first slug: %s", tags[0].Slug)
	}
	filtered, total, err := svc.List(ListTagsOptions{TagClass: TagClassSystem, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list filtered tags: %v", err)
	}
	if total != 1 || len(filtered) != 1 || filtered[0].Slug != "official" {
		t.Fatalf("unexpected filtered result: total=%d len=%d first=%v", total, len(filtered), filtered)
	}
}
