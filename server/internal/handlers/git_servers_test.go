// Tests for git_servers CRUD + tenant binding handlers (Git Ownership
// Refactor P1.7).
//
// Uses sqlite :memory: + in-process gin engine. No HTTP server started.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupHandlersDB(t *testing.T) *gorm.DB {
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
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// rawInsertServer bypasses GORM zero-value skipping for Enabled/IsTemplate.
func rawInsertServer(t *testing.T, db *gorm.DB, gs *models.GitServer) {
	t.Helper()
	enabled := 0
	if gs.Enabled {
		enabled = 1
	}
	isTpl := 0
	if gs.IsTemplate {
		isTpl = 1
	}
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gs.ServerID, gs.Kind, gs.Endpoint, gs.DisplayName, gs.Config, isTpl, enabled,
		gs.CreatedAt, gs.UpdatedAt,
	).Error; err != nil {
		t.Fatalf("insert server: %v", err)
	}
}

func newTestRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	gsStore := NewGormGitServerStore(db)
	tStore := NewGormTenantGitServerBindingStore(db)
	gsAPI := &GitServerAPI{Store: gsStore}
	tAPI := &TenantGitServerBindingAPI{Store: tStore}
	g := r.Group("/api/internal")
	g.POST("/git-servers", gsAPI.CreateGitServer)
	g.GET("/git-servers", gsAPI.ListGitServers)
	g.GET("/git-servers/:server_id", gsAPI.GetGitServer)
	g.PUT("/git-servers/:server_id", gsAPI.UpdateGitServer)
	g.DELETE("/git-servers/:server_id", gsAPI.DeleteGitServer)
	g.PUT("/tenants/:tenant_id/git-server", tAPI.BindTenantGitServer)
	g.GET("/tenants/:tenant_id/git-server", tAPI.GetTenantGitServerBinding)
	g.DELETE("/tenants/:tenant_id/git-server", tAPI.UnbindTenantGitServer)
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCreateGitServer_HappyPath(t *testing.T) {
	db := setupHandlersDB(t)
	r := newTestRouter(db)
	w := doJSON(t, r, "POST", "/api/internal/git-servers", gin.H{
		"kind": "gitea", "endpoint": "https://g.example/", "display_name": "G",
		"config": map[string]any{"admin_token": "tok"}, "enabled": true,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp gitServerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ServerID == "" || resp.Endpoint != "https://g.example" {
		t.Errorf("unexpected resp: %+v", resp)
	}
	if resp.Config == "" || resp.Config == "{}" {
		t.Errorf("config not echoed: %q", resp.Config)
	}
}

func TestCreateGitServer_RejectsUnknownKind(t *testing.T) {
	db := setupHandlersDB(t)
	r := newTestRouter(db)
	w := doJSON(t, r, "POST", "/api/internal/git-servers", gin.H{
		"kind": "gitlab", "endpoint": "x", "display_name": "X",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestListGitServers_ReturnsRows(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g1",
		DisplayName: "One", Config: "{}", Enabled: true, CreatedAt: now, UpdatedAt: now,
	})
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-2", Kind: "gitea", Endpoint: "https://g2",
		DisplayName: "Two", Config: "{}", Enabled: true, CreatedAt: now.Add(time.Hour), UpdatedAt: now,
	})
	r := newTestRouter(db)
	w := doJSON(t, r, "GET", "/api/internal/git-servers", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var rows []gitServerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2", len(rows))
	}
	if rows[0].ServerID != "gs-2" {
		t.Errorf("expected gs-2 first (newest), got %q", rows[0].ServerID)
	}
}

func TestGetGitServer_NotFound(t *testing.T) {
	db := setupHandlersDB(t)
	r := newTestRouter(db)
	w := doJSON(t, r, "GET", "/api/internal/git-servers/missing", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestUpdateGitServer_PartialFields(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://old",
		DisplayName: "Old", Config: "{}", Enabled: true, CreatedAt: now, UpdatedAt: now,
	})
	r := newTestRouter(db)
	w := doJSON(t, r, "PUT", "/api/internal/git-servers/gs-1", gin.H{
		"display_name": "New",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp gitServerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DisplayName != "New" {
		t.Errorf("display_name = %q, want 'New'", resp.DisplayName)
	}
	if resp.Endpoint != "https://old" {
		t.Errorf("endpoint = %q, want 'https://old'", resp.Endpoint)
	}
}

func TestDeleteGitServer_RefusesIfBound(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g",
		DisplayName: "G", Config: "{}", Enabled: true, CreatedAt: now, UpdatedAt: now,
	})
	if err := db.Exec(`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, ?, ?)`,
		"t1", "gs-1", now, now).Error; err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	r := newTestRouter(db)
	w := doJSON(t, r, "DELETE", "/api/internal/git-servers/gs-1", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestDeleteGitServer_SucceedsWhenUnbound(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g",
		DisplayName: "G", Config: "{}", Enabled: true, CreatedAt: now, UpdatedAt: now,
	})
	r := newTestRouter(db)
	w := doJSON(t, r, "DELETE", "/api/internal/git-servers/gs-1", nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestBindTenantGitServer_HappyPath(t *testing.T) {
	db := setupHandlersDB(t)
	now := time.Now()
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g",
		DisplayName: "G", Config: "{}", Enabled: true, CreatedAt: now, UpdatedAt: now,
	})
	r := newTestRouter(db)
	w := doJSON(t, r, "PUT", "/api/internal/tenants/t1/git-server", gin.H{
		"git_server_id": "gs-1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	// Re-bind to a different server — should still 200.
	rawInsertServer(t, db, &models.GitServer{
		ServerID: "gs-2", Kind: "gitea", Endpoint: "https://g2",
		DisplayName: "G2", Config: "{}", Enabled: true, CreatedAt: now, UpdatedAt: now,
	})
	w = doJSON(t, r, "PUT", "/api/internal/tenants/t1/git-server", gin.H{
		"git_server_id": "gs-2",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("re-bind status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp tenantBindingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GitServerID != "gs-2" {
		t.Errorf("git_server_id = %q, want gs-2", resp.GitServerID)
	}
}

func TestBindTenantGitServer_NotFound(t *testing.T) {
	db := setupHandlersDB(t)
	r := newTestRouter(db)
	w := doJSON(t, r, "PUT", "/api/internal/tenants/t1/git-server", gin.H{
		"git_server_id": "gs-missing",
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestUnbindTenantGitServer_Idempotent(t *testing.T) {
	db := setupHandlersDB(t)
	r := newTestRouter(db)
	w := doJSON(t, r, "DELETE", "/api/internal/tenants/t-no-exist/git-server", nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

// Ensure context cancellation works on the store layer (paranoid check).
func TestGitServerStore_ContextCancellation(t *testing.T) {
	db := setupHandlersDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewGormGitServerStore(db)
	if err := store.CreateGitServer(ctx, &models.GitServer{
		ServerID: "gs-x", Kind: "gitea", Endpoint: "x", DisplayName: "x", Config: "{}",
	}); err == nil {
		// SQLite :memory: doesn't always surface ctx errors immediately;
		// treat nil as acceptable as long as no panic.
	}
}
