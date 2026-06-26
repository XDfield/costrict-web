package authz

import (
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
)

// Scope permission codes. These are the fine-grained permission_grants codes that
// loosen a user's default "self + descendants" department visibility for the
// metrics dashboard (指标看板). They live here (not in systemrole) so the admin
// grant UI and external consumers (efficiency-dashboard) share one source of truth.
const (
	// ScopeAllPermission, when granted directly to a user, lets that user see
	// every department's metrics (company-wide). Platform/business admins get this
	// implicitly via their role; this constant is for granting it to a non-admin.
	ScopeAllPermission = "kanban.scope.all"

	// ScopeDeptPermission grants visibility into one extra department subtree
	// (beyond the user's own departments). The grant's dept_path defines the
	// subtree (prefix match includes all descendants). It may be granted to a
	// user directly, or to a department (every member of that department, and its
	// descendants, then inherits the extra visibility).
	ScopeDeptPermission = "kanban.scope.dept"

	// KanbanMenuCode is the resource_permissions menu code for the metrics
	// dashboard (指标看板) sidebar entry. Its visibility is the union of two paths:
	//   - role-based (menuRegistry: business_admin / platform_admin), resolved by
	//     GetUserPermissions' generic menu loop; and
	//   - grant-based (HasKanbanMenuAccess): a non-admin user holding an explicit
	//     kanban scope grant.
	KanbanMenuCode = "kanban"
)

// UserScope describes which departments a user may see metrics for in the
// dashboard. It is a read-only authorization fact: computing it never mutates
// state and never weakens the existing HasPermission / CheckGrant paths.
//
// Visibility rules (most-permissive first):
//   - AllAccess == true  → the user sees everything; VisibleDeptPrefixes is
//     ignored. This is true for platform/business admins, or when the user holds
//     a direct ScopeAllPermission grant. It does NOT depend on dept-sync.
//   - AllAccess == false → the user sees only the department subtrees whose
//     materialized dept_path appears in VisibleDeptPrefixes. A prefix matches a
//     department path that equals it or descends from it (with a '/' boundary),
//     so each prefix covers an entire subtree.
//
// Fail-safe: when dept-sync is unconfigured / unreachable / returns no
// departments, a non-admin user's VisibleDeptPrefixes is empty — i.e. they can
// see only their own metrics, never more. We never mis-grant on degradation.
type UserScope struct {
	UserID      string `json:"userId"`      // costrict-web subject_id
	UniversalID string `json:"universalId"` // casdoor_universal_id (= dept-sync universal_id)

	// DeptPaths are the user's own departments' materialized paths (a user may
	// belong to several). Empty when dept-sync is unavailable or the user has no
	// department mapping.
	DeptPaths []string `json:"deptPaths"`

	// VisibleDeptPrefixes is the full set of dept_path prefixes the user may see:
	// their own departments plus any extra subtrees opened by ScopeDeptPermission
	// grants. Each prefix covers itself and all descendants. Ignored when AllAccess.
	VisibleDeptPrefixes []string `json:"visibleDeptPrefixes"`

	// AllAccess true ⇒ the user sees all departments (admin role or a direct
	// ScopeAllPermission grant); VisibleDeptPrefixes is then irrelevant.
	AllAccess bool `json:"allAccess"`
}

