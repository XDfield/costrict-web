// Tests for user.created event consumer Phase 3 dispatch path (Git Ownership
// Refactor Phase 3).
//
// Phase 3 turns on USER_CREATED_EVENT_PROCESSING_ENABLED, which routes
// events through gitsync.ProvisionUser. We stand up a real fake Gitea HTTP
// server (mirroring user_provision_e2e_test.go) so the unexported
// clientFactory on UserProvisionService stays untouched.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupUserEventLogDB returns an in-memory sqlite DB with both the
// user_created_event_log table (idempotency) and user_git_binding table
// (ProvisionUser target) so a real UserProvisionService can run against it.
//
// Uses plain :memory: with pool pinned to 1 connection (matching setupTestDB
// elsewhere in this package). The shared-cache variant pollutes the global
// sqlite state and bleeds into unrelated marketplace tests when run together.
func setupUserEventLogDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS user_created_event_log (
			event_id TEXT PRIMARY KEY,
			event_type TEXT NOT NULL DEFAULT 'user.created',
			subject_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT 'default',
			status TEXT NOT NULL,
			error_message TEXT,
			processed_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_git_binding (
			user_subject_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT 'default',
			git_uid INTEGER,
			git_username TEXT NOT NULL,
			provider_kind TEXT NOT NULL DEFAULT 'gitea',
			sync_status TEXT NOT NULL DEFAULT 'pending',
			last_synced_at DATETIME,
			last_error TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (user_subject_id, tenant_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_user_git_binding_git_username ON user_git_binding(git_username)`,
		`CREATE TABLE IF NOT EXISTS git_servers (
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
		`CREATE TABLE IF NOT EXISTS tenant_git_server_binding (
			tenant_id TEXT PRIMARY KEY,
			git_server_id TEXT NOT NULL,
			bound_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// recordingGitea wraps the fake Gitea so we can count CreateUser calls
// across multiple deliveries — the e2e fake in gitsync doesn't expose this.
type recordingGitea struct {
	mu         sync.Mutex
	users      map[string]int64
	nextID     int64
	token      string
	createHits int
}

func newRecordingGitea(token string) *recordingGitea {
	return &recordingGitea{users: map[string]int64{}, nextID: 100, token: token}
}

func (f *recordingGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "token "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/users":
		f.handleCreate(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/users/"):
		f.handleGet(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *recordingGitea) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var opts struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	_ = json.Unmarshal(body, &opts)

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.users[opts.Username]; exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	f.createHits++
	f.nextID++
	f.users[opts.Username] = f.nextID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": f.nextID, "login": opts.Username, "email": opts.Email,
	})
}

func (f *recordingGitea) handleGet(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, ok := f.users[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": uid, "login": name})
}

// seedTenantForProvision writes a git_servers row + tenant_git_server_binding
// so the real DBResolver returns a non-soft-skip config.
func seedTenantForProvision(t *testing.T, db *gorm.DB, tenantID, endpoint, token string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at) VALUES ('gs-1', 'gitea', ?, 'fake', ?, 0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		endpoint, fmt.Sprintf(`{"admin_token":%q}`, token),
	).Error; err != nil {
		t.Fatalf("seed git_servers: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, 'gs-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		tenantID,
	).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

// withFlag temporarily sets an env var for the duration of the test.
func withFlag(t *testing.T, key, val string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if val == "" {
		_ = os.Unsetenv(key)
	} else {
		_ = os.Setenv(key, val)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func postEventWithID(t *testing.T, r *gin.Engine, body, eventID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/users/created", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if eventID != "" {
		req.Header.Set("X-Event-ID", eventID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestDispatch_HappyPath exercises Phase 3 happy path:
//   - flag on, UserProvisionService registered, idempotency row not present
//   - ProvisionUser called once, binding row written
//   - event_log row inserted with status='processed'
//   - response 202
func TestDispatch_HappyPath(t *testing.T) {
	withFlag(t, "USER_CREATED_EVENT_PROCESSING_ENABLED", "true")

	db := setupUserEventLogDB(t)

	gitea := newRecordingGitea("tok")
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	seedTenantForProvision(t, db, "t1", giteaSrv.URL, "tok")

	svc := gitsync.NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())
	InitUserProvisionService(svc)
	defer InitUserProvisionService(nil)

	api := &UserCreatedEventAPI{Log: zap.NewNop(), DB: db}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/internal/users/created", api.ReceiveUserCreated)

	body := `{
		"event_id": "11111111-1111-4111-8111-111111111111",
		"event_type": "user.created",
		"subject_id": "usr-1",
		"tenant_id": "t1",
		"user": {"subject_id": "usr-1", "username": "alice"}
	}`
	w := postEventWithID(t, r, body, "11111111-1111-4111-8111-111111111111")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	gitea.mu.Lock()
	hits := gitea.createHits
	gitea.mu.Unlock()
	if hits != 1 {
		t.Errorf("Gitea CreateUser calls = %d, want 1", hits)
	}

	// event_log row should exist with status=processed.
	var log models.UserCreatedEventLog
	if err := db.First(&log, "event_id = ?", "11111111-1111-4111-8111-111111111111").Error; err != nil {
		t.Fatalf("query event_log: %v", err)
	}
	if log.Status != models.UserCreatedEventStatusProcessed {
		t.Errorf("status = %q, want processed", log.Status)
	}

	// binding row should exist with status=synced.
	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-1").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusSynced {
		t.Errorf("binding status = %q, want synced", b.SyncStatus)
	}
}

// TestDispatch_DuplicateEventIdIsIdempotent replays the same event_id and
// verifies ProvisionUser is called exactly once across both deliveries
// (P3.4 exit criterion).
func TestDispatch_DuplicateEventIdIsIdempotent(t *testing.T) {
	withFlag(t, "USER_CREATED_EVENT_PROCESSING_ENABLED", "true")

	db := setupUserEventLogDB(t)

	gitea := newRecordingGitea("tok")
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	seedTenantForProvision(t, db, "t1", giteaSrv.URL, "tok")

	svc := gitsync.NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())
	InitUserProvisionService(svc)
	defer InitUserProvisionService(nil)

	api := &UserCreatedEventAPI{Log: zap.NewNop(), DB: db}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/internal/users/created", api.ReceiveUserCreated)

	body := `{
		"event_id": "22222222-2222-4222-8222-222222222222",
		"event_type": "user.created",
		"subject_id": "usr-2",
		"tenant_id": "t1",
		"user": {"subject_id": "usr-2", "username": "bob"}
	}`

	// First delivery.
	w1 := postEventWithID(t, r, body, "22222222-2222-4222-8222-222222222222")
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, body=%s", w1.Code, w1.Body.String())
	}
	gitea.mu.Lock()
	if gitea.createHits != 1 {
		t.Fatalf("after first delivery: createHits = %d, want 1", gitea.createHits)
	}
	gitea.mu.Unlock()

	// Duplicate delivery — same event_id.
	w2 := postEventWithID(t, r, body, "22222222-2222-4222-8222-222222222222")
	if w2.Code != http.StatusAccepted {
		t.Fatalf("duplicate delivery status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if !bytes.Contains(w2.Body.Bytes(), []byte(`"status":"duplicate"`)) {
		t.Errorf("duplicate response body should contain status=duplicate: %s", w2.Body.String())
	}
	gitea.mu.Lock()
	hits := gitea.createHits
	gitea.mu.Unlock()
	if hits != 1 {
		t.Errorf("after duplicate delivery: createHits = %d, want 1 (idempotent)", hits)
	}

	// Exactly one event_log row.
	var count int64
	db.Model(&models.UserCreatedEventLog{}).Where("event_id = ?", "22222222-2222-4222-8222-222222222222").Count(&count)
	if count != 1 {
		t.Errorf("event_log rows = %d, want 1", count)
	}

	// Exactly one binding row.
	var bindCount int64
	db.Model(&models.UserGitBinding{}).Where("user_subject_id = ?", "usr-2").Count(&bindCount)
	if bindCount != 1 {
		t.Errorf("binding rows = %d, want 1", bindCount)
	}
}

// TestDispatch_FlagOffIsLogOnly verifies that with the flag off, even when
// a service is registered, the handler returns 'accepted_log_only' and does
// not call ProvisionUser.
func TestDispatch_FlagOffIsLogOnly(t *testing.T) {
	withFlag(t, "USER_CREATED_EVENT_PROCESSING_ENABLED", "")

	db := setupUserEventLogDB(t)

	gitea := newRecordingGitea("tok")
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	seedTenantForProvision(t, db, "t1", giteaSrv.URL, "tok")

	svc := gitsync.NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())
	InitUserProvisionService(svc)
	defer InitUserProvisionService(nil)

	api := &UserCreatedEventAPI{Log: zap.NewNop(), DB: db}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/internal/users/created", api.ReceiveUserCreated)

	body := `{
		"event_id": "33333333-3333-4333-8333-333333333333",
		"event_type": "user.created",
		"subject_id": "usr-3",
		"tenant_id": "t1",
		"user": {"subject_id": "usr-3", "username": "carol"}
	}`
	w := postEventWithID(t, r, body, "33333333-3333-4333-8333-333333333333")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"status":"accepted_log_only"`)) {
		t.Errorf("body should be log-only: %s", w.Body.String())
	}

	gitea.mu.Lock()
	hits := gitea.createHits
	gitea.mu.Unlock()
	if hits != 0 {
		t.Errorf("Gitea CreateUser should not be called when flag is off; got %d", hits)
	}

	// No event_log row should exist (Phase 2 path doesn't write one).
	var count int64
	db.Model(&models.UserCreatedEventLog{}).Count(&count)
	if count != 0 {
		t.Errorf("event_log rows = %d, want 0 (Phase 2 path)", count)
	}
}

// TestDispatch_SoftSkipStillMarksProcessed ensures that when ProvisionUser
// soft-skips (tenant has no git_server binding), the event is still marked
// 'processed' so cs-user's outbox doesn't keep retrying.
func TestDispatch_SoftSkipStillMarksProcessed(t *testing.T) {
	withFlag(t, "USER_CREATED_EVENT_PROCESSING_ENABLED", "true")

	db := setupUserEventLogDB(t)
	// No tenant_git_server_binding — resolver will soft-skip.

	gitea := newRecordingGitea("tok")
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	svc := gitsync.NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())
	InitUserProvisionService(svc)
	defer InitUserProvisionService(nil)

	api := &UserCreatedEventAPI{Log: zap.NewNop(), DB: db}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/internal/users/created", api.ReceiveUserCreated)

	body := `{
		"event_id": "44444444-4444-4444-8444-444444444444",
		"event_type": "user.created",
		"subject_id": "usr-4",
		"tenant_id": "t-orphan",
		"user": {"subject_id": "usr-4", "username": "dave"}
	}`
	w := postEventWithID(t, r, body, "44444444-4444-4444-8444-444444444444")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	// Gitea never hit.
	gitea.mu.Lock()
	hits := gitea.createHits
	gitea.mu.Unlock()
	if hits != 0 {
		t.Errorf("Gitea should not be hit on soft-skip, got %d", hits)
	}

	// event_log still marked processed so cs-user doesn't retry forever.
	var log models.UserCreatedEventLog
	if err := db.First(&log, "event_id = ?", "44444444-4444-4444-8444-444444444444").Error; err != nil {
		t.Fatalf("query event_log: %v", err)
	}
	if log.Status != models.UserCreatedEventStatusProcessed {
		t.Errorf("status = %q, want processed (soft-skip must still ACK)", log.Status)
	}
}

// Suppress unused warnings if helpers below ever go unused.
var _ = context.Background
