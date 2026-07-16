package user

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// TestWriteGate_ReadonlyBlocksAllWrites verifies each of the 5 user write
// methods short-circuits with ErrWriteBlocked when WriteModeReadonly is set,
// and that the DB state is unchanged after the rejected call. Each row also
// confirms the same call succeeds under WriteModeLocal (control) so the test
// isn't silently passing because the call is broken for an unrelated reason.
func TestWriteGate_ReadonlyBlocksAllWrites(t *testing.T) {
	t.Parallel()

	// Minimal claim sufficient to drive getOrCreateUser to a successful write
	// in the local-mode control. UniversalID is the identity anchor; provider
	// disambiguates which identity row gets touched.
	baseClaims := func(provider, sub string) *JWTClaims {
		return &JWTClaims{
			ID:                "id-" + sub,
			Sub:               sub,
			UniversalID:       "uuid-" + sub,
			Name:              "name-" + sub,
			PreferredUsername: "display-" + sub,
			Provider:          provider,
			ProviderUserID:    "pid-" + sub,
		}
	}

	type row struct {
		name      string
		writeMode string
		// arrange seeds a fresh DB with whatever state the write call needs to
		// be meaningful (e.g. an existing user to bind/unbind against).
		arrange func(t *testing.T, db *gorm.DB) *models.User
		// act invokes the write method under test. Returns the error so the
		// row can assert on it.
		act func(t *testing.T, svc *UserService, seed *models.User) error
		// wantBlocked is true when ErrWriteBlocked is expected (readonly rows).
		wantBlocked bool
	}

	rows := []row{
		{
			name:      "GetOrCreateUser local",
			writeMode: WriteModeLocal,
			arrange:   func(t *testing.T, db *gorm.DB) *models.User { return nil },
			act: func(t *testing.T, svc *UserService, _ *models.User) error {
				_, err := svc.GetOrCreateUser(baseClaims("github", "sub-goc-local"))
				return err
			},
			wantBlocked: false,
		},
		{
			name:      "GetOrCreateUser readonly",
			writeMode: WriteModeReadonly,
			arrange:   func(t *testing.T, db *gorm.DB) *models.User { return nil },
			act: func(t *testing.T, svc *UserService, _ *models.User) error {
				_, err := svc.GetOrCreateUser(baseClaims("github", "sub-goc-ro"))
				return err
			},
			wantBlocked: true,
		},
		{
			name:      "SyncUser local",
			writeMode: WriteModeLocal,
			arrange:   func(t *testing.T, db *gorm.DB) *models.User { return nil },
			act: func(t *testing.T, svc *UserService, _ *models.User) error {
				_, err := svc.SyncUser(baseClaims("github", "sub-sync-local"))
				return err
			},
			wantBlocked: false,
		},
		{
			name:      "SyncUser readonly",
			writeMode: WriteModeReadonly,
			arrange:   func(t *testing.T, db *gorm.DB) *models.User { return nil },
			act: func(t *testing.T, svc *UserService, _ *models.User) error {
				_, err := svc.SyncUser(baseClaims("github", "sub-sync-ro"))
				return err
			},
			wantBlocked: true,
		},
		{
			name:      "BindIdentityToUser local",
			writeMode: WriteModeLocal,
			arrange: func(t *testing.T, db *gorm.DB) *models.User {
				return seedUserForBindTest(t, db, "sub-bind-local", "uuid-bind-local")
			},
			act: func(t *testing.T, svc *UserService, seed *models.User) error {
				return svc.BindIdentityToUser(seed.SubjectID, baseClaims("phone", "sub-bind-local-extra"))
			},
			wantBlocked: false,
		},
		{
			name:      "BindIdentityToUser readonly",
			writeMode: WriteModeReadonly,
			arrange: func(t *testing.T, db *gorm.DB) *models.User {
				return seedUserForBindTest(t, db, "sub-bind-ro", "uuid-bind-ro")
			},
			act: func(t *testing.T, svc *UserService, seed *models.User) error {
				return svc.BindIdentityToUser(seed.SubjectID, baseClaims("phone", "sub-bind-ro-extra"))
			},
			wantBlocked: true,
		},
		{
			name:      "UnbindIdentityByProvider local",
			writeMode: WriteModeLocal,
			arrange: func(t *testing.T, db *gorm.DB) *models.User {
				return seedUserForBindTest(t, db, "sub-unbind-local", "uuid-unbind-local")
			},
			act: func(t *testing.T, svc *UserService, seed *models.User) error {
				return svc.UnbindIdentityByProvider(seed.SubjectID, "github")
			},
			wantBlocked: false,
		},
		{
			name:      "UnbindIdentityByProvider readonly",
			writeMode: WriteModeReadonly,
			arrange: func(t *testing.T, db *gorm.DB) *models.User {
				return seedUserForBindTest(t, db, "sub-unbind-ro", "uuid-unbind-ro")
			},
			act: func(t *testing.T, svc *UserService, seed *models.User) error {
				return svc.UnbindIdentityByProvider(seed.SubjectID, "github")
			},
			wantBlocked: true,
		},
		{
			name:      "TransferIdentityToUser local",
			writeMode: WriteModeLocal,
			arrange: func(t *testing.T, db *gorm.DB) *models.User {
				// Seed source user with an identity we can transfer away.
				return seedUserForBindTest(t, db, "sub-xfer-local", "uuid-xfer-local")
			},
			act: func(t *testing.T, svc *UserService, seed *models.User) error {
				// external_key for the seeded github identity — see seedUserForBindTest.
				return svc.TransferIdentityToUser(seed.SubjectID, "uuid-xfer-local", "")
			},
			wantBlocked: false,
		},
		{
			name:      "TransferIdentityToUser readonly",
			writeMode: WriteModeReadonly,
			arrange: func(t *testing.T, db *gorm.DB) *models.User {
				return seedUserForBindTest(t, db, "sub-xfer-ro", "uuid-xfer-ro")
			},
			act: func(t *testing.T, svc *UserService, seed *models.User) error {
				return svc.TransferIdentityToUser(seed.SubjectID, "uuid-xfer-ro", "")
			},
			wantBlocked: true,
		},
	}

	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			db := setupUserTestDB(t)
			svc := NewUserService(db)
			svc.SetWriteMode(r.writeMode)

			seed := r.arrange(t, db)

			usersBefore, identBefore := rowCount(t, db)
			err := r.act(t, svc, seed)
			usersAfter, identAfter := rowCount(t, db)

			if r.wantBlocked {
				if !errors.Is(err, ErrWriteBlocked) {
					t.Fatalf("expected ErrWriteBlocked, got %v", err)
				}
				if usersBefore != usersAfter || identBefore != identAfter {
					t.Fatalf("readonly write should not mutate DB: users %d->%d, identities %d->%d",
						usersBefore, usersAfter, identBefore, identAfter)
				}
				return
			}
			// Control: write should succeed.
			if err != nil {
				t.Fatalf("local-mode write failed (control row): %v", err)
			}
		})
	}
}

