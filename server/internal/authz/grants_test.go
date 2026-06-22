package authz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// fakeDeptProvider is an in-memory DepartmentProvider for grant tests. It lets us
// exercise the mentor acceptance cases (department inheritance via dept_path) and
// the dept-sync degradation path without a live dept-sync service.
type fakeDeptProvider struct {
	// userDepts maps a dept-sync universal id → that user's departments.
	userDepts map[string][]DepartmentInfo
	// deptPaths maps dept_id → its materialized dept_path (for grant persistence).
	deptPaths map[string]string
	// fail makes every lookup return an error, simulating dept-sync unavailable.
	fail bool
	// calls counts GetUserDepartments invocations (to assert the skip-optimization).
	calls int
}

func (f *fakeDeptProvider) GetUserDepartments(universalID string) ([]DepartmentInfo, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("dept-sync unavailable")
	}
	return f.userDepts[universalID], nil
}

func (f *fakeDeptProvider) GetDepartmentPath(deptID string) (string, error) {
	if f.fail {
		return "", errors.New("dept-sync unavailable")
	}
	p, ok := f.deptPaths[deptID]
	if !ok {
		return "", errors.New("department not found")
	}
	return p, nil
}

// setupGrantTestDB builds an in-memory sqlite DB with the users, user_system_roles
// and permission_grants tables. We hand-create permission_grants (its model uses
// postgres-only uuid defaults) but rely on AutoMigrate for users.
func setupGrantTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	// resource_permissions is needed by NewService/loadRegistry.
	if err := db.Exec(`CREATE TABLE resource_permissions (
		id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
		resource_code TEXT NOT NULL UNIQUE,
		resource_type TEXT NOT NULL,
		allowed_roles TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME,
		updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create resource_permissions: %v", err)
	}
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
		t.Fatalf("create permission_grants: %v", err)
	}
	if err := db.Exec(`CREATE TABLE user_system_roles (
		id TEXT PRIMARY KEY, user_id TEXT, role TEXT, created_at DATETIME, deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create user_system_roles: %v", err)
	}
	return db
}

func newGrantTestService(t *testing.T, db *gorm.DB, dp DepartmentProvider) *Service {
	t.Helper()
	svc, err := NewService(db, roleProviderStub{roles: map[string][]string{}}, systemrole.CapabilityProvider{}, "", nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if dp != nil {
		svc.SetDepartmentProvider(dp)
	}
	return svc
}

func seedUser(t *testing.T, db *gorm.DB, subjectID, universalID string) {
	t.Helper()
	uid := universalID
	if err := db.Create(&models.User{
		SubjectID:          subjectID,
		Username:           subjectID,
		Status:             "active",
		IsActive:           true,
		CasdoorUniversalID: &uid,
	}).Error; err != nil {
		t.Fatalf("seed user %s: %v", subjectID, err)
	}
}

// TestCheckGrant_MentorAcceptance is the canonical mentor scenario:
//
//	kanban/admin  → user 邓彬 (subject_id usr_dengbin)
//	kanban/reader → department Costrict研发部 (dept_path /研发体系/Costrict研发部)
//
// 海俊 (usr_haijun, universal uid_haijun) is in 开发组
// (dept_path /研发体系/Costrict研发部/开发组).
//   - kanban/admin: 海俊 ≠ 邓彬 and no dept grant → false
//   - kanban/reader: 海俊's dept path is prefixed by Costrict研发部 path → true
func TestCheckGrant_MentorAcceptance(t *testing.T) {
	db := setupGrantTestDB(t)
	const researchDeptPath = "/研发体系/Costrict研发部"
	const devGroupPath = "/研发体系/Costrict研发部/开发组"

	seedUser(t, db, "usr_haijun", "uid_haijun")
	seedUser(t, db, "usr_dengbin", "uid_dengbin")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
		deptPaths: map[string]string{"6560": researchDeptPath},
	}
	svc := newGrantTestService(t, db, dp)

	// kanban/admin → user 邓彬.
	if _, err := svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_dengbin", "", "op"); err != nil {
		t.Fatalf("grant kanban/admin to 邓彬: %v", err)
	}
	// kanban/reader → department Costrict研发部 (path resolved from dept-sync).
	path, err := svc.ResolveDepartmentPath("6560")
	if err != nil {
		t.Fatalf("resolve dept path: %v", err)
	}
	if path != researchDeptPath {
		t.Fatalf("resolved dept path = %q, want %q", path, researchDeptPath)
	}
	if _, err := svc.GrantPermission("kanban/reader", models.PermissionSubjectDepartment, "6560", path, "op"); err != nil {
		t.Fatalf("grant kanban/reader to Costrict研发部: %v", err)
	}

	// 海俊 查 kanban/admin → false (he is not 邓彬, no dept grant on kanban/admin).
	ok, err := svc.CheckGrant("usr_haijun", "kanban/admin")
	if err != nil {
		t.Fatalf("CheckGrant kanban/admin: %v", err)
	}
	if ok {
		t.Fatalf("海俊 should NOT have kanban/admin")
	}

	// 海俊 查 kanban/reader → true (his dept path is a descendant of Costrict研发部).
	ok, err = svc.CheckGrant("usr_haijun", "kanban/reader")
	if err != nil {
		t.Fatalf("CheckGrant kanban/reader: %v", err)
	}
	if !ok {
		t.Fatalf("海俊 SHOULD have kanban/reader via department inheritance")
	}

	// 邓彬 查 kanban/admin → true (direct user grant).
	ok, err = svc.CheckGrant("usr_dengbin", "kanban/admin")
	if err != nil {
		t.Fatalf("CheckGrant kanban/admin for 邓彬: %v", err)
	}
	if !ok {
		t.Fatalf("邓彬 SHOULD have kanban/admin via direct grant")
	}
}

