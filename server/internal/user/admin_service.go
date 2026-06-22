package user

import (
	"errors"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Admin user-management service methods (M1 · 成员管理). These power the
// platform-admin /api/admin/users surface: a paginated/searchable/status-filtered
// user list, an account-status switch, an organization roll-up, and a per-user
// profile aggregation. They deliberately live apart from the login-sync logic so
// the management read/write paths never go through the is_active "revive" code.

// Account status values stored in users.status. Distinct from is_active (a
// login-sync flag); status is the admin-controlled gate consulted by the auth
// middleware (see middleware.SetStatusChecker).
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
	UserStatusBanned   = "banned"
)

var (
	// ErrInvalidUserStatus is returned for a status value outside the allowlist.
	ErrInvalidUserStatus = errors.New("invalid user status")
	// ErrCannotChangeOwnStatus guards against an admin locking themselves out.
	ErrCannotChangeOwnStatus = errors.New("cannot change your own status")
	// ErrAdminUserNotFound is returned when the target subject id has no user row.
	ErrAdminUserNotFound = errors.New("user not found")
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
	PageSize     int    // clamped to [1, 200], default 20
}

// ListUsers returns a page of users (newest first) for the admin console along
// with the total matching count. Unlike SearchUsers it does NOT hard-filter
// is_active, so disabled/banned members remain visible to management.
func (s *UserService) ListUsers(p ListUsersParams) ([]models.User, int64, error) {
	page := p.Page
	if page < 1 {
		page = 1
	}
	pageSize := p.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	q := s.db.Model(&models.User{})
	if p.Keyword != "" {
		pattern := "%" + p.Keyword + "%"
		q = q.Where("username LIKE ? OR display_name LIKE ? OR email LIKE ?", pattern, pattern, pattern)
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

	var users []models.User
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// GetUserStatus returns the account status for a subject id. Used by the auth
// middleware status checker, so it is intentionally fail-open: an absent row, a
// blank value, or a pre-migration default all resolve to UserStatusActive with a
// nil error (never ErrAdminUserNotFound). Only a real DB error is propagated, and
// the middleware itself also fails open on that. Net effect: a missing/legacy
// user is treated as active and never blocked by the status gate.
func (s *UserService) GetUserStatus(subjectID string) (string, error) {
	var status string
	err := s.db.Model(&models.User{}).
		Where("subject_id = ?", subjectID).
		Limit(1).
		Pluck("status", &status).Error
	if err != nil {
		return "", err
	}
	if status == "" {
		// A blank means either no row or a pre-migration default; treat empty as
		// active so callers never block on a missing/legacy value.
		return UserStatusActive, nil
	}
	return status, nil
}

// SetUserStatus flips a member's account status. It validates the value against
// the allowlist and refuses to let an operator change their own status (so an
// admin can't self-lock). Only the status column is written (is_active and the
// login-sync fields are untouched).
func (s *UserService) SetUserStatus(subjectID, status, operatorID string) error {
	if !IsValidUserStatus(status) {
		return ErrInvalidUserStatus
	}
	if operatorID != "" && operatorID == subjectID {
		return ErrCannotChangeOwnStatus
	}

	result := s.db.Model(&models.User{}).
		Where("subject_id = ?", subjectID).
		Update("status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrAdminUserNotFound
	}
	return nil
}

// OrganizationCount is one row of the organization roll-up.
type OrganizationCount struct {
	Organization string `json:"organization"`
	MemberCount  int64  `json:"memberCount"`
}

// ListOrganizations groups users by organization and returns member counts,
// busiest first. NULL/empty organizations are skipped.
func (s *UserService) ListOrganizations() ([]OrganizationCount, error) {
	var rows []OrganizationCount
	if err := s.db.Model(&models.User{}).
		Select("organization AS organization, COUNT(*) AS member_count").
		Where("organization IS NOT NULL AND organization <> ''").
		Group("organization").
		Order("member_count DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UserProfile aggregates a single member's activity for the detail drawer.
type UserProfile struct {
	CreatedItemCount int64 `json:"createdItemCount"` // capability_items.created_by = subject_id
	DistributedCount int64 `json:"distributedCount"` // item_distributions.distributor_id = subject_id
	ReceivedCount    int64 `json:"receivedCount"`    // item_distribution_receipts.user_id = subject_id
}

// GetUserProfile computes the activity counts for one member. The three counts
// are independent COUNT(*) aggregates keyed on subject_id (single-user lookups,
// so no N+1 concern here; the list endpoint stays count-free to remain cheap).
func (s *UserService) GetUserProfile(subjectID string) (*UserProfile, error) {
	profile := &UserProfile{}

	if err := s.db.Model(&models.CapabilityItem{}).
		Where("created_by = ?", subjectID).
		Count(&profile.CreatedItemCount).Error; err != nil {
		return nil, err
	}
	if err := s.db.Model(&models.ItemDistribution{}).
		Where("distributor_id = ?", subjectID).
		Count(&profile.DistributedCount).Error; err != nil {
		return nil, err
	}
	if err := s.db.Model(&models.ItemDistributionReceipt{}).
		Where("user_id = ?", subjectID).
		Count(&profile.ReceivedCount).Error; err != nil {
		return nil, err
	}
	return profile, nil
}

// rolesForUsers batch-loads system roles for a set of subject ids in ONE query
// (avoids per-row role lookups in the list endpoint), returning subject_id →
// roles. Mirrors the batch-aggregate pattern used by fetchForkCounts.
func rolesForUsers(db *gorm.DB, subjectIDs []string) map[string][]string {
	out := make(map[string][]string, len(subjectIDs))
	if len(subjectIDs) == 0 {
		return out
	}
	type roleRow struct {
		UserID string
		Role   string
	}
	var rows []roleRow
	db.Model(&models.UserSystemRole{}).
		Select("user_id, role").
		Where("user_id IN ? AND deleted_at IS NULL", subjectIDs).
		Order("created_at ASC").
		Scan(&rows)
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.Role)
	}
	return out
}

// RolesForUsers is the exported batch role loader for handlers.
func (s *UserService) RolesForUsers(subjectIDs []string) map[string][]string {
	return rolesForUsers(s.db, subjectIDs)
}
