package authz

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"gorm.io/gorm"
)

// newScopeTestService builds a Service whose RoleProvider returns the given
// per-user roles, so we can exercise the admin AllAccess path. It reuses the
// grant test DB schema (users / user_system_roles / permission_grants).
func newScopeTestService(t *testing.T, db *gorm.DB, roles map[string][]string, dp DepartmentProvider) *Service {
	t.Helper()
	svc, err := NewService(db, roleProviderStub{roles: roles}, systemrole.CapabilityProvider{}, "", nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if dp != nil {
		svc.SetDepartmentProvider(dp)
	}
	return svc
}

func containsAll(got, want []string) bool {
	set := make(map[string]struct{}, len(got))
	for _, g := range got {
		set[g] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

// TestResolveUserScope_AdminAllAccess: a platform/business admin sees everything
// (AllAccess), independent of dept-sync, and prefixes are irrelevant.
func TestResolveUserScope_AdminAllAccess(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_admin", "uid_admin")
	seedUser(t, db, "usr_biz", "uid_biz")

	// dept-sync deliberately failing: AllAccess must not depend on it.
	dp := &fakeDeptProvider{fail: true}
	svc := newScopeTestService(t, db, map[string][]string{
		"usr_admin": {systemrole.SystemRolePlatformAdmin},
		"usr_biz":   {systemrole.SystemRoleBusinessAdmin},
	}, dp)

	for _, uid := range []string{"usr_admin", "usr_biz"} {
		scope, err := svc.ResolveUserScope(uid)
		if err != nil {
			t.Fatalf("ResolveUserScope(%s): %v", uid, err)
		}
		if !scope.AllAccess {
			t.Fatalf("%s should have AllAccess (admin role)", uid)
		}
	}
	// Admin AllAccess must not have called dept-sync at all.
	if dp.calls != 0 {
		t.Fatalf("admin AllAccess should skip dept-sync; calls=%d", dp.calls)
	}
}

// TestResolveUserScope_ScopeAllGrant: a non-admin user with a direct
// ScopeAllPermission grant gets AllAccess.
func TestResolveUserScope_ScopeAllGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_ops", "uid_ops")
	svc := newScopeTestService(t, db, nil, nil)

	if _, err := svc.GrantPermission(ScopeAllPermission, models.PermissionSubjectUser, "usr_ops", "", "op"); err != nil {
		t.Fatalf("grant scope.all: %v", err)
	}
	scope, err := svc.ResolveUserScope("usr_ops")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if !scope.AllAccess {
		t.Fatalf("usr_ops should have AllAccess via direct %s grant", ScopeAllPermission)
	}
}

// TestResolveUserScope_DefaultOwnDepartments: a non-admin user's default
// visibility is exactly their own departments (self + descendants via prefix).
func TestResolveUserScope_DefaultOwnDepartments(t *testing.T) {
	db := setupGrantTestDB(t)
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
	}
	svc := newScopeTestService(t, db, nil, dp)

	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if scope.AllAccess {
		t.Fatalf("non-admin without scope grant must NOT have AllAccess")
	}
	if scope.UniversalID != "uid_haijun" {
		t.Fatalf("universalId = %q, want uid_haijun", scope.UniversalID)
	}
	if !containsAll(scope.DeptPaths, []string{devGroupPath}) || len(scope.DeptPaths) != 1 {
		t.Fatalf("DeptPaths = %v, want [%s]", scope.DeptPaths, devGroupPath)
	}
	if !containsAll(scope.VisibleDeptPrefixes, []string{devGroupPath}) || len(scope.VisibleDeptPrefixes) != 1 {
		t.Fatalf("VisibleDeptPrefixes = %v, want [%s]", scope.VisibleDeptPrefixes, devGroupPath)
	}
}

// TestResolveUserScope_ExtraDeptGrantToDepartment: a ScopeDeptPermission grant on
// a department the user belongs to (or an ancestor of it) opens an extra subtree.
func TestResolveUserScope_ExtraDeptGrantToDepartment(t *testing.T) {
	db := setupGrantTestDB(t)
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	const otherSubtree = "/研发体系/AI效能部"
	const researchDeptPath = "/研发体系/Costrict研发部"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
	}
	svc := newScopeTestService(t, db, nil, dp)

	// Grant kanban.scope.dept to AI效能部, but BOUND to a department the user is in
	// (Costrict研发部) — i.e. members of Costrict研发部 (and descendants) may also see
	// AI效能部. The grant's subject is the department whose members get the extra
	// view; its dept_path is the extra subtree they may see.
	//
	// Model: subject_type='department', subject_id/dept_path = the *target* extra
	// subtree (AI效能部). Per the design, a department grant's dept_path is what
	// gets added when the user belongs to that department subtree. So to give 海俊
	// (in 开发组, under Costrict研发部) an extra view we grant on a department whose
	// subtree contains 海俊's department — here we grant on Costrict研发部, and 海俊's
	// 开发组 path is a descendant → grant applies → its dept_path is added.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectDepartment, "6560", researchDeptPath, "op"); err != nil {
		t.Fatalf("grant dept scope: %v", err)
	}
	// Also a direct user grant opening AI效能部 for 海俊 specifically.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_haijun", otherSubtree, "op"); err != nil {
		t.Fatalf("grant user scope: %v", err)
	}

	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if scope.AllAccess {
		t.Fatalf("must not be AllAccess")
	}
	// Visible = own dept (开发组) + dept-grant subtree (Costrict研发部) + user-grant
	// subtree (AI效能部).
	want := []string{devGroupPath, researchDeptPath, otherSubtree}
	if !containsAll(scope.VisibleDeptPrefixes, want) {
		t.Fatalf("VisibleDeptPrefixes = %v, want to contain %v", scope.VisibleDeptPrefixes, want)
	}
}

