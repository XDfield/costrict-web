//go:build cgo

package user

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// capturingProvisioner is a test stub for the GiteaProvisioner interface.
// It records every Provision call + lets the test configure the returned
// error.
type capturingProvisioner struct {
	calls  int
	last   *models.User
	retErr error
}

func (p *capturingProvisioner) Provision(_ context.Context, u *models.User) error {
	p.calls++
	p.last = u
	return p.retErr
}

// newTestServiceWithGitea opens a fresh in-memory sqlite + migrates
// UserGiteaBinding (alongside User + UserAuthIdentity) and wires the
// supplied GiteaProvisioner. Mirrors newTestService but isolates the
// extra migration so the 30+ existing tests don't pay for the binding
// table they don't use.
func newTestServiceWithGitea(t *testing.T, prov GiteaProvisioner) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}, &models.UserGiteaBinding{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	s := NewService(db)
	s.SetGiteaSync(prov)
	return s
}

// TestGetOrCreateUser_FiresGiteaProvisioningOnNewUser verifies the E3a.1
// hook: a brand-new-user signup calls giteaSync.Provision exactly once with
// the freshly created user.
func TestGetOrCreateUser_FiresGiteaProvisioningOnNewUser(t *testing.T) {
	stub := &capturingProvisioner{}
	svc := newTestServiceWithGitea(t, stub)

	claims := &models.JWTClaims{
		ID:                "id-gitea-new",
		Sub:               "sub-gitea-new",
		UniversalID:       "uuid-gitea-new",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "github",
		ProviderUserID:    "gh-1",
	}
	if _, err := svc.GetOrCreateUser(context.Background(), claims); err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("provisioner calls: got %d, want 1", stub.calls)
	}
	if stub.last == nil {
		t.Fatal("captured user is nil")
	}
	if !strings.HasPrefix(stub.last.SubjectID, "usr_") {
		t.Errorf("captured SubjectID: got %q, want usr_ prefix", stub.last.SubjectID)
	}
	if stub.last.Username != "alice" {
		t.Errorf("captured Username: got %q, want alice", stub.last.Username)
	}
}

