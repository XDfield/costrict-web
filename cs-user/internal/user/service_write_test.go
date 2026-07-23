//go:build cgo

package user

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// --- GetOrCreateUser ---

// TestGetOrCreateUser_NewUserCreatesRowAndPrimaryIdentity verifies the
// brand-new-user path: a single claim with no matching row results in both
// a user row AND a primary identity row bound to it.
func TestGetOrCreateUser_NewUserCreatesRowAndPrimaryIdentity(t *testing.T) {
	svc := newTestService(t)

	claims := &models.JWTClaims{
		ID:                "id-new",
		Sub:               "sub-new",
		UniversalID:       "uuid-new",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Provider:          "github",
		ProviderUserID:    "gh-1",
	}
	user, _, err := svc.GetOrCreateUser(context.Background(), claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if !strings.HasPrefix(user.SubjectID, "usr_") {
		t.Errorf("SubjectID: got %q, want usr_ prefix", user.SubjectID)
	}
	if user.Username != "alice" {
		t.Errorf("Username: got %q, want alice", user.Username)
	}
	if user.ExternalKey == nil || *user.ExternalKey != "casdoor:github:uuid-new" {
		t.Errorf("ExternalKey: got %v, want casdoor:github:uuid-new", user.ExternalKey)
	}
	if user.AuthProvider == nil || *user.AuthProvider != "github" {
		t.Errorf("AuthProvider: got %v, want github", user.AuthProvider)
	}
	if !user.IsActive {
		t.Errorf("IsActive should be true on new user")
	}

	// Verify identity row exists with IsPrimary=true.
	identities, err := svc.ListIdentities(context.Background(), user.SubjectID)
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("got %d identities, want 1", len(identities))
	}
	if !identities[0].IsPrimary {
		t.Errorf("first identity should be primary on new user")
	}
	if identities[0].Provider != "github" {
		t.Errorf("identity Provider: got %q, want github", identities[0].Provider)
	}
}

// TestGetOrCreateUser_IdempotentWithinSyncInterval verifies a second call
// with the same claim inside syncInterval is a no-op (no error, no spurious
// updates). Mirrors server:657.
func TestGetOrCreateUser_IdempotentWithinSyncInterval(t *testing.T) {
	svc := newTestService(t)

	claims := &models.JWTClaims{
		ID:                "id-idem",
		Sub:               "sub-idem",
		UniversalID:       "uuid-idem",
		Name:              "alice",
		PreferredUsername: "alice",
		Provider:          "github",
	}
	first, _, err := svc.GetOrCreateUser(context.Background(), claims)
	if err != nil {
		t.Fatalf("first GetOrCreateUser: %v", err)
	}

	// Bump UpdatedAt into the past so we can detect a no-op vs a real write.
	past := time.Now().Add(-1 * time.Hour)
	if err := svc.db.Model(&models.User{}).Where("subject_id = ?", first.SubjectID).
		Update("updated_at", past).Error; err != nil {
		t.Fatalf("seed updated_at: %v", err)
	}

	if _, _, err := svc.GetOrCreateUser(context.Background(), claims); err != nil {
		t.Fatalf("second GetOrCreateUser: %v", err)
	}

	var reloaded models.User
	if err := svc.db.Where("subject_id = ?", first.SubjectID).Take(&reloaded).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.UpdatedAt.Equal(past) {
		t.Errorf("second call should be a no-op inside syncInterval: updated_at moved from %v to %v",
			past, reloaded.UpdatedAt)
	}
}

