// Package user — admin user-management service methods.
//
// These power the platform-admin user-management surface that lives on
// @server as /api/admin/users/* and is being migrated to cs-user as the
// single source of truth for user identity + status (admin-user-migration
// slice, option A full migration). They deliberately live apart from the
// login-sync logic (GetOrCreateUser, BindIdentityToUser) so the management
// read/write paths never go through the is_active "revive" code.
//
// Status values stored in users.status. Distinct from is_active (a
// login-sync flag); status is the admin-controlled gate consulted by the
// auth middleware on @server (middleware.SetStatusChecker) via the
// GetTenantUserStatus RPC (added later in the slice).

package user

import (
	"context"
	"errors"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
)

// Account status values stored in users.status. Mirrors @server's
// admin_service.go UserStatus* constants 1:1 — the values are part of the
// public HTTP contract (request bodies, audit payloads) so they must stay
// in lockstep across both modules.
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
	UserStatusBanned   = "banned"
)

// Default page size when ListUsersParams.PageSize <= 0; matches @server.
const DefaultAdminUserPageSize = 20

// MaxAdminUserPageSize caps page size to bound query cost; matches @server.
const MaxAdminUserPageSize = 200

var (
	// ErrInvalidUserStatus is returned for a status value outside the allowlist.
	ErrInvalidUserStatus = errors.New("user: invalid user status")
	// ErrAdminUserNotFound is returned when the target subject_id has no row.
	ErrAdminUserNotFound = errors.New("user: admin target not found")
)

// IsValidUserStatus reports whether status is one of the allowed account states.
func IsValidUserStatus(status string) bool {
	switch status {
	case UserStatusActive, UserStatusDisabled, UserStatusBanned:
		return true
	default:
		return false
	}
}

// ListUsersParams narrows the admin user list. Empty fields are ignored.
type ListUsersParams struct {
	Keyword      string // username/display_name/email LIKE
	Organization string // exact users.organization
	Status       string // exact users.status (active|disabled|banned)
	Page         int    // 1-based
	PageSize     int    // clamped to [1, MaxAdminUserPageSize], default DefaultAdminUserPageSize
}

// ListUsers returns a page of users (newest first) for the admin console
// along with the total matching count. Unlike SearchUsers it does NOT
// hard-filter is_active, so disabled/banned members remain visible to
// management — admins need to see the full roster including suspended
// accounts.
//
// B5: applies tenant.Scope(ctx) so platform-admin viewing tenant A's users
// only sees tenant A's rows (the request context carries the target tenant).
func (s *Service) ListUsers(ctx context.Context, p ListUsersParams) ([]*models.User, int64, error) {
	if s == nil || s.db == nil {
		return nil, 0, errors.New("user.Service: nil db")
	}

	page := p.Page
	if page < 1 {
		page = 1
	}
	pageSize := p.PageSize
	if pageSize <= 0 {
		pageSize = DefaultAdminUserPageSize
	}
	if pageSize > MaxAdminUserPageSize {
		pageSize = MaxAdminUserPageSize
	}

	q := s.db.WithContext(ctx).Scopes(tenant.Scope(ctx)).Model(&models.User{})
	if p.Keyword != "" {
		pattern := "%" + p.Keyword + "%"
		// Parens load-bearing: SQL AND binds tighter than OR — without
		// them the keyword filter would leak org/status filters back in.
		q = q.Where(
			"(username LIKE ? OR display_name LIKE ? OR email LIKE ?)",
			pattern, pattern, pattern,
		)
	}
	if p.Organization != "" {
		q = q.Where("organization = ?", p.Organization)
	}
	if p.Status != "" {
		q = q.Where("status = ?", p.Status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var users []*models.User
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}
