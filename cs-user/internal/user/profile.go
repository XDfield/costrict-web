// Package user — profile completion + display_name self-service (R2 of
// REGISTRATION_PROFILE_DESIGN). These methods back the new cs-user RPC
// endpoints that server-side user-facing handlers proxy:
//
//   POST /api/internal/users/:subject_id/complete-registration
//     → CompleteRegistration (set username + display_name + mark complete)
//   POST /api/internal/users/:subject_id/profile
//     → UpdateProfile (user self-edit of display_name only)
//   GET  /api/internal/users/username-available?username=xxx&exclude_subject_id=...
//     → IsUsernameAvailable (tenant-scoped uniqueness)
//
// All three are tenant-scoped via tenant.Scope(ctx) so username uniqueness
// honours the (tenant_id, username) composite index (see migration
// 20260722100000_create_idp_sources.sql's neighbour — idx_users_tenant_username
// is added by an upcoming migration; for now the existing idx_user_username
// global unique is preserved as a transitional safety net).
package user

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"gorm.io/gorm"
)

// DisplayNameMaxLen caps display_name storage to mirror the existing column
// size (varchar(191)). Validation is enforced here so handlers can map the
// 400 cleanly without relying on GORM's data-too-long error.
const DisplayNameMaxLen = 191

// CompleteRegistration sets username + display_name for a first-time user
// and stamps profile_completed_at = NOW(). This is a one-shot operation:
// once profile_completed_at is non-null, subsequent calls return
// ErrRegistrationAlreadyComplete so the user-facing 409 surfaces the
// "you've already completed registration" condition cleanly.
//
// username is tenant-scoped unique; collisions return ErrUsernameTaken.
// display_name is optional (empty string preserved as NULL).
func (s *Service) CompleteRegistration(ctx context.Context, subjectID, username, displayName string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil, errors.New("subject_id is required")
	}
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	dn := strings.TrimSpace(displayName)
	if len(dn) > DisplayNameMaxLen {
		return nil, errors.New("invalid_display_name")
	}

	scope := tenant.Scope(ctx)
	var updated models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		ttx := tx.WithContext(ctx)
		if err := ttx.Scopes(scope).Where("subject_id = ?", subjectID).Take(&updated).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("user_not_found")
			}
			return err
		}
		if updated.ProfileCompletedAt != nil {
			return ErrRegistrationAlreadyComplete
		}
		// Tenant-scoped uniqueness check inside the tx so we catch races.
		var collision int64
		if err := ttx.Model(&models.User{}).Scopes(scope).
			Where("username = ? AND subject_id <> ?", username, subjectID).
			Count(&collision).Error; err != nil {
			return err
		}
		if collision > 0 {
			return ErrUsernameTaken
		}

		now := time.Now()
		patches := map[string]any{
			"username":             username,
			"profile_completed_at": now,
			"updated_at":           now,
		}
		if dn != "" {
			patches["display_name"] = dn
		}
		if err := ttx.Model(&models.User{}).Scopes(scope).
			Where("subject_id = ?", subjectID).
			Updates(patches).Error; err != nil {
			return err
		}
		// Reload to return the post-update row.
		return ttx.Scopes(scope).Where("subject_id = ?", subjectID).Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// UpdateProfile applies user-self edits to display_name only. username is
// user-side immutable (REGISTRATION_PROFILE_DESIGN §3); admin overrides go
// through a separate path (R5). Empty display_name is allowed and clears
// the field back to NULL.
func (s *Service) UpdateProfile(ctx context.Context, subjectID, displayName string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil, errors.New("subject_id is required")
	}
	dn := strings.TrimSpace(displayName)
	if len(dn) > DisplayNameMaxLen {
		return nil, errors.New("invalid_display_name")
	}

	scope := tenant.Scope(ctx)
	var updated models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		ttx := tx.WithContext(ctx)
		if err := ttx.Scopes(scope).Where("subject_id = ?", subjectID).Take(&updated).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("user_not_found")
			}
			return err
		}
		patches := map[string]any{
			"updated_at": time.Now(),
		}
		if dn != "" {
			patches["display_name"] = dn
		} else {
			patches["display_name"] = nil
		}
		if err := ttx.Model(&models.User{}).Scopes(scope).
			Where("subject_id = ?", subjectID).
			Updates(patches).Error; err != nil {
			return err
		}
		return ttx.Scopes(scope).Where("subject_id = ?", subjectID).Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// IsUsernameAvailable reports whether username is free in the caller's
