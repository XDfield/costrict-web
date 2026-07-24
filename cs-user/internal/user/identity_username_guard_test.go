package user

import (
	"context"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// TestRefreshUserProfile_NeverOverwritesUsername locks in the existing
// protection (REGISTRATION_PROFILE_DESIGN two-layer identity):
// refreshUserProfileFromIdentitiesTx computes an IdP-derived username
// candidate but the Save on line ~150 uses Omit("username"), so the DB
// value is never touched post-creation. This means a username set by
// /register/complete (R2) or an admin override (R5) sticks across logins
// regardless of profile_completed_at.
//
// This test is a regression lock: if someone removes the Omit, this test
// fails and forces them to think about the two-layer identity model.
func TestRefreshUserProfile_NeverOverwritesUsername(t *testing.T) {
	svc := newTestService(t)

	// Case 1: post-registration user (profile_completed_at set). Username
	// must stay as the user-chosen value.
	completedAt := time.Now().Add(-1 * time.Hour)
	completed := seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-completed"
		u.Username = "alice_custom"
		u.ProfileCompletedAt = &completedAt
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = completed.SubjectID
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:1"
		i.ProviderUserID = stringPtr("alice_idp_override")
		i.IsPrimary = true
	})

	// Case 2: pre-registration user (profile_completed_at NULL). Even here,
	// the existing Omit protection means username is NOT touched by the
	// IdP sync path — it stays whatever was set at creation (often empty).
	pending := seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-pending"
		u.Username = "alice_pending"
		// ProfileCompletedAt intentionally nil.
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = pending.SubjectID
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:2"
		i.ProviderUserID = stringPtr("alice_idp_would_overwrite")
		i.IsPrimary = true
	})

	if err := refreshUserProfileFromIdentitiesTx(context.Background(), svc.db, completed.SubjectID); err != nil {
		t.Fatalf("refresh completed: %v", err)
	}
	if err := refreshUserProfileFromIdentitiesTx(context.Background(), svc.db, pending.SubjectID); err != nil {
		t.Fatalf("refresh pending: %v", err)
	}

	var reloadedCompleted models.User
	if err := svc.db.Where("subject_id = ?", completed.SubjectID).Take(&reloadedCompleted).Error; err != nil {
		t.Fatalf("reload completed: %v", err)
	}
	if reloadedCompleted.Username != "alice_custom" {
		t.Errorf("completed Username: got %q, want alice_custom (Omit(username) must protect post-registration value)",
			reloadedCompleted.Username)
	}

	var reloadedPending models.User
	if err := svc.db.Where("subject_id = ?", pending.SubjectID).Take(&reloadedPending).Error; err != nil {
		t.Fatalf("reload pending: %v", err)
	}
	if reloadedPending.Username != "alice_pending" {
		t.Errorf("pending Username: got %q, want alice_pending (Omit(username) must protect any existing value)",
			reloadedPending.Username)
	}
}
