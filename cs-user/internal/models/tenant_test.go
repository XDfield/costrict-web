//go:build cgo

package models

import (
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newTenantDB mirrors the tenant_config_test.go fixture so both models share
// the same in-memory sqlite + AutoMigrate pattern. cgo-gated because sqlite
// needs CGO to build.
func newTenantDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Tenant{}, &TenantAdmin{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// --- Tenant ---

// TestTenant_DefaultRowInsert verifies that a Tenant created with only the
// required identifying fields picks up the documented defaults (status,
// edition, the four JSON columns, timestamps).
func TestTenant_DefaultRowInsert(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	tn := &Tenant{TenantID: "t-001", Slug: "acme", DisplayName: "Acme Inc."}
	if err := db.Create(tn).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got Tenant
	if err := db.First(&got, "tenant_id = ?", "t-001").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Status != "active" {
		t.Errorf("status default: got %q, want active", got.Status)
	}
	if got.Edition != "team" {
		t.Errorf("edition default: got %q, want team", got.Edition)
	}
	if got.EmailDomains != "[]" {
		t.Errorf("email_domains default: got %q, want []", got.EmailDomains)
	}
	for col, want := range map[string]string{
		"Features": "{}",
		"Limits":   "{}",
		"Settings": "{}",
	} {
		switch col {
		case "Features":
			if got.Features != want {
				t.Errorf("features default: got %q, want %q", got.Features, want)
			}
		case "Limits":
			if got.Limits != want {
				t.Errorf("limits default: got %q, want %q", got.Limits, want)
			}
		case "Settings":
			if got.Settings != want {
				t.Errorf("settings default: got %q, want %q", got.Settings, want)
			}
		}
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should default to non-zero timestamp")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at should default to non-zero timestamp")
	}
	if got.DeletionRequestedAt != nil {
		t.Errorf("deletion_requested_at default: got %v, want nil", got.DeletionRequestedAt)
	}
	if got.DeletedAt != nil {
		t.Errorf("deleted_at default: got %v, want nil", got.DeletedAt)
	}
}

// TestTenant_JSONColumnRoundTrip verifies the four JSON-holding TEXT columns
// (email_domains / features / limits / settings) round-trip byte-for-byte
// through Create → First. Confirms GORM doesn't escape / trim the JSON text
// (matches the EmploymentIdentity.Attributes convention).
func TestTenant_JSONColumnRoundTrip(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	cases := map[string]string{
		"EmailDomains": `["example.com","example.cn"]`,
		"Features":     `{"ai_assistant":true,"sso":false}`,
		"Limits":       `{"max_users":100,"max_seats":50}`,
		"Settings":     `{"locale":"zh-CN","timezone":"Asia/Shanghai"}`,
	}
	tn := &Tenant{
		TenantID:     "t-002",
		Slug:         "globex",
		DisplayName:  "Globex Corp",
		EmailDomains: cases["EmailDomains"],
		Features:     cases["Features"],
		Limits:       cases["Limits"],
		Settings:     cases["Settings"],
	}
	if err := db.Create(tn).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got Tenant
	if err := db.First(&got, "tenant_id = ?", "t-002").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.EmailDomains != cases["EmailDomains"] {
		t.Errorf("email_domains round-trip:\n got: %q\nwant: %q", got.EmailDomains, cases["EmailDomains"])
	}
	if got.Features != cases["Features"] {
		t.Errorf("features round-trip:\n got: %q\nwant: %q", got.Features, cases["Features"])
	}
	if got.Limits != cases["Limits"] {
		t.Errorf("limits round-trip:\n got: %q\nwant: %q", got.Limits, cases["Limits"])
	}
	if got.Settings != cases["Settings"] {
		t.Errorf("settings round-trip:\n got: %q\nwant: %q", got.Settings, cases["Settings"])
	}
}

// TestTenant_TenantIDPrimaryKeyRejectsDuplicate asserts the PK on tenant_id
// rejects a second insert with the same value. Tenants are not soft-deleted
// via this table's PK (DeletedAt is informational; the canonical lifecycle
// goes through status='deleted').
func TestTenant_TenantIDPrimaryKeyRejectsDuplicate(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	if err := db.Create(&Tenant{TenantID: "dup", Slug: "first", DisplayName: "First"}).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}
	err := db.Create(&Tenant{TenantID: "dup", Slug: "second", DisplayName: "Second"}).Error
	if err == nil {
		t.Fatal("expected PK constraint failure on duplicate tenant_id, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected UNIQUE constraint failure, got: %v", err)
	}
}

