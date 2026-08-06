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
	"os"
	"path/filepath"
	"strconv"
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
		`CREATE TABLE git_capability_repositories (
				id TEXT PRIMARY KEY,
				git_server_id TEXT NOT NULL,
				git_repo_id INTEGER NOT NULL,
				default_branch TEXT NOT NULL DEFAULT ''
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
	r.POST("/api/internal/git-sync/:git_server_id", api.ReceiveGiteaEvent)
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

// ---------------------------------------------------------------------------
// Lifecycle ingress, driven by the byte-exact fixtures captured from the
// deployed Gitea 1.24.6 (testdata/gitea_lifecycle, README there).
//
// These are not synthesized payloads. They are the exact request bodies Gitea
// sent, which is the only way to pin the three traps this ingress exists to
// survive: the short-vs-full ref asymmetry, the misnamed `organization` field,
// and the fact that the ONLY lifecycle event 1.24.6 emits is repository/deleted.
// ---------------------------------------------------------------------------

func giteaLifecycleFixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "gitea_lifecycle", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	// Byte-exact: Gitea signs the bytes it sent, so any re-serialization here
	// would silently make the signature tests vacuous.
	return string(body)
}

func seedWebhookBinding(t *testing.T, db *gorm.DB, repoID int64, defaultBranch string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO git_capability_repositories (id, git_server_id, git_repo_id, default_branch) VALUES (?, 'gs-1', ?, ?)`,
		"binding-"+strconv.FormatInt(repoID, 10), repoID, defaultBranch,
	).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

func decodeWebhookResponse(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	return response.Status, response.Reason
}

// The load-bearing case. repository/deleted is the only lifecycle event that
// exists on 1.24.6, and before this change the ingress dropped it on the floor
// with "not a push", so a deleted repository's capabilities stayed live in the
// marketplace until someone noticed.
func TestGitCapabilityWebhookQueuesRepositoryDeletion(t *testing.T) {
	const secret = "webhook-secret"
	for _, fixture := range []string{"repository_deleted_user_owned.json", "repository_deleted_org_owned.json"} {
		t.Run(fixture, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			body := giteaLifecycleFixture(t, fixture)
			w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "repository", "delivery-"+fixture, secret, body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			status, reason := decodeWebhookResponse(t, w)
			if status != "queued" || reason != "repository_deleted" {
				t.Fatalf("status=%q reason=%q, want queued/repository_deleted", status, reason)
			}
			var job models.GitCapabilitySyncJob
			if err := db.First(&job).Error; err != nil {
				t.Fatalf("load persisted job: %v", err)
			}
			if job.RepoID <= 0 {
				t.Errorf("deletion job has no numeric repository identity: %+v", job)
			}
			// A lifecycle trigger carries no commit. A 40-zero AfterSHA here would
			// be read by the worker as a default-branch deletion delivery and would
			// archive for the wrong reason.
			if job.AfterSHA != "" || job.BeforeSHA != "" {
				t.Errorf("lifecycle job carries commit SHAs: %+v", job)
			}
		})
	}
}