// TestGetOrCreateUser_ResyncsAfterInterval verifies that after syncInterval
// elapses, a re-call updates the denormalized fields again.
func TestGetOrCreateUser_ResyncsAfterInterval(t *testing.T) {
	svc := newTestService(t)

	claims := &models.JWTClaims{
		ID:                "id-resync",
		Sub:               "sub-resync",
		UniversalID:       "uuid-resync",
		Name:              "alice",
		PreferredUsername: "alice",
		Provider:          "github",
	}
	first, _, err := svc.GetOrCreateUser(context.Background(), claims)
	if err != nil {
		t.Fatalf("first GetOrCreateUser: %v", err)
	}

	// Push LastSyncAt back beyond syncInterval so the next call should write.
	oldSync := time.Now().Add(-(syncInterval + time.Minute))
	if err := svc.db.Model(&models.User{}).Where("subject_id = ?", first.SubjectID).
		Update("last_sync_at", oldSync).Error; err != nil {
		t.Fatalf("seed last_sync_at: %v", err)
	}

	// Change the claim's email. After REGISTRATION_PROFILE_DESIGN the
	// re-login refresh path no longer touches user-owned profile fields, so
	// the second GetOrCreateUser must NOT clobber the (empty) email.
	claims.Email = "alice-new@example.com"
	if _, _, err := svc.GetOrCreateUser(context.Background(), claims); err != nil {
		t.Fatalf("second GetOrCreateUser: %v", err)
	}

	var reloaded models.User
	if err := svc.db.Where("subject_id = ?", first.SubjectID).Take(&reloaded).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Email != nil {
		t.Errorf("Email must NOT be auto-synced from JWT (user-owned field): got %v", *reloaded.Email)
	}
}

// TestGetOrCreateUser_FoundByLegacyExternalKey verifies the lookup chain
// falls back through legacyExternalKey (pre-provider-keyed format) when no
// row matches the modern key.
func TestGetOrCreateUser_FoundByLegacyExternalKey(t *testing.T) {
	svc := newTestService(t)

	// Seed a user + identity using the legacy format (no provider segment).
	legacy := seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-legacy"
		u.Username = "legacy-user"
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-legacy"
		i.Provider = "casdoor" // unknown provider → legacy key format
		i.ExternalKey = "casdoor:uuid-legacy"
	})

	// Modern claim with provider "github" should match via legacy key.
	claims := &models.JWTClaims{
		UniversalID: "uuid-legacy",
		Name:        "legacy-user",
		Provider:    "github",
	}
	got, _, err := svc.GetOrCreateUser(context.Background(), claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if got.SubjectID != "subj-legacy" {
		t.Errorf("SubjectID: got %q, want subj-legacy (matched via legacy key)", got.SubjectID)
	}
	_ = legacy
}

// TestGetOrCreateUser_NilClaimsRejected verifies the nil-claim guard.
func TestGetOrCreateUser_NilClaimsRejected(t *testing.T) {
	svc := newTestService(t)
	if _, _, err := svc.GetOrCreateUser(context.Background(), nil); err == nil {
		t.Fatal("nil claims should be rejected")
	}
}

// TestGetOrCreateUser_NoIdentifierRejected verifies that a claim with no
// usable identifier (no ID / Sub / UniversalID) is rejected.
func TestGetOrCreateUser_NoIdentifierRejected(t *testing.T) {
	svc := newTestService(t)
	claims := &models.JWTClaims{Name: "name-only"}
	if _, _, err := svc.GetOrCreateUser(context.Background(), claims); err == nil {
		t.Fatal("expected error for claim with no identifier")
	}
}

// --- BindIdentityToUser ---

// TestBindIdentityToUser_NewIdentityOnExistingUser verifies the happy path:
// a user with one identity gets a second identity bound.
func TestBindIdentityToUser_NewIdentityOnExistingUser(t *testing.T) {
	svc := newTestService(t)
	user := seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-bind"
		u.Username = "bind-user"
	})
	// Seed one identity so we have something to add to.
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-bind"
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:bind-gh"
		i.IsPrimary = true
	})

	claims := &models.JWTClaims{
		UniversalID: "uuid-phone-bind",
		Provider:    "phone",
		Phone:       "+8613800000000",
	}
	if err := svc.BindIdentityToUser(context.Background(), "subj-bind", claims); err != nil {
		t.Fatalf("BindIdentityToUser: %v", err)
	}

	identities, err := svc.ListIdentities(context.Background(), "subj-bind")
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("got %d identities, want 2", len(identities))
	}
	_ = user
}

// TestBindIdentityToUser_Idempotent verifies that binding the same identity
// twice is a no-op (no error, no duplicate row).
func TestBindIdentityToUser_Idempotent(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-idem-bind"
		u.Username = "idem-bind"
	})

	claims := &models.JWTClaims{
		UniversalID: "uuid-idem-bind",
		Provider:    "github",
	}
	if err := svc.BindIdentityToUser(context.Background(), "subj-idem-bind", claims); err != nil {
		t.Fatalf("first BindIdentityToUser: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), "subj-idem-bind", claims); err != nil {
		t.Fatalf("second BindIdentityToUser: %v", err)
	}

	identities, err := svc.ListIdentities(context.Background(), "subj-idem-bind")
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(identities) != 1 {
		t.Errorf("got %d identities, want 1 (idempotent bind)", len(identities))
	}
}

