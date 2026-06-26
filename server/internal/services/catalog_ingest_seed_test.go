package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newSeedIngestDB extends newIngestTestDB's schema with the capability_artifacts
// table the offline-seed path writes to. (newIngestTestDB on its own doesn't create
// it because the online ingest path never touches artifacts.)
func newSeedIngestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newIngestTestDB(t)
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

// seedTestService builds a CatalogIngestService wired for the offline-seed path: a
// LocalBackend over a temp dir + a bare GitService (the seed path never clones). A
// BundleJobService is included so the test can assert it is NOT enqueued for seeded
// plugins (seed is terminal).
func seedTestService(t *testing.T, db *gorm.DB) (*CatalogIngestService, *storage.LocalBackend) {
	t.Helper()
	store, err := storage.NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("local backend: %v", err)
	}
	svc := newIngestService(db)
	svc.BundleJobService = &BundleJobService{DB: db}
	svc.Store = store
	svc.Git = &GitService{}
	return svc, store
}

// pluginZipFiles is the lossless file set baked into the offline plugin ZIP. It
// deliberately includes a hooks file, a helper script, a binary asset, and a nested
// subdirectory — exactly the non-typed files that the lossy DB reconstruction
// (syncAssetsForItem no-op) would drop, which is the whole point of seeding the
// pre-packed ZIP.
var pluginZipFiles = map[string][]byte{
	".plugin.json":           []byte(`{"name":"Demo Plugin","install":{"plugin_name":"demo","marketplace_name":"costrict","marketplace_repo":"owner/demo"}}`),
	"hooks/hooks.json":       []byte(`{"hooks":[{"event":"PostToolUse","command":"scripts/run.py"}]}`),
	"scripts/run.py":         []byte("#!/usr/bin/env python3\nprint('hi')\n"),
	"assets/logo.bin":        {0x00, 0x01, 0x02, 0xff, 0xfe},
	"commands/nested/cmd.md": []byte("# nested command\n"),
	"README.md":              []byte("# Demo Plugin\n"),
}

// buildPluginBundleZip packs pluginZipFiles into a deterministic ZIP (sorted, fixed
// modtime) and returns the bytes + sha256 hex.
func buildPluginBundleZip(t *testing.T) ([]byte, string) {
	t.Helper()
	rels := make([]string, 0, len(pluginZipFiles))
	for rel := range pluginZipFiles {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, rel := range rels {
		w, err := zw.Create(rel)
		if err != nil {
			t.Fatalf("zip create %s: %v", rel, err)
		}
		if _, err := w.Write(pluginZipFiles[rel]); err != nil {
			t.Fatalf("zip write %s: %v", rel, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, fmt.Sprintf("%x", sum)
}

// writePluginSeedBundle materializes a one-entry catalog bundle for a PLUGIN whose
// index entry carries a `bundle` block pointing at a pre-packed ZIP under
// catalog-download/plugins/<id>/bundle.zip. withBundle=false omits the block (and the
// ZIP) so callers can exercise the lazy-clone fallback. Returns the bundle dir, the
// ZIP bytes, the ZIP sha, and the version key written into the block.
func writePluginSeedBundle(t *testing.T, entryID, version string, withBundle bool, sha256Override string) (dir string, zipBytes []byte, zipSHA, bundleVersion string) {
	t.Helper()
	dir = t.TempDir()

	zipBytes, zipSHA = buildPluginBundleZip(t)

	entry := map[string]any{
		"id":          entryID,
		"type":        "plugin",
		"source":      "claude-plugins-dev",
		"source_url":  "https://github.com/owner/demo/tree/main",
		"description": "A demo plugin",
		"category":    "tooling",
	}
	if withBundle {
		block := map[string]any{
			"file":    filepath.ToSlash(filepath.Join("catalog-download", "plugins", entryID, "bundle.zip")),
			"version": version,
		}
		if sha256Override != "" {
			block["sha256"] = sha256Override
		} else {
			block["sha256"] = zipSHA
		}
		entry["bundle"] = block
		bundleVersion = version
	}

	manifest := map[string]any{
		"schema_version": SupportedBundleSchemaVersion,
		"generated_at":   "2026-06-25T00:00:00Z",
		"entry_count":    1,
		"index_sha256":   "test-sha",
		"type_counts":    map[string]int{"plugin": 1},
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	ib, _ := json.Marshal([]map[string]any{entry})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), ib, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	// Primary plugin file (.plugin.json) that the normal ingest reads.
	pluginDir := filepath.Join(dir, "catalog-download", "plugins", entryID)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, ".plugin.json"), pluginZipFiles[".plugin.json"], 0o644); err != nil {
		t.Fatalf("write .plugin.json: %v", err)
	}
	// Pre-packed bundle ZIP, sitting alongside the primary file (only when present).
	if withBundle {
		if err := os.WriteFile(filepath.Join(pluginDir, "bundle.zip"), zipBytes, 0o644); err != nil {
			t.Fatalf("write bundle.zip: %v", err)
		}
	}
	return dir, zipBytes, zipSHA, bundleVersion
}

func loadArtifacts(t *testing.T, db *gorm.DB, itemID string) []models.CapabilityArtifact {
	t.Helper()
	var arts []models.CapabilityArtifact
	if err := db.Where("item_id = ?", itemID).Order("created_at asc").Find(&arts).Error; err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	return arts
}

func countBundleJobs(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Table("bundle_jobs").Count(&n).Error; err != nil {
		// bundle_jobs table may not exist in this minimal schema; treat as 0 (the
		// enqueue path itself would have failed loudly elsewhere). We create it below.
		t.Fatalf("count bundle_jobs: %v", err)
	}
	return n
}

// createBundleJobsTable adds the minimal bundle_jobs schema so the test can assert
// whether a refresh was enqueued (seeded → none).
func createBundleJobsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	stmt := `CREATE TABLE bundle_jobs (
		id TEXT PRIMARY KEY,
		item_id TEXT NOT NULL,
		trigger_type TEXT NOT NULL DEFAULT 'sync',
		trigger_user TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		retry_count INTEGER DEFAULT 0,
		max_attempts INTEGER DEFAULT 3,
		last_error TEXT,
		artifact_id TEXT,
		scheduled_at DATETIME NOT NULL,
		started_at DATETIME,
		finished_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME
	)`
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create bundle_jobs: %v", err)
	}
}

