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

// newResolverDB stands up an in-memory sqlite DB with both tenants and
// git_servers tables migrated. Tests seed rows directly.
func newResolverDB(t *testing.T) *gorm.DB {
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

func strPtr(s string) *string { return &s }

// TestResolver_HappyPath verifies the end-to-end flow: tenant.git_server_id
// → git_servers row → parsed admin_token returned in Config.
func TestResolver_HappyPath(t *testing.T) {
	t.Parallel()
	db := newResolverDB(t)

	if err := db.Create(&models.GitServer{
		ServerID:    "gs-acme",
		Kind:        models.GitServerKindGitea,
		Endpoint:    "https://gitea.acme.com",
		DisplayName: "Acme Gitea",
		Config:      `{"admin_token":"tok-acme-XYZ"}`,
	}).Error; err != nil {
		t.Fatalf("seed git_server: %v", err)
	}
	if err := db.Create(&models.Tenant{
		TenantID:    "t-acme",
		Slug:        "acme",
		DisplayName: "Acme",
		Status:      "active",
		GitServerID: strPtr("gs-acme"),
	}).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	r := NewDBResolver(db)
	got, err := r.Resolve(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServerID != "gs-acme" {
		t.Errorf("ServerID: got %q, want gs-acme", got.ServerID)
	}
	if got.Endpoint != "https://gitea.acme.com" {
		t.Errorf("Endpoint: got %q", got.Endpoint)
	}
	if got.AdminToken != "tok-acme-XYZ" {
		t.Errorf("AdminToken: got %q", got.AdminToken)
	}
	if got.Kind != models.GitServerKindGitea {
		t.Errorf("Kind: got %q", got.Kind)
	}
}

// TestResolver_TenantNotFound ensures an unknown tenant_id surfaces
// ErrTenantNotFound (handler maps to 404).
func TestResolver_TenantNotFound(t *testing.T) {
	t.Parallel()
	r := NewDBResolver(newResolverDB(t))
	_, err := r.Resolve(context.Background(), "t-ghost")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("got err=%v, want ErrTenantNotFound", err)
	}
}

// TestResolver_TenantMissingGitServerID exercises the migration-window case:
// tenant row exists but git_server_id is NULL (bootstrap hasn't run yet).
// Handler maps this to 500 / flagged for backfill.
func TestResolver_TenantMissingGitServerID(t *testing.T) {
	t.Parallel()
	db := newResolverDB(t)
	if err := db.Create(&models.Tenant{
		TenantID: "t-orphan",
		Slug:     "orphan",
		Status:   "active",
	}).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	r := NewDBResolver(db)
	_, err := r.Resolve(context.Background(), "t-orphan")
	if !errors.Is(err, ErrTenantMissingGitServer) {
		t.Fatalf("got err=%v, want ErrTenantMissingGitServer", err)
	}
}

// TestResolver_GitServerDisabled exercises the soft-disable switch: an
// enabled=false row is treated as unreachable so operators can drain.
func TestResolver_GitServerDisabled(t *testing.T) {
	t.Parallel()
	db := newResolverDB(t)
	if err := db.Create(&models.GitServer{
		ServerID:    "gs-drained",
		Kind:        models.GitServerKindGitea,
		Endpoint:    "https://drained.example.com",
		DisplayName: "Drained",
		Config:      `{"admin_token":"tok"}`,
	}).Error; err != nil {
		t.Fatalf("seed git_server: %v", err)
	}
	// gorm applies the `default:true` tag when the field is the zero-value
	// (false), so flip it via an explicit update.
	if err := db.Model(&models.GitServer{}).
		Where("server_id = ?", "gs-drained").
		Update("enabled", false).Error; err != nil {
		t.Fatalf("disable git_server: %v", err)
	}
	if err := db.Create(&models.Tenant{
		TenantID:    "t-drained",
		Slug:        "drained",
		Status:      "active",
		GitServerID: strPtr("gs-drained"),
	}).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	r := NewDBResolver(db)
	_, err := r.Resolve(context.Background(), "t-drained")
	if !errors.Is(err, ErrGitServerDisabled) {
		t.Fatalf("got err=%v, want ErrGitServerDisabled", err)
	}
}

// TestResolver_ConfigMissingAdminToken covers malformed JSON or missing
// admin_token: should surface ErrConfigMalformed (operator bug).
func TestResolver_ConfigMissingAdminToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		config string
	}{
		{"empty_json", `{}`},
		{"empty_admin_token", `{"admin_token":""}`},
		{"malformed", `{"admin_token":`},
		{"missing_key", `{"other":"value"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newResolverDB(t)
			if err := db.Create(&models.GitServer{
				ServerID:    "gs-bad",
				Kind:        models.GitServerKindGitea,
				Endpoint:    "https://bad.example.com",
				DisplayName: "Bad",
				Config:      tc.config,
			}).Error; err != nil {
				t.Fatalf("seed git_server: %v", err)
			}
			if err := db.Create(&models.Tenant{
				TenantID:    "t-bad",
				Slug:        "bad",
				Status:      "active",
				GitServerID: strPtr("gs-bad"),
			}).Error; err != nil {
				t.Fatalf("seed tenant: %v", err)
			}

			r := NewDBResolver(db)
			_, err := r.Resolve(context.Background(), "t-bad")
			if !errors.Is(err, ErrConfigMalformed) {
				t.Fatalf("got err=%v, want ErrConfigMalformed", err)
			}
		})
	}
}
