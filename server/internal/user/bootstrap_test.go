package user

import (
	"errors"
	"sync"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
)

// fakeGranter records GrantRole calls and can be configured to return an error.
type fakeGranter struct {
	mu    sync.Mutex
	calls []grantCall
	err   error
	// granted simulates the idempotent backing store: a (userID, role) that has
	// already been granted returns nil without recording a new effective grant.
	granted map[string]struct{}
}

type grantCall struct {
	userID     string
	role       string
	operatorID string
}

func newFakeGranter() *fakeGranter {
	return &fakeGranter{granted: map[string]struct{}{}}
}

func (f *fakeGranter) GrantRole(userID, role, operatorID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, grantCall{userID: userID, role: role, operatorID: operatorID})
	if f.err != nil {
		return f.err
	}
	// idempotent no-op when already present (mirrors systemrole.GrantRole)
	if _, ok := f.granted[userID+"|"+role]; ok {
		return nil
	}
	f.granted[userID+"|"+role] = struct{}{}
	return nil
}

func (f *fakeGranter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func userWithUniversalID(subjectID, universalID string) *models.User {
	id := universalID
	return &models.User{SubjectID: subjectID, CasdoorUniversalID: &id}
}

func TestBootstrap_UniversalIDHit_Grants(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"uuid-admin-1"})

	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 1 {
		t.Fatalf("expected 1 grant call, got %d", g.callCount())
	}
	call := g.calls[0]
	if call.userID != "usr_1" {
		t.Fatalf("userID = %q, want usr_1", call.userID)
	}
	if call.role != platformAdminRole {
		t.Fatalf("role = %q, want %q", call.role, platformAdminRole)
	}
	if call.operatorID != bootstrapGrantedBy {
		t.Fatalf("operatorID = %q, want %q", call.operatorID, bootstrapGrantedBy)
	}
}

func TestBootstrap_CaseSensitive_NoGrantOnCaseMismatch(t *testing.T) {
	// universal_id is case-sensitive: a case mismatch must NOT match (unlike the
	// old email-based, case-insensitive allowlist).
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"UUID-Admin-1"})

	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 0 {
		t.Fatalf("expected case-sensitive mismatch to NOT grant, got %d calls", g.callCount())
	}
}

func TestBootstrap_TrimsWhitespace(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"  uuid-admin-1  "})

	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 1 {
		t.Fatalf("expected whitespace-trimmed match to grant once, got %d calls", g.callCount())
	}
}

func TestBootstrap_AlreadyGranted_Idempotent(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"uuid-admin-1"})

	// Two logins: both call GrantRole (config is source of truth, checked every
	// login), but the backing store stays at a single effective grant.
	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))
	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 2 {
		t.Fatalf("expected GrantRole invoked on every login (2), got %d", g.callCount())
	}
	if len(g.granted) != 1 {
		t.Fatalf("expected a single effective grant (idempotent), got %d", len(g.granted))
	}
}

func TestBootstrap_NotInList_NoGrant(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"uuid-admin-1"})

	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-someone-else"))

	if g.callCount() != 0 {
		t.Fatalf("expected no grant for non-listed universal_id, got %d calls", g.callCount())
	}
}

func TestBootstrap_EmptyConfig_NoOp(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, nil)

	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 0 {
		t.Fatalf("expected empty allowlist to be a no-op, got %d calls", g.callCount())
	}
}

func TestBootstrap_BlankOnlyConfig_NoOp(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"", "   "})

	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 0 {
		t.Fatalf("expected blank-only allowlist to be a no-op, got %d calls", g.callCount())
	}
}

func TestBootstrap_NilOrEmptyUniversalID_NoGrant(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"uuid-admin-1"})

	// nil universal_id (e.g. claim missing) must be nil-safe, not panic.
	b.ApplyOnLogin(&models.User{SubjectID: "usr_1"})
	// empty universal_id pointer
	empty := ""
	b.ApplyOnLogin(&models.User{SubjectID: "usr_2", CasdoorUniversalID: &empty})

	if g.callCount() != 0 {
		t.Fatalf("expected no grant for users without universal_id, got %d calls", g.callCount())
	}
}

func TestBootstrap_EmptySubjectID_NoGrant(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"uuid-admin-1"})

	b.ApplyOnLogin(userWithUniversalID("", "uuid-admin-1"))

	if g.callCount() != 0 {
		t.Fatalf("expected no grant when subjectID is empty, got %d calls", g.callCount())
	}
}

func TestBootstrap_GrantFailure_DoesNotPanicOrBlock(t *testing.T) {
	g := newFakeGranter()
	g.err = errors.New("db down")
	b := NewBootstrapAdminGranter(g, []string{"uuid-admin-1"})

	// Must not panic; ApplyOnLogin returns nothing and swallows the error.
	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))

	if g.callCount() != 1 {
		t.Fatalf("expected the grant to be attempted once, got %d calls", g.callCount())
	}
}

func TestBootstrap_NilGranter_NoOp(t *testing.T) {
	b := NewBootstrapAdminGranter(nil, []string{"uuid-admin-1"})
	// Must not panic.
	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))
}