// TestTenant_SlugUniqueRejectsDuplicate verifies the slug unique constraint
// (the global URL-safe identifier) — distinct tenant_ids with the same slug
// must collide.
func TestTenant_SlugUniqueRejectsDuplicate(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	if err := db.Create(&Tenant{TenantID: "t-a", Slug: "shared", DisplayName: "A"}).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}
	err := db.Create(&Tenant{TenantID: "t-b", Slug: "shared", DisplayName: "B"}).Error
	if err == nil {
		t.Fatal("expected UNIQUE constraint failure on duplicate slug, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected UNIQUE constraint failure, got: %v", err)
	}
}

// --- TenantAdmin ---

// TestTenantAdmin_CompositePrimaryKeyRejectsDuplicate verifies the (tenant_id,
// user_id) composite PK — the same user can't be granted a role in the same
// tenant twice (revoking first via RevokedAt is required before re-granting).
// Note: the same user CAN appear in multiple tenants (multi-tenant membership).
func TestTenantAdmin_CompositePrimaryKeyRejectsDuplicate(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	a1 := &TenantAdmin{TenantID: "t-1", UserID: "u-1", Role: TenantRoleOwner, GrantedBy: "u-1"}
	if err := db.Create(a1).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}
	// Same (tenant_id, user_id) → must fail regardless of role
	err := db.Create(&TenantAdmin{TenantID: "t-1", UserID: "u-1", Role: TenantRoleAdmin, GrantedBy: "u-2"}).Error
	if err == nil {
		t.Fatal("expected composite PK failure, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected UNIQUE constraint failure, got: %v", err)
	}
}

// TestTenantAdmin_SameUserAcrossMultipleTenants verifies the multi-tenant
// membership case — one user_id with active grants in N distinct tenants.
// This is the inverse of the composite PK test.
func TestTenantAdmin_SameUserAcrossMultipleTenants(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	for _, tid := range []string{"t-a", "t-b", "t-c"} {
		a := &TenantAdmin{TenantID: tid, UserID: "u-shared", Role: TenantRoleAdmin, GrantedBy: "u-installer"}
		if err := db.Create(a).Error; err != nil {
			t.Fatalf("create %s: %v", tid, err)
		}
	}
	var count int64
	if err := db.Model(&TenantAdmin{}).Where("user_id = ?", "u-shared").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("active grants for u-shared across tenants: got %d, want 3", count)
	}
}

// TestTenantAdmin_RevokeSetsTimestampWithoutDeleting verifies the audit-trail
// contract: revoking sets RevokedAt without deleting the row. Future
// re-granting requires the historical row to be replaced (PK still occupied).
func TestTenantAdmin_RevokeSetsTimestampWithoutDeleting(t *testing.T) {
	t.Parallel()
	db := newTenantDB(t)

	a := &TenantAdmin{TenantID: "t-1", UserID: "u-9", Role: TenantRoleBilling, GrantedBy: "u-admin"}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now()
	if err := db.Model(a).Where("tenant_id = ? AND user_id = ?", "t-1", "u-9").
		Update("RevokedAt", now).Error; err != nil {
		t.Fatalf("update revoked_at: %v", err)
	}
	var got TenantAdmin
	if err := db.First(&got, "tenant_id = ? AND user_id = ?", "t-1", "u-9").Error; err != nil {
		t.Fatalf("First after revoke: %v", err)
	}
	if got.RevokedAt == nil {
		t.Fatal("revoked_at should be set, got nil")
	}
}
