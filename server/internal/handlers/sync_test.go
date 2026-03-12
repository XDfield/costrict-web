package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func newSyncRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/sync-logs/:id", GetSyncLogDetail)
	r.GET("/api/sync-jobs/:id", GetSyncJobDetail)
	r.GET("/api/registries/:id/sync-status", GetRegistrySyncStatus)
	r.GET("/api/registries/:id/sync-logs", ListRegistrySyncLogs)
	r.GET("/api/registries/:id/sync-jobs", ListRegistrySyncJobs)
	r.POST("/api/registries/:id/sync", TriggerRegistrySync)
	r.POST("/api/registries/:id/sync/cancel", CancelRegistrySync)
	r.GET("/api/repositories/:id/sync-status", GetRepoSyncStatus)
	r.GET("/api/repositories/:id/sync-logs", ListRepoSyncLogs)
	r.GET("/api/repositories/:id/sync-jobs", ListRepoSyncJobs)
	r.POST("/api/repositories/:id/sync", TriggerRepoSync)
	r.POST("/api/repositories/:id/sync/cancel", CancelRepoSync)
	return r
}

func setupSyncDB(t *testing.T) func() {
	t.Helper()
	cleanup := setupTestDB(t)

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sync_logs (
			id            TEXT PRIMARY KEY,
			registry_id   TEXT NOT NULL,
			trigger_type  TEXT NOT NULL,
			trigger_user  TEXT,
			status        TEXT NOT NULL DEFAULT 'running',
			commit_sha    TEXT,
			previous_sha  TEXT,
			total_items   INTEGER DEFAULT 0,
			added_items   INTEGER DEFAULT 0,
			updated_items INTEGER DEFAULT 0,
			deleted_items INTEGER DEFAULT 0,
			skipped_items INTEGER DEFAULT 0,
			failed_items  INTEGER DEFAULT 0,
			error_message TEXT,
			duration_ms   INTEGER,
			started_at    DATETIME NOT NULL,
			finished_at   DATETIME,
			created_at    DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS sync_jobs (
			id            TEXT PRIMARY KEY,
			registry_id   TEXT NOT NULL,
			trigger_type  TEXT NOT NULL,
			trigger_user  TEXT,
			priority      INTEGER NOT NULL DEFAULT 5,
			status        TEXT NOT NULL DEFAULT 'pending',
			payload       TEXT DEFAULT '{}',
			retry_count   INTEGER DEFAULT 0,
			max_attempts  INTEGER DEFAULT 3,
			last_error    TEXT,
			scheduled_at  DATETIME NOT NULL,
			started_at    DATETIME,
			finished_at   DATETIME,
			sync_log_id   TEXT,
			created_at    DATETIME
		)`,
	}

	for _, s := range stmts {
		if err := database.DB.Exec(s).Error; err != nil {
			t.Fatalf("failed to create sync table: %v\nSQL: %s", err, s)
		}
	}

	return cleanup
}

// ---------------------------------------------------------------------------
// verifyGitHubSignature
// ---------------------------------------------------------------------------

func TestVerifyGitHubSignature_Valid(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := "my-secret"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyGitHubSignature(body, sig, secret) {
		t.Fatal("expected valid signature")
	}
}

func TestVerifyGitHubSignature_Invalid(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	if verifyGitHubSignature(body, "sha256=invalidsig", "secret") {
		t.Fatal("expected invalid signature")
	}
}

func TestVerifyGitHubSignature_TooShort(t *testing.T) {
	if verifyGitHubSignature([]byte("body"), "short", "secret") {
		t.Fatal("expected false for too-short signature")
	}
}

func TestVerifyGitHubSignature_WrongSecret(t *testing.T) {
	body := []byte(`payload`)
	mac := hmac.New(sha256.New, []byte("correct-secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if verifyGitHubSignature(body, sig, "wrong-secret") {
		t.Fatal("expected false for wrong secret")
	}
}

// ---------------------------------------------------------------------------
// getRegistryIDForRepo
// ---------------------------------------------------------------------------

func TestGetRegistryIDForRepo_ExternalFirst(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "ext-reg", Name: "ext", SourceType: "external", Visibility: "repo", RepoID: "repo-ext", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "int-reg", Name: "int", SourceType: "internal", Visibility: "repo", RepoID: "repo-ext", OwnerID: "u1",
	})

	id, err := getRegistryIDForRepo("repo-ext")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "ext-reg" {
		t.Fatalf("expected ext-reg (external preferred), got %s", id)
	}
}

func TestGetRegistryIDForRepo_FallbackToInternal(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "int-only", Name: "int-only", SourceType: "internal", Visibility: "repo", RepoID: "repo-int", OwnerID: "u1",
	})

	id, err := getRegistryIDForRepo("repo-int")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "int-only" {
		t.Fatalf("expected int-only, got %s", id)
	}
}

func TestGetRegistryIDForRepo_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	_, err := getRegistryIDForRepo("no-such-repo")
	if err == nil {
		t.Fatal("expected error for non-existent repo")
	}
}

// ---------------------------------------------------------------------------
// GetSyncLogDetail
// ---------------------------------------------------------------------------

func TestGetSyncLogDetail_Found(t *testing.T) {
	defer setupSyncDB(t)()
	database.DB.Exec(`INSERT INTO sync_logs (id, registry_id, trigger_type, status, started_at, created_at)
		VALUES ('log-1', 'reg-1', 'manual', 'success', datetime('now'), datetime('now'))`)

	w := get(newSyncRouter(), "/api/sync-logs/log-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var log map[string]interface{}
	json.NewDecoder(w.Body).Decode(&log)
	if log["id"] != "log-1" {
		t.Fatalf("unexpected id: %v", log["id"])
	}
}

func TestGetSyncLogDetail_NotFound(t *testing.T) {
	defer setupSyncDB(t)()
	w := get(newSyncRouter(), "/api/sync-logs/no-such-log")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetSyncJobDetail
// ---------------------------------------------------------------------------

func TestGetSyncJobDetail_Found(t *testing.T) {
	defer setupSyncDB(t)()
	database.DB.Exec(`INSERT INTO sync_jobs (id, registry_id, trigger_type, status, priority, scheduled_at, created_at)
		VALUES ('job-1', 'reg-1', 'manual', 'pending', 1, datetime('now'), datetime('now'))`)

	w := get(newSyncRouter(), "/api/sync-jobs/job-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var job map[string]interface{}
	json.NewDecoder(w.Body).Decode(&job)
	if job["id"] != "job-1" {
		t.Fatalf("unexpected id: %v", job["id"])
	}
}

func TestGetSyncJobDetail_NotFound(t *testing.T) {
	defer setupSyncDB(t)()
	w := get(newSyncRouter(), "/api/sync-jobs/no-such-job")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// getSyncStatus (via GetRegistrySyncStatus)
// ---------------------------------------------------------------------------

func TestGetRegistrySyncStatus_Found(t *testing.T) {
	defer setupSyncDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-ss1", Name: "sync-reg", SourceType: "internal", Visibility: "public",
		RepoID: "repo-1", OwnerID: "u1", SyncStatus: "idle",
	})

	w := get(newSyncRouter(), "/api/registries/reg-ss1/sync-status")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["syncStatus"] != "idle" {
		t.Fatalf("expected syncStatus=idle, got %v", body["syncStatus"])
	}
}

func TestGetRegistrySyncStatus_NotFound(t *testing.T) {
	defer setupSyncDB(t)()
	w := get(newSyncRouter(), "/api/registries/no-reg/sync-status")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// listSyncLogs (via ListRegistrySyncLogs)
// ---------------------------------------------------------------------------

func TestListRegistrySyncLogs_Empty(t *testing.T) {
	defer setupSyncDB(t)()

	w := get(newSyncRouter(), "/api/registries/any-reg/sync-logs")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["total"].(float64) != 0 {
		t.Fatalf("expected total=0, got %v", body["total"])
	}
}

func TestListRegistrySyncLogs_WithData(t *testing.T) {
	defer setupSyncDB(t)()
	database.DB.Exec(`INSERT INTO sync_logs (id, registry_id, trigger_type, status, started_at, created_at)
		VALUES ('log-a', 'reg-logs', 'manual', 'success', datetime('now'), datetime('now'))`)
	database.DB.Exec(`INSERT INTO sync_logs (id, registry_id, trigger_type, status, started_at, created_at)
		VALUES ('log-b', 'reg-logs', 'scheduled', 'failed', datetime('now'), datetime('now'))`)

	w := get(newSyncRouter(), "/api/registries/reg-logs/sync-logs")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["total"].(float64) != 2 {
		t.Fatalf("expected total=2, got %v", body["total"])
	}
	logs := body["logs"].([]interface{})
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(logs))
	}
}

func TestListRegistrySyncLogs_Pagination(t *testing.T) {
	defer setupSyncDB(t)()
	for i := 0; i < 5; i++ {
		database.DB.Exec(`INSERT INTO sync_logs (id, registry_id, trigger_type, status, started_at, created_at)
			VALUES (?, 'reg-page', 'manual', 'success', datetime('now'), datetime('now'))`,
			"log-page-"+string(rune('a'+i)))
	}

	w := get(newSyncRouter(), "/api/registries/reg-page/sync-logs?page=1&pageSize=2")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["total"].(float64) != 5 {
		t.Fatalf("expected total=5, got %v", body["total"])
	}
	logs := body["logs"].([]interface{})
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs per page, got %d", len(logs))
	}
}

// ---------------------------------------------------------------------------
// listSyncJobs (via ListRegistrySyncJobs) - JobService nil case
// ---------------------------------------------------------------------------

func TestListRegistrySyncJobs_NoJobService(t *testing.T) {
	defer setupSyncDB(t)()
	origJobService := JobService
	JobService = nil
	defer func() { JobService = origJobService }()

	w := get(newSyncRouter(), "/api/registries/any-reg/sync-jobs")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when JobService is nil, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// triggerSync / cancelSync - JobService nil case
// ---------------------------------------------------------------------------

func TestTriggerRegistrySync_NoJobService(t *testing.T) {
	defer setupSyncDB(t)()
	origJobService := JobService
	JobService = nil
	defer func() { JobService = origJobService }()

	w := postJSON(newSyncRouter(), "/api/registries/any-reg/sync", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when JobService is nil, got %d", w.Code)
	}
}

func TestCancelRegistrySync_NoJobService(t *testing.T) {
	defer setupSyncDB(t)()
	origJobService := JobService
	JobService = nil
	defer func() { JobService = origJobService }()

	w := postJSON(newSyncRouter(), "/api/registries/any-reg/sync/cancel", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when JobService is nil, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Repo-level sync delegation (404 when no registry)
// ---------------------------------------------------------------------------

func TestTriggerRepoSync_NoRegistry(t *testing.T) {
	defer setupSyncDB(t)()
	w := postJSON(newSyncRouter(), "/api/repositories/no-repo/sync", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCancelRepoSync_NoRegistry(t *testing.T) {
	defer setupSyncDB(t)()
	w := postJSON(newSyncRouter(), "/api/repositories/no-repo/sync/cancel", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetRepoSyncStatus_NoRegistry(t *testing.T) {
	defer setupSyncDB(t)()
	w := get(newSyncRouter(), "/api/repositories/no-repo/sync-status")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListRepoSyncLogs_NoRegistry(t *testing.T) {
	defer setupSyncDB(t)()
	w := get(newSyncRouter(), "/api/repositories/no-repo/sync-logs")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListRepoSyncJobs_NoRegistry(t *testing.T) {
	defer setupSyncDB(t)()
	w := get(newSyncRouter(), "/api/repositories/no-repo/sync-jobs")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// EnsurePublicRegistry
// ---------------------------------------------------------------------------

func TestEnsurePublicRegistry_Creates(t *testing.T) {
	defer setupTestDB(t)()

	EnsurePublicRegistry()

	var reg models.CapabilityRegistry
	if err := database.DB.First(&reg, "id = ?", PublicRegistryID).Error; err != nil {
		t.Fatalf("expected public registry to be created: %v", err)
	}
	if reg.Name != "public" {
		t.Fatalf("expected name=public, got %s", reg.Name)
	}
}

func TestEnsurePublicRegistry_Idempotent(t *testing.T) {
	defer setupTestDB(t)()

	EnsurePublicRegistry()
	EnsurePublicRegistry()

	var count int64
	database.DB.Model(&models.CapabilityRegistry{}).Where("id = ?", PublicRegistryID).Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 public registry, got %d", count)
	}
}

// ensure datatypes import is used
var _ = datatypes.JSON(nil)
