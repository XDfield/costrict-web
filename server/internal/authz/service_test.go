package authz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// roleProviderStub is a minimal RoleProvider for tests that don't exercise role
// resolution. The resource-permission list/update path never touches it.
type roleProviderStub struct {
	roles map[string][]string
}

func (s roleProviderStub) ListRoles(userID string) ([]string, error) {
	return s.roles[userID], nil
}

func (s roleProviderStub) GetExpandedRoles(userID string) ([]string, error) {
	return systemrole.ExpandRoles(s.roles[userID]), nil
}

// setupAuthzTestDB opens an in-memory sqlite DB and hand-creates the
// resource_permissions table. We deliberately do NOT use AutoMigrate: the model
// declares ID `default:gen_random_uuid()` and AllowedRoles as postgres `text[]`,
// both postgres-only. allowed_roles is a TEXT column here; pq.StringArray's
// Value()/Scan() round-trips through the postgres array literal format
// ("{a,b}"), which sqlite stores/returns verbatim, so the registry loads fine.
func setupAuthzTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.Exec(`CREATE TABLE resource_permissions (
		id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
		resource_code TEXT NOT NULL UNIQUE,
		resource_type TEXT NOT NULL,
		allowed_roles TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME,
		updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create resource_permissions table: %v", err)
	}

	// permission_grants backs HasPermission's grant fallback (CheckGrant). The
	// resource-permission tests never grant anything, so this stays empty, but the
	// table must exist for the fallback query not to error.
	if err := db.Exec(`CREATE TABLE permission_grants (
		id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
		permission_code TEXT NOT NULL,
		subject_type TEXT NOT NULL,
		subject_id TEXT NOT NULL,
		dept_path TEXT NOT NULL DEFAULT '',
		granted_by TEXT,
		created_at DATETIME,
		UNIQUE(permission_code, subject_type, subject_id)
	)`).Error; err != nil {
		t.Fatalf("create permission_grants table: %v", err)
	}

	seed := []models.ResourcePermission{
		{ResourceCode: "repositories", ResourceType: "menu", AllowedRoles: pq.StringArray{}},
		{ResourceCode: "capabilities", ResourceType: "menu", AllowedRoles: pq.StringArray{systemrole.SystemRolePlatformAdmin}},
		{ResourceCode: "kanban", ResourceType: "menu", AllowedRoles: pq.StringArray{systemrole.SystemRoleBusinessAdmin, systemrole.SystemRolePlatformAdmin}},
		{ResourceCode: "admin.system-roles", ResourceType: "api", AllowedRoles: pq.StringArray{systemrole.SystemRolePlatformAdmin}},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed resource_permissions: %v", err)
		}
	}
	return db
}

func newTestService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	svc, err := NewService(db, roleProviderStub{roles: map[string][]string{
		"admin_user": {systemrole.SystemRolePlatformAdmin},
		"biz_user":   {systemrole.SystemRoleBusinessAdmin},
	}}, systemrole.CapabilityProvider{}, "", nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestListResourcePermissions(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	perms, err := svc.ListResourcePermissions()
	if err != nil {
		t.Fatalf("ListResourcePermissions: %v", err)
	}
	if len(perms) != 4 {
		t.Fatalf("expected 4 resource permissions, got %d", len(perms))
	}

	byCode := make(map[string]models.ResourcePermission, len(perms))
	for _, p := range perms {
		byCode[p.ResourceCode] = p
	}
	if got := byCode["repositories"]; len(got.AllowedRoles) != 0 {
		t.Errorf("repositories allowed_roles = %v, want empty (open to all)", got.AllowedRoles)
	}
	if got := byCode["capabilities"]; len(got.AllowedRoles) != 1 || got.AllowedRoles[0] != systemrole.SystemRolePlatformAdmin {
		t.Errorf("capabilities allowed_roles = %v, want [platform_admin]", got.AllowedRoles)
	}
}

func TestUpdateResourcePermissionReloadsRegistry(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	// Before: "capabilities" is platform_admin only, so a business admin is denied.
	allowed, err := svc.HasPermission("biz_user", "capabilities")
	if err != nil {
		t.Fatalf("HasPermission before update: %v", err)
	}
	if allowed {
		t.Fatalf("biz_user should NOT have capabilities before update")
	}

	// Update to allow business_admin too.
	if err := svc.UpdateResourcePermission("capabilities", []string{
		systemrole.SystemRoleBusinessAdmin, systemrole.SystemRolePlatformAdmin,
	}); err != nil {
		t.Fatalf("UpdateResourcePermission: %v", err)
	}

	// After: registry must be reloaded in-memory (no restart), so biz_user is now allowed.
	allowed, err = svc.HasPermission("biz_user", "capabilities")
	if err != nil {
		t.Fatalf("HasPermission after update: %v", err)
	}
	if !allowed {
		t.Fatalf("biz_user should have capabilities after reload-on-write")
	}

	// And it persisted to the DB.
	perms, err := svc.ListResourcePermissions()
	if err != nil {
		t.Fatalf("ListResourcePermissions: %v", err)
	}
	for _, p := range perms {
		if p.ResourceCode == "capabilities" {
			if len(p.AllowedRoles) != 2 {
				t.Fatalf("persisted allowed_roles = %v, want 2 roles", p.AllowedRoles)
			}
		}
	}
}

func TestUpdateResourcePermissionEmptyOpensToAll(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	if err := svc.UpdateResourcePermission("capabilities", []string{}); err != nil {
		t.Fatalf("UpdateResourcePermission: %v", err)
	}

	// Empty allowed_roles = any authenticated user. biz_user (and any user) now allowed.
	allowed, err := svc.HasPermission("biz_user", "capabilities")
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if !allowed {
		t.Fatalf("empty allowed_roles should open the resource to all authenticated users")
	}
}

func TestUpdateResourcePermissionRejectsInvalidRole(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	err := svc.UpdateResourcePermission("capabilities", []string{"not_a_real_role"})
	if !errors.Is(err, ErrInvalidResourceRole) {
		t.Fatalf("expected ErrInvalidResourceRole, got %v", err)
	}

	// The bad write must not have changed anything.
	allowed, err := svc.HasPermission("biz_user", "capabilities")
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if allowed {
		t.Fatalf("capabilities should still be platform_admin only after a rejected update")
	}
}

func TestUpdateResourcePermissionNotFound(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	err := svc.UpdateResourcePermission("does-not-exist", []string{systemrole.SystemRolePlatformAdmin})
	if !errors.Is(err, ErrResourcePermissionNotFound) {
		t.Fatalf("expected ErrResourcePermissionNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Handler-layer tests (bypass RequirePlatformAdmin; the route group already
// applies it in production).
// ---------------------------------------------------------------------------

func newTestContext(t *testing.T, method, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, rec
}

func TestListResourcePermissionsHandler(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	c, rec := newTestContext(t, http.MethodGet, "")
	ListResourcePermissionsHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resource_code") && !strings.Contains(rec.Body.String(), "resourceCode") {
		t.Fatalf("response missing resource codes; body=%s", rec.Body.String())
	}
}

func TestUpdateResourcePermissionHandler(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	c, rec := newTestContext(t, http.MethodPut, `{"allowedRoles":["business_admin","platform_admin"]}`)
	c.Params = gin.Params{{Key: "code", Value: "capabilities"}}
	UpdateResourcePermissionHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	allowed, err := svc.HasPermission("biz_user", "capabilities")
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if !allowed {
		t.Fatalf("handler update should have reloaded the registry")
	}
}

func TestUpdateResourcePermissionHandlerInvalidRole(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	c, rec := newTestContext(t, http.MethodPut, `{"allowedRoles":["bogus"]}`)
	c.Params = gin.Params{{Key: "code", Value: "capabilities"}}
	UpdateResourcePermissionHandler(svc)(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateResourcePermissionHandlerNotFound(t *testing.T) {
	db := setupAuthzTestDB(t)
	svc := newTestService(t, db)

	c, rec := newTestContext(t, http.MethodPut, `{"allowedRoles":["platform_admin"]}`)
	c.Params = gin.Params{{Key: "code", Value: "nope"}}
	UpdateResourcePermissionHandler(svc)(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
