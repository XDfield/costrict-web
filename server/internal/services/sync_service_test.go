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