// Creation is fixture-proven but deliberately not actionable: the repository has
// no commit on its default branch yet, so a convergence job could only fail and
// retry, and onboarding binds repositories explicitly rather than discovering
// them off an event.
func TestGitCapabilityWebhookAcknowledgesRepositoryCreationWithoutQueueing(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	body := giteaLifecycleFixture(t, "repository_created.json")
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "repository", "delivery-created", secret, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if status, reason := decodeWebhookResponse(t, w); status != "ignored" || reason != "repository_created" {
		t.Fatalf("status=%q reason=%q, want ignored/repository_created", status, reason)
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

// Trap 1: create/delete carry a SHORT ref (`main`), push carries the full form
// (`refs/heads/main`). validWebhookRef demands a refs/ prefix, so routing a
// create/delete payload through it rejects every one of them with 400.
func TestGitCapabilityWebhookAcceptsShortRefsOnCreateAndDelete(t *testing.T) {
	const secret = "webhook-secret"
	for _, tt := range []struct {
		name          string
		event         string
		fixture       string
		storedDefault string
		wantStatus    string
		wantReason    string
	}{
		{
			// delete_branch.json deletes `main` while the repository already reports
			// `probe-feature` as its default: exactly the two-step, partially silent
			// sequence that is the only way a default branch can vanish on 1.24.6.
			name: "delete of our believed default forces a re-read", event: "delete",
			fixture: "delete_branch.json", storedDefault: "main",
			wantStatus: "queued", wantReason: "stored_default_branch_deleted",
		},
		{
			// Same payload, but our stored default already matches the repository's:
			// a plain non-default branch deletion, nothing to converge.
			name: "delete of an unrelated branch is ignored", event: "delete",
			fixture: "delete_branch.json", storedDefault: "probe-feature",
			wantStatus: "ignored", wantReason: "non_default_branch",
		},
		{
			// The payload reports default_branch=main while we stored something else,
			// which is the ONLY push-time evidence of an otherwise silent default
			// branch change.
			name: "create reveals a silent default-branch change", event: "create",
			fixture: "create_branch.json", storedDefault: "old-default",
			wantStatus: "queued", wantReason: "default_branch_changed",
		},
		{
			name: "create of an unrelated branch is ignored", event: "create",
			fixture: "create_branch.json", storedDefault: "main",
			wantStatus: "ignored", wantReason: "non_default_branch",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			seedWebhookBinding(t, db, 1285, tt.storedDefault)
			body := giteaLifecycleFixture(t, tt.fixture)
			w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), tt.event, "delivery-"+tt.name, secret, body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			status, reason := decodeWebhookResponse(t, w)
			if status != tt.wantStatus || reason != tt.wantReason {
				t.Fatalf("status=%q reason=%q, want %q/%q", status, reason, tt.wantStatus, tt.wantReason)
			}
			want := int64(0)
			if tt.wantStatus == "queued" {
				want = 1
			}
			if count := countGitCapabilitySyncJobs(t, db); count != want {
				t.Errorf("jobs = %d, want %d", count, want)
			}
		})
	}
}

// LH-4: an unbound repository is not discovered off a ref event.
func TestGitCapabilityWebhookIgnoresRefChangeOnUnboundRepository(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	body := giteaLifecycleFixture(t, "delete_branch.json")
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "delete", "delivery-unbound", secret, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if status, reason := decodeWebhookResponse(t, w); status != "ignored" || reason != "unbound_repository" {
		t.Fatalf("status=%q reason=%q, want ignored/unbound_repository", status, reason)
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Errorf("jobs = %d, want 0", count)
	}
}

// The real push fixture, so the widened router is proven not to have broken the
// path that already worked.
func TestGitCapabilityWebhookQueuesFixturePushAndIgnoresNonDefaultBranch(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	r := newGitCapabilityWebhookRouter(db)

	w := signedGitCapabilityWebhook(t, r, "push", "delivery-fixture-push", secret, giteaLifecycleFixture(t, "push_default_branch.json"))
	if status, _ := decodeWebhookResponse(t, w); w.Code != http.StatusAccepted || status != "queued" {
		t.Fatalf("default-branch push status=%d body=%s", w.Code, w.Body.String())
	}
	w = signedGitCapabilityWebhook(t, r, "push", "delivery-fixture-new-branch", secret, giteaLifecycleFixture(t, "push_new_branch.json"))
	if status, reason := decodeWebhookResponse(t, w); status != "ignored" || reason != "non_default_branch" {
		t.Fatalf("new-branch push status=%q reason=%q", status, reason)
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 1 {
		t.Errorf("jobs = %d, want 1", count)
	}
}

// Signature verification is unchanged by the widening, and this proves it
// against real bytes rather than against a synthesized body: X-Gitea-Signature
// is bare lowercase hex HMAC-SHA256 over the exact payload Gitea sent.
func TestGitCapabilityWebhookVerifiesFixtureSignaturesAndRejectsTampering(t *testing.T) {
	const secret = "webhook-secret"
	for _, fixture := range []string{
		"repository_deleted_user_owned.json", "create_branch.json", "delete_branch.json", "push_default_branch.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			seedWebhookBinding(t, db, 1285, "main")
			body := giteaLifecycleFixture(t, fixture)
			if w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), eventForFixture(fixture), "d-"+fixture, "wrong-secret", body); w.Code != http.StatusUnauthorized {
				t.Fatalf("tampered signature status = %d", w.Code)
			}
			if count := countGitCapabilitySyncJobs(t, db); count != 0 {
				t.Fatalf("jobs = %d after a bad signature, want 0", count)
			}
		})
	}
}

