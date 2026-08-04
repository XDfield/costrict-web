package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type failingSyncStorage struct{}

func (failingSyncStorage) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("object store unavailable")
}

func (failingSyncStorage) Get(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("object store unavailable")
}

func TestParseFile_DispatchesPluginJSON(t *testing.T) {
	s := &SyncService{Parser: &ParserService{}}
	content := []byte(`{
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "foo",
    "marketplace_name": "mp",
    "marketplace_repo": "o/r",
    "marketplace_verified": true
  }
}`)
	items, err := s.parseFile(content, "plugins/foo-bar/.plugin.json")
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 ParsedItem, got %d", len(items))
	}
	if items[0].ItemType != "plugin" {
		t.Errorf("ItemType = %q, want plugin (dispatched to wrong parser)", items[0].ItemType)
	}
	if items[0].Slug != "foo-bar" {
		t.Errorf("Slug = %q, want foo-bar (from dir name)", items[0].Slug)
	}
}

func TestSyncAssetsStoresBinaryInConfiguredBackend(t *testing.T) {
	db := newIngestTestDB(t)
	repoDir := t.TempDir()
	itemDir := filepath.Join(repoDir, "skills", "image-skill")
	if err := os.MkdirAll(filepath.Join(itemDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "SKILL.md"), []byte("# Image skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	png := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02}
	if err := os.WriteFile(filepath.Join(itemDir, "assets", "icon.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "notes.md"), []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := &SyncService{
		DB:      db,
		Git:     &GitService{},
		Storage: &storage.ConfiguredBackend{Kind: storage.KindS3, Backend: local},
	}
	legacyText := string(png)
	legacy := &models.CapabilityAsset{
		ID:          "legacy-binary",
		ItemID:      "item-1",
		RelPath:     "assets/icon.png",
		TextContent: &legacyText,
		MimeType:    "image/png",
		FileSize:    int64(len(png)),
		ContentSHA:  sha256Hex(png),
	}
	if err := db.Create(legacy).Error; err != nil {
		t.Fatal(err)
	}
	var syncErrors []string
	failures := svc.syncAssets(context.Background(), repoDir, "skills/image-skill/SKILL.md", "item-1", &syncErrors)
	if failures != 0 {
		t.Fatalf("sync asset failures: %d", failures)
	}
	if len(syncErrors) != 0 {
		t.Fatalf("sync assets errors: %v", syncErrors)
	}

	var assets []models.CapabilityAsset
	if err := db.Where("item_id = ?", "item-1").Order("rel_path").Find(&assets).Error; err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %#v", assets)
	}
	var binary, text models.CapabilityAsset
	for _, asset := range assets {
		if asset.RelPath == "assets/icon.png" {
			binary = asset
		} else if asset.RelPath == "notes.md" {
			text = asset
		}
	}
	if binary.TextContent != nil || binary.StorageBackend != storage.KindS3 || binary.StorageKey == "" {
		t.Fatalf("legacy binary asset not migrated to external storage: %+v", binary)
	}
	if !strings.Contains(binary.StorageKey, "/"+binary.ContentSHA+"/") {
		t.Fatalf("binary key %q is not content addressed", binary.StorageKey)
	}
	reader, _, err := local.Get(context.Background(), binary.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	stored, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, png) {
		t.Fatalf("stored binary = %v, want %v", stored, png)
	}
	if text.TextContent == nil || *text.TextContent != "plain text" ||
		text.StorageBackend != "" || text.StorageKey != "" {
		t.Fatalf("text asset should remain in DB: %+v", text)
	}
}

func TestSyncAssetsCountsPutFailure(t *testing.T) {
	db := newIngestTestDB(t)
	repoDir := t.TempDir()
	itemDir := filepath.Join(repoDir, "skills", "image-skill")
	if err := os.MkdirAll(filepath.Join(itemDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "SKILL.md"), []byte("# Image skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "assets", "icon.png"), []byte{0x89, 'P', 'N', 'G', 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &SyncService{
		DB:      db,
		Git:     &GitService{},
		Storage: &storage.ConfiguredBackend{Kind: storage.KindS3, Backend: failingSyncStorage{}},
	}
	var syncErrors []string
	failures := svc.syncAssets(context.Background(), repoDir, "skills/image-skill/SKILL.md", "item-1", &syncErrors)
	if failures != 1 || len(syncErrors) != 1 || !strings.Contains(syncErrors[0], "object store unavailable") {
		t.Fatalf("expected one reported Put failure, failures=%d errors=%v", failures, syncErrors)
	}
	var count int64
	if err := db.Model(&models.CapabilityAsset{}).Where("item_id = ?", "item-1").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed Put must not create DB asset mapping, count=%d", count)
	}

	result := &SyncResult{Failed: failures}
	updates, retryErr := completeSyncResult(result, "commit-sha", time.Now())
	if retryErr == nil || result.Status != "failed" {
		t.Fatalf("partial asset failure must be retryable: status=%q err=%v", result.Status, retryErr)
	}
	if _, advanced := updates["last_sync_sha"]; advanced {
		t.Fatalf("failed sync must not advance last_sync_sha: %v", updates)
	}
}

func TestSyncAssetsDeletesStaleRowsAfterSuccessfulSync(t *testing.T) {
	db := newIngestTestDB(t)
	repoDir := t.TempDir()
	itemDir := filepath.Join(repoDir, "skills", "cleanup-skill")
	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "SKILL.md"), []byte("# Cleanup skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "current.md"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	staleText := "stale"
	for _, asset := range []*models.CapabilityAsset{
		{
			ID:          "current-asset",
			ItemID:      "item-1",
			RelPath:     "current.md",
			TextContent: &staleText,
			ContentSHA:  "outdated",
		},
		{
			ID:          "stale-asset",
			ItemID:      "item-1",
			RelPath:     "removed.md",
			TextContent: &staleText,
			ContentSHA:  sha256Hex([]byte(staleText)),
		},
	} {
		if err := db.Create(asset).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := &SyncService{DB: db, Git: &GitService{}}
	var syncErrors []string
	failures := svc.syncAssets(
		context.Background(),
		repoDir,
		"skills/cleanup-skill/SKILL.md",
		"item-1",
		&syncErrors,
	)
	if failures != 0 || len(syncErrors) != 0 {
		t.Fatalf("sync asset failures=%d errors=%v", failures, syncErrors)
	}

	var assets []models.CapabilityAsset
	if err := db.Where("item_id = ?", "item-1").Find(&assets).Error; err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].RelPath != "current.md" {
		t.Fatalf("stale asset row was not removed: %+v", assets)
	}
	if assets[0].TextContent == nil || *assets[0].TextContent != "current" {
		t.Fatalf("current asset was not refreshed: %+v", assets[0])
	}
}

func newSyncRegistryGuardDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE capability_registries (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT,
			source_type TEXT NOT NULL DEFAULT 'internal', external_url TEXT,
			external_branch TEXT DEFAULT 'main', sync_enabled INTEGER DEFAULT 0,
			sync_interval INTEGER DEFAULT 3600, last_synced_at DATETIME,
			last_sync_sha TEXT, sync_status TEXT DEFAULT 'idle',
			sync_config TEXT DEFAULT '{}', last_sync_log_id TEXT,
			repo_id TEXT, owner_id TEXT NOT NULL,
			created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE sync_logs (
			id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, trigger_type TEXT, trigger_user TEXT,
			previous_sha TEXT, commit_sha TEXT, status TEXT, added_items INTEGER, updated_items INTEGER,
			deleted_items INTEGER, skipped_items INTEGER, failed_items INTEGER, error_message TEXT,
			duration_ms INTEGER, started_at DATETIME, finished_at DATETIME
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// Git-backed registries belong to the push-webhook pipeline. The legacy clone
// sync must refuse them even when something outside the scheduler (a stale
// queued job, a manual admin trigger) hands one over, because its closing
// sweep archives every registry row whose source path it cannot match.
func TestSyncRegistryRejectsGitBackedRegistry(t *testing.T) {
	db := newSyncRegistryGuardDB(t)
	if err := db.Exec(
		`INSERT INTO capability_registries (id, name, source_type, external_url, sync_enabled, owner_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"git-registry", "u-alice/plugin", "git", "https://git.example/u-alice/plugin", true, "owner-1",
	).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// Git is nil on purpose: reaching the clone would panic rather than
	// silently pass.
	svc := &SyncService{DB: db}
	if _, err := svc.SyncRegistry(context.Background(), "git-registry", SyncOptions{TriggerType: "scheduled"}); err == nil {
		t.Fatal("legacy sync accepted a Git-backed registry")
	}

	var logCount int64
	if err := db.Table("sync_logs").Count(&logCount).Error; err != nil {
		t.Fatalf("count sync logs: %v", err)
	}
	if logCount != 0 {
		t.Errorf("rejected sync still opened a sync log (%d rows)", logCount)
	}

	var syncStatus string
	if err := db.Table("capability_registries").
		Where("id = ?", "git-registry").
		Select("sync_status").Scan(&syncStatus).Error; err != nil {
		t.Fatalf("read sync status: %v", err)
	}
	if syncStatus != "idle" {
		t.Errorf("rejected sync mutated sync_status to %q", syncStatus)
	}
}