// TestIngest_SeedsBundleArtifact_FromOfflineBundle is the core PR5 acceptance: a
// plugin entry carrying a `bundle` block is ingested, and the offline ZIP is written
// as a `seeded` CapabilityArtifact (IsLatest, correct version/sha), WITHOUT any clone
// and WITHOUT enqueuing a refresh job. It then verifies the stored ZIP is lossless.
func TestIngest_SeedsBundleArtifact_FromOfflineBundle(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, store := seedTestService(t, db)

	const version = "deadbeefcafe1234"
	dir, wantZip, wantSHA, _ := writePluginSeedBundle(t, "demo", version, true, "")

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("expected Added=1, got added=%d updated=%d failed=%d errors=%v", res.Added, res.Updated, res.Failed, res.Errors)
	}

	item := loadItemBySlug(t, db, "demo")
	arts := loadArtifacts(t, db, item.ID)
	if len(arts) != 1 {
		t.Fatalf("expected exactly 1 artifact, got %d: %+v", len(arts), arts)
	}
	a := arts[0]
	if a.SourceType != BundleSourceTypeSeeded {
		t.Errorf("SourceType = %q, want %q", a.SourceType, BundleSourceTypeSeeded)
	}
	if a.ArtifactVersion != version {
		t.Errorf("ArtifactVersion = %q, want %q (whole-bundle truth, not semver)", a.ArtifactVersion, version)
	}
	if a.ChecksumSHA256 != wantSHA {
		t.Errorf("ChecksumSHA256 = %q, want %q", a.ChecksumSHA256, wantSHA)
	}
	if !a.IsLatest {
		t.Error("seeded artifact should be IsLatest")
	}
	if a.UploadedBy != seedBundleUploader {
		t.Errorf("UploadedBy = %q, want %q", a.UploadedBy, seedBundleUploader)
	}
	if a.MimeType != bundleMimeType {
		t.Errorf("MimeType = %q, want %q", a.MimeType, bundleMimeType)
	}
	if a.FileSize != int64(len(wantZip)) {
		t.Errorf("FileSize = %d, want %d", a.FileSize, len(wantZip))
	}

	// No clone happened (Git has no TempBaseDir and was never invoked) and NO refresh
	// job was enqueued — seed is terminal.
	if n := countBundleJobs(t, db); n != 0 {
		t.Errorf("expected 0 bundle jobs after seeding, got %d (seed must not enqueue a clone refresh)", n)
	}

	// Stored ZIP is byte-identical to the offline ZIP (lossless).
	reader, _, err := store.Get(context.Background(), a.StorageKey)
	if err != nil {
		t.Fatalf("store get %s: %v", a.StorageKey, err)
	}
	defer reader.Close()
	got, _ := io.ReadAll(reader)
	if !bytes.Equal(got, wantZip) {
		t.Fatalf("stored bundle is not byte-identical to offline ZIP (got %d bytes, want %d)", len(got), len(wantZip))
	}

	// And the ZIP content is lossless: every file (hooks/scripts/binary/nested) is
	// present and unmodified.
	assertZipMatches(t, got, pluginZipFiles)
}

