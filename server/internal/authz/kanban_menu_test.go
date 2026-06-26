package authz

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
)

// menusContainKanban reports whether a permission snapshot surfaces the kanban
// sidebar entry.
func menusContainKanban(t *testing.T, svc *Service, userID string) bool {
	t.Helper()
	perms, err := svc.GetUserPermissions(userID)
	if err != nil {
		t.Fatalf("GetUserPermissions(%s): %v", userID, err)
	}
	return containsAll(perms.Menus, []string{KanbanMenuCode})
}

// TestKanbanMenu_HiddenWithoutGrant is the core guard: a non-admin user with only
// the default self-department scope (no explicit kanban grant) must NOT see the
// kanban entry. Otherwise every employee would get it, defeating the
// admin-authorized-only requirement.
func TestKanbanMenu_HiddenWithoutGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_plain", "uid_plain")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_plain": {{DeptID: "6571", DeptPath: "/研发体系/Costrict研发部/开发组"}},
		},
	}
	svc := newScopeTestService(t, db, map[string][]string{}, dp)

	if menusContainKanban(t, svc, "usr_plain") {
		t.Fatalf("a non-admin with no kanban grant must NOT see the kanban entry (default self-scope must not surface it)")
	}
}

// TestKanbanMenu_VisibleWithScopeAllGrant: a non-admin holding a direct
// kanban.scope.all grant sees the entry.
func TestKanbanMenu_VisibleWithScopeAllGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_ops", "uid_ops")
	svc := newScopeTestService(t, db, map[string][]string{}, nil)

	if _, err := svc.GrantPermission(ScopeAllPermission, models.PermissionSubjectUser, "usr_ops", "", "op"); err != nil {
		t.Fatalf("grant scope.all: %v", err)
	}
	if !menusContainKanban(t, svc, "usr_ops") {
		t.Fatalf("a non-admin with kanban.scope.all grant SHOULD see the kanban entry")
	}
}

// TestKanbanMenu_VisibleWithDirectScopeDeptGrant: a non-admin with a direct
// user-subject kanban.scope.dept grant (target subtree stored in dept_path) sees
// the entry, even with dept-sync absent (direct user grants need no dept lookup).
func TestKanbanMenu_VisibleWithDirectScopeDeptGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_lead", "uid_lead")
	svc := newScopeTestService(t, db, map[string][]string{}, nil)

	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_lead", "/研发体系/AI效能部", "op"); err != nil {
		t.Fatalf("grant scope.dept: %v", err)
	}
	if !menusContainKanban(t, svc, "usr_lead") {
		t.Fatalf("a non-admin with a direct kanban.scope.dept grant SHOULD see the kanban entry")
	}
}

// TestKanbanMenu_VisibleWithDeptScopeDeptGrant: a non-admin inherits the entry
// from a department-subject kanban.scope.dept grant whose subtree contains their
// own department.
func TestKanbanMenu_VisibleWithDeptScopeDeptGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	const researchDeptPath = "/研发体系/Costrict研发部"
	const devGroupPath = "/研发体系/Costrict研发部/开发组"
	seedUser(t, db, "usr_haijun", "uid_haijun")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_haijun": {{DeptID: "6571", DeptPath: devGroupPath}},
		},
	}
	svc := newScopeTestService(t, db, map[string][]string{}, dp)

	// Department-subject grant on the parent department subtree.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectDepartment, "6560", researchDeptPath, "op"); err != nil {
		t.Fatalf("grant dept scope.dept: %v", err)
	}
	if !menusContainKanban(t, svc, "usr_haijun") {
		t.Fatalf("a user in a subtree covered by a department kanban.scope.dept grant SHOULD see the kanban entry")
	}
}

