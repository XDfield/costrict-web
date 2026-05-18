package main

import (
	"os"
	"path/filepath"
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
			source TEXT DEFAULT '',
			experience_score REAL DEFAULT 0,
			status TEXT DEFAULT 'active',
			security_status TEXT DEFAULT 'unscanned',
			last_scan_id TEXT,
			created_by TEXT NOT NULL,
			updated_by TEXT,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS security_scans (
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

func TestBackfillUserExternalIdentities(t *testing.T) {
	db := newMigrateTestDB(t)
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}

	u1 := models.User{SubjectID: "u1", Username: "alice", CasdoorUniversalID: strPtr("uuid-1"), IsActive: true}
	u2 := models.User{SubjectID: "u2", Username: "phone_15500000001", Email: strPtr("15500000001"), IsActive: true}
	if err := db.Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	if err := backfillUserExternalIdentities(db, false); err != nil {
		t.Fatalf("backfill external identities: %v", err)
	}

	var got1, got2 models.User
	if err := db.First(&got1, "subject_id = ?", "u1").Error; err != nil {
		t.Fatalf("reload u1: %v", err)
	}
	if err := db.First(&got2, "subject_id = ?", "u2").Error; err != nil {
		t.Fatalf("reload u2: %v", err)
	}
	if got1.ExternalKey == nil || *got1.ExternalKey != "casdoor:uuid-1" {
		t.Fatalf("expected u1 external_key backfilled, got %+v", got1)
	}
	if got1.AuthProvider == nil || *got1.AuthProvider != "casdoor" {
		t.Fatalf("expected u1 auth_provider backfilled, got %+v", got1)
	}
	if got2.AuthProvider == nil || *got2.AuthProvider != "phone" {
		t.Fatalf("expected u2 auth_provider backfilled, got %+v", got2)
	}
	if got2.Phone == nil || *got2.Phone != "15500000001" {
		t.Fatalf("expected u2 phone backfilled from legacy email-like value, got %+v", got2)
	}
}

func TestBackfillUserAuthIdentities(t *testing.T) {
	db := newMigrateTestDB(t)
	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}); err != nil {
		t.Fatalf("migrate users/auth identities: %v", err)
	}
	u := models.User{SubjectID: "u1", Username: "alice", AuthProvider: strPtr("github"), ExternalKey: strPtr("casdoor:uuid-1"), ProviderUserID: strPtr("18633160"), CasdoorUniversalID: strPtr("uuid-1"), IsActive: true}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := backfillUserAuthIdentities(db, false); err != nil {
		t.Fatalf("backfill auth identities: %v", err)
	}
	var count int64
	if err := db.Model(&models.UserAuthIdentity{}).Where("user_subject_id = ?", "u1").Count(&count).Error; err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 backfilled identity, got %d", count)
	}
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