// assertZipMatches verifies the ZIP bytes decompress to exactly want (path -> bytes).
func assertZipMatches(t *testing.T, zipBytes []byte, want map[string][]byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = b
	}
	if len(got) != len(want) {
		t.Fatalf("zip has %d entries, want %d (got=%v)", len(got), len(want), keysOf(got))
	}
	for rel, wantBytes := range want {
		gb, ok := got[rel]
		if !ok {
			t.Errorf("zip missing %q", rel)
			continue
		}
		if !bytes.Equal(gb, wantBytes) {
			t.Errorf("zip entry %q content mismatch", rel)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestIngest_SeedIsIdempotent_OnReimport verifies re-importing the SAME offline
// bundle is a no-op for the artifact (same version → reuse, no duplicate row).
func TestIngest_SeedIsIdempotent_OnReimport(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, _ := seedTestService(t, db)

	const version = "v-stable-001"
	dir, _, _, _ := writePluginSeedBundle(t, "demo", version, true, "")

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	item := loadItemBySlug(t, db, "demo")
	if got := len(loadArtifacts(t, db, item.ID)); got != 1 {
		t.Fatalf("after first import expected 1 artifact, got %d", got)
	}

	// Re-import the identical bundle (same content sha → metadata-only path for the
	// item; the seed idempotency still guards a fresh write if the content path runs).
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	arts := loadArtifacts(t, db, item.ID)
	if len(arts) != 1 {
		t.Fatalf("re-import should not duplicate the seeded artifact; got %d", len(arts))
	}
	if arts[0].ArtifactVersion != version {
		t.Errorf("artifact version drifted on re-import: %q", arts[0].ArtifactVersion)
	}
}

// TestIngest_MetadataOnly_ReseedsChangedBundleVersion is the #4 regression guard: an
// offline plugin whose pre-packed bundle changed (new version) while its primary file
// SHA stayed identical routes through the METADATA-ONLY path. Without the re-seed,
// air-gap clients stall on the old bundle. This verifies the metadata-only path
// detects the version drift and writes a new seeded artifact (IsLatest flips).
func TestIngest_MetadataOnly_ReseedsChangedBundleVersion(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, _ := seedTestService(t, db)
	ctx := context.Background()

	// First ingest: version v1 (same primary .plugin.json content as v2 below).
	dir1, _, _, _ := writePluginSeedBundle(t, "demo", "v1", true, "")
	if _, err := svc.Ingest(ctx, IngestSource{Dir: dir1}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	item := loadItemBySlug(t, db, "demo")
	arts := loadArtifacts(t, db, item.ID)
	if len(arts) != 1 || arts[0].ArtifactVersion != "v1" || !arts[0].IsLatest {
		t.Fatalf("after v1 ingest expected 1 IsLatest seeded artifact at v1, got %+v", arts)
	}

	// Second ingest: SAME primary .plugin.json (unchanged SHA → metadata-only path),
	// but the bundle block declares a NEW version v2.
	dir2, _, _, _ := writePluginSeedBundle(t, "demo", "v2", true, "")
	res, err := svc.Ingest(ctx, IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	// The entry must NOT have gone through the content-changed path (primary SHA same).
	if res.Added != 0 {
		t.Fatalf("v2 re-ingest should not Add (primary file unchanged); added=%d", res.Added)
	}

	arts = loadArtifacts(t, db, item.ID)
	if len(arts) != 2 {
		t.Fatalf("expected 2 seeded artifacts after re-seed (v1 demoted, v2 latest), got %d: %+v", len(arts), arts)
	}

	// Exactly one IsLatest seeded artifact, and it is v2.
	best, ok := pickLatestBundleArtifact(arts)
	if !ok {
		t.Fatal("expected a latest bundle artifact after re-seed")
	}
	if best.ArtifactVersion != "v2" {
		t.Errorf("latest seeded version = %q, want v2 (the re-seed must flip IsLatest)", best.ArtifactVersion)
	}
	var latestCount int64
	db.Model(&models.CapabilityArtifact{}).
		Where("item_id = ? AND source_type = ? AND is_latest = ?", item.ID, BundleSourceTypeSeeded, true).
		Count(&latestCount)
	if latestCount != 1 {
		t.Errorf("expected exactly 1 IsLatest seeded artifact after re-seed, got %d", latestCount)
	}

	// Re-importing v2 again must be a no-op (idempotent: same version → no new row).
	dir3, _, _, _ := writePluginSeedBundle(t, "demo", "v2", true, "")
	if _, err := svc.Ingest(ctx, IngestSource{Dir: dir3}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	if got := len(loadArtifacts(t, db, item.ID)); got != 2 {
		t.Errorf("re-importing the same v2 must not duplicate; expected 2 artifacts, got %d", got)
	}
}

// TestIngest_MetadataOnly_NoStore_DoesNotReseed verifies the nil-guard: when no Store
// is wired the metadata-only path must NOT attempt to seed (no panic, no artifact).
func TestIngest_MetadataOnly_NoStore_DoesNotReseed(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	// Service WITHOUT a Store (seed disabled). Provide a BundleJobService so the lazy
	// path's enqueue doesn't blow up.
	svc := newIngestService(db)
	svc.BundleJobService = &BundleJobService{DB: db}
	ctx := context.Background()

	dir, _, _, _ := writePluginSeedBundle(t, "demo", "v1", true, "")
	if _, err := svc.Ingest(ctx, IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	item := loadItemBySlug(t, db, "demo")
	// No Store -> no seeded artifact at all (stays lazy).
	if arts := loadArtifacts(t, db, item.ID); len(arts) != 0 {
		t.Fatalf("without a Store no seeded artifact should exist, got %d", len(arts))
	}

	// Re-ingest with a new bundle version; the metadata-only re-seed must be a safe
	// no-op (nil-guard), not a panic.
	dir2, _, _, _ := writePluginSeedBundle(t, "demo", "v2", true, "")
	if _, err := svc.Ingest(ctx, IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest (no store): %v", err)
	}
	if arts := loadArtifacts(t, db, item.ID); len(arts) != 0 {
		t.Errorf("metadata-only re-seed without a Store must remain a no-op, got %d artifacts", len(arts))
	}
}

// TestIngest_NoBundleBlock_StaysLazy is the zero-regression guard: a plugin entry
// WITHOUT a `bundle` block writes NO seeded artifact (it keeps the online lazy-clone
// path) and DOES enqueue a refresh job.
func TestIngest_NoBundleBlock_StaysLazy(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, _ := seedTestService(t, db)

	dir, _, _, _ := writePluginSeedBundle(t, "demo", "", false, "")

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	item := loadItemBySlug(t, db, "demo")
	if arts := loadArtifacts(t, db, item.ID); len(arts) != 0 {
		t.Fatalf("plugin without a bundle block must NOT seed an artifact; got %d", len(arts))
	}
	// The lazy-clone refresh trigger fires instead (item has a source_url).
	if n := countBundleJobs(t, db); n != 1 {
		t.Errorf("expected 1 lazy-clone refresh job for a non-seeded plugin, got %d", n)
	}
}

// TestIngest_SeedShaMismatch_FallsBackToLazy verifies a declared sha256 that does NOT
// match the ZIP bytes is rejected: no seeded artifact, and the plugin degrades to the
// lazy-clone path (refresh enqueued) rather than failing the ingest.
func TestIngest_SeedShaMismatch_FallsBackToLazy(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, _ := seedTestService(t, db)

	dir, _, _, _ := writePluginSeedBundle(t, "demo", "v1", true, "0000000000000000000000000000000000000000000000000000000000000000")

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("sha mismatch should degrade to lazy, not fail the ingest; failed=%d errors=%v", res.Failed, res.Errors)
	}
	item := loadItemBySlug(t, db, "demo")
	if arts := loadArtifacts(t, db, item.ID); len(arts) != 0 {
		t.Fatalf("sha mismatch must NOT write a seeded artifact; got %d", len(arts))
	}
	if n := countBundleJobs(t, db); n != 1 {
		t.Errorf("sha mismatch should fall back to lazy clone (1 refresh job), got %d", n)
	}
}

// TestIngest_SeededArtifact_IsBundleReady proves the bundle endpoint / ItemResponse
// helpers (latestBundleArtifactFrom equivalent) treat a `seeded` artifact as a valid
// bundle — i.e. seeded is in BundleSourceTypes, so online (clone_pack) and offline
// (seeded) are indistinguishable downstream. This mirrors handlers'
// latestBundleArtifactFrom selection without importing the handlers package.
func TestIngest_SeededArtifact_IsBundleReady(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, _ := seedTestService(t, db)

	const version = "abc123seeded"
	dir, _, wantSHA, _ := writePluginSeedBundle(t, "demo", version, true, "")
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	item := loadItemBySlug(t, db, "demo")
	arts := loadArtifacts(t, db, item.ID)

	best, ok := pickLatestBundleArtifact(arts)
	if !ok {
		t.Fatal("seeded artifact should be selected as the latest bundle artifact (bundleReady=true)")
	}
	if best.SourceType != BundleSourceTypeSeeded {
		t.Errorf("selected artifact SourceType = %q, want %q", best.SourceType, BundleSourceTypeSeeded)
	}
	if best.ArtifactVersion != version {
		t.Errorf("bundleVersion = %q, want %q", best.ArtifactVersion, version)
	}
	if best.ChecksumSHA256 != wantSHA {
		t.Errorf("checksum = %q, want %q", best.ChecksumSHA256, wantSHA)
	}
}

// TestIngest_SeedPathTraversal_FallsBackToLazy verifies a `bundle.file` that escapes
// the bundle directory (e.g. "../../etc/passwd") is rejected: no seeded artifact is
// written (no arbitrary server file is read into the artifact store) and the plugin
// degrades to the lazy-clone path rather than failing the ingest.
func TestIngest_SeedPathTraversal_FallsBackToLazy(t *testing.T) {
	db := newSeedIngestDB(t)
	createBundleJobsTable(t, db)
	svc, _ := seedTestService(t, db)

	dir := t.TempDir()
	// A secret file OUTSIDE the bundle dir that the traversal would target.
	secret := filepath.Join(filepath.Dir(dir), "outside-secret.zip")
	if err := os.WriteFile(secret, []byte("PK\x03\x04 not really a zip but secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	entry := map[string]any{
		"id":         "demo",
		"type":       "plugin",
		"source":     "claude-plugins-dev",
		"source_url": "https://github.com/owner/demo/tree/main",
		"bundle": map[string]any{
			"file":    "../outside-secret.zip",
			"version": "v-traversal",
		},
	}
	manifest := map[string]any{
		"schema_version": SupportedBundleSchemaVersion,
		"generated_at":   "2026-06-25T00:00:00Z",
		"entry_count":    1,
		"index_sha256":   "test-sha",
		"type_counts":    map[string]int{"plugin": 1},
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	ib, _ := json.Marshal([]map[string]any{entry})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), ib, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	pluginDir := filepath.Join(dir, "catalog-download", "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, ".plugin.json"), pluginZipFiles[".plugin.json"], 0o644); err != nil {
		t.Fatalf("write .plugin.json: %v", err)
	}

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("traversal should degrade to lazy, not fail the ingest; failed=%d errors=%v", res.Failed, res.Errors)
	}
	item := loadItemBySlug(t, db, "demo")
	if arts := loadArtifacts(t, db, item.ID); len(arts) != 0 {
		t.Fatalf("path traversal must NOT seed an artifact; got %d", len(arts))
	}
	if n := countBundleJobs(t, db); n != 1 {
		t.Errorf("traversal should fall back to lazy clone (1 refresh job), got %d", n)
	}
}

// pickLatestBundleArtifact mirrors handlers.latestBundleArtifactFrom: the newest
// IsLatest artifact whose SourceType is in BundleSourceTypes. Kept local so the
// services-package test doesn't import handlers (which would be a cycle).
func pickLatestBundleArtifact(arts []models.CapabilityArtifact) (*models.CapabilityArtifact, bool) {
	bundleTypes := map[string]bool{}
	for _, st := range BundleSourceTypes {
		bundleTypes[st] = true
	}
	var best *models.CapabilityArtifact
	for i := range arts {
		a := &arts[i]
		if !a.IsLatest || !bundleTypes[a.SourceType] {
			continue
		}
		if best == nil || a.CreatedAt.After(best.CreatedAt) {
			best = a
		}
	}
	return best, best != nil
}

// compile-time guards that the test references the sqlite/logger imports (the helper
// DB builders pull them in indirectly; keep an explicit touch so goimports doesn't
// strip them if the file is edited).
var _ = sqlite.Open
var _ = logger.Silent
