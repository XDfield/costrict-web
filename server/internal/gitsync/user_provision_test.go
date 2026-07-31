// Tests for UserProvisionService state machine (Git Ownership Refactor P1.7).
//
// Uses sqlite :memory: for the binding table and a stub GiteaUserProvisioner
// to drive each state-machine path deterministically.

package gitsync

import (
	"context"
	"errors"
	"fmt"
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
	editCalls     []editCall
	editErr       error
}

// editCall records a single EditUser invocation for assertions.
// EmailIsSet captures whether opts.Email was non-nil — backfill's
// display-name sync must NEVER touch email on already-provisioned Gitea
// accounts (locked by TestBackfill_SyncedBindingEmailNeverTouched).
// LoginNameIsSet / LoginName capture the login_name field — the costrict
// Gitea fork marks login_name as required on PATCH /admin/users/{username}
// and returns 422 `[LoginName]: Required` if absent; every EditUser call
// MUST set it (locked by TestBackfill_EditUserAlwaysSendsLoginName).
type editCall struct {
	Username       string
	FullName       string
	EmailIsSet     bool
	LoginNameIsSet bool
	LoginName      string
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

func (s *stubProvisioner) EditUser(ctx context.Context, username string, opts EditUserOptions) (*GiteaUser, error) {
	full := ""
	if opts.FullName != nil {
		full = *opts.FullName
	}
	login := ""
	if opts.LoginName != nil {
		login = *opts.LoginName
	}
	s.editCalls = append(s.editCalls, editCall{
		Username:       username,
		FullName:       full,
		EmailIsSet:     opts.Email != nil,
		LoginNameIsSet: opts.LoginName != nil,
		LoginName:      login,
	})
	if s.editErr != nil {
		return nil, s.editErr
	}
	return &GiteaUser{ID: 99, Login: username, FullName: full}, nil
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
		SubjectID: "usr-1", TenantID: "t1", ShortID: "u-alice01", Username: "alice",
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
	if b.GitUsername != "u-alice01" {
		t.Errorf("git_username = %q, want u-alice01", b.GitUsername)
	}
	if len(stub.createCalls) != 1 {
		t.Errorf("expected 1 CreateUser call, got %d", len(stub.createCalls))
	}
}

// TestProvisionUser_EmailUsesShortIDTemplate locks the contract: the Gitea
// account's email MUST be {short_id}@costrict.com, derived from ShortID,
// NOT forwarded from cs-user's payload. cs-user's email field is not
// globally unique (multiple Casdoor sources can yield the same address),
// so forwarding it triggered Gitea 422 email-collision rejections. This
// test passes a non-empty Email in params and asserts it is IGNORED.
func TestProvisionUser_EmailUsesShortIDTemplate(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "https://g.example", AdminToken: "tok"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 9, Login: "u-carol03"}}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	csUserEmail := "carol.duplicate@example.com" // would collide if forwarded
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-3", TenantID: "t1", ShortID: "u-carol03", Username: "carol",
		Email: &csUserEmail,
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}
	if len(stub.createCalls) != 1 {
		t.Fatalf("expected 1 CreateUser call, got %d", len(stub.createCalls))
	}
	got := stub.createCalls[0].Email
	want := "u-carol03@costrict.com"
	if got != want {
		t.Errorf("create email: got %q, want %q (cs-user email must NOT be forwarded)", got, want)
	}
}

