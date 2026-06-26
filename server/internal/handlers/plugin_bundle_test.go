package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// newBundleRouter wires the bundle endpoint with the package-level services it reads
// (StorageBackend, BundlePackSvc, BundleJobSvc) and an optional injected user.
func newBundleRouter(userID string, backend storage.Backend) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	StorageBackend = backend
	BundleJobSvc = &services.BundleJobService{DB: database.DB}
	BundlePackSvc = services.NewBundlePackService(database.DB, &services.GitService{}, backend, "")
	r.GET("/api/plugins/:slug/bundle", injectUser, DownloadPluginBundle)
	return r
}

// addBundleJobsTable adds bundle_jobs to the shared test DB (setupTestDB omits it).
//
// It also pins the SQLite pool to a single connection. The default in-memory
// SQLite database is per-connection, so the async download_count++ goroutine in
// streamBundleArtifact (which grabs a second pool connection) would otherwise see
// an empty schema and a subsequent main-goroutine query could land on that
// uninitialised connection ("no such table"). A single connection keeps all of a
// test's queries on the one DB.
func addBundleJobsTable(t *testing.T) {
	t.Helper()
	if sqlDB, err := database.DB.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	stmt := `CREATE TABLE IF NOT EXISTS bundle_jobs (
		id TEXT PRIMARY KEY,
		item_id TEXT NOT NULL,
		trigger_type TEXT NOT NULL DEFAULT 'sync',
		trigger_user TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		retry_count INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		last_error TEXT,
		artifact_id TEXT,
		scheduled_at DATETIME NOT NULL,
		started_at DATETIME,
		finished_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME
	)`
	if err := database.DB.Exec(stmt).Error; err != nil {
		t.Fatalf("create bundle_jobs: %v", err)
	}
}

func seedPublicPlugin(t *testing.T, id, slug, sourceURL string) {
	t.Helper()
	database.DB.Create(&models.Repository{ID: "repo-pub", Name: "public-repo", Visibility: "public", OwnerID: "owner"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pub", Name: "pub-reg", SourceType: "internal", RepoID: "repo-pub", OwnerID: "owner",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: id, RegistryID: "reg-pub", RepoID: "repo-pub", Slug: slug, ItemType: "plugin",
		Name: "Demo Plugin", Status: "active", SourceURL: sourceURL, CreatedBy: "owner",
		Metadata: datatypes.JSON([]byte("{}")),
	})
}

func TestDownloadPluginBundle_CacheHitStreamsZip(t *testing.T) {
	defer setupTestDB(t)()
	addBundleJobsTable(t)
	backend := newMemBackend()

	seedPublicPlugin(t, "item-bndl-1", "demo-plugin", "https://github.com/owner/repo/tree/main")

	// A real ZIP stored as the latest clone_pack artifact.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("hooks/run.sh")
	w.Write([]byte("#!/bin/sh\necho hi\n"))
	zw.Close()
	zipBytes := zbuf.Bytes()

	key := "repo-pub/item-bndl-1/bundle/deadbeef.zip"
	backend.data[key] = zipBytes
	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-bndl-1", ItemID: "item-bndl-1", Filename: "demo-plugin.zip",
		FileSize: int64(len(zipBytes)), ChecksumSHA256: "sha-of-zip", StorageKey: key,
		ArtifactVersion: "deadbeefcommitsha", IsLatest: true, SourceType: "clone_pack",
		UploadedBy: "system", CreatedAt: time.Now(),
	})

	rec := get(newBundleRouter("", backend), "/api/plugins/demo-plugin/bundle")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if v := rec.Header().Get("X-Bundle-Version"); v != "deadbeefcommitsha" {
		t.Errorf("X-Bundle-Version = %q, want deadbeefcommitsha", v)
	}
	if !bytes.Equal(rec.Body.Bytes(), zipBytes) {
		t.Error("streamed body does not match stored ZIP")
	}
}

func TestDownloadPluginBundle_CatalogMissEnqueuesAndAccepts(t *testing.T) {
	defer setupTestDB(t)()
	addBundleJobsTable(t)
	backend := newMemBackend()

	// Catalog plugin (has source_url), no artifact yet -> 202 + enqueue.
	seedPublicPlugin(t, "item-bndl-2", "catalog-plugin", "https://github.com/owner/repo/tree/main/sub")

	rec := get(newBundleRouter("", backend), "/api/plugins/catalog-plugin/bundle")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "packing" {
		t.Errorf("status = %v, want packing", body["status"])
	}

	var count int64
	database.DB.Model(&models.BundleJob{}).Where("item_id = ? AND status IN ('pending','running')", "item-bndl-2").Count(&count)
	if count != 1 {
		t.Errorf("expected 1 in-flight bundle job, got %d", count)
	}

	// A second request must dedup (no duplicate clone job).
	get(newBundleRouter("", backend), "/api/plugins/catalog-plugin/bundle")
	database.DB.Model(&models.BundleJob{}).Where("item_id = ? AND status IN ('pending','running')", "item-bndl-2").Count(&count)
	if count != 1 {
		t.Errorf("dedup failed: expected 1 in-flight job, got %d", count)
	}
}

