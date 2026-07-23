package services

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
)

func writeCatalogAsset(t *testing.T, bundleDir, entryID, relPath string, content []byte) {
	t.Helper()
	absPath := filepath.Join(bundleDir, "catalog-download", "skills", entryID, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("mkdir asset parent: %v", err)
	}
	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		t.Fatalf("write asset %s: %v", relPath, err)
	}
}

func loadCatalogAssets(t *testing.T, svc *CatalogIngestService, itemID string) map[string]models.CapabilityAsset {
	t.Helper()
	var assets []models.CapabilityAsset
	if err := svc.DB.Where("item_id = ?", itemID).Find(&assets).Error; err != nil {
		t.Fatalf("load assets: %v", err)
	}
	byPath := make(map[string]models.CapabilityAsset, len(assets))
	for _, asset := range assets {
		byPath[asset.RelPath] = asset
	}
	return byPath
}

func TestCatalogIngestSyncsCompleteSkillDirectory(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	entry := catalogEntry{ID: "asset-skill", Type: "skill", Description: "assets"}
	body := "---\nname: Asset Skill\ndescription: assets\n---\n# Asset Skill\n"
	dir := writeSkillBundle(t, entry, body)

	writeCatalogAsset(t, dir, entry.ID, "scripts/setup.sh", []byte("#!/bin/sh\necho setup\n"))
	writeCatalogAsset(t, dir, entry.ID, "references/guide.md", []byte("# Guide\n"))
	writeCatalogAsset(t, dir, entry.ID, "examples/nested/SKILL.md", []byte("# Nested helper\n"))

	result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Failed != 0 {
		t.Fatalf("asset ingest failed: %#v", result.Errors)
	}

	item := loadItemBySlug(t, db, entry.ID)
	assets := loadCatalogAssets(t, svc, item.ID)
	for _, path := range []string{"scripts/setup.sh", "references/guide.md", "examples/nested/SKILL.md"} {
		if _, ok := assets[path]; !ok {
			t.Errorf("asset %q was not ingested; got paths %#v", path, assets)
		}
	}
	if len(assets) != 3 {
		t.Fatalf("expected 3 assets, got %d: %#v", len(assets), assets)
	}
}

func TestCatalogIngestReconcilesAssetsWhenPrimaryIsUnchanged(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	entry := catalogEntry{ID: "asset-update", Type: "skill", Description: "assets"}
	body := "---\nname: Asset Update\ndescription: assets\n---\n# Asset Update\n"

	firstDir := writeSkillBundle(t, entry, body)
	writeCatalogAsset(t, firstDir, entry.ID, "scripts/run.sh", []byte("echo v1\n"))
	writeCatalogAsset(t, firstDir, entry.ID, "references/removed.md", []byte("remove me\n"))
	if result, err := svc.Ingest(context.Background(), IngestSource{Dir: firstDir}, IngestOptions{TriggerUser: "tester"}); err != nil || result.Failed != 0 {
		t.Fatalf("first ingest: result=%+v err=%v", result, err)
	}

	secondDir := writeSkillBundle(t, entry, body)
	writeCatalogAsset(t, secondDir, entry.ID, "scripts/run.sh", []byte("echo v2\n"))
	writeCatalogAsset(t, secondDir, entry.ID, "references/added.md", []byte("new file\n"))
	result, err := svc.Ingest(context.Background(), IngestSource{Dir: secondDir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if result.Failed != 0 {
		t.Fatalf("asset reconcile failed: %#v", result.Errors)
	}

	item := loadItemBySlug(t, db, entry.ID)
	assets := loadCatalogAssets(t, svc, item.ID)
	if len(assets) != 2 {
		t.Fatalf("expected 2 reconciled assets, got %d: %#v", len(assets), assets)
	}
	run := assets["scripts/run.sh"]
	if run.TextContent == nil || *run.TextContent != "echo v2\n" {
		t.Fatalf("script was not updated: %+v", run)
	}
	if _, ok := assets["references/removed.md"]; ok {
		t.Fatal("removed upstream asset still exists")
	}
	if _, ok := assets["references/added.md"]; !ok {
		t.Fatal("new upstream asset was not added")
	}
}

func TestCatalogIngestStoresBinarySkillAssets(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	backend, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create storage backend: %v", err)
	}
	svc.Storage = &storage.ConfiguredBackend{Kind: storage.KindS3, Backend: backend}

	entry := catalogEntry{ID: "binary-asset", Type: "skill", Description: "binary"}
	body := "---\nname: Binary Asset\ndescription: binary\n---\n# Binary Asset\n"
	dir := writeSkillBundle(t, entry, body)
	png := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02}
	writeCatalogAsset(t, dir, entry.ID, "assets/icon.png", png)

	result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Failed != 0 {
		t.Fatalf("binary asset ingest failed: %#v", result.Errors)
	}

	item := loadItemBySlug(t, db, entry.ID)
	asset := loadCatalogAssets(t, svc, item.ID)["assets/icon.png"]
	if asset.TextContent != nil || asset.StorageKey == "" {
		t.Fatalf("binary asset was not stored externally: %+v", asset)
	}
	if asset.StorageBackend != storage.KindS3 {
		t.Fatalf("binary asset backend = %q, want %q", asset.StorageBackend, storage.KindS3)
	}
	if !strings.Contains(asset.StorageKey, "/"+asset.ContentSHA+"/") {
		t.Fatalf("binary asset key %q is not content addressed by %q", asset.StorageKey, asset.ContentSHA)
	}
	reader, _, err := backend.Get(context.Background(), asset.StorageKey)
	if err != nil {
		t.Fatalf("read stored binary: %v", err)
	}
	defer reader.Close()
	stored, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stored binary content: %v", err)
	}
	if len(stored) != len(png) {
		t.Fatalf("stored binary length = %d, want %d", len(stored), len(png))
	}
	for i := range png {
		if stored[i] != png[i] {
			t.Fatalf("stored binary differs at byte %d: got %v want %v", i, stored, png)
		}
	}
}

