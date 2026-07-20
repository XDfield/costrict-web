//go:build cgo

package gitserver

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBootstrapDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Tenant{}, &models.GitServer{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestBootstrap_CreatesTemplateFromEnv covers the cold-start case: no
// template row exists, env input is supplied → row created with is_template=true.
func TestBootstrap_CreatesTemplateFromEnv(t *testing.T) {
	t.Parallel()
	db := newBootstrapDB(t)

	serverID, err := BootstrapTemplate(context.Background(), db, TemplateInput{
		Endpoint:   "https://gitea.example.com",
		AdminToken: "tok-xyz",
	})
	if err != nil {
		t.Fatalf("BootstrapTemplate: %v", err)
	}
	if serverID == "" {
		t.Fatal("server_id empty")
	}

	var gs models.GitServer
	if err := db.First(&gs, "server_id = ?", serverID).Error; err != nil {
		t.Fatalf("load template: %v", err)
	}
	if !gs.IsTemplate {
		t.Error("IsTemplate: got false, want true")
	}
	if gs.Endpoint != "https://gitea.example.com" {
		t.Errorf("Endpoint: got %q", gs.Endpoint)
	}
	if !gs.Enabled {
		t.Error("Enabled: got false, want true")
	}
	if gs.Config != `{"admin_token":"tok-xyz"}` {
		t.Errorf("Config: got %q", gs.Config)
	}
}

// TestBootstrap_IdempotentOnReRun verifies that a second call returns the
// existing template's server_id without mutating the row.
func TestBootstrap_IdempotentOnReRun(t *testing.T) {
	t.Parallel()
	db := newBootstrapDB(t)

	in := TemplateInput{Endpoint: "https://gitea.example.com", AdminToken: "tok"}
	first, err := BootstrapTemplate(context.Background(), db, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Even with different display name / token, the existing row wins.
	second, err := BootstrapTemplate(context.Background(), db, TemplateInput{
		Endpoint:    "https://gitea.example.com",
		AdminToken:  "different-token",
		DisplayName: "Should Not Win",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("server_id drift: first=%s second=%s", first, second)
	}
	var gs models.GitServer
	_ = db.First(&gs, "server_id = ?", first).Error
	if gs.DisplayName == "Should Not Win" {
		t.Error("idempotent re-run mutated display_name")
	}
}

// TestBootstrap_ErrNoTemplateInputOnEmptyEnv confirms that unset env vars
// surface ErrNoTemplateInput (caller treats as "feature disabled").
func TestBootstrap_ErrNoTemplateInputOnEmptyEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   TemplateInput
	}{
		{"both_empty", TemplateInput{}},
		{"endpoint_only", TemplateInput{Endpoint: "https://x.example.com"}},
		{"token_only", TemplateInput{AdminToken: "tok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BootstrapTemplate(context.Background(), newBootstrapDB(t), tc.in)
			if !errors.Is(err, ErrNoTemplateInput) {
				t.Fatalf("got err=%v, want ErrNoTemplateInput", err)
			}
		})
	}
}

// TestBootstrap_BackfillsUnboundTenants verifies the migration-window repair:
// existing tenants without git_server_id get bound to the new template row
// inside the same transaction.
func TestBootstrap_BackfillsUnboundTenants(t *testing.T) {
	t.Parallel()
	db := newBootstrapDB(t)
	if err := db.Create(&models.Tenant{
		TenantID: "t-old-1",
		Slug:     "old1",
		Status:   "active",
	}).Error; err != nil {
		t.Fatalf("seed t-old-1: %v", err)
	}
	if err := db.Create(&models.Tenant{
		TenantID: "t-old-2",
		Slug:     "old2",
		Status:   "active",
	}).Error; err != nil {
		t.Fatalf("seed t-old-2: %v", err)
	}

	serverID, err := BootstrapTemplate(context.Background(), db, TemplateInput{
		Endpoint:   "https://gitea.example.com",
		AdminToken: "tok",
	})
	if err != nil {
		t.Fatalf("BootstrapTemplate: %v", err)
	}

	for _, tid := range []string{"t-old-1", "t-old-2"} {
		var tn models.Tenant
		if err := db.First(&tn, "tenant_id = ?", tid).Error; err != nil {
			t.Fatalf("load %s: %v", tid, err)
		}
		if tn.GitServerID == nil || *tn.GitServerID != serverID {
			t.Errorf("%s: git_server_id got %v, want %s", tid, tn.GitServerID, serverID)
		}
	}
}

// TestBootstrap_BackfillDoesntTouchBoundTenants ensures the backfill UPDATE
// only targets rows where git_server_id IS NULL.
func TestBootstrap_BackfillDoesntTouchBoundTenants(t *testing.T) {
	t.Parallel()
	db := newBootstrapDB(t)
	// Pre-existing template + bound tenant — second bootstrap shouldn't
	// rebind it.
	first, err := BootstrapTemplate(context.Background(), db, TemplateInput{
		Endpoint:   "https://gitea.example.com",
		AdminToken: "tok",
	})
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := db.Create(&models.Tenant{
		TenantID:    "t-bound",
		Slug:        "bound",
		Status:      "active",
		GitServerID: strPtr(first),
	}).Error; err != nil {
		t.Fatalf("seed t-bound: %v", err)
	}
	if err := db.Create(&models.Tenant{
		TenantID: "t-unbound",
		Slug:     "unbound",
		Status:   "active",
	}).Error; err != nil {
		t.Fatalf("seed t-unbound: %v", err)
	}

	// Second boot — should leave t-bound alone, bind t-unbound.
	_, err = BootstrapTemplate(context.Background(), db, TemplateInput{
		Endpoint:   "https://gitea.example.com",
		AdminToken: "tok",
	})
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	var bound models.Tenant
	_ = db.First(&bound, "tenant_id = ?", "t-bound").Error
	if bound.GitServerID == nil || *bound.GitServerID != first {
		t.Errorf("t-bound rebound: got %v", bound.GitServerID)
	}

	var unbound models.Tenant
	_ = db.First(&unbound, "tenant_id = ?", "t-unbound").Error
	if unbound.GitServerID == nil || *unbound.GitServerID != first {
		t.Errorf("t-unbound not bound: got %v", unbound.GitServerID)
	}
}
