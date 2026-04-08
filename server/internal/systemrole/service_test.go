package systemrole

import (
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSystemRoleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserSystemRole{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	users := []models.User{
		{SubjectID: "u1", Username: "u1", IsActive: true},
		{SubjectID: "u2", Username: "u2", IsActive: true},
		{SubjectID: "u3", Username: "u3", IsActive: true},
	}
	for _, user := range users {
		if err := db.Create(&user).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	return db
}

func TestGrantAndListRoles(t *testing.T) {
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)

	if err := svc.GrantRole("u1", SystemRoleBusinessAdmin, "u2"); err != nil {
		t.Fatalf("GrantRole error: %v", err)
	}
	roles, err := svc.ListRoles("u1")
	if err != nil {
		t.Fatalf("ListRoles error: %v", err)
	}
	if len(roles) != 1 || roles[0] != SystemRoleBusinessAdmin {
		t.Fatalf("unexpected roles: %+v", roles)
	}
	capabilities, err := svc.GetCapabilities("u1")
	if err != nil {
		t.Fatalf("GetCapabilities error: %v", err)
	}
	if len(capabilities) == 0 {
		t.Fatalf("expected business admin capabilities, got empty")
	}
}

func TestPlatformAdminImpliesBusinessAdmin(t *testing.T) {
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)
	if err := svc.GrantRole("u1", SystemRolePlatformAdmin, "u2"); err != nil {
		t.Fatalf("GrantRole error: %v", err)
	}
	hasRole, err := svc.HasRole("u1", SystemRoleBusinessAdmin)
	if err != nil {
		t.Fatalf("HasRole error: %v", err)
	}
	if !hasRole {
		t.Fatalf("expected platform admin to imply business admin")
	}
}

func TestGrantRejectsInvalidRoleAndMissingUser(t *testing.T) {
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)
	if err := svc.GrantRole("u1", "invalid", "u2"); !errors.Is(err, ErrInvalidSystemRole) {
		t.Fatalf("expected ErrInvalidSystemRole, got %v", err)
	}
	if err := svc.GrantRole("missing", SystemRoleBusinessAdmin, "u2"); !errors.Is(err, ErrSystemRoleUserNotFound) {
		t.Fatalf("expected ErrSystemRoleUserNotFound, got %v", err)
	}
}

func TestCannotRevokeLastPlatformAdmin(t *testing.T) {
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)
	if err := svc.GrantRole("u1", SystemRolePlatformAdmin, "u2"); err != nil {
		t.Fatalf("GrantRole error: %v", err)
	}
	if err := svc.RevokeRole("u1", SystemRolePlatformAdmin, "u2"); !errors.Is(err, ErrCannotRevokeLastPlatformAdmin) {
		t.Fatalf("expected ErrCannotRevokeLastPlatformAdmin, got %v", err)
	}
	if err := svc.GrantRole("u2", SystemRolePlatformAdmin, "u1"); err != nil {
		t.Fatalf("GrantRole second admin error: %v", err)
	}
	if err := svc.RevokeRole("u1", SystemRolePlatformAdmin, "u2"); err != nil {
		t.Fatalf("RevokeRole error: %v", err)
	}
}

func TestListUsersByRoleIncludesPlatformAdminForBusinessAdminQuery(t *testing.T) {
	db := setupSystemRoleTestDB(t)
	svc := NewSystemRoleService(db)
	_ = svc.GrantRole("u1", SystemRolePlatformAdmin, "u2")
	_ = svc.GrantRole("u2", SystemRoleBusinessAdmin, "u1")
	users, err := svc.ListUsersByRole(SystemRoleBusinessAdmin)
	if err != nil {
		t.Fatalf("ListUsersByRole error: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %+v", users)
	}
}