// TestGetOrCreateUser_DoesNotFireGiteaOnExistingUser verifies the hook is
// gated to the new-user branch only — a resync / idempotent call must not
// trigger duplicate provisioning (giteasync.Service.Provision is itself
// idempotent on synced bindings, but skipping the call entirely is cheaper
// and matches the ADR-3 v3 "user-level provisioning happens once" intent).
func TestGetOrCreateUser_DoesNotFireGiteaOnExistingUser(t *testing.T) {
	stub := &capturingProvisioner{}
	svc := newTestServiceWithGitea(t, stub)

	claims := &models.JWTClaims{
		ID:                "id-existing",
		Sub:               "sub-existing",
		UniversalID:       "uuid-existing",
		Name:              "bob",
		PreferredUsername: "bob",
		Email:             "bob@example.com",
		Provider:          "github",
		ProviderUserID:    "gh-2",
	}
	if _, err := svc.GetOrCreateUser(context.Background(), claims); err != nil {
		t.Fatalf("first GetOrCreateUser: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("after first call: provisioner calls=%d, want 1", stub.calls)
	}

	// Second call within syncInterval is a no-op path; hook should not fire.
	if _, err := svc.GetOrCreateUser(context.Background(), claims); err != nil {
		t.Fatalf("second GetOrCreateUser: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("after second (existing-user) call: provisioner calls=%d, want still 1", stub.calls)
	}
}

// TestGetOrCreateUser_GiteaProvisionFailureDoesNotAbortUserCreation
// verifies the best-effort contract: even if the provisioner returns an
// error, GetOrCreateUser must succeed (the users row is already committed;
// Gitea outage is not a signup failure).
func TestGetOrCreateUser_GiteaProvisionFailureDoesNotAbortUserCreation(t *testing.T) {
	stub := &capturingProvisioner{retErr: errors.New("simulated gitea outage")}
	svc := newTestServiceWithGitea(t, stub)

	claims := &models.JWTClaims{
		ID:                "id-fail",
		Sub:               "sub-fail",
		UniversalID:       "uuid-fail",
		Name:              "carol",
		PreferredUsername: "carol",
		Email:             "carol@example.com",
		Provider:          "github",
		ProviderUserID:    "gh-3",
	}
	user, err := svc.GetOrCreateUser(context.Background(), claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser must not fail when provisioner errors: %v", err)
	}
	if user == nil || user.SubjectID == "" {
		t.Errorf("returned user: got %+v, want non-nil with SubjectID", user)
	}
	if stub.calls != 1 {
		t.Errorf("provisioner calls: got %d, want 1 (hook still fires; error is ignored)", stub.calls)
	}
}

// TestGetOrCreateUser_NilProvisionerIsSkipped verifies the feature-off
// path: when SetGiteaSync was never called (or called with nil), the hook
// is a no-op and signup works normally.
func TestGetOrCreateUser_NilProvisionerIsSkipped(t *testing.T) {
	// Plain NewService — no SetGiteaSync call.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	svc := NewService(db)

	claims := &models.JWTClaims{
		ID:                "id-nil-prov",
		Sub:               "sub-nil-prov",
		UniversalID:       "uuid-nil-prov",
		Name:              "dave",
		PreferredUsername: "dave",
		Email:             "dave@example.com",
		Provider:          "github",
		ProviderUserID:    "gh-4",
	}
	if _, err := svc.GetOrCreateUser(context.Background(), claims); err != nil {
		t.Fatalf("GetOrCreateUser with nil provisioner: %v", err)
	}
}

// TestService_GetGiteaBinding covers the read-side method powering the new
// GET /api/internal/users/:subject_id/gitea-binding endpoint.
func TestService_GetGiteaBinding(t *testing.T) {
	stub := &capturingProvisioner{}
	svc := newTestServiceWithGitea(t, stub)
	db := svc.db

	// Seed a synced binding row directly.
	now := time.Now()
	uid := int64(42)
	binding := &models.UserGiteaBinding{
		UserSubjectID: "usr_lookup",
		TenantID:      "default",
		GiteaUID:      &uid,
		GiteaUsername: "u-lookup",
		SyncStatus:    models.GiteaSyncStatusSynced,
		LastSyncedAt:  &now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := db.Create(binding).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	got, err := svc.GetGiteaBinding(context.Background(), "usr_lookup")
	if err != nil {
		t.Fatalf("GetGiteaBinding: %v", err)
	}
	if got.GiteaUsername != "u-lookup" {
		t.Errorf("GiteaUsername: got %q, want u-lookup", got.GiteaUsername)
	}
	if got.GiteaUID == nil || *got.GiteaUID != 42 {
		t.Errorf("GiteaUID: got %v, want 42", got.GiteaUID)
	}
}

// TestService_GetGiteaBinding_NotFound verifies the missing-binding case
// surfaces as gorm.ErrRecordNotFound (handler maps to 404).
func TestService_GetGiteaBinding_NotFound(t *testing.T) {
	svc := newTestServiceWithGitea(t, &capturingProvisioner{})
	_, err := svc.GetGiteaBinding(context.Background(), "usr_ghost")
	if err == nil {
		t.Fatal("GetGiteaBinding: got nil err, want non-nil")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("err: got %v, want gorm.ErrRecordNotFound", err)
	}
}

// TestService_GetGiteaBinding_EmptySubjectID verifies the input guard.
func TestService_GetGiteaBinding_EmptySubjectID(t *testing.T) {
	svc := newTestServiceWithGitea(t, &capturingProvisioner{})
	_, err := svc.GetGiteaBinding(context.Background(), "")
	if !errors.Is(err, ErrEmptySubjectID) {
		t.Errorf("err: got %v, want ErrEmptySubjectID", err)
	}
}