// TestCheckGrant_DirectExactDepartment verifies a user directly in the granted
// department (path == grant path, not a descendant) is matched.
func TestCheckGrant_DirectExactDepartment(t *testing.T) {
	db := setupGrantTestDB(t)
	const deptPath = "/研发体系/Costrict研发部"
	seedUser(t, db, "usr_weitidong", "uid_wei")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_wei": {{DeptID: "6560", DeptPath: deptPath}},
		},
		deptPaths: map[string]string{"6560": deptPath},
	}
	svc := newGrantTestService(t, db, dp)

	if _, err := svc.GrantPermission("kanban/reader", models.PermissionSubjectDepartment, "6560", deptPath, "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	ok, err := svc.CheckGrant("usr_weitidong", "kanban/reader")
	if err != nil {
		t.Fatalf("CheckGrant: %v", err)
	}
	if !ok {
		t.Fatalf("a user directly in the granted department should match (exact path)")
	}
}

// TestCheckGrant_FakePrefixRejected guards against the "fake prefix" bug: a grant
// on /A/B must NOT match a user in /A/Bc (a sibling whose name starts with "B").
func TestCheckGrant_FakePrefixRejected(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_x", "uid_x")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			// User is in "/研发体系/Costrict研发部测试" — name starts with the granted
			// department's name but is a different department (sibling), not a child.
			"uid_x": {{DeptID: "9999", DeptPath: "/研发体系/Costrict研发部测试"}},
		},
	}
	svc := newGrantTestService(t, db, dp)

	if _, err := svc.GrantPermission("kanban/reader", models.PermissionSubjectDepartment, "6560", "/研发体系/Costrict研发部", "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	ok, err := svc.CheckGrant("usr_x", "kanban/reader")
	if err != nil {
		t.Fatalf("CheckGrant: %v", err)
	}
	if ok {
		t.Fatalf("a fake prefix (/研发体系/Costrict研发部测试) must NOT match a grant on /研发体系/Costrict研发部")
	}
}

// TestCheckGrant_SkipsDeptSyncWhenNoDeptGrant asserts the cost-free fast path:
// when a permission has no department grants, dept-sync is never called.
func TestCheckGrant_SkipsDeptSyncWhenNoDeptGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: "/研发体系/Costrict研发部/开发组"}},
		},
	}
	svc := newGrantTestService(t, db, dp)

	// Only a user grant exists for kanban/admin — no department grant at all.
	if _, err := svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_other", "", "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	ok, err := svc.CheckGrant("usr_haijun", "kanban/admin")
	if err != nil {
		t.Fatalf("CheckGrant: %v", err)
	}
	if ok {
		t.Fatalf("海俊 should not have kanban/admin (only usr_other granted)")
	}
	if dp.calls != 0 {
		t.Fatalf("dept-sync should be skipped when no department grant exists; calls=%d", dp.calls)
	}
}

