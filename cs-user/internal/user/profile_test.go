//go:build cgo

package user

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// inDefaultTenant is a seedUser opt that pins TenantID to the default so
// tenant.Scope (which falls back to "default" on an empty ctx) matches.
func inDefaultTenant(m *models.User) { m.TenantID = "default" }

func TestCompleteRegistration_Success(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)

	got, err := svc.CompleteRegistration(context.Background(), u.SubjectID, "newname", "Alice")
	if err != nil {
		t.Fatalf("CompleteRegistration: %v", err)
	}
	if got.Username != "newname" {
		t.Errorf("Username: got %q want newname", got.Username)
	}
	if got.DisplayName == nil || *got.DisplayName != "Alice" {
		t.Errorf("DisplayName: got %v want Alice", got.DisplayName)
	}
	if got.ProfileCompletedAt == nil {
		t.Errorf("ProfileCompletedAt should be set")
	}
}

func TestCompleteRegistration_AlreadyComplete(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)

	if _, err := svc.CompleteRegistration(context.Background(), u.SubjectID, "first", ""); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := svc.CompleteRegistration(context.Background(), u.SubjectID, "second", "")
	if !errors.Is(err, ErrRegistrationAlreadyComplete) {
		t.Errorf("err: got %v want ErrRegistrationAlreadyComplete", err)
	}
}

func TestCompleteRegistration_UsernameTaken(t *testing.T) {
	svc := newTestService(t)
	u1 := seedUser(t, svc, inDefaultTenant)
	u2 := seedUser(t, svc, inDefaultTenant)

	if _, err := svc.CompleteRegistration(context.Background(), u1.SubjectID, "shared", ""); err != nil {
		t.Fatalf("u1: %v", err)
	}
	_, err := svc.CompleteRegistration(context.Background(), u2.SubjectID, "shared", "")
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("u2 err: got %v want ErrUsernameTaken", err)
	}
}

func TestUpdateProfile_DisplayNameClearsToNull(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)
	dn := "Bob"
	if _, err := svc.UpdateProfile(context.Background(), u.SubjectID, dn); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := svc.UpdateProfile(context.Background(), u.SubjectID, "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got.DisplayName != nil {
		t.Errorf("DisplayName: got %v want nil", *got.DisplayName)
	}
}

// --- R5: AdminUpdateProfile ---

func TestAdminUpdateProfile_RenamesUsername(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)

	got, err := svc.AdminUpdateProfile(context.Background(), u.SubjectID, "admin_set", nil, "operator-1")
	if err != nil {
		t.Fatalf("AdminUpdateProfile: %v", err)
	}
	if got.Username != "admin_set" {
		t.Errorf("Username: got %q want admin_set", got.Username)
	}
}

func TestAdminUpdateProfile_BypassesRegistrationGate(t *testing.T) {
	// User has ProfileCompletedAt already set (i.e. finished registration).
	// The user-self path can't touch username; admin can rename regardless.
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)
	if _, err := svc.CompleteRegistration(context.Background(), u.SubjectID, "first", ""); err != nil {
		t.Fatalf("seed complete: %v", err)
	}

	got, err := svc.AdminUpdateProfile(context.Background(), u.SubjectID, "renamed", nil, "operator-1")
	if err != nil {
		t.Fatalf("AdminUpdateProfile after registration: %v", err)
	}
	if got.Username != "renamed" {
		t.Errorf("Username: got %q want renamed", got.Username)
	}
}

func TestAdminUpdateProfile_UsernameTakenSameTenant(t *testing.T) {
	svc := newTestService(t)
	u1 := seedUser(t, svc, inDefaultTenant)
	u2 := seedUser(t, svc, inDefaultTenant)

	if _, err := svc.AdminUpdateProfile(context.Background(), u1.SubjectID, "claimed", nil, "op"); err != nil {
		t.Fatalf("u1 set: %v", err)
	}
	_, err := svc.AdminUpdateProfile(context.Background(), u2.SubjectID, "claimed", nil, "op")
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("u2 err: got %v want ErrUsernameTaken", err)
	}
}

func TestAdminUpdateProfile_NoFieldsToUpdate(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)

	_, err := svc.AdminUpdateProfile(context.Background(), u.SubjectID, "", nil, "op")
	if err == nil || !strings.Contains(err.Error(), "no fields to update") {
		t.Errorf("err: got %v want no fields to update", err)
	}
}

func TestAdminUpdateProfile_InvalidUsername(t *testing.T) {
	svc := newTestService(t)
	u := seedUser(t, svc, inDefaultTenant)

	_, err := svc.AdminUpdateProfile(context.Background(), u.SubjectID, "bad name!", nil, "op")
	if !errors.Is(err, ErrUsernameInvalid) {
		t.Errorf("err: got %v want ErrUsernameInvalid", err)
	}
}
