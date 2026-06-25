package services

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// addAssetsSchema adds the capability_assets table to a bundle test DB (which only
// ships capability_artifacts by default), so PackUploadBundle's asset reconstruction
// can be exercised.
func addAssetsSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	stmt := `CREATE TABLE capability_assets (
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
	)`
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create capability_assets: %v", err)
	}
}

func TestPackUploadBundle_FromTextAndBinaryAssets(t *testing.T) {
	db := setupBundleTestDB(t)
	addAssetsSchema(t, db)
	svc := newBundleService(t, db)
	ctx := context.Background()

	// One binary asset stored in the backend, one inline text asset.
	binKey := "public/item-1/v1/hooks/tool.bin"
	binData := []byte{0x00, 0x01, 0x02, 0xff, 0x10}
	if err := svc.Storage.Put(ctx, binKey, bytes.NewReader(binData), int64(len(binData))); err != nil {
		t.Fatalf("put binary asset: %v", err)
	}

	text := "#!/bin/sh\necho hi\n"
	assets := []models.CapabilityAsset{
		{ID: "a1", ItemID: "item-1", RelPath: "hooks/run.sh", TextContent: &text},
		{ID: "a2", ItemID: "item-1", RelPath: "hooks/tool.bin", StorageKey: binKey},
	}
	if err := db.Create(&assets).Error; err != nil {
		t.Fatalf("seed assets: %v", err)
	}

	item := &models.CapabilityItem{
		ID:         "item-1",
		Slug:       "uploaded-plugin",
		ItemType:   "plugin",
		Content:    `{"name":"uploaded"}`,
		SourcePath: ".plugin.json",
		// No SourceURL: this is the uploaded-plugin path.
	}

	art, err := svc.PackUploadBundle(ctx, item)
	if err != nil {
		t.Fatalf("PackUploadBundle: %v", err)
	}

	if art.SourceType != BundleSourceTypeUploadPack {
		t.Errorf("SourceType = %q, want %q", art.SourceType, BundleSourceTypeUploadPack)
	}
	if !art.IsLatest {
		t.Error("artifact should be IsLatest")
	}
	if art.ArtifactVersion != art.ChecksumSHA256 {
		t.Errorf("upload bundle version (%q) should equal content hash (%q)", art.ArtifactVersion, art.ChecksumSHA256)
	}
	if art.MimeType != bundleMimeType {
		t.Errorf("MimeType = %q, want %q", art.MimeType, bundleMimeType)
	}

	// Verify the stored ZIP is lossless: both assets + the fallback main file.
	reader, _, err := svc.Storage.Get(ctx, art.StorageKey)
	if err != nil {
		t.Fatalf("get stored bundle: %v", err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read stored zip: %v", err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = b
	}
	if string(got["hooks/run.sh"]) != text {
		t.Errorf("hooks/run.sh content = %q, want %q", string(got["hooks/run.sh"]), text)
	}
	if !bytes.Equal(got["hooks/tool.bin"], binData) {
		t.Errorf("hooks/tool.bin content mismatch")
	}
	if string(got[".plugin.json"]) != item.Content {
		t.Errorf("fallback main file = %q, want %q", string(got[".plugin.json"]), item.Content)
	}
}

func TestPackUploadBundle_Idempotent(t *testing.T) {
	db := setupBundleTestDB(t)
	addAssetsSchema(t, db)
	svc := newBundleService(t, db)
	ctx := context.Background()

	text := "hello"
	asset := models.CapabilityAsset{ID: "a1", ItemID: "item-1", RelPath: "file.txt", TextContent: &text}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	item := &models.CapabilityItem{ID: "item-1", Slug: "demo", ItemType: "plugin"}

	first, err := svc.PackUploadBundle(ctx, item)
	if err != nil {
		t.Fatalf("first pack: %v", err)
	}
	second, err := svc.PackUploadBundle(ctx, item)
	if err != nil {
		t.Fatalf("second pack: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("expected idempotent reuse, got %s vs %s", first.ID, second.ID)
	}
	var count int64
	db.Model(&models.CapabilityArtifact{}).Where("item_id = ? AND source_type = ?", item.ID, BundleSourceTypeUploadPack).Count(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 upload_pack artifact, got %d", count)
	}
}