func TestProvisionUser_AlreadySyncedIsNoop(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	// Seed a synced binding.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-2", TenantID: "t1", ShortID: "u-bob02", Username: "bob",
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call should not invoke CreateUser again.
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-2", TenantID: "t1", ShortID: "u-bob02", Username: "bob",
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
		SubjectID: "usr-3", TenantID: "t1", ShortID: "u-carol03", Username: "carol",
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
		SubjectID: "usr-4", TenantID: "t1", ShortID: "u-dave04", Username: "dave",
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
		SubjectID: "usr-5", TenantID: "t1", ShortID: "u-eve05", Username: "eve",
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
		SubjectID: "usr-6", TenantID: "t1", ShortID: "u-frank06", Username: "frank",
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

// TestProvisionUser_ValidationFailureMarksErrorWithoutRecovery reproduces
// the dev-env bug where a 422 (email collision) was misclassified as
// ErrUsernameTaken, triggering a meaningless GetUserByName call that 404'd
// and producing a misleading "username taken; lookup also failed" error.
// 422 now surfaces as ErrGiteaValidationFailed and routes straight to
// markError — no GetUserByName call, error message preserves Gitea's body.
func TestProvisionUser_ValidationFailureMarksErrorWithoutRecovery(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{createErr: fmt.Errorf("%w: status=422 body=e-mail already in use", ErrGiteaValidationFailed)}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-val", TenantID: "t1", ShortID: "u-val01", Username: "val",
	})
	if err == nil {
		t.Fatalf("expected validation error to surface")
	}

	if len(stub.lookupCalls) != 0 {
		t.Errorf("GetUserByName must NOT be called on 422 (no user was created), got %d calls: %v",
			len(stub.lookupCalls), stub.lookupCalls)
	}

	var b models.UserGitBinding
	if err := db.First(&b, "user_subject_id = ?", "usr-val").Error; err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if b.SyncStatus != models.GitSyncStatusError {
		t.Errorf("status = %q, want error", b.SyncStatus)
	}
	if b.LastError == nil || !strings.Contains(*b.LastError, "e-mail already in use") {
		t.Errorf("last_error = %v, want Gitea's 422 body preserved for operator diagnostics", b.LastError)
	}
}

func TestProvisionUser_MissingShortIDFails(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{}
	svc, db := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-7", TenantID: "t1",
		// ShortID intentionally empty — caller bug or stale payload.
	})
	if err == nil {
		t.Fatalf("expected error when ShortID missing")
	}
	if !strings.Contains(err.Error(), "ShortID") {
		t.Errorf("err should mention ShortID, got %v", err)
	}
	if len(stub.createCalls) != 0 {
		t.Errorf("expected 0 CreateUser calls, got %d", len(stub.createCalls))
	}
	// No binding row should have been inserted.
	var count int64
	db.Model(&models.UserGitBinding{}).Where("user_subject_id = ?", "usr-7").Count(&count)
	if count != 0 {
		t.Errorf("expected 0 binding rows, got %d", count)
	}
}

func TestProvisionUser_PassesDisplayNameAsFullName(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "https://g.example", AdminToken: "tok"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 11}}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	name := "Alice Wonderland"
	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID:   "usr-1",
		TenantID:    "t1",
		ShortID:     "u-alice01",
		Username:    "alice",
		DisplayName: &name,
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}
	if len(stub.createCalls) != 1 {
		t.Fatalf("expected 1 CreateUser call, got %d", len(stub.createCalls))
	}
	if got := stub.createCalls[0].FullName; got != name {
		t.Errorf("FullName = %q, want %q", got, name)
	}
	if got := stub.createCalls[0].Login; got != "u-alice01" {
		t.Errorf("Login = %q, want u-alice01", got)
	}
}

func TestProvisionUser_NilDisplayNameYieldsEmptyFullName(t *testing.T) {
	resolver := &stubResolver{cfg: &gitserver.Config{Endpoint: "x", AdminToken: "y"}}
	stub := &stubProvisioner{created: &GiteaUser{ID: 12}}
	svc, _ := newSvcForTest(t, resolver, func(_ GitServerConfig) GitProvider { return stub })

	if err := svc.ProvisionUser(context.Background(), UserProvisionParams{
		SubjectID: "usr-2",
		TenantID:  "t1",
		ShortID:   "u-bob02",
		Username:  "bob",
	}); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}
	if got := stub.createCalls[0].FullName; got != "" {
		t.Errorf("FullName = %q, want empty when DisplayName is nil", got)
	}
}