func TestCatalogIngestRewritesBinaryWhenBackendChanges(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	localBackend, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.Storage = &storage.ConfiguredBackend{Kind: storage.KindLocal, Backend: localBackend}

	entry := catalogEntry{ID: "backend-change", Type: "skill", Description: "backend change"}
	body := "---\nname: Backend Change\ndescription: backend change\n---\n# Backend Change\n"
	dir := writeSkillBundle(t, entry, body)
	png := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02}
	writeCatalogAsset(t, dir, entry.ID, "assets/icon.png", png)

	if result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil || result.Failed != 0 {
		t.Fatalf("local ingest: result=%+v err=%v", result, err)
	}
	item := loadItemBySlug(t, db, entry.ID)
	first := loadCatalogAssets(t, svc, item.ID)["assets/icon.png"]
	if first.StorageBackend != storage.KindLocal {
		t.Fatalf("first backend = %q, want local", first.StorageBackend)
	}

	s3Objects, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.Storage = &storage.ConfiguredBackend{Kind: storage.KindS3, Backend: s3Objects}
	if result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil || result.Failed != 0 {
		t.Fatalf("s3 ingest: result=%+v err=%v", result, err)
	}

	rewritten := loadCatalogAssets(t, svc, item.ID)["assets/icon.png"]
	if rewritten.StorageBackend != storage.KindS3 {
		t.Fatalf("rewritten backend = %q, want s3", rewritten.StorageBackend)
	}
	reader, _, err := s3Objects.Get(context.Background(), rewritten.StorageKey)
	if err != nil {
		t.Fatalf("binary was not written to the new backend: %v", err)
	}
	defer reader.Close()
	stored, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(png) {
		t.Fatalf("rewritten binary = %v, want %v", stored, png)
	}
}

func TestCatalogIngestDoesNotReuseBinaryWithoutConfiguredBackend(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	localBackend, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.Storage = &storage.ConfiguredBackend{Kind: storage.KindLocal, Backend: localBackend}

	entry := catalogEntry{ID: "missing-backend", Type: "skill", Description: "missing backend"}
	body := "---\nname: Missing Backend\ndescription: missing backend\n---\n# Missing Backend\n"
	dir := writeSkillBundle(t, entry, body)
	writeCatalogAsset(t, dir, entry.ID, "assets/icon.png", []byte{0x89, 'P', 'N', 'G', 0x00})

	if result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil || result.Failed != 0 {
		t.Fatalf("initial ingest: result=%+v err=%v", result, err)
	}

	svc.Storage = nil
	result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if result.Failed == 0 {
		t.Fatalf("expected missing backend to reject binary reuse, got result=%+v", result)
	}
	if !strings.Contains(strings.Join(result.Errors, "\n"), "storage backend is not configured") {
		t.Fatalf("expected missing backend error, got %v", result.Errors)
	}
}