func TestBootstrap_NilReceiver_NoOp(t *testing.T) {
	var b *BootstrapAdminGranter
	// Must not panic.
	b.ApplyOnLogin(userWithUniversalID("usr_1", "uuid-admin-1"))
}

func TestBootstrap_NilUser_NoOp(t *testing.T) {
	g := newFakeGranter()
	b := NewBootstrapAdminGranter(g, []string{"admin@example.com"})
	b.ApplyOnLogin(nil)
	if g.callCount() != 0 {
		t.Fatalf("expected no grant for nil user, got %d calls", g.callCount())
	}
}

// TestGetOrCreateUser_FiresPostLoginHook proves the hook runs on the real login
// choke point, for both the create path and the existing-user (update) path —
// i.e. it covers every login regardless of which internal branch resolved the
// user. This is what makes the bootstrap granter actually fire on login.
func TestGetOrCreateUser_FiresPostLoginHook(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	var seen []*models.User
	svc.SetPostLoginHook(func(u *models.User) { seen = append(seen, u) })

	claims := &JWTClaims{
		ID:                "u1",
		Sub:               "org/alice",
		UniversalID:       "uuid-u1",
		Name:              "alice",
		PreferredUsername: "Alice",
		Email:             "alice@example.com",
		Owner:             "org",
	}

	// First login: create path.
	created, err := svc.GetOrCreateUser(claims)
	if err != nil {
		t.Fatalf("first GetOrCreateUser: %v", err)
	}
	// Second login: existing-user path.
	if _, err := svc.GetOrCreateUser(claims); err != nil {
		t.Fatalf("second GetOrCreateUser: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected post-login hook to fire on both logins, got %d", len(seen))
	}
	for i, u := range seen {
		if u == nil || u.SubjectID != created.SubjectID {
			t.Fatalf("hook call %d got user %+v, want subject %q", i, u, created.SubjectID)
		}
		if u.Email == nil || *u.Email != "alice@example.com" {
			t.Fatalf("hook call %d missing email: %+v", i, u)
		}
	}
}

// TestSyncUser_DoesNotFireHook proves the read-only-sync path (used by
// user-search backfill) upserts the user WITHOUT running the post-login hook,
// while GetOrCreateUser on the same claims DOES. This guards the invariant that
// bootstrap platform-admin granting only happens on the user's own genuine login,
// never when a third party merely searches for / backfills them.
func TestSyncUser_DoesNotFireHook(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	var hookCalls int
	svc.SetPostLoginHook(func(_ *models.User) { hookCalls++ })

	claims := &JWTClaims{
		ID:                "u1",
		Sub:               "org/alice",
		UniversalID:       "uuid-u1",
		Name:              "alice",
		PreferredUsername: "Alice",
		Email:             "alice@example.com",
		Owner:             "org",
	}

	// Backfill/sync path: must upsert the user but NOT fire the hook.
	synced, err := svc.SyncUser(claims)
	if err != nil {
		t.Fatalf("SyncUser create path: %v", err)
	}
	if synced == nil || synced.SubjectID == "" {
		t.Fatalf("SyncUser returned no user: %+v", synced)
	}
	if hookCalls != 0 {
		t.Fatalf("SyncUser must not fire the post-login hook, got %d calls", hookCalls)
	}

	// Sync again on the now-existing user: still no hook.
	if _, err := svc.SyncUser(claims); err != nil {
		t.Fatalf("SyncUser existing path: %v", err)
	}
	if hookCalls != 0 {
		t.Fatalf("SyncUser (existing user) must not fire the hook, got %d calls", hookCalls)
	}

	// The genuine login path on the same user DOES fire the hook, confirming the
	// only difference between the two entry points is the hook.
	if _, err := svc.GetOrCreateUser(claims); err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("GetOrCreateUser should fire the hook exactly once, got %d", hookCalls)
	}
}

// TestGetOrCreateUser_NoHookByDefault confirms zero behaviour change when no
// hook is installed (default path unchanged).
func TestGetOrCreateUser_NoHookByDefault(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	_, err := svc.GetOrCreateUser(&JWTClaims{
		ID:    "u1",
		Sub:   "org/alice",
		Name:  "alice",
		Email: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("GetOrCreateUser without hook: %v", err)
	}
	// No panic / no error == pass.
}

// platformAdminRole must stay in sync with systemrole.SystemRolePlatformAdmin.
// This guards against the constant drifting (the user package can't import
// systemrole due to the cycle, so the constant is duplicated; this test pins the
// literal value the production grant relies on).
func TestBootstrap_PlatformAdminRoleConstant(t *testing.T) {
	if platformAdminRole != "platform_admin" {
		t.Fatalf("platformAdminRole = %q, want platform_admin", platformAdminRole)
	}
	if bootstrapGrantedBy != "bootstrap" {
		t.Fatalf("bootstrapGrantedBy = %q, want bootstrap", bootstrapGrantedBy)
	}
}