func TestBackfillCapabilityContentVersioning_SkipsMalformedMCPContent(t *testing.T) {
	db := newMigrateTestDB(t)
	db.Create(&models.CapabilityRegistry{ID: publicRegistryID, Name: "public", SourceType: "internal", RepoID: publicRepoID, OwnerID: "system"})

	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "item-bad-mcp", publicRegistryID, publicRepoID, "bad-mcp", "mcp", "Bad MCP", "{", 0, "active", "system", "{}").Error; err != nil {
		t.Fatalf("insert malformed item: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "item-good-skill", publicRegistryID, publicRepoID, "good-skill", "skill", "Good Skill", "hello", 0, "active", "system", "{}").Error; err != nil {
		t.Fatalf("insert good item: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_versions (id, item_id, revision, content, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?)`, "ver-bad-mcp", "item-bad-mcp", 1, "{", "system", "{}").Error; err != nil {
		t.Fatalf("insert malformed version: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_versions (id, item_id, revision, content, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?)`, "ver-good-skill", "item-good-skill", 1, "hello", "system", "{}").Error; err != nil {
		t.Fatalf("insert good version: %v", err)
	}

	if err := backfillCapabilityContentVersioning(db); err != nil {
		t.Fatalf("backfill should skip malformed records, got: %v", err)
	}

	var badItem models.CapabilityItem
	if err := db.First(&badItem, "id = ?", "item-bad-mcp").Error; err != nil {
		t.Fatalf("reload bad item: %v", err)
	}
	if badItem.ContentMD5 != "" {
		t.Fatalf("expected malformed item content_md5 to remain empty, got %q", badItem.ContentMD5)
	}
	if badItem.CurrentRevision != 0 {
		t.Fatalf("expected malformed item current_revision to remain 0, got %d", badItem.CurrentRevision)
	}

	var badVersion models.CapabilityVersion
	if err := db.First(&badVersion, "id = ?", "ver-bad-mcp").Error; err != nil {
		t.Fatalf("reload bad version: %v", err)
	}
	if badVersion.ContentMD5 != "" {
		t.Fatalf("expected malformed version content_md5 to remain empty, got %q", badVersion.ContentMD5)
	}

	var goodItem models.CapabilityItem
	if err := db.First(&goodItem, "id = ?", "item-good-skill").Error; err != nil {
		t.Fatalf("reload good item: %v", err)
	}
	if goodItem.ContentMD5 == "" {
		t.Fatal("expected good item content_md5 to be backfilled")
	}
	if goodItem.CurrentRevision != 1 {
		t.Fatalf("expected good item current_revision=1, got %d", goodItem.CurrentRevision)
	}

	var goodVersion models.CapabilityVersion
	if err := db.First(&goodVersion, "id = ?", "ver-good-skill").Error; err != nil {
		t.Fatalf("reload good version: %v", err)
	}
	if goodVersion.ContentMD5 == "" {
		t.Fatal("expected good version content_md5 to be backfilled")
	}
}

func strPtr(v string) *string { return &v }

// writeCatalogIndex creates a fixture <root>/catalog/index.json with given content.
func writeCatalogIndex(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, "catalog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write catalog/index.json: %v", err)
	}
}

const fixtureCatalogWithSecurity = `[
  {
    "id": "skill-safe",
    "type": "skill",
    "source": "anthropic",
    "stars": 100,
    "security": {
      "risk_level": "low",
      "verdict": "safe",
      "red_flags": [],
      "permissions": {"files": ["/etc/hosts"], "network": [], "commands": []},
      "summary": "low risk skill",
      "recommendations": [],
      "scan_model": "deepseek-v4-flash",
      "rubric_version": "1.bc4d9d0a",
      "content_hash": "hash-abc",
      "scanned_at": "2026-05-18T00:00:00Z"
    }
  },
  {
    "id": "skill-plain",
    "type": "skill",
    "source": "github",
    "stars": 50
  },
  {
    "id": "skill-mismatch",
    "type": "skill",
    "source": "github",
    "stars": 25,
    "security": {
      "risk_level": "high",
      "verdict": "safe",
      "red_flags": [],
      "permissions": {"files": [], "network": [], "commands": []},
      "summary": "invalid mapping",
      "recommendations": [],
      "scan_model": "deepseek-v4-flash",
      "rubric_version": "1.bc4d9d0a",
      "content_hash": "hash-bad",
      "scanned_at": "2026-05-18T00:00:00Z"
    }
  }
]`

func seedItemsForSecurityBackfill(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Create(&models.CapabilityRegistry{ID: publicRegistryID, Name: "public", SourceType: "internal", RepoID: publicRepoID, OwnerID: "system"})
	rows := []struct {
		id         string
		slug       string
		sourcePath string
	}{
		{"item-safe", "skill-safe", "skills/skill-safe/SKILL.md"},
		{"item-plain", "skill-plain", "skills/skill-plain/SKILL.md"},
		{"item-mismatch", "skill-mismatch", "skills/skill-mismatch/SKILL.md"},
	}
	for _, r := range rows {
		if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, security_status, source_path, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, publicRegistryID, publicRepoID, r.slug, "skill", r.slug, "content", 1, "active", "unscanned", r.sourcePath, "system", "{}").Error; err != nil {
			t.Fatalf("insert item %s: %v", r.id, err)
		}
	}
}

