package services

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// initLocalGitRepo creates a real local git repo with one commit containing a small
// plugin tree, so GitService.Clone's local-dir copy path can open it and return a
// real commit SHA. Returns the repo dir and the commit SHA.
func initLocalGitRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	mustWrite := func(rel string, content []byte, mode os.FileMode) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, content, mode); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mustWrite(".plugin.json", []byte(`{"name":"demo"}`), 0o644)
	mustWrite("hooks/run.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	mustWrite("README.md", []byte("# demo\n"), 0o644)

	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	sha, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return dir, sha.String()
}

func setupBundleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Inline SQLite schema for capability_artifacts (AutoMigrate can't handle the
	// Postgres-specific gen_random_uuid() default in the model tags).
	stmt := `CREATE TABLE capability_artifacts (
		id TEXT PRIMARY KEY,
		item_id TEXT NOT NULL,
		filename TEXT NOT NULL,
		file_size INTEGER NOT NULL,
		checksum_sha256 TEXT NOT NULL,
		mime_type TEXT,
		storage_backend TEXT DEFAULT 'local',
		storage_key TEXT NOT NULL,
		artifact_version TEXT NOT NULL,
		is_latest INTEGER DEFAULT 0,
		source_type TEXT DEFAULT 'upload',
		download_count INTEGER DEFAULT 0,
		uploaded_by TEXT NOT NULL,
		created_at DATETIME
	)`
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create capability_artifacts: %v", err)
	}
	return db
}

func newBundleService(t *testing.T, db *gorm.DB) *BundlePackService {
	t.Helper()
	store, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("local backend: %v", err)
	}
	git := &GitService{TempBaseDir: t.TempDir()}
	svc := NewBundlePackService(db, git, store, "")
	// Tests clone from a temp local git repo (no network); production never sets this.
	svc.AllowLocalClone = true
	return svc
}

func TestPackItemBundle_EndToEnd(t *testing.T) {
	repoDir, wantSHA := initLocalGitRepo(t)
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)

	item := &models.CapabilityItem{
		ID:        "item-1",
		Slug:      "demo-plugin",
		ItemType:  "plugin",
		SourceURL: "file://" + repoDir, // local repo -> GitService.Clone copies + PlainOpen
	}

	art, err := svc.PackItemBundle(context.Background(), item)
	if err != nil {
		t.Fatalf("PackItemBundle: %v", err)
	}

	if art.ArtifactVersion != wantSHA {
		t.Errorf("ArtifactVersion = %q, want commit SHA %q", art.ArtifactVersion, wantSHA)
	}
	if art.SourceType != bundleArtifactSourceType {
		t.Errorf("SourceType = %q, want %q", art.SourceType, bundleArtifactSourceType)
	}
	if !art.IsLatest {
		t.Error("artifact should be IsLatest")
	}
	if art.MimeType != bundleMimeType {
		t.Errorf("MimeType = %q, want %q", art.MimeType, bundleMimeType)
	}
	if art.Filename != "demo-plugin.zip" {
		t.Errorf("Filename = %q, want demo-plugin.zip", art.Filename)
	}
	if art.ChecksumSHA256 == "" {
		t.Error("ChecksumSHA256 empty")
	}

	// The stored bytes must be a valid ZIP that includes the plugin files (lossless),
	// excludes .git, and keeps the hook executable.
	reader, _, err := svc.Storage.Get(context.Background(), art.StorageKey)
	if err != nil {
		t.Fatalf("get stored bundle: %v", err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read stored zip: %v", err)
	}
	names := map[string]bool{}
	var hookExec bool
	for _, f := range zr.File {
		names[f.Name] = true
		if f.Name == "hooks/run.sh" {
			hookExec = f.Mode()&0o100 != 0
		}
		if f.Name == ".git" || hasPrefixSlash(f.Name, ".git/") {
			t.Errorf(".git leaked into stored bundle: %s", f.Name)
		}
	}
	for _, want := range []string{".plugin.json", "hooks/run.sh", "README.md"} {
		if !names[want] {
			t.Errorf("stored bundle missing %s", want)
		}
	}
	if !hookExec {
		t.Error("hooks/run.sh lost exec bit in stored bundle")
	}
}