// TestCheckGrant_DeptSyncDegradeFailsClosed asserts that when dept-sync is
// unavailable, the department path fails closed (false), while the direct user
// grant still works.
func TestCheckGrant_DeptSyncDegradeFailsClosed(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")
	seedUser(t, db, "usr_dengbin", "uid_dengbin")

	dp := &fakeDeptProvider{fail: true}
	svc := newGrantTestService(t, db, dp)

	// Department grant on kanban/reader (dept_path was persisted earlier when
	// dept-sync was healthy; here it's unreachable at check time).
	if _, err := svc.GrantPermission("kanban/reader", models.PermissionSubjectDepartment, "6560", "/研发体系/Costrict研发部", "op"); err != nil {
		t.Fatalf("grant dept: %v", err)
	}
	// Direct user grant on kanban/admin.
	if _, err := svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_dengbin", "", "op"); err != nil {
		t.Fatalf("grant user: %v", err)
	}

	// Department path can't be resolved → fail closed (false), no error.
	ok, err := svc.CheckGrant("usr_haijun", "kanban/reader")
	if err != nil {
		t.Fatalf("CheckGrant should degrade gracefully, got err: %v", err)
	}
	if ok {
		t.Fatalf("department grant must fail closed when dept-sync is unavailable")
	}

	// Direct user grant is unaffected by dept-sync being down.
	ok, err = svc.CheckGrant("usr_dengbin", "kanban/admin")
	if err != nil {
		t.Fatalf("CheckGrant direct: %v", err)
	}
	if !ok {
		t.Fatalf("direct user grant should still work while dept-sync is down")
	}
}

// TestCheckGrant_NoDeptProviderFailsClosed verifies department grants never match
// when dept-sync is not wired at all, while user grants keep working.
func TestCheckGrant_NoDeptProviderFailsClosed(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")

	svc := newGrantTestService(t, db, nil) // no department provider

	if _, err := svc.GrantPermission("kanban/reader", models.PermissionSubjectDepartment, "6560", "/研发体系/Costrict研发部", "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	ok, err := svc.CheckGrant("usr_haijun", "kanban/reader")
	if err != nil {
		t.Fatalf("CheckGrant: %v", err)
	}
	if ok {
		t.Fatalf("department grant must not match when no department provider is configured")
	}
}

// TestGrantPermission_Idempotent verifies granting the same (code,type,id) twice
// is a no-op returning the existing grant.
func TestGrantPermission_Idempotent(t *testing.T) {
	db := setupGrantTestDB(t)
	svc := newGrantTestService(t, db, nil)

	g1, err := svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_a", "", "op")
	if err != nil {
		t.Fatalf("first grant: %v", err)
	}
	g2, err := svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_a", "", "op")
	if err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatalf("idempotent grant should return the same row; %s != %s", g1.ID, g2.ID)
	}

	grants, err := svc.ListGrants("kanban/admin")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant after idempotent re-grant, got %d", len(grants))
	}
}

// TestGrantPermission_InvalidSubjectType rejects unknown subject types.
func TestGrantPermission_InvalidSubjectType(t *testing.T) {
	db := setupGrantTestDB(t)
	svc := newGrantTestService(t, db, nil)

	_, err := svc.GrantPermission("kanban/admin", "group", "x", "", "op")
	if !errors.Is(err, ErrInvalidSubjectType) {
		t.Fatalf("expected ErrInvalidSubjectType, got %v", err)
	}
}

// TestRevokePermission removes a grant and reports not-found for unknown ids.
func TestRevokePermission(t *testing.T) {
	db := setupGrantTestDB(t)
	svc := newGrantTestService(t, db, nil)

	g, err := svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_a", "", "op")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := svc.RevokePermission(g.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	ok, err := svc.CheckGrant("usr_a", "kanban/admin")
	if err != nil {
		t.Fatalf("CheckGrant after revoke: %v", err)
	}
	if ok {
		t.Fatalf("grant should be gone after revoke")
	}
	if err := svc.RevokePermission("nonexistent"); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("expected ErrGrantNotFound, got %v", err)
	}
	// A malformed (non-uuid) id must be rejected as not-found, never reach the
	// driver as an invalid-uuid-syntax error (which on postgres would be a 500).
	for _, badID := range []string{"not-a-uuid", "123", "../../etc", ""} {
		if err := svc.RevokePermission(badID); !errors.Is(err, ErrGrantNotFound) {
			t.Fatalf("RevokePermission(%q): expected ErrGrantNotFound, got %v", badID, err)
		}
	}
}