func TestBackfillCatalogMetadata_WritesSecurityScanWhenSecurityBlockPresent(t *testing.T) {
	db := newMigrateTestDB(t)
	seedItemsForSecurityBackfill(t, db)
	tmp := t.TempDir()
	writeCatalogIndex(t, tmp, fixtureCatalogWithSecurity)

	if err := backfillCatalogMetadata(db, tmp, false); err != nil {
		t.Fatalf("backfill failed: %v", err)
	}

	var safeItem models.CapabilityItem
	if err := db.First(&safeItem, "id = ?", "item-safe").Error; err != nil {
		t.Fatalf("reload item-safe: %v", err)
	}
	if safeItem.SecurityStatus != "completed" {
		t.Fatalf("expected security_status=completed, got %q", safeItem.SecurityStatus)
	}
	if safeItem.LastScanID == nil || *safeItem.LastScanID == "" {
		t.Fatalf("expected last_scan_id populated, got %+v", safeItem.LastScanID)
	}
	if safeItem.Source != "anthropic" {
		t.Fatalf("expected source=anthropic, got %q", safeItem.Source)
	}

	var safeScan models.SecurityScan
	if err := db.First(&safeScan, "item_id = ?", "item-safe").Error; err != nil {
		t.Fatalf("reload security_scan for item-safe: %v", err)
	}
	if safeScan.RiskLevel != "low" || safeScan.Verdict != "safe" {
		t.Fatalf("unexpected scan risk/verdict: %+v", safeScan)
	}
	if safeScan.ScanModel != "deepseek-v4-flash" {
		t.Fatalf("expected scan_model=deepseek-v4-flash, got %q", safeScan.ScanModel)
	}
	if safeScan.TriggerType != "sync" {
		t.Fatalf("expected trigger_type=sync, got %q", safeScan.TriggerType)
	}
	if safeScan.ItemRevision != 1 {
		t.Fatalf("expected item_revision=1, got %d", safeScan.ItemRevision)
	}
	if string(safeScan.Permissions) == "" || string(safeScan.Permissions) == "{}" {
		t.Fatalf("expected permissions populated, got %s", string(safeScan.Permissions))
	}
	if *safeItem.LastScanID != safeScan.ID {
		t.Fatalf("expected last_scan_id=%s, got %s", safeScan.ID, *safeItem.LastScanID)
	}

	var plainItem models.CapabilityItem
	if err := db.First(&plainItem, "id = ?", "item-plain").Error; err != nil {
		t.Fatalf("reload item-plain: %v", err)
	}
	if plainItem.SecurityStatus != "unscanned" {
		t.Fatalf("plain item should remain unscanned, got %q", plainItem.SecurityStatus)
	}
	if plainItem.LastScanID != nil {
		t.Fatalf("plain item should have nil last_scan_id, got %+v", plainItem.LastScanID)
	}
	var plainCount int64
	db.Model(&models.SecurityScan{}).Where("item_id = ?", "item-plain").Count(&plainCount)
	if plainCount != 0 {
		t.Fatalf("plain item should have 0 security_scans, got %d", plainCount)
	}

	var mismatchItem models.CapabilityItem
	if err := db.First(&mismatchItem, "id = ?", "item-mismatch").Error; err != nil {
		t.Fatalf("reload item-mismatch: %v", err)
	}
	if mismatchItem.SecurityStatus != "unscanned" {
		t.Fatalf("mismatch item should be skipped (status unchanged), got %q", mismatchItem.SecurityStatus)
	}
	var mismatchCount int64
	db.Model(&models.SecurityScan{}).Where("item_id = ?", "item-mismatch").Count(&mismatchCount)
	if mismatchCount != 0 {
		t.Fatalf("mismatch item should have 0 security_scans (invalid mapping), got %d", mismatchCount)
	}
}

