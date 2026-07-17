//go:build cgo

package models

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newPlatformAdminDB mirrors the tenant_test.go fixture for in-memory sqlite +
// AutoMigrate. cgo-gated because sqlite needs CGO.
func newPlatformAdminDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&PlatformAdmin{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestPlatformAdmin_DefaultsScopeFull verifies the documented default scope
// is "full" when not explicitly set — matches the migration's DEFAULT 'full'
// and the §14.3 scope matrix.
func TestPlatformAdmin_DefaultsScopeFull(t *testing.T) {
	t.Parallel()
	db := newPlatformAdminDB(t)

	a := &PlatformAdmin{UserID: "u-001", GrantedBy: "u-001"}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got PlatformAdmin
	if err := db.First(&got, "user_id = ?", "u-001").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Scope != PlatformScopeFull {
		t.Errorf("scope default: got %q, want %q", got.Scope, PlatformScopeFull)
	}
	// Note: granted_at's DEFAULT now() lives in the Postgres migration; sqlite
	// AutoMigrate does not honor it. The migration's DEFAULT is exercised in
	// real-deploy schema verification, not in unit tests. Same convention as
	// tenant_test.go's TestTenant_DefaultRowInsert — that test also does not
	// assert GrantedAt non-zero on a row created via AutoMigrate.
}

// TestPlatformAdmin_ExplicitScopeRoundTrip verifies each scope constant
// round-trips through Create→First without normalization.
func TestPlatformAdmin_ExplicitScopeRoundTrip(t *testing.T) {
	t.Parallel()
	db := newPlatformAdminDB(t)

	cases := []string{PlatformScopeFull, PlatformScopeSupport, PlatformScopeReadOnly}
	for i, scope := range cases {
		uid := "u-scope-" + strings.ToLower(scope)
		a := &PlatformAdmin{UserID: uid, GrantedBy: "u-installer", Scope: scope}
		if err := db.Create(a).Error; err != nil {
			t.Fatalf("case %d create %q: %v", i, scope, err)
		}
		var got PlatformAdmin
		if err := db.First(&got, "user_id = ?", uid).Error; err != nil {
			t.Fatalf("case %d First: %v", i, err)
		}
		if got.Scope != scope {
			t.Errorf("case %d scope: got %q, want %q", i, got.Scope, scope)
		}
	}
}

// TestPlatformAdmin_UserIDPrimaryKeyRejectsDuplicate verifies the user_id PK
// — one platform_admin row per user. To change scope, UPDATE; not INSERT.
func TestPlatformAdmin_UserIDPrimaryKeyRejectsDuplicate(t *testing.T) {
	t.Parallel()
	db := newPlatformAdminDB(t)

	a := &PlatformAdmin{UserID: "u-dup", GrantedBy: "u-1", Scope: PlatformScopeFull}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}
	err := db.Create(&PlatformAdmin{UserID: "u-dup", GrantedBy: "u-2", Scope: PlatformScopeReadOnly}).Error
	if err == nil {
		t.Fatal("expected PK constraint failure on duplicate user_id, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected UNIQUE constraint failure, got: %v", err)
	}
}

// TestPlatformAdmin_UpdateScope verifies scope changes go through UPDATE
// (the documented lifecycle), not INSERT-with-different-scope.
func TestPlatformAdmin_UpdateScope(t *testing.T) {
	t.Parallel()
	db := newPlatformAdminDB(t)

	a := &PlatformAdmin{UserID: "u-up", GrantedBy: "u-1", Scope: PlatformScopeFull}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Model(a).Where("user_id = ?", "u-up").
		Update("scope", PlatformScopeReadOnly).Error; err != nil {
		t.Fatalf("update scope: %v", err)
	}
	var got PlatformAdmin
	if err := db.First(&got, "user_id = ?", "u-up").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Scope != PlatformScopeReadOnly {
		t.Errorf("scope after update: got %q, want %q", got.Scope, PlatformScopeReadOnly)
	}
}

// TestPlatformAdmin_DeleteRevokesGrant verifies DELETE removes the row —
// platform_admins has no soft-delete column; revocation is hard delete,
// audit trail lives in user_center_audit_log per §16.2.
func TestPlatformAdmin_DeleteRevokesGrant(t *testing.T) {
	t.Parallel()
	db := newPlatformAdminDB(t)

	a := &PlatformAdmin{UserID: "u-del", GrantedBy: "u-1", Scope: PlatformScopeFull}
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Where("user_id = ?", "u-del").Delete(&PlatformAdmin{}).Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	var got PlatformAdmin
	err := db.First(&got, "user_id = ?", "u-del").Error
	if err == nil {
		t.Fatal("expected record-not-found after delete, got nil error")
	}
}
