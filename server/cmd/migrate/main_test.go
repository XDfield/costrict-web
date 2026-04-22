package main

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newMigrateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS capability_registries (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT,
			source_type TEXT NOT NULL DEFAULT 'internal',
			repo_id TEXT,
			owner_id TEXT NOT NULL,
			created_at DATETIME,
			updated_at DATETIME
		)` ,
		`CREATE TABLE IF NOT EXISTS capability_items (
			id TEXT PRIMARY KEY,
			registry_id TEXT NOT NULL,
			repo_id TEXT NOT NULL DEFAULT 'public',
			slug TEXT NOT NULL,
			item_type TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			category TEXT,
			version TEXT DEFAULT '1.0.0',
			content TEXT,
			content_md5 TEXT DEFAULT '',
			current_revision INTEGER NOT NULL DEFAULT 1,
			metadata TEXT DEFAULT '{}',
			source_path TEXT,
			source_sha TEXT,
			source_type TEXT NOT NULL DEFAULT 'direct',
			status TEXT DEFAULT 'active',
			created_by TEXT NOT NULL,
			updated_by TEXT,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_versions (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			content TEXT NOT NULL,
			content_md5 TEXT DEFAULT '',
			metadata TEXT DEFAULT '{}',
			commit_msg TEXT,
			created_by TEXT NOT NULL,
			created_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_assets (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			rel_path TEXT NOT NULL,
			text_content TEXT,
			storage_backend TEXT DEFAULT 'local',
			storage_key TEXT,
			mime_type TEXT,
			file_size INTEGER DEFAULT 0,
			content_sha TEXT,
			created_at DATETIME,
			updated_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table failed: %v\nSQL: %s", err, stmt)
		}
	}

	return db
}

func TestBackfillCapabilityContentVersioning_SingleFile(t *testing.T) {
	db := newMigrateTestDB(t)
	db.Create(&models.CapabilityRegistry{ID: publicRegistryID, Name: "public", SourceType: "internal", RepoID: publicRepoID, OwnerID: "system"})
	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "item-1", publicRegistryID, publicRepoID, "demo", "skill", "Demo", "hello\r\nworld\r\n", 0, "active", "system", "{}").Error; err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_versions (id, item_id, revision, content, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?)`, "ver-1", "item-1", 1, "hello\nworld\n", "system", "{}").Error; err != nil {
		t.Fatalf("insert version: %v", err)
	}

	if err := backfillCapabilityContentVersioning(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var item models.CapabilityItem
	if err := db.First(&item, "id = ?", "item-1").Error; err != nil {
		t.Fatalf("reload item: %v", err)
	}
	if item.ContentMD5 == "" {
		t.Fatal("expected item content_md5 to be backfilled")
	}
	if item.CurrentRevision != 1 {
		t.Fatalf("expected current_revision=1, got %d", item.CurrentRevision)
	}

	var version models.CapabilityVersion
	if err := db.First(&version, "id = ?", "ver-1").Error; err != nil {
		t.Fatalf("reload version: %v", err)
	}
	if version.ContentMD5 == "" {
		t.Fatal("expected version content_md5 to be backfilled")
	}
	if version.ContentMD5 != item.ContentMD5 {
		t.Fatalf("expected item/version md5 match, got %s vs %s", item.ContentMD5, version.ContentMD5)
	}
}

func TestBackfillCapabilityContentVersioning_ArchiveUsesAssetsManifest(t *testing.T) {
	db := newMigrateTestDB(t)
	db.Create(&models.CapabilityRegistry{ID: publicRegistryID, Name: "public", SourceType: "internal", RepoID: publicRepoID, OwnerID: "system"})
	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, source_type, source_path, current_revision, status, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "item-2", publicRegistryID, publicRepoID, "archive", "skill", "Archive", "# Skill\n", "archive", "SKILL.md", 0, "active", "system", "{}").Error; err != nil {
		t.Fatalf("insert archive item: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_assets (id, item_id, rel_path, text_content, content_sha) VALUES (?, ?, ?, ?, ?)`, "asset-1", "item-2", "scripts/run.sh", "echo hi\n", "asset-sha-1").Error; err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_versions (id, item_id, revision, content, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?)`, "ver-2-1", "item-2", 1, "# Skill\n", "system", "{}").Error; err != nil {
		t.Fatalf("insert version 1: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_versions (id, item_id, revision, content, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?)`, "ver-2-2", "item-2", 3, "# Skill\n", "system", "{}").Error; err != nil {
		t.Fatalf("insert version 2: %v", err)
	}

	if err := backfillCapabilityContentVersioning(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var item models.CapabilityItem
	if err := db.Preload("Assets").First(&item, "id = ?", "item-2").Error; err != nil {
		t.Fatalf("reload item: %v", err)
	}
	if item.ContentMD5 == "" {
		t.Fatal("expected archive item content_md5 to be backfilled")
	}
	if item.CurrentRevision != 3 {
		t.Fatalf("expected current_revision=3, got %d", item.CurrentRevision)
	}

	hashSvc := services.NewContentHashService()
	expected, err := hashCurrentItemContent(hashSvc, item)
	if err != nil {
		t.Fatalf("hash current item content: %v", err)
	}
	if item.ContentMD5 != expected {
		t.Fatalf("expected archive md5=%s, got %s", expected, item.ContentMD5)
	}

	var version models.CapabilityVersion
	if err := db.First(&version, "id = ?", "ver-2-1").Error; err != nil {
		t.Fatalf("reload version: %v", err)
	}
	if version.ContentMD5 == "" {
		t.Fatal("expected archive version content_md5 to be backfilled")
	}
}