// newGrantHandlerContext builds a gin context for GrantPermissionHandler tests
// with an authenticated operator and a JSON body.
func newGrantHandlerContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set(appmiddleware.UserIDKey, "usr_admin")
	return c, rec
}

// TestGrantPermissionHandler_UserTargetDept covers the #2 metrics-view preset path
// through the HTTP handler: a user-subject kanban.scope.dept grant with a
// targetDeptId resolves the TARGET department's path and stores it as the grant
// dept_path — so ResolveUserScope later opens that target subtree for the user.
func TestGrantPermissionHandler_UserTargetDept(t *testing.T) {
	db := setupGrantTestDB(t)
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	const targetSubtree = "/研发体系/AI效能部"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
		deptPaths: map[string]string{"5889": targetSubtree},
	}
	svc := newGrantTestService(t, db, dp)

	c, rec := newGrantHandlerContext(t,
		`{"permissionCode":"kanban.scope.dept","subjectType":"user","subjectId":"usr_haijun","targetDeptId":"5889"}`)
	GrantPermissionHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	grants, err := svc.ListGrants(ScopeDeptPermission)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if grants[0].SubjectType != models.PermissionSubjectUser || grants[0].SubjectID != "usr_haijun" {
		t.Fatalf("grant subject wrong: %+v", grants[0])
	}
	if grants[0].DeptPath != targetSubtree {
		t.Fatalf("grant dept_path = %q, want target %q", grants[0].DeptPath, targetSubtree)
	}

	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if !containsAll(scope.VisibleDeptPrefixes, []string{devGroupPath, targetSubtree}) {
		t.Fatalf("VisibleDeptPrefixes = %v, want to contain own dept + target subtree", scope.VisibleDeptPrefixes)
	}
}

// TestGrantPermissionHandler_ScopeAllUser covers the "see all company" preset: a
// user-subject kanban.scope.all grant (no target) confers AllAccess.
func TestGrantPermissionHandler_ScopeAllUser(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_ops", "uid_ops")
	svc := newGrantTestService(t, db, nil)

	c, rec := newGrantHandlerContext(t,
		`{"permissionCode":"kanban.scope.all","subjectType":"user","subjectId":"usr_ops"}`)
	GrantPermissionHandler(svc)(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	scope, err := svc.ResolveUserScope("usr_ops")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if !scope.AllAccess {
		t.Fatalf("usr_ops should have AllAccess via kanban.scope.all grant")
	}
}

// TestGrantPermissionHandler_TargetDeptUnavailable: when dept-sync can't resolve
// the target department, the grant is rejected with a degraded-dependency signal
// rather than silently persisting an empty (meaningless) target path.
func TestGrantPermissionHandler_TargetDeptUnavailable(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")
	svc := newGrantTestService(t, db, &fakeDeptProvider{fail: true})

	c, rec := newGrantHandlerContext(t,
		`{"permissionCode":"kanban.scope.dept","subjectType":"user","subjectId":"usr_haijun","targetDeptId":"5889"}`)
	GrantPermissionHandler(svc)(c)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dept_sync_unavailable") {
		t.Fatalf("body should carry dept_sync_unavailable code; got %s", rec.Body.String())
	}
	grants, _ := svc.ListGrants("")
	if len(grants) != 0 {
		t.Fatalf("no grant should persist on degraded dept-sync; got %d", len(grants))
	}
}

// TestListGrants_FilterByCode verifies the optional permissionCode filter.
func TestListGrants_FilterByCode(t *testing.T) {
	db := setupGrantTestDB(t)
	svc := newGrantTestService(t, db, nil)

	_, _ = svc.GrantPermission("kanban/admin", models.PermissionSubjectUser, "usr_a", "", "op")
	_, _ = svc.GrantPermission("kanban/reader", models.PermissionSubjectUser, "usr_b", "", "op")

	all, err := svc.ListGrants("")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 grants total, got %d", len(all))
	}

	filtered, err := svc.ListGrants("kanban/admin")
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].PermissionCode != "kanban/admin" {
		t.Fatalf("expected 1 kanban/admin grant, got %+v", filtered)
	}
}