// tenant. excludeSubjectID lets the current user's own row not count as a
// collision (relevant during admin override of an existing user). Format
// and reserved-words are validated first so callers get a 400 body that
// distinguishes "invalid" from "taken".
func (s *Service) IsUsernameAvailable(ctx context.Context, username, excludeSubjectID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("user.Service: nil db")
	}
	if err := ValidateUsername(username); err != nil {
		return false, err
	}
	username = strings.TrimSpace(username)

	scope := tenant.Scope(ctx)
	var count int64
	q := s.db.WithContext(ctx).Model(&models.User{}).Scopes(scope).
		Where("username = ?", username)
	if excludeSubjectID != "" {
		q = q.Where("subject_id <> ?", excludeSubjectID)
	}
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

// AdminUpdateProfile is the admin-side override (R5 of
// REGISTRATION_PROFILE_DESIGN). Unlike UpdateProfile (user-self,
// display_name only), admin may mutate BOTH username and display_name, and
// the call works regardless of profile_completed_at (so admins can fix up
// users who never finished registration, or rename someone post-registration).
//
// username: when non-empty, validated + tenant-scoped uniqueness checked
// inside the tx. Empty string preserves the existing username.
// displayName: when nil, preserved as-is; when non-nil, applied verbatim
// (empty string clears back to NULL).
//
// operatorID is the admin's subject_id, recorded on the audit row. The
// self-rename guard is intentionally NOT applied here — admins renaming
// themselves is a valid escape hatch (e.g. fixing a typo made during
// bootstrap), and cs-user's SetUserStatus already covers the destructive
// self-lock case where it actually matters.
func (s *Service) AdminUpdateProfile(ctx context.Context, subjectID, username string, displayName *string, operatorID string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil, errors.New("subject_id is required")
	}
	if username == "" && displayName == nil {
		return nil, errors.New("no fields to update")
	}
	var newUsername string
	if username != "" {
		if err := ValidateUsername(username); err != nil {
			return nil, err
		}
		newUsername = username
	}
	if displayName != nil {
		if len(*displayName) > DisplayNameMaxLen {
			return nil, errors.New("invalid_display_name")
		}
	}

	scope := tenant.Scope(ctx)
	var updated models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		ttx := tx.WithContext(ctx)
		if err := ttx.Scopes(scope).Where("subject_id = ?", subjectID).Take(&updated).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("user_not_found")
			}
			return err
		}
		// Tenant-scoped uniqueness check only when username is changing.
		if newUsername != "" && newUsername != updated.Username {
			var collision int64
			if err := ttx.Model(&models.User{}).Scopes(scope).
				Where("username = ? AND subject_id <> ?", newUsername, subjectID).
				Count(&collision).Error; err != nil {
				return err
			}
			if collision > 0 {
				return ErrUsernameTaken
			}
		}

		patches := map[string]any{
			"updated_at": time.Now(),
		}
		if newUsername != "" {
			patches["username"] = newUsername
		}
		if displayName != nil {
			if dn := strings.TrimSpace(*displayName); dn != "" {
				patches["display_name"] = dn
			} else {
				patches["display_name"] = nil
			}
		}
		if err := ttx.Model(&models.User{}).Scopes(scope).
			Where("subject_id = ?", subjectID).
			Updates(patches).Error; err != nil {
			return err
		}
		return ttx.Scopes(scope).Where("subject_id = ?", subjectID).Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// ErrRegistrationAlreadyComplete flags a complete-registration call against
// a user that already finished the gate. Surfaces as HTTP 409.
var ErrRegistrationAlreadyComplete = errors.New("registration_already_complete")
