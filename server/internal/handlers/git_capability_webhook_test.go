package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupGitCapabilityWebhookDB(t *testing.T, webhookSecret string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	for _, ddl := range []string{
		`CREATE TABLE git_servers (
			server_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			endpoint TEXT NOT NULL,
			display_name TEXT NOT NULL,
			config TEXT NOT NULL DEFAULT '{}',
			is_template INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE git_capability_sync_jobs (
			id TEXT PRIMARY KEY,
			git_server_id TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			repo_id INTEGER NOT NULL,
			repo_full_name TEXT NOT NULL,
			default_branch TEXT NOT NULL,
			ref TEXT NOT NULL,
			before_sha TEXT NOT NULL DEFAULT '',
			after_sha TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			retry_count INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 3,
			last_error TEXT,
			scheduled_at DATETIME NOT NULL,
			started_at DATETIME,
			lease_token TEXT NOT NULL DEFAULT '',
			finished_at DATETIME,
			created_at DATETIME NOT NULL,
			CONSTRAINT uq_git_capability_sync_jobs_delivery
					UNIQUE (git_server_id, delivery_id)
			)`,
		`CREATE TABLE capability_items (
				id TEXT PRIMARY KEY,
				content_backend TEXT NOT NULL DEFAULT 'db',
				source_git_server_id TEXT NOT NULL DEFAULT '',
				source_git_repo_id INTEGER NOT NULL DEFAULT 0
			)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create test table: %v", err)
		}
	}
	config, err := json.Marshal(map[string]string{
		"admin_token":    "admin-token",
		"webhook_secret": webhookSecret,
	})
	if err != nil {
		t.Fatalf("marshal git server config: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		 VALUES ('gs-1', 'gitea', 'https://gitea.example', 'test', ?, 0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		string(config),
	).Error; err != nil {
		t.Fatalf("seed git server: %v", err)
	}
	return db
}

func newGitCapabilityWebhookRouter(db *gorm.DB) *gin.Engine {
	return newGitCapabilityWebhookRouterWithResolver(db, gitserver.NewDBResolver(db))
}

func newGitCapabilityWebhookRouterWithResolver(db *gorm.DB, resolver gitServerByIDResolver) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := NewGitCapabilityWebhookAPI(db, resolver)
	r.POST("/api/internal/git-sync/:git_server_id", api.ReceiveGiteaPush)
	return r
}

func signedGitCapabilityWebhook(t *testing.T, r *gin.Engine, event, deliveryID, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/api/internal/git-sync/gs-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", event)
	if deliveryID != "" {
		req.Header.Set("X-Gitea-Delivery", deliveryID)
	}
	req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type stubGitServerByIDResolver struct {
	cfg *gitserver.Config
	err error
}

func (s stubGitServerByIDResolver) ResolveByServerID(_ context.Context, _ string) (*gitserver.Config, error) {
	return s.cfg, s.err
}

func countGitCapabilitySyncJobs(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&models.GitCapabilitySyncJob{}).Count(&count).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return count
}

const validGiteaPushBody = `{
  "ref": "refs/heads/main",
  "before": "1111111111111111111111111111111111111111",
  "after": "2222222222222222222222222222222222222222",
  "repository": {
    "id": 42,
    "full_name": "alice/plugin-one",
    "default_branch": "main"
  }
}`

func TestGitCapabilityWebhookQueuesVerifiedDefaultBranchPush(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "push", "delivery-1", secret, validGiteaPushBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var job models.GitCapabilitySyncJob
	if err := db.First(&job).Error; err != nil {
		t.Fatalf("load persisted job: %v", err)
	}
	if job.GitServerID != "gs-1" || job.RepoID != 42 || job.RepoFullName != "alice/plugin-one" {
		t.Errorf("unexpected repo identity: %+v", job)
	}
	if job.DeliveryID != "delivery-1" || job.LeaseToken != "" {
		t.Errorf("unexpected delivery state: %+v", job)
	}
	if job.Ref != "refs/heads/main" || job.BeforeSHA == "" || job.AfterSHA == "" {
		t.Errorf("unexpected push state: %+v", job)
	}
	if job.Status != models.GitCapabilitySyncJobStatusPending || job.MaxAttempts != 3 || job.ScheduledAt.IsZero() {
		t.Errorf("unexpected queued state: %+v", job)
	}
}

func TestGitCapabilityWebhookExcludesUnboundMirrorButQueuesBoundRepo(t *testing.T) {
	const secret = "webhook-secret"
	t.Setenv("PLUGIN_GIT_MIRROR_OWNER", "mirror-owner")
	db := setupGitCapabilityWebhookDB(t, secret)
	router := newGitCapabilityWebhookRouter(db)
	body := strings.Replace(validGiteaPushBody, "alice/plugin-one", "mirror-owner/plugin-one", 1)

	w := signedGitCapabilityWebhook(t, router, "push", "delivery-excluded", secret, body)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "discovery_owner_excluded") {
		t.Fatalf("unbound mirror status = %d, body=%s", w.Code, w.Body.String())
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Fatalf("unbound mirror queued %d jobs", count)
	}

	if err := db.Exec(`INSERT INTO capability_items (id, content_backend, source_git_server_id, source_git_repo_id)
		VALUES ('bound-item', 'git', 'gs-1', 42)`).Error; err != nil {
		t.Fatalf("seed bound item: %v", err)
	}
	w = signedGitCapabilityWebhook(t, router, "push", "delivery-bound", secret, body)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "queued") {
		t.Fatalf("bound mirror status = %d, body=%s", w.Code, w.Body.String())
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 1 {
		t.Fatalf("bound mirror queued %d jobs, want 1", count)
	}
}

func TestGitCapabilityWebhookRejectsBadSignatureWithoutQueueing(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "push", "delivery-1", "wrong-secret", validGiteaPushBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

func TestGitCapabilityWebhookIgnoresNonPushAndNonDefaultBranch(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	r := newGitCapabilityWebhookRouter(db)

	w := signedGitCapabilityWebhook(t, r, "pull_request", "delivery-non-push", secret, validGiteaPushBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("non-push status = %d, body=%s", w.Code, w.Body.String())
	}
	nonDefaultBody := `{
  "ref": "refs/heads/feature",
  "before": "1111111111111111111111111111111111111111",
  "after": "2222222222222222222222222222222222222222",
  "repository": {"id": 42, "full_name": "alice/plugin-one", "default_branch": "main"}
}`
	w = signedGitCapabilityWebhook(t, r, "push", "delivery-non-default", secret, nonDefaultBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("non-default status = %d, body=%s", w.Code, w.Body.String())
	}

	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

func TestGitCapabilityWebhookDeduplicatesSameDelivery(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	r := newGitCapabilityWebhookRouter(db)
	if w := signedGitCapabilityWebhook(t, r, "push", "delivery-1", secret, validGiteaPushBody); w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, body=%s", w.Code, w.Body.String())
	}
	w := signedGitCapabilityWebhook(t, r, "push", "delivery-1", secret, validGiteaPushBody)
	if w.Code != http.StatusAccepted {
		t.Fatalf("duplicate status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Status    string `json:"status"`
		Duplicate bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode duplicate response: %v", err)
	}
	if response.Status != "duplicate" || !response.Duplicate {
		t.Errorf("unexpected duplicate response: %+v", response)
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 1 {
		t.Errorf("jobs = %d, want 1", count)
	}
}

func TestGitCapabilityWebhookQueuesDifferentDeliveriesForSameCommit(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	r := newGitCapabilityWebhookRouter(db)
	for _, deliveryID := range []string{"delivery-1", "delivery-2"} {
		if w := signedGitCapabilityWebhook(t, r, "push", deliveryID, secret, validGiteaPushBody); w.Code != http.StatusAccepted {
			t.Fatalf("delivery %s status = %d, body=%s", deliveryID, w.Code, w.Body.String())
		}
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 2 {
		t.Errorf("jobs = %d, want 2", count)
	}
}

func TestGitCapabilityWebhookRejectsMissingDeliveryWithoutQueueing(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "push", "", secret, validGiteaPushBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

func TestGitCapabilityWebhookRejectsMissingEventWithoutQueueing(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "", "delivery-1", secret, validGiteaPushBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

func TestGitCapabilityWebhookRejectsInvalidPushIdentityWithoutQueueing(t *testing.T) {
	const secret = "webhook-secret"
	tests := []struct {
		name string
		body string
	}{
		{
			name: "sha",
			body: `{"ref":"refs/heads/main","before":"bad","after":"2222222222222222222222222222222222222222","repository":{"id":42,"full_name":"alice/plugin-one","default_branch":"main"}}`,
		},
		{
			name: "ref",
			body: `{"ref":"refs/heads/../main","before":"1111111111111111111111111111111111111111","after":"2222222222222222222222222222222222222222","repository":{"id":42,"full_name":"alice/plugin-one","default_branch":"main"}}`,
		},
		{
			name: "repository",
			body: `{"ref":"refs/heads/main","before":"1111111111111111111111111111111111111111","after":"2222222222222222222222222222222222222222","repository":{"id":42,"full_name":"alice//plugin-one","default_branch":"main"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "push", "delivery-invalid-"+tt.name, secret, tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if count := countGitCapabilitySyncJobs(t, db); count != 0 {
				t.Errorf("jobs = %d, want 0", count)
			}
		})
	}
}