// TestBindIdentityToUser_SoftDeleteRecovery verifies a soft-deleted identity
// is restored (un-deleted) when re-bound, rather than creating a duplicate.
// Preserves audit history and prevents ExternalKey unique-index conflicts.
func TestBindIdentityToUser_SoftDeleteRecovery(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-softdel"
		u.Username = "softdel"
	})

	claims := &models.JWTClaims{
		UniversalID: "uuid-softdel",
		Provider:    "github",
	}
	// First bind creates the identity.
	if err := svc.BindIdentityToUser(context.Background(), "subj-softdel", claims); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	// Soft-delete it manually.
	if err := svc.db.Where("external_key = ?", "casdoor:github:uuid-softdel").
		Delete(&models.UserAuthIdentity{}).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Second bind should restore, not duplicate.
	if err := svc.BindIdentityToUser(context.Background(), "subj-softdel", claims); err != nil {
		t.Fatalf("second bind: %v", err)
	}

	var count int64
	svc.db.Unscoped().Model(&models.UserAuthIdentity{}).
		Where("external_key = ?", "casdoor:github:uuid-softdel").Count(&count)
	if count != 1 {
		t.Errorf("soft-delete recovery: got %d rows, want 1 (no duplicate)", count)
	}

	// And the surviving row should be un-deleted.
	var restored models.UserAuthIdentity
	if err := svc.db.Where("external_key = ?", "casdoor:github:uuid-softdel").Take(&restored).Error; err != nil {
		t.Errorf("restored row should be visible without Unscoped: %v", err)
	}
}

// TestBindIdentityToUser_ExplicitlyUnboundWithoutForceRebind verifies the
// ExplicitlyUnbound marker blocks re-bind unless ForceRebind is set.
// Server silently returns nil in this case; cs-user surfaces a sentinel so
// RPCWriter can distinguish skipped vs succeeded.
func TestBindIdentityToUser_ExplicitlyUnboundWithoutForceRebind(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-unbound"
		u.Username = "unbound"
	})

	claims := &models.JWTClaims{
		UniversalID: "uuid-unbound",
		Provider:    "github",
	}
	// Initial bind.
	if err := svc.BindIdentityToUser(context.Background(), "subj-unbound", claims); err != nil {
		t.Fatalf("initial bind: %v", err)
	}
	// Mark as ExplicitlyUnbound (simulating an unbind).
	if err := svc.db.Model(&models.UserAuthIdentity{}).
		Where("external_key = ?", "casdoor:github:uuid-unbound").
		Update("explicitly_unbound", true).Error; err != nil {
		t.Fatalf("mark explicitly unbound: %v", err)
	}

	// Re-bind without ForceRebind — should be refused.
	err := svc.BindIdentityToUser(context.Background(), "subj-unbound", claims)
	if !errors.Is(err, ErrExplicitlyUnbound) {
		t.Fatalf("got %v, want ErrExplicitlyUnbound", err)
	}

	// Re-bind with ForceRebind should succeed.
	if err := svc.BindIdentityToUser(context.Background(), "subj-unbound", claims,
		models.BindIdentityOptions{ForceRebind: true}); err != nil {
		t.Fatalf("ForceRebind: %v", err)
	}
}

// TestBindIdentityToUser_PrimaryCascadePromotesHigherRank verifies that
// binding a higher-rank identity (idtrust) promotes it to primary over the
// existing lower-rank primary (phone).
func TestBindIdentityToUser_PrimaryCascadePromotesHigherRank(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-cascade"
		u.Username = "cascade"
	})
	// Seed a phone primary.
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-cascade"
		i.Provider = "phone"
		i.ExternalKey = "casdoor:phone:cascade"
		i.IsPrimary = true
	})

	// Bind idtrust (rank 300 > phone rank 100).
	claims := &models.JWTClaims{
		UniversalID: "uuid-idtrust",
		Provider:    "idtrust",
	}
	if err := svc.BindIdentityToUser(context.Background(), "subj-cascade", claims); err != nil {
		t.Fatalf("BindIdentityToUser idtrust: %v", err)
	}

	identities, err := svc.ListIdentities(context.Background(), "subj-cascade")
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	for _, id := range identities {
		if id.Provider == "idtrust" && !id.IsPrimary {
			t.Errorf("idtrust should be primary after cascade")
		}
		if id.Provider == "phone" && id.IsPrimary {
			t.Errorf("phone should NOT be primary after cascade")
		}
	}
}

