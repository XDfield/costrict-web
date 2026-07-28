// Tests for UserProvisionService state machine (Git Ownership Refactor P1.7).
//
// Uses sqlite :memory: for the binding table and a stub GiteaUserProvisioner
// to drive each state-machine path deterministically.

package gitsync

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// stubResolver is a canned gitserver.Resolver for tests.
type stubResolver struct {
	cfg *gitserver.Config
	err error
}

func (r *stubResolver) Resolve(ctx context.Context, tenantID string) (*gitserver.Config, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.cfg, nil
}

// stubProvisioner records calls + returns configured results.
type stubProvisioner struct {
	createCalls   []CreateUserOptions
	created       *GiteaUser
	createErr     error
	lookupCalls   []string
	lookupResult  *GiteaUser
	lookupErr     error
}

func (s *stubProvisioner) CreateUser(ctx context.Context, opts CreateUserOptions) (*GiteaUser, error) {
	s.createCalls = append(s.createCalls, opts)
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.created != nil {
		return s.created, nil
	}
	return &GiteaUser{ID: 42, Login: opts.Login}, nil
}

func (s *stubProvisioner) GetUserByName(ctx context.Context, username string) (*GiteaUser, error) {
	s.lookupCalls = append(s.lookupCalls, username)
	return s.lookupResult, s.lookupErr
}

func setupProvisionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE user_git_binding (
		user_subject_id TEXT NOT NULL,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		git_uid INTEGER,
		git_username TEXT NOT NULL,
		provider_kind TEXT NOT NULL DEFAULT 'gitea',
		sync_status TEXT NOT NULL DEFAULT 'pending',
		last_synced_at DATETIME,
		last_error TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		PRIMARY KEY (user_subject_id, tenant_id)
	)`).Error; err != nil {
		t.Fatalf("create user_git_binding: %v", err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX uq_user_git_binding_git_username ON user_git_binding(git_username)`).Error; err != nil {
		t.Fatalf("create unique index: %v", err)
	}
	return db
}

func newSvcForTest(t *testing.T, resolver gitserver.Resolver, factory func(GitServerConfig) GitProvider) (*UserProvisionService, *gorm.DB) {
	t.Helper()
	db := setupProvisionDB(t)
	svc := NewUserProvisionService(db, resolver, zap.NewNop())
	svc.providerFactory = factory
	return svc, db
}

func TestProvisionUser_HappyPath(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "https://g.example", AdminToken: "tok"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 7, Login: "u-alice"}}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-1", TenantID: "t1", Username: "alice",
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-1").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusSynced {
		t.Errorf("status = %q, want synced", b.SyncStatus)
	}
	if b.GitUID == nil || *b.GitUID != 7 {
		t.Errorf("git_uid = %v, want 7", b.GitUID)
	}
	if len(stub.createCalls) != 1 {
		t.Errorf("expected 1 CreateUser call, got %d", len(stub.createCalls))
	}
}

func TestProvisionUser_AlreadySyncedIsNoop(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed a synced binding.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-2", TenantID: "t1", Username: "bob",
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call should not invoke CreateUser again.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-2", TenantID: "t1", Username: "bob",
	}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(stub.createCalls) != 1 {
		t.Errorf("expected 1 CreateUser call total, got %d", len(stub.createCalls))
	}
}

func TestProvisionUser_MissingGitServerSoftSkips(t *testing.T) {
	resolver := &stubResolver{err: gitserver.ErrTenantMissingGitServer}
	stub := &stubProvisioner{}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-3", TenantID: "t1", Username: "carol",
	})
	if err != nil {
		t.Errorf("soft-skip should return nil, got %v", err)
	}
	if len(stub.createCalls) != 0 {
		t.Errorf("expected 0 client calls on soft-skip, got %d", len(stub.createCalls))
	}
	// Binding row should be left in 'pending'.
	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-3").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusPending {
		t.Errorf("status = %q, want pending", b.SyncStatus)
	}
}

func TestProvisionUser_ResolverTransientErrorSurfaces(t *testing.T) {
	resolver := &stubResolver{err: errors.New("transient")}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-4", TenantID: "t1", Username: "dave",
	})
	if err == nil {
		t.Fatalf("expected non-soft error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "transient") {
		t.Errorf("err should wrap original, got %v", err)
	}
}

func TestProvisionUser_UserExistsRecovers(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{
		createErr:    fmt.Errorf("%w: status=409", ErrGiteaUsernameTaken),
		lookupResult: &GiteaUser{ID: 99, Login: "u-eve"},
	}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-5", TenantID: "t1", Username: "eve",
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-5").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusSynced {
		t.Errorf("status = %q, want synced", b.SyncStatus)
	}
	if b.GitUID == nil || *b.GitUID != 99 {
		t.Errorf("git_uid = %v, want 99", b.GitUID)
	}
	if len(stub.lookupCalls) != 1 {
		t.Errorf("expected 1 lookup call, got %d", len(stub.lookupCalls))
	}
}

func TestProvisionUser_NonConflictErrorMarksError(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{createErr: errors.New("500 internal")}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-6", TenantID: "t1", Username: "frank",
	})
	if err == nil {
		t.Fatalf("expected error to surface")
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-6").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusError {
		t.Errorf("status = %q, want error", b.SyncStatus)
	}
	if b.LastError == nil || !strings.Contains(*b.LastError, "500 internal") {
		t.Errorf("last_error = %v, want '500 internal'", b.LastError)
	}
}

func TestBuildGitUsername_Sanitizes(t *testing.T) {
	cases := []struct {
		username, subjectID, want string
	}{
		{"alice", "usr-1", "u-alice"},
		{"", "usr-2", "u-usr-2"},
		{"!@#bad chars", "usr-3", "u----bad-chars"},
		{"this-is-a-very-long-username-that-exceeds-the-limit-of-forty-chars-yes", "usr-4", regexp.MustCompile(`^u-.{0,38}$`).String()},
	}
	for _, tc := range cases {
		got := buildGitUsername(tc.username, tc.subjectID)
		if len(got) > 40 {
			t.Errorf("got %q (%d chars), must be ≤40", got, len(got))
		}
		if !strings.HasPrefix(got, "u-") {
			t.Errorf("got %q, must start with 'u-'", got)
		}
	}
}