// ResolveUserScope computes the dashboard visibility scope for a costrict-web
// user (keyed by subject_id). It is the authorization fact source the metrics
// dashboard reuses to enforce "see only your own department subtree, unless
// specially opened up".
//
// Algorithm:
//  1. Resolve the user's casdoor_universal_id (the dept-sync bridge key).
//  2. AllAccess if the user is a platform/business admin OR holds a direct
//     ScopeAllPermission grant → return immediately (company-wide, no dept-sync).
//  3. Otherwise resolve default visibility: the user's own departments' dept_paths
//     via the DepartmentProvider (dept-sync), keyed by universal id.
//  4. Add any extra subtrees from ScopeDeptPermission grants that apply to this
//     user (direct user grants, or department grants whose subtree contains the
//     user). Each such grant's redundantly-stored dept_path is added as a prefix.
//  5. Degrade fail-closed: any dept-sync failure / missing universal id / no
//     departments leaves prefixes empty (non-admin = self only). AllAccess is
//     unaffected by dept-sync (pure role/grant decision).
func (s *Service) ResolveUserScope(userID string) (*UserScope, error) {
	userID = strings.TrimSpace(userID)
	scope := &UserScope{
		UserID:              userID,
		DeptPaths:           []string{},
		VisibleDeptPrefixes: []string{},
	}
	if userID == "" {
		return scope, nil
	}

	// (1) Map subject_id → casdoor_universal_id (the dept-sync bridge key).
	if universalID, ok := s.universalIDFor(userID); ok {
		scope.UniversalID = universalID
	}

	// (2) AllAccess: admin role OR a direct ScopeAllPermission grant. Pure
	// role/grant decision — independent of dept-sync availability.
	expandedRoles, err := s.roleProvider.GetExpandedRoles(userID)
	if err != nil {
		return nil, err
	}
	allAccess, err := s.hasAllAccess(userID, expandedRoles)
	if err != nil {
		return nil, err
	}
	if allAccess {
		scope.AllAccess = true
		return scope, nil
	}

	// (3) Default visibility: the user's own departments' dept_paths. dept-sync
	// failures fail closed (empty prefixes), never error out the whole resolve.
	if s.deptProvider != nil && scope.UniversalID != "" {
		userDepts, derr := s.deptProvider.GetUserDepartments(scope.UniversalID)
		if derr != nil {
			logger.Warn("[authz] dept-sync lookup for user scope failed (failing closed): %v", derr)
		} else {
			for _, d := range userDepts {
				if d.DeptPath != "" {
					scope.DeptPaths = appendUnique(scope.DeptPaths, d.DeptPath)
					scope.VisibleDeptPrefixes = appendUnique(scope.VisibleDeptPrefixes, d.DeptPath)
				}
			}
		}
	}

	// (4) Extra subtrees from ScopeDeptPermission grants that apply to this user.
	extra, eerr := s.extraVisibleDeptPrefixes(userID, scope.DeptPaths)
	if eerr != nil {
		return nil, eerr
	}
	for _, p := range extra {
		scope.VisibleDeptPrefixes = appendUnique(scope.VisibleDeptPrefixes, p)
	}

	return scope, nil
}