func TestBackfillCatalogMetadata_IsIdempotent(t *testing.T) {
	db := newMigrateTestDB(t)
	seedItemsForSecurityBackfill(t, db)
	tmp := t.TempDir()
	writeCatalogIndex(t, tmp, fixtureCatalogWithSecurity)

	if err := backfillCatalogMetadata(db, tmp, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := backfillCatalogMetadata(db, tmp, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var count int64
	db.Model(&models.SecurityScan{}).Where("item_id = ?", "item-safe").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 security_scan after rerun (idempotent), got %d", count)
	}
}

func TestBackfillCatalogMetadata_DryRunDoesNotWrite(t *testing.T) {
	db := newMigrateTestDB(t)
	seedItemsForSecurityBackfill(t, db)
	tmp := t.TempDir()
	writeCatalogIndex(t, tmp, fixtureCatalogWithSecurity)

	if err := backfillCatalogMetadata(db, tmp, true); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	var scanCount int64
	db.Model(&models.SecurityScan{}).Count(&scanCount)
	if scanCount != 0 {
		t.Fatalf("dry-run should not write security_scans, got %d rows", scanCount)
	}
	var safeItem models.CapabilityItem
	if err := db.First(&safeItem, "id = ?", "item-safe").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if safeItem.SecurityStatus != "unscanned" {
		t.Fatalf("dry-run should not update security_status, got %q", safeItem.SecurityStatus)
	}
	if safeItem.Source != "" {
		t.Fatalf("dry-run should not update source, got %q", safeItem.Source)
	}
}

func TestBackfillCatalogMetadata_MiniCatalogIntegration(t *testing.T) {
	db := newMigrateTestDB(t)
	db.Create(&models.CapabilityRegistry{ID: publicRegistryID, Name: "public", SourceType: "internal", RepoID: publicRepoID, OwnerID: "system"})

	rows := []struct {
		id         string
		slug       string
		sourcePath string
	}{
		{"item-a", "skill-a", "skills/skill-a/SKILL.md"},
		{"item-b", "skill-b", "skills/skill-b/SKILL.md"},
		{"item-c", "skill-c", "skills/skill-c/SKILL.md"},
		{"item-d", "skill-d", "skills/skill-d/SKILL.md"},
		{"item-e", "skill-e", "skills/skill-e/SKILL.md"},
	}
	for _, r := range rows {
		if err := db.Exec(`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, content, current_revision, status, security_status, source_path, created_by, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, publicRegistryID, publicRepoID, r.slug, "skill", r.slug, "content", 1, "active", "unscanned", r.sourcePath, "system", "{}").Error; err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// Mini catalog: 3 valid security blocks, 1 without security, 1 with mismatched mapping.
	miniCatalog := `[
  {"id":"skill-a","source":"src-a","stars":10,"security":{"risk_level":"clean","verdict":"safe","red_flags":[],"permissions":{"files":[],"network":[],"commands":[]},"summary":"a","recommendations":[],"scan_model":"m","scanned_at":"2026-05-18T00:00:00Z"}},
  {"id":"skill-b","source":"src-b","stars":20,"security":{"risk_level":"medium","verdict":"caution","red_flags":["unsafe-eval"],"permissions":{"files":["/tmp/x"],"network":["api.example.com"],"commands":["rm"]},"summary":"b","recommendations":["lock"],"scan_model":"m","scanned_at":"2026-05-18T00:00:00Z"}},
  {"id":"skill-c","source":"src-c","stars":30,"security":{"risk_level":"high","verdict":"reject","red_flags":["exec"],"permissions":{"files":[],"network":[],"commands":["sudo"]},"summary":"c","recommendations":[],"scan_model":"m","scanned_at":"2026-05-18T00:00:00Z"}},
  {"id":"skill-d","source":"src-d","stars":40},
  {"id":"skill-e","source":"src-e","stars":50,"security":{"risk_level":"low","verdict":"reject","red_flags":[],"permissions":{"files":[],"network":[],"commands":[]},"summary":"e","recommendations":[],"scan_model":"m","scanned_at":"2026-05-18T00:00:00Z"}}
]`
	tmp := t.TempDir()
	writeCatalogIndex(t, tmp, miniCatalog)

	if err := backfillCatalogMetadata(db, tmp, false); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var scanCount int64
	db.Model(&models.SecurityScan{}).Count(&scanCount)
	if scanCount != 3 {
		t.Fatalf("expected 3 SecurityScans (a/b/c valid; d=no-security; e=mismatch), got %d", scanCount)
	}

	checks := []struct {
		itemID         string
		wantSecurity   string
		wantVerdict    string
		wantSource     string
		wantHasScanRow bool
	}{
		{"item-a", "completed", "safe", "src-a", true},
		{"item-b", "completed", "caution", "src-b", true},
		{"item-c", "completed", "reject", "src-c", true},
		{"item-d", "unscanned", "", "src-d", false},
		{"item-e", "unscanned", "", "src-e", false}, // mismatch -> skipped, source still updated
	}
	for _, c := range checks {
		var item models.CapabilityItem
		if err := db.First(&item, "id = ?", c.itemID).Error; err != nil {
			t.Fatalf("reload %s: %v", c.itemID, err)
		}
		if item.SecurityStatus != c.wantSecurity {
			t.Errorf("%s: security_status=%q want %q", c.itemID, item.SecurityStatus, c.wantSecurity)
		}
		if item.Source != c.wantSource {
			t.Errorf("%s: source=%q want %q", c.itemID, item.Source, c.wantSource)
		}
		var scanRow models.SecurityScan
		err := db.Where("item_id = ?", c.itemID).First(&scanRow).Error
		if c.wantHasScanRow {
			if err != nil {
				t.Errorf("%s: expected SecurityScan row, got err: %v", c.itemID, err)
			} else if scanRow.Verdict != c.wantVerdict {
				t.Errorf("%s: verdict=%q want %q", c.itemID, scanRow.Verdict, c.wantVerdict)
			}
		} else if err == nil {
			t.Errorf("%s: expected no SecurityScan row but found one", c.itemID)
		}
	}
}

func TestBackfillCatalogMetadata_ScanModelChangeWritesNewRow(t *testing.T) {
	db := newMigrateTestDB(t)
	seedItemsForSecurityBackfill(t, db)
	tmp := t.TempDir()
	writeCatalogIndex(t, tmp, fixtureCatalogWithSecurity)

	if err := backfillCatalogMetadata(db, tmp, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	upgraded := `[
  {
    "id": "skill-safe",
    "type": "skill",
    "source": "anthropic",
    "stars": 100,
    "security": {
      "risk_level": "low",
      "verdict": "safe",
      "red_flags": [],
      "permissions": {"files": [], "network": [], "commands": []},
      "summary": "low risk skill v2",
      "recommendations": [],
      "scan_model": "deepseek-v5",
      "rubric_version": "1.bc4d9d0a",
      "content_hash": "hash-v2",
      "scanned_at": "2026-05-18T00:00:00Z"
    }
  }
]`
	writeCatalogIndex(t, tmp, upgraded)
	if err := backfillCatalogMetadata(db, tmp, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var rows []models.SecurityScan
	if err := db.Where("item_id = ?", "item-safe").Order("scan_model").Find(&rows).Error; err != nil {
		t.Fatalf("list scans: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 security_scans after scan_model upgrade, got %d", len(rows))
	}
	var safeItem models.CapabilityItem
	if err := db.First(&safeItem, "id = ?", "item-safe").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if safeItem.LastScanID == nil {
		t.Fatalf("last_scan_id should be set")
	}
	var lastScan models.SecurityScan
	if err := db.First(&lastScan, "id = ?", *safeItem.LastScanID).Error; err != nil {
		t.Fatalf("reload last scan: %v", err)
	}
	if lastScan.ScanModel != "deepseek-v5" {
		t.Fatalf("expected last_scan_id to point at deepseek-v5 row, got %q", lastScan.ScanModel)
	}
}

func TestBackfillProviderAwareExternalKeys(t *testing.T) {
	db := newMigrateTestDB(t)
	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	u1 := models.User{SubjectID: "u1", Username: "alice", AuthProvider: strPtr("github"), ExternalKey: strPtr("casdoor:uuid-1"), CasdoorUniversalID: strPtr("uuid-1"), IsActive: true}
	if err := db.Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	id1 := models.UserAuthIdentity{UserSubjectID: "u1", Provider: "github", ExternalKey: "casdoor:uuid-1", IsPrimary: true}
	if err := db.Create(&id1).Error; err != nil {
		t.Fatalf("create id1: %v", err)
	}

	u2 := models.User{SubjectID: "u2", Username: "bob", ExternalKey: strPtr("casdoor:uuid-2"), CasdoorUniversalID: strPtr("uuid-2"), IsActive: true}
	if err := db.Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}

	if err := backfillProviderAwareExternalKeys(db, false); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var gotID1 models.UserAuthIdentity
	if err := db.First(&gotID1, "user_subject_id = ? AND provider = ?", "u1", "github").Error; err != nil {
		t.Fatalf("reload id1: %v", err)
	}
	if gotID1.ExternalKey != "casdoor:github:uuid-1" {
		t.Fatalf("expected identity key upgraded to casdoor:github:uuid-1, got %s", gotID1.ExternalKey)
	}

	var gotU1 models.User
	if err := db.First(&gotU1, "subject_id = ?", "u1").Error; err != nil {
		t.Fatalf("reload u1: %v", err)
	}
	if gotU1.ExternalKey == nil || *gotU1.ExternalKey != "casdoor:github:uuid-1" {
		t.Fatalf("expected user key upgraded to casdoor:github:uuid-1, got %v", gotU1.ExternalKey)
	}

	var gotU2 models.User
	if err := db.First(&gotU2, "subject_id = ?", "u2").Error; err != nil {
		t.Fatalf("reload u2: %v", err)
	}
	if gotU2.ExternalKey == nil || *gotU2.ExternalKey != "casdoor:uuid-2" {
		t.Fatalf("expected u2 key unchanged (no auth_provider), got %v", gotU2.ExternalKey)
	}
}
