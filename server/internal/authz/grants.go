package authz

import (
	"errors"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrInvalidSubjectType is returned when a grant subject type is neither
// "user" nor "department".
var ErrInvalidSubjectType = errors.New("invalid subject type")

// ErrGrantNotFound is returned when revoking a grant id that does not exist.
var ErrGrantNotFound = errors.New("permission grant not found")

// DepartmentInfo is the minimal department shape the grant engine needs: only
// the materialized path is used for prefix-based inheritance.
type DepartmentInfo struct {
	DeptID   string
	DeptPath string
}

// DepartmentProvider resolves a dept-sync user's departments. It is intentionally
// narrow so authz does not depend on the deptsync package directly (the deptsync
// client satisfies it via a thin adapter in main.go) and so tests can inject a
// fake. ErrNotConfigured-style failures must be surfaced as errors so the caller
// can fail-closed (deny the department path) rather than mis-grant.
type DepartmentProvider interface {
	// GetUserDepartments returns the departments a dept-sync user (keyed by
	// universal id) belongs to. May return an error when dept-sync is not
	// configured / unreachable.
	GetUserDepartments(deptSyncUserID string) ([]DepartmentInfo, error)
	// GetDepartmentPath returns the materialized dept_path for a department id,
	// used when persisting a department grant so rechecks need no tree lookup.
	GetDepartmentPath(deptID string) (string, error)
}

// SetDepartmentProvider wires the optional dept-sync-backed department provider.
// When nil (dept-sync not configured), department grants simply never match
// (fail-closed) while user grants and the role path keep working.
func (s *Service) SetDepartmentProvider(p DepartmentProvider) {
	s.deptProvider = p
}

// GrantPermission records a fine-grained grant of permissionCode to a subject.
// For department subjects (and user-subject metrics-scope grants) deptPath is the
// materialized path stored redundantly to make rechecks a pure prefix comparison.
//
// The operation is idempotent on (code, type, id). When such a grant already
// exists, it is returned unchanged UNLESS a non-empty deptPath differs from the
// stored one — that means re-targeting an existing grant (e.g. moving a user's
// kanban.scope.dept from department Y to Z), so the stored dept_path is updated
// in place (the unique key forbids a second row for the same triple).
func (s *Service) GrantPermission(permissionCode, subjectType, subjectID, deptPath, grantedBy string) (*models.PermissionGrant, error) {
	permissionCode = strings.TrimSpace(permissionCode)
	subjectID = strings.TrimSpace(subjectID)
	if permissionCode == "" || subjectID == "" {
		return nil, errors.New("permissionCode and subjectId are required")
	}
	if subjectType != models.PermissionSubjectUser && subjectType != models.PermissionSubjectDepartment {
		return nil, ErrInvalidSubjectType
	}

	// Existing grant → idempotent, but allow re-targeting its dept_path.
	var existing models.PermissionGrant
	err := s.db.Where("permission_code = ? AND subject_type = ? AND subject_id = ?",
		permissionCode, subjectType, subjectID).First(&existing).Error
	if err == nil {
		if deptPath != "" && deptPath != existing.DeptPath {
			if uerr := s.db.Model(&existing).Update("dept_path", deptPath).Error; uerr != nil {
				return nil, uerr
			}
			existing.DeptPath = deptPath
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	grant := models.PermissionGrant{
		PermissionCode: permissionCode,
		SubjectType:    subjectType,
		SubjectID:      subjectID,
		DeptPath:       deptPath,
		GrantedBy:      grantedBy,
	}
	if err := s.db.Create(&grant).Error; err != nil {
		return nil, err
	}
	return &grant, nil
}

// ResolveDepartmentPath returns the materialized dept_path for a department id
// via the dept-sync provider, so a department grant can store it redundantly.
// Returns an error when dept-sync is not wired/unavailable — a department grant
// is meaningless without its path (inheritance relies on prefix matching), so
// the caller should reject the grant rather than persist an empty path.
func (s *Service) ResolveDepartmentPath(deptID string) (string, error) {
	if s.deptProvider == nil {
		return "", errors.New("department provider not configured")
	}
	return s.deptProvider.GetDepartmentPath(deptID)
}

// RevokePermission deletes a grant by id. The id column is a postgres uuid, so a
// malformed id would otherwise reach the driver as "invalid input syntax for type
// uuid" (a 500). We reject non-uuid ids up front as not-found, since they can
// never match a real grant. (sqlite test DBs use a TEXT id, so this guard simply
// turns those unknown ids into the same ErrGrantNotFound they'd produce anyway.)
func (s *Service) RevokePermission(id string) error {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return ErrGrantNotFound
	}
	result := s.db.Where("id = ?", id).Delete(&models.PermissionGrant{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGrantNotFound
	}
	return nil
}

// ListGrants returns all grants, optionally filtered by permission code.
func (s *Service) ListGrants(permissionCode string) ([]models.PermissionGrant, error) {
	q := s.db.Model(&models.PermissionGrant{}).Order("created_at DESC")
	if strings.TrimSpace(permissionCode) != "" {
		q = q.Where("permission_code = ?", strings.TrimSpace(permissionCode))
	}
	var grants []models.PermissionGrant
	if err := q.Find(&grants).Error; err != nil {
		return nil, err
	}
	return grants, nil
}

// CheckGrant is the core fine-grained authorization check (mentor RBAC).
//
// It returns true when permissionCode is granted to the user, by either:
//
//	(a) a direct user grant: (permissionCode, 'user', userID) exists; or
//	(b) a department grant whose dept_path P_D is a prefix (with '/' boundary)
//	    of any department path U the user belongs to — i.e. the user is in that
//	    department or any of its descendants.
//
// Performance / degradation contract:
//   - If the permission has NO department grants, dept-sync is never called
//     (the cost-free fast path: direct-grant lookup only).
//   - userID (a costrict-web subject_id) is mapped to its casdoor_universal_id
//     before querying dept-sync, since dept-sync keys users by universal id.
//   - If dept-sync is unconfigured/unreachable, the department path fails closed
//     (returns false for that path). The direct-grant and role paths are
//     unaffected, so a grant simply can't confer extra access while dept-sync is
//     down — we never mis-grant, and never mis-deny role/direct access.
func (s *Service) CheckGrant(userID, permissionCode string) (bool, error) {
	permissionCode = strings.TrimSpace(permissionCode)
	userID = strings.TrimSpace(userID)
	if permissionCode == "" || userID == "" {
		return false, nil
	}

	// (a) Direct user grant.
	var directCount int64
	if err := s.db.Model(&models.PermissionGrant{}).
		Where("permission_code = ? AND subject_type = ? AND subject_id = ?",
			permissionCode, models.PermissionSubjectUser, userID).
		Count(&directCount).Error; err != nil {
		return false, err
	}
	if directCount > 0 {
		return true, nil
	}

	// (b) Department grants for this permission. Load their dept_paths first.
	var deptGrants []models.PermissionGrant
	if err := s.db.Where("permission_code = ? AND subject_type = ?",
		permissionCode, models.PermissionSubjectDepartment).
		Find(&deptGrants).Error; err != nil {
		return false, err
	}
	// Fast path: no department grants → skip the dept-sync round-trip entirely.
	if len(deptGrants) == 0 {
		return false, nil
	}

	// dept-sync not wired → department path fails closed (no extra grant), but
	// the direct/role paths already had their say above.
	if s.deptProvider == nil {
		return false, nil
	}

	// Map subject_id → casdoor_universal_id (dept-sync keys users by universal id).
	universalID, ok := s.universalIDFor(userID)
	if !ok || universalID == "" {
		return false, nil
	}

	userDepts, err := s.deptProvider.GetUserDepartments(universalID)
	if err != nil {
		// Degrade: dept-sync unavailable → fail closed on the department path.
		logger.Warn("[authz] dept-sync lookup for grant check failed (failing closed): %v", err)
		return false, nil
	}

	for _, g := range deptGrants {
		grantPath := g.DeptPath
		if grantPath == "" {
			continue
		}
		for _, ud := range userDepts {
			if pathHasPrefix(ud.DeptPath, grantPath) {
				return true, nil
			}
		}
	}
	return false, nil
}

// universalIDFor resolves a costrict-web subject_id to its casdoor_universal_id.
func (s *Service) universalIDFor(subjectID string) (string, bool) {
	var user models.User
	if err := s.db.Select("casdoor_universal_id").
		Where("subject_id = ?", subjectID).First(&user).Error; err != nil {
		return "", false
	}
	if user.CasdoorUniversalID == nil {
		return "", false
	}
	return *user.CasdoorUniversalID, true
}

// pathHasPrefix reports whether userPath is the granted department path or one of
// its descendants. It guards against "fake prefix" matches (e.g. "/A/Bc" must not
// match a grant on "/A/B") by requiring an exact match or a '/' boundary.
func pathHasPrefix(userPath, grantPath string) bool {
	if grantPath == "" || userPath == "" {
		return false
	}
	if userPath == grantPath {
		return true
	}
	return strings.HasPrefix(userPath, grantPath+"/")
}
