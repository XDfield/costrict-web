// local_reader.go — local-DB implementation of AdminUserRPC.
//
// Activated when USER_SERVICE_BACKEND=local (see main.go wiring). Reads and
// writes the local users table directly, mirroring the RPC surface so
// adminuser.Module's handlers work without cs-user. This is the dev /
// single-box posture; production keeps Backend=rpc so cs-user stays the
// single source of truth for identity + status (ADR D1).
//
// Sentinel mapping matches RPCClient (rpc_client_admin_user.go) so handler
// error branches don't fork between backends:
//
//	invalid status / invalid profile input → ErrAdminUserRPCInvalidStatus / ErrAdminUserRPCInvalidProfile
//	self status change                     → ErrAdminUserRPCCannotChangeOwn
//	username collision (profile override)  → ErrAdminUserRPCUsernameTaken
//	unknown subject_id                     → ErrAdminUserRPCNotFound
package adminuser

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"gorm.io/gorm"
)

// LocalReader serves the admin member-management surface from the local DB.
// Constructed by main.go when USER_SERVICE_BACKEND=local; otherwise the RPC
// client is used and this type stays unused.
type LocalReader struct {
	db *gorm.DB
}

// NewLocalReader constructs a LocalReader. db must be non-nil.
func NewLocalReader(db *gorm.DB) *LocalReader {
	return &LocalReader{db: db}
}

// Compile-time assertion that LocalReader satisfies AdminUserRPC.
var _ AdminUserRPC = (*LocalReader)(nil)

// Configured is always true — the local DB is the configured backend when
// this reader is selected. The bool matches the AdminUserRPC contract so
// Module.rpcUnavailable() returns false and handlers proceed.
func (r *LocalReader) Configured() bool { return true }

// clampPaging normalises page/pageSize to the same bounds as the deleted
// admin_service.ListUsers: page ≥ 1, 1 ≤ pageSize ≤ 200, default 20.
func clampPaging(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
}

// ListUsers returns a page of local users (newest first) plus the total
// matching count. Mirrors admin_service.ListUsers: keyword LIKE matches
// username/display_name/email; organization/status are exact; is_active is
// NOT hard-filtered so disabled/banned members stay visible to management.
func (r *LocalReader) ListUsers(_ context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error) {
	page, pageSize := clampPaging(p.Page, p.PageSize)

	q := r.db.Model(&models.User{})
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
		return nil, err
	}

	var rows []models.User
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]userpkg.AdminUser, 0, len(rows))
	for _, u := range rows {
		out = append(out, toAdminUser(u))
	}
	return &userpkg.AdminUserListResult{
		Users: out,
		Total: total,
		Page:  page,
		Size:  pageSize,
	}, nil
}

// SetUserStatus flips the local users.status column. The allowlist + self-lock
// guards mirror the deleted admin_service.SetUserStatus; the result carries
// the from/to pair the handler echoes back.
func (r *LocalReader) SetUserStatus(_ context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
	switch status {
	case userpkg.UserStatusActive, userpkg.UserStatusDisabled, userpkg.UserStatusBanned:
	default:
		return nil, userpkg.ErrAdminUserRPCInvalidStatus
	}
	if operatorID != "" && operatorID == subjectID {
		return nil, userpkg.ErrAdminUserRPCCannotChangeOwn
	}

	var prev string
	if err := r.db.Model(&models.User{}).
		Where("subject_id = ?", subjectID).
		Limit(1).
		Pluck("status", &prev).Error; err != nil {
		return nil, err
	}
	if prev == "" {
		// No row matched — Pluck returns "" for both "no row" and "row with
		// blank status"; the unique-index lookup above ensures a hit, so ""
		// here means unknown subject_id (local users.status defaults to
		// 'active' via column DEFAULT, never stored blank).
		return nil, userpkg.ErrAdminUserRPCNotFound
	}

	result := r.db.Model(&models.User{}).
		Where("subject_id = ?", subjectID).
		Update("status", status)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, userpkg.ErrAdminUserRPCNotFound
	}
	return &userpkg.AdminSetUserStatusResult{FromStatus: prev, ToStatus: status}, nil
}

// ListOrganizations groups local users by organization, busiest first.
// NULL/empty organizations are skipped — same shape as the deleted
// admin_service.ListOrganizations.
func (r *LocalReader) ListOrganizations(_ context.Context) ([]userpkg.AdminOrganization, error) {
	var rows []userpkg.AdminOrganization
	if err := r.db.Model(&models.User{}).
		Select("organization AS organization, COUNT(*) AS member_count").
		Where("organization IS NOT NULL AND organization <> ''").
		Group("organization").
		Order("member_count DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []userpkg.AdminOrganization{}
	}
	return rows, nil
}

