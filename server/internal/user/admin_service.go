package user

import (
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Local admin user-management helpers (M1 · 成员管理). After the
// admin-user-migration slice (option A full migration), identity + status
// reads/writes are proxied to cs-user via *RPCClient. What remains here is
// the local-only surface:
//
//   - GetUserStatus: fail-open status lookup consulted by the auth middleware
//     status gate (middleware.SetStatusChecker).
//   - GetUserProfile: locally-computed activity counts (capability_items,
//     item_distributions, item_distribution_receipts) used by the admin
//     profile detail endpoint.
//   - RolesForUsers: batch system-role lookup (user_system_roles) used by the
//     admin list + profile endpoints.
//
// These deliberately live apart from the login-sync logic so the management
// read paths never go through the is_active "revive" code.

// Account status values stored in users.status. Distinct from is_active (a
// login-sync flag); status is the admin-controlled gate consulted by the auth
// middleware (see middleware.SetStatusChecker).
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
	UserStatusBanned   = "banned"
)

// GetUserStatus returns the account status for a subject id. Used by the auth
// middleware status checker, so it is intentionally fail-open: an absent row, a
// blank value, or a pre-migration default all resolve to UserStatusActive with a
// nil error. Only a real DB error is propagated, and the middleware itself also
// fails open on that. Net effect: a missing/legacy user is treated as active
// and never blocked by the status gate.
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

// UserProfile aggregates a single member's activity for the detail drawer.
// These counts are local to @server — the underlying tables (capability_items,
// item_distributions, item_distribution_receipts) live in costrict_db and are
// not replicated to cs_user.
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
