//go:build cgo

package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newPermissionTestService mirrors newTestService but adds the permission-
// related tables (TenantAdmin + PlatformAdmin) to the AutoMigrate list so the
// permission reader methods have a schema to query against.
func newPermissionTestService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&models.User{},
		&models.TenantAdmin{},
		&models.PlatformAdmin{},
	); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return NewService(db)
}

// --- GetPlatformAdmin ---

// TestGetPlatformAdmin_HappyPath verifies a single row round-trips through
// the reader.
func TestGetPlatformAdmin_HappyPath(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	seedPlatformAdmin(t, svc, "usr-pa-1", "usr-installer", models.PlatformScopeFull)

	got, err := svc.GetPlatformAdmin(context.Background(), "usr-pa-1")
	if err != nil {
		t.Fatalf("GetPlatformAdmin: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil platform admin")
	}
	if got.UserID != "usr-pa-1" {
		t.Errorf("UserID: got %q", got.UserID)
	}
	if got.Scope != models.PlatformScopeFull {
		t.Errorf("Scope: got %q, want %q", got.Scope, models.PlatformScopeFull)
	}
}

// TestGetPlatformAdmin_NotFoundReturnsNil verifies the graceful-degradation
// contract: a user with no platform_admins row returns (nil, nil), not an
// error. The reissue-token handler uses this to decide whether to emit the
// platform_admin / platform_scope claims.
func TestGetPlatformAdmin_NotFoundReturnsNil(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)

	got, err := svc.GetPlatformAdmin(context.Background(), "usr-nope")
	if err != nil {
		t.Fatalf("expected nil error for missing row, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil row for missing user, got %+v", got)
	}
}

// TestGetPlatformAdmin_EmptySubjectIDErrors verifies the caller-programming-
// error path — empty subject surfaces ErrEmptySubjectID (handler maps to 400).
func TestGetPlatformAdmin_EmptySubjectIDErrors(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	_, err := svc.GetPlatformAdmin(context.Background(), "")
	if !errors.Is(err, ErrEmptySubjectID) {
		t.Errorf("expected ErrEmptySubjectID, got %v", err)
	}
}

// TestGetPlatformAdmin_NilDBGuard verifies nil-receiver safety — callers may
// hold a nil *Service during boot/wiring.
func TestGetPlatformAdmin_NilDBGuard(t *testing.T) {
	t.Parallel()
	var svc *Service
	_, err := svc.GetPlatformAdmin(context.Background(), "usr-x")
	if err == nil {
		t.Fatal("expected error on nil service, got nil")
	}
}

// --- ListActiveTenantRoles ---

// TestListActiveTenantRoles_HappyPath verifies a single active role for a
// user comes back in the slice. The composite PK (tenant_id, user_id) limits
// each user to exactly one role per tenant — so a "multi-role" path is not
// representable in the schema; the TenantScoped test below covers the
// across-tenant fan-out.
func TestListActiveTenantRoles_HappyPath(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	seedTenantAdmin(t, svc, "t-acme", "u-1", models.TenantRoleOwner)

	roles, err := svc.ListActiveTenantRoles(context.Background(), "u-1", "t-acme")
	if err != nil {
		t.Fatalf("ListActiveTenantRoles: %v", err)
	}
	if len(roles) != 1 || roles[0] != models.TenantRoleOwner {
		t.Fatalf("roles: got %v, want [owner]", roles)
	}
}

// TestListActiveTenantRoles_SkipsRevoked verifies the revoked_at IS NULL
// filter — a revoked row must not surface in the result.
func TestListActiveTenantRoles_SkipsRevoked(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	seedTenantAdmin(t, svc, "t-acme", "u-rev", models.TenantRoleAdmin)
	// Now revoke the row directly via gorm (no Service.revoke helper yet).
	now := time.Now()
	if err := svc.db.Model(&models.TenantAdmin{}).
		Where("tenant_id = ? AND user_id = ?", "t-acme", "u-rev").
		Update("revoked_at", now).Error; err != nil {
		t.Fatalf("revoke: %v", err)
	}

	roles, err := svc.ListActiveTenantRoles(context.Background(), "u-rev", "t-acme")
	if err != nil {
		t.Fatalf("ListActiveTenantRoles: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("revoked roles should not surface: got %v", roles)
	}
}

// TestListActiveTenantRoles_TenantScoped verifies the same user has
// different roles across tenants — the (user_id, tenant_id) filter is the
// correctness gate against cross-tenant role leakage.
func TestListActiveTenantRoles_TenantScoped(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	seedTenantAdmin(t, svc, "t-acme", "u-multi", models.TenantRoleOwner)
	seedTenantAdmin(t, svc, "t-globex", "u-multi", models.TenantRoleBilling)

	acme, err := svc.ListActiveTenantRoles(context.Background(), "u-multi", "t-acme")
	if err != nil {
		t.Fatalf("acme: %v", err)
	}
	if len(acme) != 1 || acme[0] != models.TenantRoleOwner {
		t.Errorf("acme roles: got %v, want [owner]", acme)
	}
	globex, err := svc.ListActiveTenantRoles(context.Background(), "u-multi", "t-globex")
	if err != nil {
		t.Fatalf("globex: %v", err)
	}
	if len(globex) != 1 || globex[0] != models.TenantRoleBilling {
		t.Errorf("globex roles: got %v, want [billing]", globex)
	}
}

// TestListActiveTenantRoles_NotFoundReturnsEmpty verifies a user with no
// tenant_admins rows returns an empty (non-nil) slice — distinguishes "no
// admin role" from "lookup failed".
func TestListActiveTenantRoles_NotFoundReturnsEmpty(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	roles, err := svc.ListActiveTenantRoles(context.Background(), "u-nobody", "t-acme")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if roles == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(roles) != 0 {
		t.Errorf("expected empty slice, got %v", roles)
	}
}

// TestListActiveTenantRoles_EmptyArgsErrors verifies both inputs are
// required — empty subject or tenant surfaces the respective sentinel.
func TestListActiveTenantRoles_EmptyArgsErrors(t *testing.T) {
	t.Parallel()
	svc := newPermissionTestService(t)
	_, err := svc.ListActiveTenantRoles(context.Background(), "", "t-acme")
	if !errors.Is(err, ErrEmptySubjectID) {
		t.Errorf("empty subject: expected ErrEmptySubjectID, got %v", err)
	}
	_, err = svc.ListActiveTenantRoles(context.Background(), "u-x", "")
	if !errors.Is(err, ErrEmptyTenantID) {
		t.Errorf("empty tenant: expected ErrEmptyTenantID, got %v", err)
	}
}

// --- helpers ---

func seedPlatformAdmin(t *testing.T, svc *Service, userID, grantedBy, scope string) {
	t.Helper()
	row := &models.PlatformAdmin{
		UserID:    userID,
		GrantedBy: grantedBy,
		Scope:     scope,
	}
	if err := svc.db.Create(row).Error; err != nil {
		t.Fatalf("seed platform_admin %s: %v", userID, err)
	}
}

func seedTenantAdmin(t *testing.T, svc *Service, tenantID, userID, role string) {
	t.Helper()
	row := &models.TenantAdmin{
		TenantID:  tenantID,
		UserID:    userID,
		Role:      role,
		GrantedBy: "u-installer",
	}
	if err := svc.db.Create(row).Error; err != nil {
		t.Fatalf("seed tenant_admin %s/%s: %v", tenantID, userID, err)
	}
}