func eventForFixture(fixture string) string {
	switch {
	case strings.HasPrefix(fixture, "repository_"):
		return "repository"
	case strings.HasPrefix(fixture, "create_"):
		return "create"
	case strings.HasPrefix(fixture, "delete_"):
		return "delete"
	default:
		return "push"
	}
}

// A signed event outside the fixture-proven set is acknowledged (so Gitea stops
// retrying) and audited, but changes nothing.
func TestGitCapabilityWebhookIgnoresUnsupportedSignedEvents(t *testing.T) {
	const secret = "webhook-secret"
	for _, event := range []string{"pull_request", "issues", "fork", "release", "repository"} {
		t.Run(event, func(t *testing.T) {
			db := setupGitCapabilityWebhookDB(t, secret)
			// The `repository` case is an unsupported ACTION, not an unsupported
			// event: 1.24.6 has no `renamed`/`transferred`, but a future version
			// might, and it must not be applied before a fixture proves its shape.
			body := `{"action":"renamed","repository":{"id":42,"full_name":"alice/plugin-one","default_branch":"main","owner":{"login":"alice"}}}`
			w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), event, "delivery-"+event, secret, body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if status, _ := decodeWebhookResponse(t, w); status != "ignored" {
				t.Fatalf("status = %q, want ignored", status)
			}
			if count := countGitCapabilitySyncJobs(t, db); count != 0 {
				t.Errorf("jobs = %d, want 0", count)
			}
		})
	}
}

func TestGitCapabilityWebhookDeduplicatesLifecycleDelivery(t *testing.T) {
	const secret = "webhook-secret"
	db := setupGitCapabilityWebhookDB(t, secret)
	r := newGitCapabilityWebhookRouter(db)
	body := giteaLifecycleFixture(t, "repository_deleted_user_owned.json")
	for i := 0; i < 3; i++ {
		if w := signedGitCapabilityWebhook(t, r, "repository", "delivery-repeat", secret, body); w.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status = %d", i, w.Code)
		}
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 1 {
		t.Fatalf("jobs = %d after 3 identical deliveries, want 1", count)
	}
}

// Trap 2: `organization` on the repository event carries the OWNER even when
// that owner is an ordinary user, and it is absent from push/create/delete.
// repository.owner.login is the only path that works everywhere, and the owner
// exclusion policy has to read it — otherwise a mirror-namespace repository
// would be converged from an event.
func TestGitCapabilityWebhookReadsOwnerFromRepositoryOwnerNotOrganization(t *testing.T) {
	const secret = "webhook-secret"
	t.Setenv("PLUGIN_GIT_MIRROR_OWNER", "fixture-probe-org")
	db := setupGitCapabilityWebhookDB(t, secret)
	// The org-owned deletion fixture has organization.login == repository.owner.login
	// == fixture-probe-org, and no capability is bound to it.
	body := giteaLifecycleFixture(t, "repository_deleted_org_owned.json")
	w := signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "repository", "delivery-owner", secret, body)
	if status, reason := decodeWebhookResponse(t, w); status != "ignored" || reason != "discovery_owner_excluded" {
		t.Fatalf("status=%q reason=%q, want ignored/discovery_owner_excluded", status, reason)
	}
	if count := countGitCapabilitySyncJobs(t, db); count != 0 {
		t.Fatalf("jobs = %d, want 0", count)
	}

	// Bound rows still converge: a mirror that something depends on must not be
	// allowed to disappear silently.
	if err := db.Exec(`INSERT INTO capability_items (id, content_backend, source_git_server_id, source_git_repo_id)
		VALUES ('bound-item', 'git', 'gs-1', 1285)`).Error; err != nil {
		t.Fatalf("seed bound item: %v", err)
	}
	w = signedGitCapabilityWebhook(t, newGitCapabilityWebhookRouter(db), "repository", "delivery-owner-bound", secret, body)
	if status, _ := decodeWebhookResponse(t, w); status != "queued" {
		t.Fatalf("bound mirror status = %q, want queued", status)
	}
}