// TestResolveUserScope_ScopeDeptUserTarget is the metrics-view preset for #2: a
// platform admin grants a specific USER (海俊) the right to see a TARGET department
// (AI效能部) and its subtree, beyond his own department. The grant is on the user
// (subject_type=user), and its dept_path holds the TARGET department path — the
// exact value ResolveUserScope adds as an extra-visible prefix for that user. This
// pins the grant-creation ↔ scope-consumption contract: dept_path written ==
// prefix read.
func TestResolveUserScope_ScopeDeptUserTarget(t *testing.T) {
	db := setupGrantTestDB(t)
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	const targetSubtree = "/研发体系/AI效能部"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
		// The grant-creation handler resolves the TARGET department's path from here.
		deptPaths: map[string]string{"5889": targetSubtree},
	}
	svc := newScopeTestService(t, db, nil, dp)

	// Mirror what GrantPermissionHandler does for a user subject + targetDeptId:
	// resolve the target department's path, then store it as the grant dept_path.
	targetPath, err := svc.ResolveDepartmentPath("5889")
	if err != nil {
		t.Fatalf("resolve target dept path: %v", err)
	}
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_haijun", targetPath, "op"); err != nil {
		t.Fatalf("grant user scope.dept with target: %v", err)
	}

	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if scope.AllAccess {
		t.Fatalf("must not be AllAccess")
	}
	want := []string{devGroupPath, targetSubtree}
	if !containsAll(scope.VisibleDeptPrefixes, want) || len(scope.VisibleDeptPrefixes) != 2 {
		t.Fatalf("VisibleDeptPrefixes = %v, want exactly %v", scope.VisibleDeptPrefixes, want)
	}
}

// TestGrantPermission_RetargetsDeptPath: re-granting the same user's
// kanban.scope.dept with a different target department updates the stored
// dept_path in place (the unique key forbids a second row), so ResolveUserScope
// then reflects the NEW target only.
func TestGrantPermission_RetargetsDeptPath(t *testing.T) {
	db := setupGrantTestDB(t)
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	const firstTarget = "/研发体系/AI效能部"
	const secondTarget = "/研发体系/平台部"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
	}
	svc := newScopeTestService(t, db, nil, dp)

	g1, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_haijun", firstTarget, "op")
	if err != nil {
		t.Fatalf("first grant: %v", err)
	}
	g2, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_haijun", secondTarget, "op")
	if err != nil {
		t.Fatalf("re-target grant: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatalf("re-target should reuse the same grant row; %s != %s", g1.ID, g2.ID)
	}
	if g2.DeptPath != secondTarget {
		t.Fatalf("re-target should update dept_path; got %q want %q", g2.DeptPath, secondTarget)
	}

	grants, err := svc.ListGrants(ScopeDeptPermission)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("re-target must not create a second row; got %d", len(grants))
	}

	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	want := []string{devGroupPath, secondTarget}
	if !containsAll(scope.VisibleDeptPrefixes, want) || len(scope.VisibleDeptPrefixes) != 2 {
		t.Fatalf("VisibleDeptPrefixes = %v, want exactly %v (old target must be gone)", scope.VisibleDeptPrefixes, want)
	}
}