func TestPackItemBundle_Idempotent(t *testing.T) {
	repoDir, _ := initLocalGitRepo(t)
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)

	item := &models.CapabilityItem{ID: "item-1", Slug: "demo", ItemType: "plugin", SourceURL: "file://" + repoDir}

	first, err := svc.PackItemBundle(context.Background(), item)
	if err != nil {
		t.Fatalf("first pack: %v", err)
	}
	second, err := svc.PackItemBundle(context.Background(), item)
	if err != nil {
		t.Fatalf("second pack: %v", err)
	}

	// Same commit SHA -> reused artifact, no new row.
	if first.ID != second.ID {
		t.Errorf("expected idempotent reuse, got distinct artifacts %s vs %s", first.ID, second.ID)
	}
	var count int64
	db.Model(&models.CapabilityArtifact{}).Where("item_id = ? AND source_type = ?", item.ID, bundleArtifactSourceType).Count(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 clone_pack artifact, got %d", count)
	}
}

// TestPackItemBundle_ReuseRepromotesNonLatest is the #3 regression guard: when the
// idempotent path reuses a cached clone_pack artifact that is NOT IsLatest (e.g. the
// upstream branch was reset/rolled back to a commit we packed before, so a
// newer-but-now-stale artifact holds IsLatest), the reuse MUST promote the cached
// artifact back to IsLatest (and demote the stale one) — otherwise list/detail keep
// selecting the wrong latest and the client pulls the wrong bundle.
func TestPackItemBundle_ReuseRepromotesNonLatest(t *testing.T) {
	repoDir, wantSHA := initLocalGitRepo(t)
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)

	item := &models.CapabilityItem{ID: "item-1", Slug: "demo", ItemType: "plugin", SourceURL: "file://" + repoDir}

	// First pack -> this commit's artifact, IsLatest=true.
	first, err := svc.PackItemBundle(context.Background(), item)
	if err != nil {
		t.Fatalf("first pack: %v", err)
	}
	if first.ArtifactVersion != wantSHA {
		t.Fatalf("first ArtifactVersion = %q, want %q", first.ArtifactVersion, wantSHA)
	}

	// Simulate a newer-but-stale clone_pack artifact taking over IsLatest (e.g. a
	// later commit that has since been reverted upstream). This demotes `first`.
	if err := db.Model(&models.CapabilityArtifact{}).
		Where("id = ?", first.ID).Update("is_latest", false).Error; err != nil {
		t.Fatalf("demote first: %v", err)
	}
	stale := &models.CapabilityArtifact{
		ID: "art-stale", ItemID: item.ID, Filename: "demo.zip", FileSize: 10,
		ChecksumSHA256: "stalesha", StorageKey: "k-stale", ArtifactVersion: "newer-but-reverted-sha",
		IsLatest: true, SourceType: bundleArtifactSourceType, UploadedBy: "system", CreatedAt: time.Now(),
	}
	if err := db.Create(stale).Error; err != nil {
		t.Fatalf("seed stale latest: %v", err)
	}

	// Re-pack at the SAME commit SHA as `first` -> idempotent reuse of `first`.
	second, err := svc.PackItemBundle(context.Background(), item)
	if err != nil {
		t.Fatalf("second pack: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected idempotent reuse of %s, got %s", first.ID, second.ID)
	}
	if !second.IsLatest {
		t.Error("reused artifact should have been re-promoted to IsLatest")
	}

	// The reused artifact is IsLatest in the DB; the stale one is demoted.
	var reloadedFirst, reloadedStale models.CapabilityArtifact
	if err := db.First(&reloadedFirst, "id = ?", first.ID).Error; err != nil {
		t.Fatalf("reload first: %v", err)
	}
	if err := db.First(&reloadedStale, "id = ?", "art-stale").Error; err != nil {
		t.Fatalf("reload stale: %v", err)
	}
	if !reloadedFirst.IsLatest {
		t.Error("first (correct-version) artifact should be IsLatest after re-pack")
	}
	if reloadedStale.IsLatest {
		t.Error("stale (reverted) artifact should have been demoted")
	}

	// Exactly one IsLatest clone_pack artifact for the item.
	var latestCount int64
	db.Model(&models.CapabilityArtifact{}).
		Where("item_id = ? AND source_type = ? AND is_latest = ?", item.ID, bundleArtifactSourceType, true).
		Count(&latestCount)
	if latestCount != 1 {
		t.Errorf("expected exactly 1 IsLatest clone_pack artifact, got %d", latestCount)
	}
}

func TestPackItemBundle_NoSourceURLErrors(t *testing.T) {
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)
	item := &models.CapabilityItem{ID: "item-1", Slug: "demo", ItemType: "plugin"}
	if _, err := svc.PackItemBundle(context.Background(), item); err == nil {
		t.Fatal("expected error when source_url is empty")
	}
}