// TestKanbanMenu_VisibleForAdminRole is the role-path regression: an admin sees
// the entry via the resource_permissions menu registry (allowed_roles), and the
// grant probe is skipped because the role loop already added it.
func TestKanbanMenu_VisibleForAdminRole(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_admin", "uid_admin")

	// Seed the kanban menu row, then reload the registry so the role path can fire.
	if err := db.Exec(`INSERT INTO resource_permissions (resource_code, resource_type, allowed_roles)
		VALUES ('kanban', 'menu', '{business_admin,platform_admin}')`).Error; err != nil {
		t.Fatalf("seed kanban menu row: %v", err)
	}
	svc := newScopeTestService(t, db, map[string][]string{
		"usr_admin": {systemrole.SystemRolePlatformAdmin},
	}, nil)
	if err := svc.ReloadRegistry(); err != nil {
		t.Fatalf("ReloadRegistry: %v", err)
	}

	if !menusContainKanban(t, svc, "usr_admin") {
		t.Fatalf("a platform_admin SHOULD see the kanban entry via the role registry path")
	}
}

// TestHasKanbanMenuAccess_SkipsDeptSyncWhenNoDeptGrant asserts the cost-free fast
// path on the hot /api/auth/permissions route: a non-admin with no kanban scope
// grant (only an unrelated direct user grant for someone else) resolves without
// ever calling dept-sync.
func TestHasKanbanMenuAccess_SkipsDeptSyncWhenNoDeptGrant(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_plain", "uid_plain")

	dp := &fakeDeptProvider{
		userDepts: map[string][]DepartmentInfo{
			"uid_plain": {{DeptID: "6571", DeptPath: "/研发体系/Costrict研发部/开发组"}},
		},
	}
	svc := newScopeTestService(t, db, map[string][]string{}, dp)

	// A direct user scope.dept grant exists, but for a DIFFERENT user — and there
	// is no department-subject grant at all.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_other", "/研发体系/AI效能部", "op"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	ok, err := svc.HasKanbanMenuAccess("usr_plain")
	if err != nil {
		t.Fatalf("HasKanbanMenuAccess: %v", err)
	}
	if ok {
		t.Fatalf("usr_plain has no kanban grant and must not see the entry")
	}
	if dp.calls != 0 {
		t.Fatalf("dept-sync must be skipped when no department grant exists; calls=%d", dp.calls)
	}
}

// TestHasKanbanMenuAccess_DeptSyncDegradeFailsClosed: when dept-sync is down, a
// department-inherited grant cannot be resolved (fail closed), while a direct
// user grant is unaffected.
func TestHasKanbanMenuAccess_DeptSyncDegradeFailsClosed(t *testing.T) {
	db := setupGrantTestDB(t)
	seedUser(t, db, "usr_haijun", "uid_haijun")
	seedUser(t, db, "usr_lead", "uid_lead")
	svc := newScopeTestService(t, db, map[string][]string{}, &fakeDeptProvider{fail: true})

	// Department grant that would match usr_haijun if dept-sync were healthy.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectDepartment, "6560", "/研发体系/Costrict研发部", "op"); err != nil {
		t.Fatalf("grant dept: %v", err)
	}
	// Direct user grant for usr_lead.
	if _, err := svc.GrantPermission(ScopeDeptPermission, models.PermissionSubjectUser, "usr_lead", "/研发体系/AI效能部", "op"); err != nil {
		t.Fatalf("grant user: %v", err)
	}

	ok, err := svc.HasKanbanMenuAccess("usr_haijun")
	if err != nil {
		t.Fatalf("HasKanbanMenuAccess should degrade gracefully, got err: %v", err)
	}
	if ok {
		t.Fatalf("department-inherited kanban access must fail closed when dept-sync is down")
	}

	ok, err = svc.HasKanbanMenuAccess("usr_lead")
	if err != nil {
		t.Fatalf("HasKanbanMenuAccess direct: %v", err)
	}
	if !ok {
		t.Fatalf("a direct user kanban.scope.dept grant should still surface the entry while dept-sync is down")
	}
}