// TestBindIdentityToUser_AlreadyBoundToOtherUser verifies that binding an
// identity already owned by another (non-deleted) user is refused.
func TestBindIdentityToUser_AlreadyBoundToOtherUser(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-owner1"
		u.Username = "owner1"
	})
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-owner2"
		u.Username = "owner2"
	})

	claims := &models.JWTClaims{
		UniversalID: "uuid-shared",
		Provider:    "github",
	}
	// Bind to first user.
	if err := svc.BindIdentityToUser(context.Background(), "subj-owner1", claims); err != nil {
		t.Fatalf("initial bind: %v", err)
	}
	// Attempt to bind same identity to second user.
	err := svc.BindIdentityToUser(context.Background(), "subj-owner2", claims)
	if !errors.Is(err, ErrIdentityAlreadyBound) {
		t.Fatalf("got %v, want ErrIdentityAlreadyBound", err)
	}
}

// --- TransferIdentityToUser ---

// TestTransferIdentityToUser_HappyPath verifies an identity moves from one
// user to another cleanly.
func TestTransferIdentityToUser_HappyPath(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-from"
		u.Username = "from-user"
	})
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-to"
		u.Username = "to-user"
	})

	externalKey := "casdoor:github:xfer"
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-from"
		i.Provider = "github"
		i.ExternalKey = externalKey
		i.IsPrimary = true
	})

	if err := svc.TransferIdentityToUser(context.Background(), "subj-to", externalKey, ""); err != nil {
		t.Fatalf("TransferIdentityToUser: %v", err)
	}

	// Identity should now belong to subj-to.
	var moved models.UserAuthIdentity
	if err := svc.db.Where("external_key = ?", externalKey).Take(&moved).Error; err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	if moved.UserSubjectID != "subj-to" {
		t.Errorf("UserSubjectID: got %q, want subj-to", moved.UserSubjectID)
	}
}

// TestTransferIdentityToUser_AlreadyOwnedIsNoOp verifies transferring to the
// same user that already owns the identity is a no-op (no error).
func TestTransferIdentityToUser_AlreadyOwnedIsNoOp(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-same"
		u.Username = "same-user"
	})

	externalKey := "casdoor:github:same"
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-same"
		i.Provider = "github"
		i.ExternalKey = externalKey
	})

	if err := svc.TransferIdentityToUser(context.Background(), "subj-same", externalKey, ""); err != nil {
		t.Fatalf("TransferIdentityToUser to same owner: %v", err)
	}
}

// TestTransferIdentityToUser_NotFound verifies missing identities bubble as
// "identity_not_found".
func TestTransferIdentityToUser_NotFound(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-xfer-target"
		u.Username = "xfer-target"
	})

	err := svc.TransferIdentityToUser(context.Background(), "subj-xfer-target", "casdoor:github:missing", "")
	if err == nil || !strings.Contains(err.Error(), "identity_not_found") {
		t.Fatalf("got %v, want identity_not_found", err)
	}
}

// TestTransferIdentityToUser_RequiredArgs verifies the empty-arg guard.
func TestTransferIdentityToUser_RequiredArgs(t *testing.T) {
	svc := newTestService(t)
	if err := svc.TransferIdentityToUser(context.Background(), "", "casdoor:github:x", ""); err == nil {
		t.Fatal("empty target should be rejected")
	}
	if err := svc.TransferIdentityToUser(context.Background(), "subj-x", "", ""); err == nil {
		t.Fatal("empty externalKey should be rejected")
	}
}

// --- UnbindIdentityByProvider ---