// hasAllAccess reports whether the user sees every department: either an admin
// role (platform_admin / business_admin) or a direct user ScopeAllPermission
// grant. Neither path touches dept-sync, so AllAccess is robust to dept-sync
// being down. The caller passes the user's already-expanded roles so this stays
// a single role-expansion per request (the auth bootstrap path expands once).
func (s *Service) hasAllAccess(userID string, expandedRoles []string) (bool, error) {
	for _, r := range expandedRoles {
		if r == systemrole.SystemRolePlatformAdmin || r == systemrole.SystemRoleBusinessAdmin {
			return true, nil
		}
	}

	// A direct user grant of the "see all" scope (granted to a non-admin operator).
	var count int64
	if err := s.db.Model(&models.PermissionGrant{}).
		Where("permission_code = ? AND subject_type = ? AND subject_id = ?",
			ScopeAllPermission, models.PermissionSubjectUser, userID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// extraVisibleDeptPrefixes returns the dept_path prefixes opened up for this user
// by ScopeDeptPermission grants. A grant applies when:
//   - it is a direct user grant (subject_type='user', subject_id=userID); or
//   - it is a department grant (subject_type='department') whose subtree contains
//     one of the user's own departments (the user is in that department or a
//     descendant — same prefix rule as CheckGrant).
//
// The grant's redundantly-stored dept_path is the prefix to add. Department
// grants with an empty dept_path are skipped (they convey nothing without a path).
func (s *Service) extraVisibleDeptPrefixes(userID string, userDeptPaths []string) ([]string, error) {
	var grants []models.PermissionGrant
	if err := s.db.Where("permission_code = ?", ScopeDeptPermission).
		Find(&grants).Error; err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(grants))
	for _, g := range grants {
		if g.DeptPath == "" {
			continue
		}
		switch g.SubjectType {
		case models.PermissionSubjectUser:
			if g.SubjectID == userID {
				out = append(out, g.DeptPath)
			}
		case models.PermissionSubjectDepartment:
			// The grant is on a department; it applies to this user iff the user
			// belongs to that department or one of its descendants — i.e. one of
			// the user's own dept paths is at-or-below the grant's dept_path.
			for _, ud := range userDeptPaths {
				if pathHasPrefix(ud, g.DeptPath) {
					out = append(out, g.DeptPath)
					break
				}
			}
		}
	}
	return out, nil
}

// HasKanbanMenuAccess reports whether the kanban sidebar entry should be visible
// to the user via an EXPLICIT metrics authorization:
//   - an admin role or a direct ScopeAllPermission grant (hasAllAccess), or
//   - any ScopeDeptPermission grant that applies to them (a direct user grant, or
//     a department grant whose subtree contains one of their departments).
//
// It deliberately IGNORES the default "self department" scope that every employee
// implicitly has (surfacing the entry from that would make it visible to everyone,
// defeating the admin-authorized-only requirement). It is read-only and never
// weakens the existing HasPermission / CheckGrant paths.
//
// Cost: this is reached from the hot /api/auth/permissions bootstrap path, so —
// mirroring CheckGrant's fast path — it only consults dept-sync when a
// department-subject grant actually exists. Admins, direct user grants, and the
// common "no kanban grant yet" case all resolve without any dept-sync call.
// dept-sync failures fail closed (a user with no resolvable departments inherits
// no department grant; direct user/all grants still count).
func (s *Service) HasKanbanMenuAccess(userID string) (bool, error) {
	roles, err := s.roleProvider.GetExpandedRoles(userID)
	if err != nil {
		return false, err
	}
	return s.hasKanbanMenuAccess(userID, roles)
}

// hasKanbanMenuAccess is the role-aware core of HasKanbanMenuAccess. It takes the
// user's already-expanded roles so the hot /api/auth/permissions path, which has
// expanded them once for the menu loop, needn't expand again.
func (s *Service) hasKanbanMenuAccess(userID string, expandedRoles []string) (bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}

	// (1) Admin role or a direct "see all" grant — no dept-sync needed.
	allAccess, err := s.hasAllAccess(userID, expandedRoles)
	if err != nil {
		return false, err
	}
	if allAccess {
		return true, nil
	}

	// (2) Load the scope.dept grants once. A direct user grant matches immediately
	// (no dept-sync); dept-sync is consulted only when a department-subject grant
	// exists. The loaded rows are reused for the inheritance match below, so this
	// path issues a single grants query.
	var grants []models.PermissionGrant
	if err := s.db.Where("permission_code = ?", ScopeDeptPermission).Find(&grants).Error; err != nil {
		return false, err
	}
	hasDeptSubjectGrant := false
	for _, g := range grants {
		if g.DeptPath == "" {
			continue
		}
		switch g.SubjectType {
		case models.PermissionSubjectUser:
			if g.SubjectID == userID {
				return true, nil
			}
		case models.PermissionSubjectDepartment:
			hasDeptSubjectGrant = true
		}
	}
	if !hasDeptSubjectGrant {
		return false, nil
	}

	// (3) A department grant exists: resolve the user's own dept paths (fail
	// closed) and match the already-loaded department grants by the same prefix
	// rule as extraVisibleDeptPrefixes (shared pathHasPrefix primitive).
	userDeptPaths := s.ownDeptPaths(userID)
	for _, g := range grants {
		if g.SubjectType != models.PermissionSubjectDepartment || g.DeptPath == "" {
			continue
		}
		for _, ud := range userDeptPaths {
			if pathHasPrefix(ud, g.DeptPath) {
				return true, nil
			}
		}
	}
	return false, nil
}

// ownDeptPaths resolves the user's own department materialized paths via dept-sync,
// keyed by casdoor_universal_id. It fails closed: any missing universal id /
// unavailable dept-sync / lookup error yields an empty slice (logged), never an
// error, so callers degrade to "direct grants only". (Kept separate from
// ResolveUserScope's inline step 3 so the data-scope path stays untouched.)
func (s *Service) ownDeptPaths(userID string) []string {
	paths := []string{}
	if s.deptProvider == nil {
		return paths
	}
	universalID, ok := s.universalIDFor(userID)
	if !ok || universalID == "" {
		return paths
	}
	userDepts, err := s.deptProvider.GetUserDepartments(universalID)
	if err != nil {
		logger.Warn("[authz] dept-sync lookup for kanban menu access failed (failing closed): %v", err)
		return paths
	}
	for _, d := range userDepts {
		if d.DeptPath != "" {
			paths = appendUnique(paths, d.DeptPath)
		}
	}
	return paths
}

// appendUnique appends v to s only if not already present, preserving order.
func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