func TestNormalizeLegacyCapabilityVersions_CollapsesToSingleV1PerItem(t *testing.T) {
	db := newMigrateTestDB(t)

	if err := db.Exec(`ALTER TABLE capability_versions ADD COLUMN version TEXT`).Error; err != nil {
		t.Fatalf("add version column: %v", err)
	}
	if err := db.Exec(`ALTER TABLE capability_versions RENAME TO capability_versions_old`).Error; err != nil {
		t.Fatalf("rename capability_versions: %v", err)
	}
	if err := db.Exec(`CREATE TABLE capability_versions (
		id TEXT PRIMARY KEY,
		item_id TEXT NOT NULL,
		revision INTEGER,
		version TEXT,
		content TEXT NOT NULL,
		content_md5 TEXT DEFAULT '',
		metadata TEXT DEFAULT '{}',
		commit_msg TEXT,
		created_by TEXT NOT NULL,
		created_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create legacy capability_versions: %v", err)
	}
	if err := db.Exec(`DROP TABLE capability_versions_old`).Error; err != nil {
		t.Fatalf("drop old capability_versions: %v", err)
	}

	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "item-legacy-1", publicRegistryID, publicRepoID, "legacy-1", "skill", "Legacy 1", "content", 0, "active", "system", "{}").Error; err != nil {
		t.Fatalf("insert item 1: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "item-legacy-2", publicRegistryID, publicRepoID, "legacy-2", "skill", "Legacy 2", "content", 0, "active", "system", "{}").Error; err != nil {
		t.Fatalf("insert item 2: %v", err)
	}

	legacyRows := []struct {
		id        string
		itemID    string
		version   string
		createdAt string
	}{
		{"ver-1-a", "item-legacy-1", "1.0.0", "2024-01-01 00:00:00"},
		{"ver-1-b", "item-legacy-1", "2.0.0", "2024-01-02 00:00:00"},
		{"ver-2-a", "item-legacy-2", "v3", "2024-01-03 00:00:00"},
	}
	for _, row := range legacyRows {
		if err := db.Exec(`INSERT INTO capability_versions (id, item_id, revision, version, content, created_by, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, row.id, row.itemID, nil, row.version, "legacy", "system", "{}", row.createdAt).Error; err != nil {
			t.Fatalf("insert legacy version %s: %v", row.id, err)
		}
	}

	if err := normalizeLegacyCapabilityVersions(db); err != nil {
		t.Fatalf("normalize legacy versions: %v", err)
	}

	var countItem1 int64
	if err := db.Table("capability_versions").Where("item_id = ?", "item-legacy-1").Count(&countItem1).Error; err != nil {
		t.Fatalf("count item 1 versions: %v", err)
	}
	if countItem1 != 1 {
		t.Fatalf("expected item-legacy-1 to keep 1 version, got %d", countItem1)
	}

	var countItem2 int64
	if err := db.Table("capability_versions").Where("item_id = ?", "item-legacy-2").Count(&countItem2).Error; err != nil {
		t.Fatalf("count item 2 versions: %v", err)
	}
	if countItem2 != 1 {
		t.Fatalf("expected item-legacy-2 to keep 1 version, got %d", countItem2)
	}

	var kept struct {
		ID       string
		Revision int
	}
	if err := db.Table("capability_versions").Select("id, revision").Where("item_id = ?", "item-legacy-1").First(&kept).Error; err != nil {
		t.Fatalf("load kept version: %v", err)
	}
	if kept.ID != "ver-1-a" {
		t.Fatalf("expected earliest item-legacy-1 version to be kept, got %s", kept.ID)
	}
	if kept.Revision != 1 {
		t.Fatalf("expected kept revision=1, got %d", kept.Revision)
	}

	var currentRevision int
	if err := db.Table("capability_items").Select("current_revision").Where("id = ?", "item-legacy-1").Scan(&currentRevision).Error; err != nil {
		t.Fatalf("load current_revision: %v", err)
	}
	if currentRevision != 1 {
		t.Fatalf("expected current_revision=1, got %d", currentRevision)
	}
}

func strPtr(v string) *string { return &v }