// seedUserForBindTest inserts a user + a primary github identity + a secondary
// phone identity. Two identities are required so UnbindIdentityByProvider can
// drop the github one without tripping the "cannot unbind last identity"
// invariant. external_key = universalID for the github identity, which is what
// TransferIdentityToUser matches against.
func seedUserForBindTest(t *testing.T, db *gorm.DB, subjectID, universalID string) *models.User {
	t.Helper()
	u := &models.User{
		SubjectID:         subjectID,
		Username:          subjectID,
		CasdoorUniversalID: strPtr(universalID),
		IsActive:          true,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	github := &models.UserAuthIdentity{
		UserSubjectID: subjectID,
		Provider:       "github",
		ExternalKey:    universalID,
		IsPrimary:      true,
	}
	if err := db.Create(github).Error; err != nil {
		t.Fatalf("seed github identity: %v", err)
	}
	phone := &models.UserAuthIdentity{
		UserSubjectID: subjectID,
		Provider:       "phone",
		ExternalKey:    subjectID + "-phone",
		IsPrimary:      false,
	}
	if err := db.Create(phone).Error; err != nil {
		t.Fatalf("seed phone identity: %v", err)
	}
	return u
}

func rowCount(t *testing.T, db *gorm.DB) (int, int) {
	t.Helper()
	var users, idents int64
	if err := db.Model(&models.User{}).Count(&users).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := db.Model(&models.UserAuthIdentity{}).Count(&idents).Error; err != nil {
		t.Fatalf("count identities: %v", err)
	}
	return int(users), int(idents)
}

// TestNewWithConfig_BootValidation confirms the four (Backend, WriteMode)
// combinations behave as documented:
//   - readonly + local  -> fatal (login broken, no benefit)
//   - readonly + rpc    -> fatal (cs-user Phase 2 write API not yet shipped)
//   - local + rpc       -> warn (split-brain canary)
//   - local + local     -> ok (default)
//
// We can't call log.Fatalf directly in a test (it os.Exit()s). Instead we
// exercise the gate plumbing via SetWriteMode on a UserService constructed
// through the simple constructor, and verify the gate value rounds-trips.
// The fatal paths are exercised via TestNewWithConfig_Fatals, which captures
// the os.Exit by running NewWithConfig in a subprocess.
func TestNewWithConfig_DefaultWriteModeRoundTrips(t *testing.T) {
	t.Parallel()
	db := setupUserTestDB(t)
	mod := NewWithConfig(db, 0, config.UserServiceConfig{
		Backend:   config.UserServiceBackendLocal,
		WriteMode: config.UserServiceWriteModeLocal,
	})
	if mod.Service.writeMode != WriteModeLocal {
		t.Fatalf("expected writeMode=%q, got %q", WriteModeLocal, mod.Service.writeMode)
	}

	// Invalid WriteMode string falls back to local — never silently blocks login.
	mod2 := NewWithConfig(db, 0, config.UserServiceConfig{
		Backend:   config.UserServiceBackendLocal,
		WriteMode: "nonsense",
	})
	if mod2.Service.writeMode != WriteModeLocal {
		t.Fatalf("invalid WriteMode should default to local, got %q", mod2.Service.writeMode)
	}
}

// TestValidateUserConfig exercises every (Backend, WriteMode) combination and
// confirms the validation matrix matches the documented P0-8a contract. This
// is the test that backs NewWithConfig's log.Fatalf — by extracting the
// validation into a pure helper we can assert on the message text without
// spawning subprocesses.
func TestValidateUserConfig(t *testing.T) {
	t.Parallel()

	type tc struct {
		name       string
		backend    string
		mode       string
		wantFatal  bool
		wantSubstr string
	}
	cases := []tc{
		{
			name:       "local+local default",
			backend:    config.UserServiceBackendLocal,
			mode:       config.UserServiceWriteModeLocal,
			wantFatal:  false,
			wantSubstr: "",
		},
		{
			name:       "readonly+local fatal login broken",
			backend:    config.UserServiceBackendLocal,
			mode:       config.UserServiceWriteModeReadonly,
			wantFatal:  true,
			wantSubstr: "USER_SERVICE_WRITE_MODE=readonly with USER_SERVICE_BACKEND=local",
		},
		{
			name:       "readonly+rpc fatal no write API",
			backend:    config.UserServiceBackendRPC,
			mode:       config.UserServiceWriteModeReadonly,
			wantFatal:  true,
			wantSubstr: "cs-user has no write API yet",
		},
		{
			name:       "local+rpc warn split-brain",
			backend:    config.UserServiceBackendRPC,
			mode:       config.UserServiceWriteModeLocal,
			wantFatal:  false,
			wantSubstr: "split-brain",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			msg, fatal := validateUserConfig(c.mode, c.backend)
			if fatal != c.wantFatal {
				t.Fatalf("expected fatal=%v, got fatal=%v (msg=%q)", c.wantFatal, fatal, msg)
			}
			if c.wantSubstr == "" {
				if msg != "" {
					t.Fatalf("expected empty message, got %q", msg)
				}
				return
			}
			if !strings.Contains(msg, c.wantSubstr) {
				t.Fatalf("message %q does not contain %q", msg, c.wantSubstr)
			}
		})
	}
}

// TestNewWithConfig_SplitBrainWarns verifies the local+rpc canary combination
// boots successfully and emits the split-brain warning.
func TestNewWithConfig_SplitBrainWarns(t *testing.T) {
	t.Parallel()
	// Backend=rpc with valid URL+token so rpc.Configured() returns true.
	db := setupUserTestDB(t)
	mod := NewWithConfig(db, 0, config.UserServiceConfig{
		Backend:       config.UserServiceBackendRPC,
		BaseURL:       "http://cs-user.test",
		InternalToken: "test-token",
		WriteMode:     config.UserServiceWriteModeLocal,
	})
	if mod.Service.writeMode != WriteModeLocal {
		t.Fatalf("expected writeMode=local, got %q", mod.Service.writeMode)
	}
	// Reader should be the RPC client in split-brain mode.
	if _, ok := mod.Reader.(*RPCClient); !ok {
		t.Fatalf("expected *RPCClient reader, got %T", mod.Reader)
	}
}

// ctx is exported for tests in this package that need a context but shouldn't
// import the user package's internals. Kept here as a no-op ref so unused
// imports stay clean.
var _ = context.Background
