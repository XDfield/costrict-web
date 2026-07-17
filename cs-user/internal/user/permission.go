// Package user — permission.go provides role-lookup primitives backing the
// Phase C1 JWT permission claims. Two surfaces:
//
//   - GetPlatformAdmin — single-row lookup by user_id; (nil, nil) means the
//     user is not a platform admin (graceful degradation, mirrors
//     GetEmploymentIdentity's contract).
//   - ListActiveTenantRoles — multi-row lookup of tenant_admins rows where
//     revoked_at IS NULL for the (user_id, tenant_id) pair; returns the
//     role names (owner / admin / billing).
//
// The A7 reissue-token handler consumes both to populate the new
// `platform_admin` / `platform_scope` / `tenant_roles` JWT claims. The
// middlewares on server-side then read these claims to gate admin endpoints.
package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// GetPlatformAdmin loads the user's platform_admins row. Returns (nil, nil)
// when no row exists — reissue-token treats this as "user is not a platform
// admin" and emits no platform_admin / platform_scope claims, mirroring the
// graceful-degradation contract of GetEmploymentIdentity.
//
// Empty userSubjectID is a caller-programming error (400-mappable).
func (s *Service) GetPlatformAdmin(ctx context.Context, userSubjectID string) (*models.PlatformAdmin, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if userSubjectID == "" {
		return nil, ErrEmptySubjectID
	}
	var row models.PlatformAdmin
	err := s.db.WithContext(ctx).
		Where("user_id = ?", userSubjectID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query platform_admin: %w", err)
	}
	return &row, nil
}

// ListActiveTenantRoles loads the user's active (revoked_at IS NULL)
// tenant_admins rows for the given tenant. Returns the role names (owner /
// admin / billing) — empty slice means the user has no admin role on this
// tenant (a regular tenant_member).
//
// tenant_id is required because a user can be a tenant_admin in tenant A
// while being a regular member in tenant B. Empty userSubjectID / tenantID
// is a caller-programming error (400-mappable).
func (s *Service) ListActiveTenantRoles(ctx context.Context, userSubjectID, tenantID string) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if userSubjectID == "" {
		return nil, ErrEmptySubjectID
	}
	if tenantID == "" {
		return nil, ErrEmptyTenantID
	}
	var rows []models.TenantAdmin
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND tenant_id = ? AND revoked_at IS NULL", userSubjectID, tenantID).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query tenant_admins: %w", err)
	}
	roles := make([]string, 0, len(rows))
	for _, r := range rows {
		roles = append(roles, r.Role)
	}
	return roles, nil
}

// ErrEmptyTenantID is returned by tenant-scoped role lookups when the
// caller passes an empty tenantID. Mirrors ErrEmptySubjectID's role as a
// 400-mappable sentinel.
var ErrEmptyTenantID = errors.New("user: empty tenant id")
