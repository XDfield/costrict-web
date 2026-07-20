//go:build cgo

package giteasync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/gitserver"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newServiceDB mirrors auditlog/service_test.go's pattern — in-memory
// sqlite + AutoMigrate. cgo-gated.
func newServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.UserGiteaBinding{}, &models.AuditLog{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// stubProvisioner is a configurable GiteaUserProvisioner for tests. Each
// field controls one branch: if non-nil, that branch fires; otherwise the
// happy-path stub is used.
type stubProvisioner struct {
	provisionErr  error
	provisionUser *GiteaUser
	lookupErr     error
	lookupUser    *GiteaUser

	provisionCalls int
	lookupCalls    int
	lastParams     GiteaUserParams
}

func (s *stubProvisioner) ProvisionGiteaUser(ctx context.Context, p GiteaUserParams) (*GiteaUser, error) {
	s.provisionCalls++
	s.lastParams = p
	if s.provisionErr != nil {
		return nil, s.provisionErr
	}
	if s.provisionUser != nil {
		return s.provisionUser, nil
	}
	return &GiteaUser{ID: 99, Username: p.Username}, nil
}

func (s *stubProvisioner) LookupUserByName(ctx context.Context, username string) (*GiteaUser, error) {
	s.lookupCalls++
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.lookupUser != nil {
		return s.lookupUser, nil
	}
	return &GiteaUser{ID: 88, Username: username}, nil
}

// stubResolver is the per-tenant gitserver.Resolver used by the service
// tests. Returns a fixed Config for any tenant by default; if resolveErr
// is set, returns that instead. tenantCalls tracks Resolve invocations
// and lastTenantID captures the most recent tenant_id passed in.
type stubResolver struct {
	cfg         *gitserver.Config
	resolveErr  error
	tenantCalls int
	lastTenant  string
}

func (r *stubResolver) Resolve(ctx context.Context, tenantID string) (*gitserver.Config, error) {
	r.tenantCalls++
	r.lastTenant = tenantID
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	if r.cfg != nil {
		return r.cfg, nil
	}
	return &gitserver.Config{
		ServerID:   "gs-test",
		Kind:       models.GitServerKindGitea,
		Endpoint:   "https://gitea.test.local",
		AdminToken: "tok-test",
	}, nil
}

// newSvcWithStub wires a Service with a stub resolver + stub client factory.
// Returns the Service + both stubs so individual tests can configure them.
func newSvcWithStub(db *gorm.DB, resolver *stubResolver, client *stubProvisioner) *Service {
	svc := NewService(db, resolver, nil, nil)
	svc.clientFactory = func(endpoint, adminToken string) GiteaUserProvisioner { return client }
	return svc
}

// stringAddr is the local equivalent of helper seen in audit_log_test.go.
func stringAddr(s string) *string { return &s }

// newTestUser returns a *models.User with the minimum required fields
// populated for Provision to work.
func newTestUser(subjectID, username, tenantID, email string) *models.User {
	u := &models.User{
		SubjectID: subjectID,
		Username:  username,
		TenantID:  tenantID,
	}
	if email != "" {
		u.Email = stringAddr(email)
	}
	return u
}

// TestProvision_HappyPath verifies the 201 path: pending INSERT → POST
// 201 → synced UPDATE with gitea_uid + last_synced_at populated.
func TestProvision_HappyPath(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{provisionUser: &GiteaUser{ID: 42, Username: "u-alice"}}
	resolver := &stubResolver{}
	svc := newSvcWithStub(db, resolver, client)

	err := svc.Provision(context.Background(), newTestUser("usr_1", "alice", "default", "alice@example.com"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var binding models.UserGiteaBinding
	if err := db.First(&binding, "user_subject_id = ?", "usr_1").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if binding.SyncStatus != models.GiteaSyncStatusSynced {
		t.Errorf("sync_status: got %q, want synced", binding.SyncStatus)
	}
	if binding.GiteaUID == nil || *binding.GiteaUID != 42 {
		t.Errorf("gitea_uid: got %v, want 42", binding.GiteaUID)
	}
	if binding.LastSyncedAt == nil {
		t.Errorf("last_synced_at: got nil, want non-nil")
	}
	if binding.LastError != nil {
		t.Errorf("last_error: got %v, want nil", binding.LastError)
	}
	if binding.GiteaUsername != "u-alice" {
		t.Errorf("gitea_username: got %q, want u-alice", binding.GiteaUsername)
	}
	if client.provisionCalls != 1 {
		t.Errorf("provisionCalls: got %d, want 1", client.provisionCalls)
	}
}

// TestProvision_409RecoversViaLookup verifies the 409 → LookupUserByName →
// synced recovery path.
func TestProvision_409RecoversViaLookup(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{
		provisionErr: ErrGiteaUserExists,
		lookupUser:   &GiteaUser{ID: 77, Username: "u-bob"},
	}
	svc := newSvcWithStub(db, &stubResolver{}, client)

	err := svc.Provision(context.Background(), newTestUser("usr_2", "bob", "default", "bob@example.com"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var binding models.UserGiteaBinding
	if err := db.First(&binding, "user_subject_id = ?", "usr_2").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if binding.SyncStatus != models.GiteaSyncStatusSynced {
		t.Errorf("sync_status: got %q, want synced", binding.SyncStatus)
	}
	if binding.GiteaUID == nil || *binding.GiteaUID != 77 {
		t.Errorf("gitea_uid: got %v, want 77 (from lookup)", binding.GiteaUID)
	}
	if client.provisionCalls != 1 || client.lookupCalls != 1 {
		t.Errorf("calls: provision=%d lookup=%d, want 1/1", client.provisionCalls, client.lookupCalls)
	}
}

// TestProvision_ClientErrorMarksError verifies the non-timeout failure
// path lands the binding in 'error' with last_error populated.
func TestProvision_ClientErrorMarksError(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{provisionErr: ErrGiteaUnauthorized}
	svc := newSvcWithStub(db, &stubResolver{}, client)

	err := svc.Provision(context.Background(), newTestUser("usr_3", "carol", "default", "carol@example.com"))
	if err == nil {
		t.Fatalf("Provision: got nil err, want non-nil")
	}
	var binding models.UserGiteaBinding
	if err := db.First(&binding, "user_subject_id = ?", "usr_3").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if binding.SyncStatus != models.GiteaSyncStatusError {
		t.Errorf("sync_status: got %q, want error", binding.SyncStatus)
	}
	if binding.LastError == nil || !strings.Contains(*binding.LastError, "unauthorized") {
		t.Errorf("last_error: got %v, want containing 'unauthorized'", binding.LastError)
	}
}

// TestProvision_AlreadySyncedIsNoop verifies idempotency — a second
// Provision call for a user whose binding is already synced does NOT
// resolve the git server (no Provision call).
func TestProvision_AlreadySyncedIsNoop(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{provisionUser: &GiteaUser{ID: 42, Username: "u-dave"}}
	resolver := &stubResolver{}
	svc := newSvcWithStub(db, resolver, client)

	ctx := context.Background()
	user := newTestUser("usr_4", "dave", "default", "dave@example.com")
	if err := svc.Provision(ctx, user); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if resolver.tenantCalls != 1 || client.provisionCalls != 1 {
		t.Fatalf("after first call: resolver=%d provision=%d, want 1/1",
			resolver.tenantCalls, client.provisionCalls)
	}

	// Second call — should be no-op (short-circuits before resolver).
	if err := svc.Provision(ctx, user); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if resolver.tenantCalls != 1 {
		t.Errorf("after second call: resolver.tenantCalls=%d, want still 1 (no-op)",
			resolver.tenantCalls)
	}
	if client.provisionCalls != 1 {
		t.Errorf("after second call: provisionCalls=%d, want still 1 (no-op)",
			client.provisionCalls)
	}
}

// TestProvision_NilAuditDoesNotPanic verifies the best-effort audit
// contract holds when no audit service is wired.
func TestProvision_NilAuditDoesNotPanic(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{provisionUser: &GiteaUser{ID: 5, Username: "u-eve"}}
	svc := newSvcWithStub(db, &stubResolver{}, client)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Provision panicked with nil audit: %v", r)
		}
	}()
	if err := svc.Provision(context.Background(),
		newTestUser("usr_5", "eve", "default", "eve@example.com")); err != nil {
		t.Fatalf("Provision: %v", err)
	}
}

// TestProvision_NilResolverReturnsError verifies the config-off path: when
// no resolver is wired, Provision fails fast without touching the DB.
func TestProvision_NilResolverReturnsError(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	svc := NewService(db, nil, nil, nil)

	err := svc.Provision(context.Background(), newTestUser("usr_6", "frank", "default", "frank@example.com"))
	if err == nil {
		t.Fatalf("Provision: got nil err, want non-nil")
	}
	if !strings.Contains(err.Error(), "nil resolver") {
		t.Errorf("err: got %v, want containing 'nil resolver'", err)
	}
	var count int64
	db.Model(&models.UserGiteaBinding{}).Count(&count)
	if count != 0 {
		t.Errorf("binding rows: got %d, want 0", count)
	}
}

// TestProvision_TimeoutKeepsBindingPending verifies the timeout path
// does NOT mark the binding 'error' — it stays 'pending' so the
// reconciliation cron picks it up.
func TestProvision_TimeoutKeepsBindingPending(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{provisionErr: ErrGiteaTimeout}
	svc := newSvcWithStub(db, &stubResolver{}, client)

	err := svc.Provision(context.Background(),
		newTestUser("usr_7", "gina", "default", "gina@example.com"))
	if err == nil {
		t.Fatalf("Provision: got nil err, want non-nil")
	}
	var binding models.UserGiteaBinding
	if err := db.First(&binding, "user_subject_id = ?", "usr_7").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if binding.SyncStatus != models.GiteaSyncStatusPending {
		t.Errorf("sync_status: got %q, want pending (timeout should not mark error)",
			binding.SyncStatus)
	}
}

// TestProvision_AuditRowWrittenOnSynced verifies the C4.1 audit hook
// fires for the happy path.
func TestProvision_AuditRowWrittenOnSynced(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	audit := auditlog.NewService(db, nil)
	client := &stubProvisioner{provisionUser: &GiteaUser{ID: 42, Username: "u-henry"}}
	svc := NewService(db, &stubResolver{}, audit, nil)
	svc.clientFactory = func(endpoint, adminToken string) GiteaUserProvisioner { return client }

	if err := svc.Provision(context.Background(),
		newTestUser("usr_8", "henry", "tenant-acme", "henry@example.com")); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var auditRows []models.AuditLog
	db.Find(&auditRows)
	if len(auditRows) != 1 {
		t.Fatalf("audit rows: got %d, want 1", len(auditRows))
	}
	row := auditRows[0]
	if row.Action != models.ActionUserGiteaProvisioned {
		t.Errorf("action: got %q, want %q", row.Action, models.ActionUserGiteaProvisioned)
	}
	if row.TargetType == nil || *row.TargetType != models.TargetTypeUserGiteaBinding {
		t.Errorf("target_type: got %v, want %q", row.TargetType, models.TargetTypeUserGiteaBinding)
	}
	if row.TenantID == nil || *row.TenantID != "tenant-acme" {
		t.Errorf("tenant_id: got %v, want tenant-acme", row.TenantID)
	}
	if !strings.Contains(string(row.Payload), `"sync_status":"synced"`) {
		t.Errorf("payload missing sync_status=synced: %s", string(row.Payload))
	}
	if !strings.Contains(string(row.Payload), `"gitea_uid":42`) {
		t.Errorf("payload missing gitea_uid=42: %s", string(row.Payload))
	}
}

// TestProvision_BestEffortTimeoutSurfacesButCallerCanIgnore verifies
// that the 5s provisionTimeout caps the roundtrip.
func TestProvision_BestEffortTimeoutSurfacesButCallerCanIgnore(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &slowProvisioner{delay: 7 * time.Second}
	svc := NewService(db, &stubResolver{}, nil, nil)
	svc.clientFactory = func(endpoint, adminToken string) GiteaUserProvisioner { return client }

	start := time.Now()
	err := svc.Provision(context.Background(),
		newTestUser("usr_9", "iris", "default", "iris@example.com"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Provision: got nil err, want timeout")
	}
	if elapsed > 6*time.Second {
		t.Errorf("elapsed: got %v, want <=6s (stubbed 7s should be cut off)", elapsed)
	}
}

// slowProvisioner blocks past any reasonable ctx deadline so the
// Service's provisionTimeout can fire.
type slowProvisioner struct{ delay time.Duration }

func (s *slowProvisioner) ProvisionGiteaUser(ctx context.Context, p GiteaUserParams) (*GiteaUser, error) {
	select {
	case <-time.After(s.delay):
		return &GiteaUser{ID: 1, Username: p.Username}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slowProvisioner) LookupUserByName(ctx context.Context, username string) (*GiteaUser, error) {
	return nil, errors.New("slowProvisioner: lookup not implemented")
}

// TestProvision_PerTenantResolverCalled verifies the per-tenant fix
// (E3b.1.1): Provisioning a user in tenant "t-acme" calls Resolve with
// "t-acme", not the legacy global default. This is the regression test
// for the bug the refactor fixes.
func TestProvision_PerTenantResolverCalled(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{provisionUser: &GiteaUser{ID: 1, Username: "u-zoe"}}
	resolver := &stubResolver{}
	svc := newSvcWithStub(db, resolver, client)

	if err := svc.Provision(context.Background(),
		newTestUser("usr_z", "zoe", "t-acme", "zoe@acme.example")); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if resolver.tenantCalls != 1 {
		t.Fatalf("resolver.tenantCalls: got %d, want 1", resolver.tenantCalls)
	}
	if resolver.lastTenant != "t-acme" {
		t.Errorf("resolver.lastTenant: got %q, want t-acme (per-tenant resolution)",
			resolver.lastTenant)
	}
}

// TestProvision_ResolverErrorSurfaces verifies that a failed Resolve (e.g.
// gitserver.ErrTenantMissingGitServer during the migration window)
// surfaces as an error without leaving the binding in a terminal state.
// The binding row remains 'pending' for the reconciliation cron.
func TestProvision_ResolverErrorSurfaces(t *testing.T) {
	t.Parallel()
	db := newServiceDB(t)
	client := &stubProvisioner{}
	resolver := &stubResolver{resolveErr: gitserver.ErrTenantMissingGitServer}
	svc := newSvcWithStub(db, resolver, client)

	err := svc.Provision(context.Background(),
		newTestUser("usr_x", "xander", "t-orphan", "x@orphan.example"))
	if err == nil {
		t.Fatalf("Provision: got nil err, want resolver error")
	}
	if !errors.Is(err, gitserver.ErrTenantMissingGitServer) {
		t.Errorf("err: got %v, want wrapping ErrTenantMissingGitServer", err)
	}
	if client.provisionCalls != 0 {
		t.Errorf("client.provisionCalls: got %d, want 0 (no client call on resolver miss)",
			client.provisionCalls)
	}
	// Binding row stays pending — never transitioned to error/synced.
	var binding models.UserGiteaBinding
	if err := db.First(&binding, "user_subject_id = ?", "usr_x").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if binding.SyncStatus != models.GiteaSyncStatusPending {
		t.Errorf("sync_status: got %q, want pending (resolver miss keeps binding pending)",
			binding.SyncStatus)
	}
}

// TestBuildGiteaUsername_Sanitizes verifies the sanitizer handles
// non-ASCII + spaces + empty username fallbacks.
func TestBuildGiteaUsername_Sanitizes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		user    *models.User
		wantPre string
		wantMax int
	}{
		{
			name:    "ascii username",
			user:    newTestUser("usr_1", "alice", "default", ""),
			wantPre: "u-alice",
		},
		{
			name:    "spaces become dashes",
			user:    newTestUser("usr_2", "alice cooper", "default", ""),
			wantPre: "u-alice-cooper",
		},
		{
			name:    "empty username falls back to subject_id",
			user:    newTestUser("usr_3", "", "default", ""),
			wantPre: "u-usr_3",
		},
		{
			name:    "truncation at 40 chars",
			user:    newTestUser("usr_4", strings.Repeat("a", 100), "default", ""),
			wantMax: 40,
		},
	}
	for _, tc := range cases {
		got := buildGiteaUsername(tc.user)
		if tc.wantPre != "" && got != tc.wantPre {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.wantPre)
		}
		if tc.wantMax > 0 && len(got) > tc.wantMax {
			t.Errorf("%s: len got %d, want <= %d", tc.name, len(got), tc.wantMax)
		}
	}
}