// GetUserProfile returns the identity+status projection for one member.
// Distinct from UserService.GetUserProfile (which returns activity counts);
// this method mirrors cs-user's adminUserProfileDTO field set.
func (r *LocalReader) GetUserProfile(_ context.Context, subjectID string) (*userpkg.AdminUserProfile, error) {
	var u models.User
	err := r.db.Where("subject_id = ?", subjectID).Limit(1).First(&u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, userpkg.ErrAdminUserRPCNotFound
		}
		return nil, err
	}
	profile := toAdminUserProfile(u)
	return &profile, nil
}

// AdminUpdateProfile mutates local users.username and/or display_name.
// Username is checked for collision against the unique index BEFORE the
// write so a clean ErrAdminUserRPCUsernameTaken surfaces (the DB-level
// duplicate-key error is also caught as a fallback). nil DisplayName
// preserves existing; non-nil empty clears to NULL — mirrors cs-user.
func (r *LocalReader) AdminUpdateProfile(_ context.Context, subjectID string, args userpkg.AdminUpdateProfileArgs) (*userpkg.AdminUserProfile, error) {
	if args.Username == "" && args.DisplayName == nil {
		return nil, userpkg.ErrAdminUserRPCInvalidProfile
	}

	var u models.User
	err := r.db.Where("subject_id = ?", subjectID).Limit(1).First(&u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, userpkg.ErrAdminUserRPCNotFound
		}
		return nil, err
	}

	// Username collision pre-check (skip if unchanged).
	if args.Username != "" && args.Username != u.Username {
		var dup int64
		if err := r.db.Model(&models.User{}).
			Where("username = ? AND subject_id <> ?", args.Username, subjectID).
			Count(&dup).Error; err != nil {
			return nil, err
		}
		if dup > 0 {
			return nil, userpkg.ErrAdminUserRPCUsernameTaken
		}
	}

	updates := map[string]any{}
	if args.Username != "" && args.Username != u.Username {
		updates["username"] = args.Username
	}
	if args.DisplayName != nil {
		if *args.DisplayName == "" {
			updates["display_name"] = nil
		} else {
			updates["display_name"] = *args.DisplayName
		}
	}
	if len(updates) > 0 {
		result := r.db.Model(&models.User{}).
			Where("subject_id = ?", subjectID).
			Updates(updates)
		if result.Error != nil {
			// Fallback uniqueness guard — the pre-check races in concurrent
			// admin writes; the DB constraint is authoritative.
			if isDuplicateKey(result.Error) {
				return nil, userpkg.ErrAdminUserRPCUsernameTaken
			}
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			return nil, userpkg.ErrAdminUserRPCNotFound
		}
	}

	// Reload to pick up the merged row (and avoid reconstructing timestamps).
	var refreshed models.User
	if err := r.db.Where("subject_id = ?", subjectID).Limit(1).First(&refreshed).Error; err != nil {
		return nil, err
	}
	out := toAdminUserProfile(refreshed)
	return &out, nil
}

// toAdminUser maps a local user row to the list-endpoint projection. Matches
// AdminUser's field set exactly; nullable strings pass through as pointers so
// the omitempty tags behave the same as the RPC path.
func toAdminUser(u models.User) userpkg.AdminUser {
	return userpkg.AdminUser{
		SubjectID:    u.SubjectID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		Email:        u.Email,
		AvatarURL:    u.AvatarURL,
		Organization: u.Organization,
		Status:       statusOrActive(u.Status),
		IsActive:     u.IsActive,
		CreatedAt:    formatTime(u.CreatedAt),
	}
}

// toAdminUserProfile maps a local user row to the detail-endpoint projection
// (the superset that adds phone / auth_provider / last_login_at).
func toAdminUserProfile(u models.User) userpkg.AdminUserProfile {
	return userpkg.AdminUserProfile{
		SubjectID:    u.SubjectID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		Email:        u.Email,
		Phone:        u.Phone,
		AvatarURL:    u.AvatarURL,
		AuthProvider: u.AuthProvider,
		Organization: u.Organization,
		Status:       statusOrActive(u.Status),
		IsActive:     u.IsActive,
		LastLoginAt:  formatTimePtr(u.LastLoginAt),
		CreatedAt:    formatTime(u.CreatedAt),
	}
}

// statusOrActive defaults a blank status to active — matches the legacy
// admin_service path and the toResponseFromAdminUser post-processing in
// handlers.go (defensive, since column DEFAULT already enforces this).
func statusOrActive(s string) string {
	if s == "" {
		return userpkg.UserStatusActive
	}
	return s
}

// formatTime renders a time.Time as RFC3339 — matches cs-user's JSON output
// shape (ISO 8601 with timezone) so the frontend parses both backends
// identically.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// formatTimePtr is the pointer variant of formatTime; nil → nil so the
// omitempty tag drops the field entirely.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

// isDuplicateKey reports whether err is a DB-level unique-constraint
// violation. Covers MySQL/Postgres/SQLite dialect error strings; the
// exact code varies by driver but the substring match is stable.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	return strings.Contains(msg, "Duplicate entry") || // MySQL
		strings.Contains(lower, "unique constraint") // Postgres / SQLite
}