// TestPackItemBundle_ExceedsMaxSizeFails verifies the OOM guard: when a packed
// bundle is larger than MaxBundleBytes the job fails with ErrBundleTooLarge and no
// artifact is written (so a huge/malicious repo can't OOM the worker).
func TestPackItemBundle_ExceedsMaxSizeFails(t *testing.T) {
	repoDir, _ := initLocalGitRepo(t)
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)
	svc.MaxBundleBytes = 1 // any real ZIP is larger than 1 byte

	item := &models.CapabilityItem{ID: "item-big", Slug: "big", ItemType: "plugin", SourceURL: "file://" + repoDir}

	_, err := svc.PackItemBundle(context.Background(), item)
	if err == nil {
		t.Fatal("expected an error when the bundle exceeds the size limit")
	}
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Errorf("expected ErrBundleTooLarge, got %v", err)
	}
	var count int64
	db.Model(&models.CapabilityArtifact{}).Where("item_id = ?", "item-big").Count(&count)
	if count != 0 {
		t.Errorf("no artifact should be written when oversized, got %d", count)
	}
}

// TestPackUploadBundle_ExceedsMaxSizeFails verifies the same OOM guard on the
// synchronous upload-pack path (asset reconstruction, no clone).
func TestPackUploadBundle_ExceedsMaxSizeFails(t *testing.T) {
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)
	svc.MaxBundleBytes = 1

	// Inline assets table so packAssetsZip has something to read.
	if err := db.Exec(`CREATE TABLE capability_assets (
		id TEXT PRIMARY KEY, item_id TEXT NOT NULL, rel_path TEXT NOT NULL,
		text_content TEXT, storage_backend TEXT, storage_key TEXT, mime_type TEXT,
		file_size INTEGER, content_sha TEXT, created_at DATETIME, updated_at DATETIME)`).Error; err != nil {
		t.Fatalf("create capability_assets: %v", err)
	}
	text := "#!/bin/sh\necho hi\n"
	db.Create(&models.CapabilityAsset{ID: "a1", ItemID: "item-u", RelPath: "hooks/run.sh", TextContent: &text})

	item := &models.CapabilityItem{ID: "item-u", Slug: "u", ItemType: "plugin"}
	_, err := svc.PackUploadBundle(context.Background(), item)
	if err == nil {
		t.Fatal("expected an error when the upload bundle exceeds the size limit")
	}
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Errorf("expected ErrBundleTooLarge, got %v", err)
	}
}

// TestPackItemBundle_CloneTimeoutCancels verifies a clone context timeout aborts
// the pack (the local-copy path ignores ctx, so we trip it with a remote URL and a
// zero-deadline context-equivalent: a 1ns timeout that expires before the network
// dial). Production sets CloneTimeout from BUNDLE_PACK_TIMEOUT.
func TestPackItemBundle_CloneTimeoutCancels(t *testing.T) {
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)
	svc.AllowLocalClone = false // force the remote PlainCloneContext path
	svc.CloneTimeout = time.Nanosecond

	item := &models.CapabilityItem{
		ID: "item-to", Slug: "to", ItemType: "plugin",
		// Unroutable test address; the 1ns timeout fires before any real network IO.
		SourceURL: "https://github.com/owner/repo/tree/main",
	}
	_, err := svc.PackItemBundle(context.Background(), item)
	if err == nil {
		t.Fatal("expected clone to fail under a 1ns timeout")
	}
	var count int64
	db.Model(&models.CapabilityArtifact{}).Where("item_id = ?", "item-to").Count(&count)
	if count != 0 {
		t.Errorf("no artifact should be written on a timed-out clone, got %d", count)
	}
}

// TestPackItemBundle_RejectsNonHTTPSource verifies the defense-in-depth guard:
// a file:// (or otherwise non-http) source_url must be refused BEFORE any clone, so
// a malicious/compromised catalog entry can't make the backend clone a repo off its
// own filesystem and republish it as a public bundle.
func TestPackItemBundle_RejectsNonHTTPSource(t *testing.T) {
	repoDir, _ := initLocalGitRepo(t)
	db := setupBundleTestDB(t)
	svc := newBundleService(t, db)
	svc.AllowLocalClone = false // production posture: refuse file:// / local paths
	item := &models.CapabilityItem{
		ID: "item-evil", Slug: "evil", ItemType: "plugin",
		SourceURL: "file://" + repoDir,
	}
	_, err := svc.PackItemBundle(context.Background(), item)
	if err == nil {
		t.Fatal("expected file:// source_url to be refused, but pack succeeded")
	}
	// And nothing should have been written.
	var count int64
	db.Model(&models.CapabilityArtifact{}).Where("item_id = ?", "item-evil").Count(&count)
	if count != 0 {
		t.Errorf("expected no artifact for a refused source, got %d", count)
	}
}