// TestResolveUserScope_DeptGrantNotApplicable: a ScopeDeptPermission department
// grant on a department the user is NOT under must not open that subtree.
func TestResolveUserScope_DeptGrantNotApplicable(t *testing.T) {
	db := setupGrantTestDB(t)
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
	}
	svc := newScopeTestService(t, db, nil, dp)

	// Grant on a sibling subtree the user is NOT in.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectDepartment, "5889", "/研发体系/AI效能部", "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	// Only the user's own department; the inapplicable grant must NOT appear.
	if len(scope.VisibleDeptPrefixes) != 1 || scope.VisibleDeptPrefixes[0] != devGroupPath {
		t.Fatalf("VisibleDeptPrefixes = %v, want only [%s] (inapplicable dept grant must not open subtree)", scope.VisibleDeptPrefixes, devGroupPath)
	}
}

// TestResolveUserScope_DeptSyncDegradeFailsSafe: when dept-sync is down, a
// non-admin user's visible prefixes are empty (self only) — never mis-opened.
func TestResolveUserScope_DeptSyncDegradeFailsSafe(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{fail: true}
	svc := newScopeTestService(t, db, nil, dp)

	// Even a department grant can't take effect: the user's own dept paths are
	// unknown (dept-sync down), so there is nothing to prefix-match against.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectDepartment, "6560", "/研发体系/Costrict研发部", "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope should degrade gracefully: %v", err)
	}
	if scope.AllAccess {
		t.Fatalf("dept-sync down must not grant AllAccess")
	}
	if len(scope.DeptPaths) != 0 || len(scope.VisibleDeptPrefixes) != 0 {
		t.Fatalf("dept-sync down → non-admin must see nothing extra; DeptPaths=%v Visible=%v", scope.DeptPaths, scope.VisibleDeptPrefixes)
	}
}

// TestResolveUserScope_NoUniversalID: a user without a casdoor_universal_id (no
// dept-sync mapping) gets empty prefixes (non-admin = self only).
func TestResolveUserScope_NoUniversalID(t *testing.T) {
	db := setupGrantTestDB(t)
	// User row with a NULL universal id.
	if err := db.Create(&models.User{SubjectID: "usr_nouid", Username: "usr_nouid", Status: "active", IsActive: true}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: "/研发体系/Costrict研发部/开发组"}},
		},
	}
	svc := newScopeTestService(t, db, nil, dp)

	scope, err := svc.ResolveUserScope("usr_nouid")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if scope.UniversalID != "" {
		t.Fatalf("UniversalID should be empty for a user with no casdoor_universal_id")
	}
	if len(scope.VisibleDeptPrefixes) != 0 {
		t.Fatalf("no universal id → no visibility; got %v", scope.VisibleDeptPrefixes)
	}
	// dept-sync must not be queried without a universal id.
	if dp.calls != 0 {
		t.Fatalf("dept-sync should not be called without a universal id; calls=%d", dp.calls)
	}
}

// TestResolveUserScope_NoDeptProvider: with no dept-sync wired, a non-admin user
// has empty visibility, and any role/grant decision is unaffected.
func TestResolveUserScope_NoDeptProvider(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")
	svc := newScopeTestService(t, db, nil, nil) // no department provider

	scope, err := svc.ResolveUserScope("usr_haijun")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if scope.AllAccess {
		t.Fatalf("must not be AllAccess")
	}
	if len(scope.VisibleDeptPrefixes) != 0 {
		t.Fatalf("no dept provider → empty visibility; got %v", scope.VisibleDeptPrefixes)
	}
}

// TestResolveUserScope_EmptyUser returns a safe empty scope for an empty userID.
func TestResolveUserScope_EmptyUser(t *testing.T) {
	db := setupGrantTestDB(t)
	svc := newScopeTestService(t, db, nil, nil)

	scope, err := svc.ResolveUserScope("")
	if err != nil {
		t.Fatalf("ResolveUserScope: %v", err)
	}
	if scope.AllAccess || len(scope.VisibleDeptPrefixes) != 0 {
		t.Fatalf("empty user → empty, non-all scope; got %+v", scope)
	}
}
