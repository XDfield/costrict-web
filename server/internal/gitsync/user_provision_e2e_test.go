// End-to-end test exercising UserProvisionService against a fake Gitea
// HTTP server (Git Ownership Refactor P1.8).
//
// Goal: prove the full Phase 1 exit criterion — server can standalone
// invoke ProvisionUser and complete account provisioning end-to-end with:
//
//   - real *gitsync.Client (defaultUserClientFactory) talking to an
//     httptest HTTP server
//   - real *gitserver.DBResolver reading from a sqlite pool
//   - real user_git_binding table writes
//   - matching 201 happy path AND 409 → GET lookup recovery
//
// No mocks for the Client — only the Gitea HTTP edge is faked, exactly as
// it would be in a real Gitea instance.

package gitsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// fakeGitea emulates the user-provisioning surface of Gitea:
//
//	POST /api/v1/admin/users   → 201 with new user JSON; 409 if username exists
//	GET  /api/v1/users/{name}  → 200 with user JSON; 404 if not found
//
// Auth: token must be "test-admin-token" (matches the one in git_servers.config).
type fakeGitea struct {
	mu     sync.Mutex
	users  map[string]int64 // username → uid
	nextID int64
	token  string
}

func newFakeGitea(token string) *fakeGitea {
	return &fakeGitea{users: map[string]int64{}, nextID: 100, token: token}
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (f *fakeGitea) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var opts struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	_ = json.Unmarshal(body, &opts)

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.users[opts.Username]; exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	f.nextID++
	f.users[opts.Username] = f.nextID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": f.nextID, "login": opts.Username, "email": opts.Email,
	})
}

func (f *fakeGitea) handleGet(w http.ResponseWriter, r *http.Request) {
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": uid, "login": name,
	})
}

func setupE2EDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
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
		`CREATE TABLE tenant_git_server_binding (
			tenant_id TEXT PRIMARY KEY,
			git_server_id TEXT NOT NULL,
			bound_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE user_git_binding (
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
		`CREATE UNIQUE INDEX uq_user_git_binding_git_username ON user_git_binding(git_username)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// seedBindingE2E writes a tenant_git_server_binding + matching git_servers row.
// Uses raw SQL to bypass GORM zero-value skipping on Enabled.
func seedBindingE2E(t *testing.T, db *gorm.DB, tenantID, serverID, endpoint, adminToken string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		serverID, "gitea", endpoint, "fake", fmt.Sprintf(`{"admin_token":%q}`, adminToken),
	).Error; err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		tenantID, serverID,
	).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

// TestE2E_ProvisionUser_HappyPath covers the Phase 1 exit criterion:
// server-side UserProvisionService + real Client + real DBResolver +
// user_git_binding row, all talking to a fake Gitea over HTTP.
func TestE2E_ProvisionUser_HappyPath(t *testing.T) {
	gitea := newFakeGitea("test-admin-token")
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	db := setupE2EDB(t)
	seedBindingE2E(t, db, "t-e2e-1", "gs-e2e-1", giteaSrv.URL, "test-admin-token")

	svc := NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-e2e-1", TenantID: "t-e2e-1", Username: "alice",
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-e2e-1").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusSynced {
		t.Errorf("status = %q, want synced", b.SyncStatus)
	}
	// fakeGitea.nextID starts at 100; first created user gets 101 (post-increment).
	if b.GitUID == nil || *b.GitUID != 101 {
		t.Errorf("git_uid = %v, want 101", b.GitUID)
	}
	if b.GitUsername != "u-alice" {
		t.Errorf("git_username = %q, want u-alice", b.GitUsername)
	}
	if b.LastSyncedAt == nil {
		t.Errorf("last_synced_at should be set")
	}

	gitea.mu.Lock()
	defer gitea.mu.Unlock()
	if _, ok := gitea.users["u-alice"]; !ok {
		t.Errorf("fake Gitea didn't record user u-alice (have %v)", gitea.users)
	}
}

// TestE2E_ProvisionUser_409Recovery covers the idempotent re-entry path: a
// prior provisioning call left a user in Gitea (mocked by pre-seeding the
// fake). New provisioning call hits 409 → GET recovers UID → binding synced.
func TestE2E_ProvisionUser_409Recovery(t *testing.T) {
	gitea := newFakeGitea("test-admin-token")
	// Pre-seed existing Gitea user (uid 555).
	gitea.users["u-bob"] = 555
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	db := setupE2EDB(t)
	seedBindingE2E(t, db, "t-e2e-2", "gs-e2e-2", giteaSrv.URL, "test-admin-token")

	svc := NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-e2e-2", TenantID: "t-e2e-2", Username: "bob",
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-e2e-2").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusSynced {
		t.Errorf("status = %q, want synced", b.SyncStatus)
	}
	if b.GitUID == nil || *b.GitUID != 555 {
		t.Errorf("git_uid = %v, want 555 (recovered via GET)", b.GitUID)
	}
}

// TestE2E_ProvisionUser_MissingBindingSoftSkips covers the no-binding path:
// tenant has no git_server binding, resolver returns ErrTenantMissingGitServer,
// service leaves the row pending and returns nil.
func TestE2E_ProvisionUser_MissingBindingSoftSkips(t *testing.T) {
	gitea := newFakeGitea("test-admin-token")
	giteaSrv := httptest.NewServer(gitea)
	defer giteaSrv.Close()

	db := setupE2EDB(t)
	// No binding seeded — tenant "t-orphan" has no git_server.
	svc := NewUserProvisionService(db, gitserver.NewDBResolver(db), zap.NewNop())

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-orphan", TenantID: "t-orphan", Username: "carol",
	}); err != nil {
		t.Fatalf("ProvisionUser on orphan tenant should soft-skip, got %v", err)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-orphan").Error; err != nil {
		t.Fatalf("binding row should still exist (pending): %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusPending {
		t.Errorf("status = %q, want pending", b.SyncStatus)
	}

	gitea.mu.Lock()
	if len(gitea.users) != 0 {
		t.Errorf("Gitea should have 0 users, got %v", gitea.users)
	}
	gitea.mu.Unlock()
}