// TestUnbindIdentityByProvider_HappyPath verifies a successful unbind marks
// identities as ExplicitlyUnbound + soft-deleted.
func TestUnbindIdentityByProvider_HappyPath(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-unbind"
		u.Username = "unbind-user"
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-unbind"
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:unbind"
		i.IsPrimary = true
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-unbind"
		i.Provider = "phone"
		i.ExternalKey = "casdoor:phone:unbind"
		i.IsPrimary = false
	})

	if err := svc.UnbindIdentityByProvider(context.Background(), "subj-unbind", "github"); err != nil {
		t.Fatalf("UnbindIdentityByProvider: %v", err)
	}

	// Github identity should be soft-deleted.
	var githubCount int64
	svc.db.Model(&models.UserAuthIdentity{}).
		Where("user_subject_id = ? AND provider = ?", "subj-unbind", "github").Count(&githubCount)
	if githubCount != 0 {
		t.Errorf("github should be soft-deleted; got %d visible rows", githubCount)
	}

	// And marked explicitly unbound (via Unscoped).
	var marked models.UserAuthIdentity
	if err := svc.db.Unscoped().
		Where("external_key = ?", "casdoor:github:unbind").Take(&marked).Error; err != nil {
		t.Fatalf("reload github: %v", err)
	}
	if !marked.ExplicitlyUnbound {
		t.Errorf("github should be marked explicitly unbound")
	}

	// Phone should now be primary (promoted by cascade).
	var phone models.UserAuthIdentity
	if err := svc.db.Where("external_key = ?", "casdoor:phone:unbind").Take(&phone).Error; err != nil {
		t.Fatalf("reload phone: %v", err)
	}
	if !phone.IsPrimary {
		t.Errorf("phone should be promoted to primary after github unbind")
	}
}

// TestUnbindIdentityByProvider_LastIdentityRefused verifies the
// last-identity invariant — unbinding the only identity is refused.
func TestUnbindIdentityByProvider_LastIdentityRefused(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-last"
		u.Username = "last-user"
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-last"
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:last"
		i.IsPrimary = true
	})

	err := svc.UnbindIdentityByProvider(context.Background(), "subj-last", "github")
	if !errors.Is(err, ErrLastIdentity) {
		t.Fatalf("got %v, want ErrLastIdentity", err)
	}
}

// TestUnbindIdentityByProvider_NotFound verifies unbinding a provider that
// has no identities on the user is rejected.
func TestUnbindIdentityByProvider_NotFound(t *testing.T) {
	svc := newTestService(t)
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-no-provider"
		u.Username = "no-provider-user"
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-no-provider"
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:no-provider"
	})

	err := svc.UnbindIdentityByProvider(context.Background(), "subj-no-provider", "phone")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, want 'not found'", err)
	}
}

// TestUnbindIdentityByProvider_EmptyProviderRejected verifies the arg guard.
func TestUnbindIdentityByProvider_EmptyProviderRejected(t *testing.T) {
	svc := newTestService(t)
	if err := svc.UnbindIdentityByProvider(context.Background(), "subj-x", ""); err == nil {
		t.Fatal("empty provider should be rejected")
	}
	if err := svc.UnbindIdentityByProvider(context.Background(), "subj-x", "   "); err == nil {
		t.Fatal("whitespace provider should be rejected")
	}
}

// --- Nil-db guards for write methods ---

// TestService_NilDBGuards_WriteMethods verifies the write methods also
// short-circuit on a nil-db service (defensive against future wiring
// mistakes).
func TestService_NilDBGuards_WriteMethods(t *testing.T) {
	svc := &Service{}
	ctx := context.Background()
	claims := &models.JWTClaims{UniversalID: "uuid-x", Provider: "github"}

	if _, _, err := svc.GetOrCreateUser(ctx, claims); err == nil {
		t.Error("GetOrCreateUser on nil db should error")
	}
	if err := svc.BindIdentityToUser(ctx, "subj-x", claims); err == nil {
		t.Error("BindIdentityToUser on nil db should error")
	}
	if err := svc.TransferIdentityToUser(ctx, "subj-x", "key", ""); err == nil {
		t.Error("TransferIdentityToUser on nil db should error")
	}
	if err := svc.UnbindIdentityByProvider(ctx, "subj-x", "github"); err == nil {
		t.Error("UnbindIdentityByProvider on nil db should error")
	}
}

// Compile-time: ensure we use gorm.ErrRecordNotFound reference (avoids
// unused import if tests shrink later).
var _ = gorm.ErrRecordNotFound