func TestGitCapabilityWebhookQueuesDeletedDefaultBranch(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	body := `{"ref":"refs/heads/main","before":"1111111111111111111111111111111111111111","after":"0000000000000000000000000000000000000000","repository":{"id":42,"full_name":"alice/plugin-one","default_branch":"main"}}`
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "push", "delivery-deleted", secret, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Status    string `json:"status"`
		Duplicate bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != "queued" || response.Duplicate {
		t.Errorf("unexpected response: %+v", response)
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 1 {
		t.Errorf("jobs = %d, want 1", count)
	}
	var job models.GitCapabilitySyncJob
	if err := db.First(&job).Error; err != nil {
		t.Fatalf("load persisted deletion job: %v", err)
	}
	if job.AfterSHA != strings.Repeat("0", 40) || job.DeliveryID != "delivery-deleted" {
		t.Errorf("unexpected persisted deletion job: %+v", job)
	}
}

func TestGitCapabilityWebhookHidesAuthenticationConfigurationFailures(t *testing.T) {
	const secret = "webhook-secret"
	tests := []struct {
		name     string
		resolver gitServerByIDResolver
	}{
		{
			name:     "not found",
			resolver: stubGitServerByIDResolver{err: gitserver.ErrGitServerNotFound},
		},
		{
			name:     "disabled",
			resolver: stubGitServerByIDResolver{err: gitserver.ErrGitServerDisabled},
		},
		{
			name:     "missing secret",
			resolver: stubGitServerByIDResolver{cfg: &gitserver.Config{ServerID: "gs-1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouterWithResolver(db, tt.resolver), "push", "delivery-1", secret, validGiteaPushBody)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if count := countGitCapabilitySyncJobs(t, db); count != 0 {
				t.Errorf("jobs = %d, want 0", count)
			}
		})
	}
}

func TestGitCapabilityWebhookReturnsServiceUnavailableForResolverFailure(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	resolver := stubGitServerByIDResolver{err: errors.New("database unavailable")}
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouterWithResolver(db, resolver), "push", "delivery-1", secret, validGiteaPushBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

func TestGitCapabilityWebhookOversizedBodyDoesNotRevealServerConfiguration(t *testing.T) {
	const secret = "webhook-secret"
	body := strings.Repeat("x", maxGitCapabilityWebhookBytes+1)
	tests := []struct {
		name     string
		resolver gitServerByIDResolver
	}{
		{
			name: "configured server",
			resolver: stubGitServerByIDResolver{cfg: &gitserver.Config{
				ServerID: "gs-1", WebhookSecret: secret,
			}},
		},
		{
			name:     "unknown server",
			resolver: stubGitServerByIDResolver{err: gitserver.ErrGitServerNotFound},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouterWithResolver(db, tt.resolver), "push", "delivery-1", secret, body)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if count := countGitCapabilitySyncJobs(t, db); count != 0 {
				t.Errorf("jobs = %d, want 0", count)
			}
		})
	}
}
