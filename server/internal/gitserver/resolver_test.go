// Tests for gitserver.DBResolver (Git Ownership Refactor P1.7).

package gitserver

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupResolverDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE git_servers (
		server_id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		display_name TEXT NOT NULL,
		config TEXT NOT NULL DEFAULT '{}',
		is_template INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create git_servers: %v", err)
	}
	if err := db.Exec(`CREATE TABLE tenant_git_server_binding (
		tenant_id TEXT PRIMARY KEY,
		git_server_id TEXT NOT NULL,
		bound_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create binding: %v", err)
	}
	return db
}

func seedServer(t *testing.T, db *gorm.DB, gs *models.GitServer) {
	t.Helper()
	// Use raw SQL — GORM Create silently swaps zero-value Enabled=false for
	// the column default, which would break the disabled-server test.
	enabled := 1
	if !gs.Enabled {
		enabled = 0
	}
	isTpl := 0
	if gs.IsTemplate {
		isTpl = 1
	}
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		gs.ServerID, gs.Kind, gs.Endpoint, gs.DisplayName, gs.Config, isTpl, enabled,
	).Error; err != nil {
		t.Fatalf("seed server: %v", err)
	}
}

func seedBinding(t *testing.T, db *gorm.DB, tenantID, serverID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, tenantID, serverID).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

func TestDBResolver_NoBindingReturnsTenantMissing(t *testing.T) {
	r := NewDBResolver(setupResolverDB(t))
	_, err := r.Resolve(context.Background(), "t1")
	if !errors.Is(err, ErrTenantMissingGitServer) {
		t.Errorf("got %v, want ErrTenantMissingGitServer", err)
	}
}

func TestDBResolver_DisabledServerReturnsDisabled(t *testing.T) {
	db := setupResolverDB(t)
	seedServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g.example",
		DisplayName: "x", Config: `{"admin_token":"tok"}`, Enabled: false,
	})
	seedBinding(t, db, "t1", "gs-1")
	_, err := NewDBResolver(db).Resolve(context.Background(), "t1")
	if !errors.Is(err, ErrGitServerDisabled) {
		t.Errorf("got %v, want ErrGitServerDisabled", err)
	}
}

func TestDBResolver_MissingAdminTokenReturnsMalformed(t *testing.T) {
	db := setupResolverDB(t)
	seedServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g.example",
		DisplayName: "x", Config: `{}`, Enabled: true,
	})
	seedBinding(t, db, "t1", "gs-1")
	_, err := NewDBResolver(db).Resolve(context.Background(), "t1")
	if !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("got %v, want ErrConfigMalformed", err)
	}
}

func TestDBResolver_HappyPathReturnsFullConfig(t *testing.T) {
	db := setupResolverDB(t)
	seedServer(t, db, &models.GitServer{
		ServerID: "gs-1", Kind: "gitea", Endpoint: "https://g.example/",
		DisplayName: "x", Config: `{"admin_token":"tok","admin_user":"root","admin_password":"pw"}`,
		Enabled: true,
	})
	seedBinding(t, db, "t1", "gs-1")
	cfg, err := NewDBResolver(db).Resolve(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Endpoint != "https://g.example/" {
		t.Errorf("endpoint = %q", cfg.Endpoint)
	}
	if cfg.AdminToken != "tok" {
		t.Errorf("admin_token = %q", cfg.AdminToken)
	}
	if cfg.AdminUser != "root" || cfg.AdminPassword != "pw" {
		t.Errorf("admin_user/pw = %q/%q", cfg.AdminUser, cfg.AdminPassword)
	}
}

func TestDBResolver_FKViolationReturnsNotFound(t *testing.T) {
	db := setupResolverDB(t)
	seedBinding(t, db, "t1", "gs-missing")
	_, err := NewDBResolver(db).Resolve(context.Background(), "t1")
	if !errors.Is(err, ErrGitServerNotFound) {
		t.Errorf("got %v, want ErrGitServerNotFound", err)
	}
}

func TestBootstrapTemplate_Idempotent(t *testing.T) {
	db := setupResolverDB(t)
	in := TemplateInput{Endpoint: "https://g.example", AdminToken: "tok"}
	id1, err := BootstrapTemplate(context.Background(), db, in)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	id2, err := BootstrapTemplate(context.Background(), db, in)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id1 != id2 {
		t.Errorf("template id changed: %q → %q", id1, id2)
	}
}

func TestBootstrapTemplate_EmptyInputReturnsErrNoTemplateInput(t *testing.T) {
	db := setupResolverDB(t)
	_, err := BootstrapTemplate(context.Background(), db, TemplateInput{})
	if !errors.Is(err, ErrNoTemplateInput) {
		t.Errorf("got %v, want ErrNoTemplateInput", err)
	}
}