func TestDownloadPluginBundle_UploadMissPacksSynchronously(t *testing.T) {
	defer setupTestDB(t)()
	addBundleJobsTable(t)
	backend := newMemBackend()

	// Uploaded plugin: no source_url but has stored assets -> synchronous pack.
	database.DB.Create(&models.Repository{ID: "repo-pub", Name: "public-repo", Visibility: "public", OwnerID: "owner"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pub", Name: "pub-reg", SourceType: "internal", RepoID: "repo-pub", OwnerID: "owner",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-bndl-3", RegistryID: "reg-pub", RepoID: "repo-pub", Slug: "uploaded-plugin",
		ItemType: "plugin", Name: "Uploaded", Status: "active", CreatedBy: "owner",
		Content: `{"name":"uploaded"}`, SourcePath: ".plugin.json",
		Metadata: datatypes.JSON([]byte("{}")),
	})
	text := "#!/bin/sh\necho run\n"
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-3", ItemID: "item-bndl-3", RelPath: "hooks/run.sh", TextContent: &text,
	})

	rec := get(newBundleRouter("", backend), "/api/plugins/uploaded-plugin/bundle")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("response is not a valid zip: %v", err)
	}
	names := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = string(b)
	}
	if names["hooks/run.sh"] != text {
		t.Errorf("hooks/run.sh = %q, want %q", names["hooks/run.sh"], text)
	}
	if names[".plugin.json"] != `{"name":"uploaded"}` {
		t.Errorf("fallback main file missing/incorrect: %q", names[".plugin.json"])
	}

	// An upload_pack artifact should have been persisted as latest.
	var art models.CapabilityArtifact
	if err := database.DB.Where("item_id = ? AND source_type = ? AND is_latest = ?", "item-bndl-3", "upload_pack", true).First(&art).Error; err != nil {
		t.Fatalf("expected an upload_pack artifact: %v", err)
	}
}

func TestDownloadPluginBundle_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	addBundleJobsTable(t)
	backend := newMemBackend()
	rec := get(newBundleRouter("", backend), "/api/plugins/no-such/bundle")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetItem_PluginBundleFieldsReady(t *testing.T) {
	defer setupTestDB(t)()

	seedPublicPlugin(t, "item-resp-1", "ready-plugin", "https://github.com/owner/repo/tree/main")
	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-resp-1", ItemID: "item-resp-1", Filename: "ready-plugin.zip", FileSize: 100,
		ChecksumSHA256: "zsha", StorageKey: "k", ArtifactVersion: "commit-sha-123",
		IsLatest: true, SourceType: "clone_pack", UploadedBy: "system", CreatedAt: time.Now(),
	})

	rec := get(newItemRouter(""), "/api/items/item-resp-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)

	if resp["bundleReady"] != true {
		t.Errorf("bundleReady = %v, want true", resp["bundleReady"])
	}
	if resp["bundleVersion"] != "commit-sha-123" {
		t.Errorf("bundleVersion = %v, want commit-sha-123", resp["bundleVersion"])
	}
	url, _ := resp["bundleUrl"].(string)
	if url == "" || !bytes.Contains([]byte(url), []byte("/api/plugins/ready-plugin/bundle")) {
		t.Errorf("bundleUrl = %q, want it to contain /api/plugins/ready-plugin/bundle", url)
	}
}

// TestGetItem_PluginBundleFieldsReady_Seeded is the regression guard for the
// offline-seed cross-layer contract: a plugin whose only IsLatest bundle artifact is
// `seeded` (air-gap path) MUST be advertised to csc as bundleReady=true with the
// bundle version, exactly like an online clone_pack artifact. Before the fix,
// latestBundleArtifactFrom hard-coded clone_pack|upload_pack and silently dropped
// seeded, so GetItem reported bundleReady=false and an empty bundleVersion for
// offline-seeded plugins — csc could never deterministically detect/cache them.
func TestGetItem_PluginBundleFieldsReady_Seeded(t *testing.T) {
	defer setupTestDB(t)()

	seedPublicPlugin(t, "item-resp-seed", "seeded-plugin", "https://github.com/owner/repo/tree/main")
	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-resp-seed", ItemID: "item-resp-seed", Filename: "seeded-plugin.zip", FileSize: 100,
		ChecksumSHA256: "zsha", StorageKey: "k", ArtifactVersion: "offline-bundle-sha",
		IsLatest: true, SourceType: services.BundleSourceTypeSeeded, UploadedBy: "system:seed", CreatedAt: time.Now(),
	})

	rec := get(newItemRouter(""), "/api/items/item-resp-seed")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)

	if resp["bundleReady"] != true {
		t.Errorf("bundleReady = %v, want true (seeded artifact must be advertised as ready)", resp["bundleReady"])
	}
	if resp["bundleVersion"] != "offline-bundle-sha" {
		t.Errorf("bundleVersion = %v, want offline-bundle-sha", resp["bundleVersion"])
	}
}

func TestGetItem_PluginBundleFieldsNotReady(t *testing.T) {
	defer setupTestDB(t)()

	// Plugin with no bundle artifact yet: URL advertised, not ready.
	seedPublicPlugin(t, "item-resp-2", "pending-plugin", "https://github.com/owner/repo/tree/main")

	rec := get(newItemRouter(""), "/api/items/item-resp-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)

	if resp["bundleReady"] != false {
		t.Errorf("bundleReady = %v, want false", resp["bundleReady"])
	}
	if _, hasVersion := resp["bundleVersion"]; hasVersion {
		t.Errorf("bundleVersion should be omitted when not ready, got %v", resp["bundleVersion"])
	}
	url, _ := resp["bundleUrl"].(string)
	if url == "" {
		t.Error("bundleUrl should be advertised even before the bundle is ready")
	}
}
