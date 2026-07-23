// Package user — server-side username validation + registration/profile
// service methods (R2 of REGISTRATION_PROFILE_DESIGN). Mirrors cs-user's
// internal/user/username.go rule-for-rule so the server's local backend
// and cs-user's rpc backend behave identically; both sides use the same
// regex/length/reserved-words to keep "invalid" → 400 stable across the
// dual-write boundary.
package user

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

const (
	UsernameMinLen = 3
	UsernameMaxLen = 32
	DisplayNameMaxLen = 191
)

var (
	// usernamePattern allows [A-Za-z0-9_-], must start with a letter or digit.
	usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

	// reservedUsernames blocks route-conflict / system words. Keep in sync
	// with cs-user/internal/user/username.go's reservedUsernames.
	reservedUsernames = map[string]bool{
		"admin": true, "root": true, "system": true, "me": true,
		"api": true, "auth": true, "register": true, "login": true,
		"logout": true, "settings": true, "help": true, "support": true,
		"sysop": true, "operator": true, "staff": true, "moderator": true,
		"official": true, "costrict": true, "casdoor": true,
		"self": true, "null": true, "undefined": true, "none": true,
		"new": true, "edit": true, "delete": true, "create": true,
		"superuser": true, "superadmin": true, "everyone": true,
	}

	// Server-side error tokens — surface verbatim to the user-facing 4xx body.
	// (ErrInvalidDisplayName lives in rpc_client_platform_tenant.go.)
	ErrUsernameInvalid      = errors.New("invalid_username")
	ErrUsernameReserved     = errors.New("username_reserved")
	ErrUsernameTaken        = errors.New("username_taken")
	ErrRegistrationComplete = errors.New("registration_already_complete")
)

// ValidateUsername checks charset + length + reserved-words. Returns nil on
// success. Server has no tenant_id on users, so uniqueness is enforced by
// the unique index idx_user_username (global) and queried separately by
// UserService.IsUsernameAvailable.
func ValidateUsername(username string) error {
	u := strings.TrimSpace(username)
	if len(u) < UsernameMinLen || len(u) > UsernameMaxLen {
		return ErrUsernameInvalid
	}
	if !usernamePattern.MatchString(u) {
		return ErrUsernameInvalid
	}
	if reservedUsernames[strings.ToLower(u)] {
		return ErrUsernameReserved
	}
	return nil
}

// CompleteRegistration sets username + display_name and stamps
// profile_completed_at = NOW() for a first-time user. One-shot — calling
// again returns ErrRegistrationComplete so the handler can 409 cleanly.
// Username uniqueness is enforced by the unique index at the DB layer; the
// pre-check here gives a friendly "username_taken" 409 instead of relying on
// GORM's duplicate-key error string.
func (s *UserService) CompleteRegistration(ctx context.Context, subjectID, username, displayName string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if s.writeMode == WriteModeReadonly {
		return nil, ErrWriteBlocked
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

	var updated models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		ttx := tx.WithContext(ctx)
		if err := ttx.Where("subject_id = ?", subjectID).Take(&updated).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("user_not_found")
			}
			return err
		}
		if updated.ProfileCompletedAt != nil {
			return ErrRegistrationComplete
		}
		// Global-unique check (server has no tenant column). GORM's unique
		// index will catch races anyway, but the explicit check yields a
		// precise error token instead of a generic 500.
		var collision int64
		if err := ttx.Model(&models.User{}).
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
		if err := ttx.Model(&models.User{}).
			Where("subject_id = ?", subjectID).
			Updates(patches).Error; err != nil {
			return err
		}
		return ttx.Where("subject_id = ?", subjectID).Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	s.notifyUserUpdated(updated.SubjectID)
	return &updated, nil
}

// UpdateMyProfile applies user-self edits to display_name only. username is
// user-side immutable (REGISTRATION_PROFILE_DESIGN §3); admin overrides go
// through a separate RPC (R5).
func (s *UserService) UpdateMyProfile(ctx context.Context, subjectID, displayName string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if s.writeMode == WriteModeReadonly {
		return nil, ErrWriteBlocked
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil, errors.New("subject_id is required")
	}
	dn := strings.TrimSpace(displayName)
	if len(dn) > DisplayNameMaxLen {
		return nil, errors.New("invalid_display_name")
	}

	var updated models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		ttx := tx.WithContext(ctx)
		if err := ttx.Where("subject_id = ?", subjectID).Take(&updated).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("user_not_found")
			}
			return err
		}
		now := time.Now()
		patches := map[string]any{"updated_at": now}
		if dn != "" {
			patches["display_name"] = dn
		} else {
			patches["display_name"] = nil
		}
		if err := ttx.Model(&models.User{}).
			Where("subject_id = ?", subjectID).
			Updates(patches).Error; err != nil {
			return err
		}
		return ttx.Where("subject_id = ?", subjectID).Take(&updated).Error
	})
	if err != nil {
		return nil, err
	}
	s.notifyUserUpdated(updated.SubjectID)
	return &updated, nil
}

// IsUsernameAvailable reports whether username is free. excludeSubjectID
// lets the current user's own row not count as a collision.
func (s *UserService) IsUsernameAvailable(ctx context.Context, username, excludeSubjectID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("user.Service: nil db")
	}
	if err := ValidateUsername(username); err != nil {
		return false, err
	}
	username = strings.TrimSpace(username)
	var count int64
	q := s.db.WithContext(ctx).Model(&models.User{}).Where("username = ?", username)
	if excludeSubjectID != "" {
		q = q.Where("subject_id <> ?", excludeSubjectID)
	}
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

// IsProfileComplete reports whether the user has finished first-time
// registration (profile_completed_at IS NOT NULL). Backs the
// middleware.RequireProfileComplete gate. Returns true when the user is
// missing from the local mirror — the gate is best-effort and a missing
// row shouldn't lock a logged-in user out (server-side mirror lag during
// dual-write canary is the realistic trigger; cs-user is authoritative).
func (s *UserService) IsProfileComplete(subjectID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("user.Service: nil db")
	}
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return false, errors.New("subject_id is required")
	}
	var u models.User
	err := s.db.Where("subject_id = ?", subjectID).Take(&u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, nil // fail open
		}
		return false, err
	}
	return u.ProfileCompletedAt != nil, nil
}
