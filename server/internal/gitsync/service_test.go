package gitsync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"go.uber.org/zap"
)

// stubClient is a recording GiteaTeamMemberAPI for service tests. It
// returns a canned current list and records every Add/Remove call so
// tests can assert on the diff/apply outcome.
type stubClient struct {
	current       []GiteaMember
	addCalls      []addCall
	removeCalls   []removeCall
	addErrorOn    string // if set, AddTeamMember for this username returns err
	removeErrorOn string
	listErr       error
}

type addCall struct {
	teamID   int64
	username string
}

type removeCall struct {
	teamID   int64
	username string
}

func (s *stubClient) ListTeamMembers(ctx context.Context, giteaTeamID int64) ([]GiteaMember, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]GiteaMember, len(s.current))
	copy(out, s.current)
	return out, nil
}

func (s *stubClient) AddTeamMember(ctx context.Context, giteaTeamID int64, username string) error {
	if s.addErrorOn == username {
		return fmt.Errorf("injected add error for %s", username)
	}
	s.addCalls = append(s.addCalls, addCall{teamID: giteaTeamID, username: username})
	return nil
}

func (s *stubClient) RemoveTeamMember(ctx context.Context, giteaTeamID int64, username string) error {
	if s.removeErrorOn == username {
		return fmt.Errorf("injected remove error for %s", username)
	}
	s.removeCalls = append(s.removeCalls, removeCall{teamID: giteaTeamID, username: username})
	return nil
}

// stubGitResolver is a canned GitServerResolver. Returns cfg on success,
// or err if set. Captures the last tenant_id passed for assertion.
type stubGitResolver struct {
	cfg        *GitServerConfig
	err        error
	lastTenant string
	calls      int
}

func (r *stubGitResolver) Resolve(ctx context.Context, tenantID string) (*GitServerConfig, error) {
	r.calls++
	r.lastTenant = tenantID
	if r.err != nil {
		return nil, r.err
	}
	if r.cfg != nil {
		return r.cfg, nil
	}
	return &GitServerConfig{
		ServerID:   "gs-test",
		Kind:       "gitea",
		Endpoint:   "https://gitea.test.local",
		AdminToken: "tok-test",
	}, nil
}

// newTestService wires a Service with a stub GitServerResolver and a
// clientFactory that returns the supplied stubClient regardless of input
// config. This keeps tests focused on diff/apply logic; per-tenant
// resolution is exercised separately in TestSyncTeam_PerTenantResolverCalled.
func newTestService(t *testing.T, provider TeamDataProvider, client GiteaTeamMemberAPI, teamResolver GiteaTeamResolver) *Service {
	t.Helper()
	svc := NewService(provider, &stubGitResolver{}, teamResolver, zap.NewNop())
	svc.clientFactory = func(GitServerConfig) GiteaTeamMemberAPI { return client }
	return svc
}

func sorted(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func TestSyncTeam_AddsExpectedNotInCurrent(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}, {GiteaUsername: "bob"}},
	})
	client := &stubClient{current: []GiteaMember{{Login: "alice"}}} // bob missing
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	result, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Added) != 1 || result.Added[0] != "bob" {
		t.Errorf("expected Added=[bob], got %v", result.Added)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "alice" {
		t.Errorf("expected Skipped=[alice], got %v", result.Skipped)
	}
	if len(client.addCalls) != 1 || client.addCalls[0].username != "bob" {
		t.Errorf("expected 1 add call for bob, got %v", client.addCalls)
	}
	if len(client.removeCalls) != 0 {
		t.Errorf("expected 0 remove calls, got %v", client.removeCalls)
	}
}

func TestSyncTeam_RemovesCurrentNotInExpected(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}},
	})
	client := &stubClient{current: []GiteaMember{{Login: "alice"}, {Login: "charlie"}}} // charlie is extra
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	result, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "charlie" {
		t.Errorf("expected Removed=[charlie], got %v", result.Removed)
	}
	if len(client.removeCalls) != 1 || client.removeCalls[0].username != "charlie" {
		t.Errorf("expected 1 remove call for charlie, got %v", client.removeCalls)
	}
}

func TestSyncTeam_IdempotentWhenInSync(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}, {GiteaUsername: "bob"}},
	})
	client := &stubClient{current: []GiteaMember{{Login: "alice"}, {Login: "bob"}}}
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	result, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Added) != 0 || len(result.Removed) != 0 {
		t.Errorf("expected no add/remove, got added=%v removed=%v", result.Added, result.Removed)
	}
	got := sorted(result.Skipped)
	want := []string{"alice", "bob"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("expected Skipped=%v, got %v", want, got)
	}
	if len(client.addCalls) != 0 || len(client.removeCalls) != 0 {
		t.Errorf("expected 0 API calls, got add=%v remove=%v", client.addCalls, client.removeCalls)
	}
}

func TestSyncTeam_PerMemberErrorContinuesBatch(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}, {GiteaUsername: "bob"}, {GiteaUsername: "carol"}},
	})
	// Current empty → all 3 should be added; inject failure on "bob".
	client := &stubClient{current: nil, addErrorOn: "bob"}
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	result, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	gotAdded := sorted(result.Added)
	wantAdded := []string{"alice", "carol"}
	if len(gotAdded) != 2 || gotAdded[0] != wantAdded[0] || gotAdded[1] != wantAdded[1] {
		t.Errorf("expected Added=%v, got %v", wantAdded, gotAdded)
	}
	if len(result.Errors) != 1 || result.Errors[0].GiteaUsername != "bob" {
		t.Errorf("expected 1 error for bob, got %v", result.Errors)
	}
}

func TestSyncTeam_UnknownTeamReturnsErrTeamNotFound(t *testing.T) {
	provider := NewStubProvider() // no teams
	client := &stubClient{}
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	_, err := svc.SyncTeam(context.Background(), "t-acme", "nonexistent")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("expected ErrTeamNotFound, got %v", err)
	}
}

func TestSyncTeam_NoResolverMappingReturnsErrTeamNotFound(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}},
	})
	client := &stubClient{}
	resolver := NewConfigTeamResolver(nil) // no mappings
	svc := newTestService(t, provider, client, resolver)

	_, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("expected ErrTeamNotFound, got %v", err)
	}
	if len(client.addCalls) != 0 {
		t.Errorf("expected no API calls on unresolved team, got %v", client.addCalls)
	}
}

func TestSyncTeam_ListErrorPropagatesAsSentinel(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}},
	})
	client := &stubClient{listErr: ErrGiteaUnauthorized}
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	_, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if !errors.Is(err, ErrGiteaUnauthorized) {
		t.Errorf("expected ErrGiteaUnauthorized, got %v", err)
	}
}

func TestSyncTeam_NilServiceReturnsErr(t *testing.T) {
	var svc *Service
	_, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if !errors.Is(err, ErrGiteaUnreachable) {
		t.Errorf("expected ErrGiteaUnreachable, got %v", err)
	}
}

func TestSyncTeam_EmptyExpectedPurgesAllCurrent(t *testing.T) {
	// Known empty team triggers full purge — this is intentional and the
	// provider interface explicitly does NOT collapse empty into not-found.
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"empty-team": {},
	})
	client := &stubClient{current: []GiteaMember{{Login: "alice"}, {Login: "bob"}}}
	resolver := NewConfigTeamResolver(map[string]int64{"empty-team": 42})
	svc := newTestService(t, provider, client, resolver)

	result, err := svc.SyncTeam(context.Background(), "t-acme", "empty-team")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotRemoved := sorted(result.Removed)
	wantRemoved := []string{"alice", "bob"}
	if len(gotRemoved) != 2 || gotRemoved[0] != wantRemoved[0] || gotRemoved[1] != wantRemoved[1] {
		t.Errorf("expected Removed=%v, got %v", wantRemoved, gotRemoved)
	}
	if len(client.removeCalls) != 2 {
		t.Errorf("expected 2 remove calls, got %d", len(client.removeCalls))
	}
}

func TestSyncTeam_SkipsEmptyUsernameInExpected(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: ""}, {GiteaUsername: "alice"}},
	})
	client := &stubClient{current: nil}
	resolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})
	svc := newTestService(t, provider, client, resolver)

	result, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Added) != 1 || result.Added[0] != "alice" {
		t.Errorf("expected Added=[alice], got %v", result.Added)
	}
	if len(result.Errors) != 1 {
		t.Errorf("expected 1 error for malformed entry, got %v", result.Errors)
	}
}

func TestSyncTeam_EmptyTeamIDReturnsError(t *testing.T) {
	svc := newTestService(t, NewStubProvider(), &stubClient{}, NewConfigTeamResolver(nil))
	_, err := svc.SyncTeam(context.Background(), "t-acme", "")
	if err == nil {
		t.Errorf("expected error for empty team_id, got nil")
	}
}

// TestSyncTeam_EmptyTenantIDReturnsError verifies the per-tenant fix's
// new input guard: SyncTeam without a tenant_id fails fast.
func TestSyncTeam_EmptyTenantIDReturnsError(t *testing.T) {
	svc := newTestService(t, NewStubProvider(), &stubClient{}, NewConfigTeamResolver(nil))
	_, err := svc.SyncTeam(context.Background(), "", "team-a")
	if err == nil {
		t.Errorf("expected error for empty tenant_id, got nil")
	}
}

// TestSyncTeam_PerTenantResolverCalled verifies the per-tenant fix
// (E3b.1.1): syncing a team for tenant "t-acme" calls Resolve("t-acme"),
// not the legacy global default. Regression test for the bug this
// refactor fixes.
func TestSyncTeam_PerTenantResolverCalled(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}},
	})
	client := &stubClient{current: nil}
	teamResolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})

	gitResolver := &stubGitResolver{}
	svc := NewService(provider, gitResolver, teamResolver, zap.NewNop())
	svc.clientFactory = func(GitServerConfig) GiteaTeamMemberAPI { return client }

	if _, err := svc.SyncTeam(context.Background(), "t-acme", "team-a"); err != nil {
		t.Fatalf("SyncTeam: %v", err)
	}
	if gitResolver.calls != 1 {
		t.Fatalf("resolver.calls: got %d, want 1", gitResolver.calls)
	}
	if gitResolver.lastTenant != "t-acme" {
		t.Errorf("resolver.lastTenant: got %q, want t-acme (per-tenant resolution)",
			gitResolver.lastTenant)
	}
}

// TestSyncTeam_ResolverErrorSurfaces verifies that a failed Resolve (e.g.
// cs-user RPC returns ErrGitServerNoBinding) surfaces as an error from
// SyncTeam without firing any Gitea API calls.
func TestSyncTeam_ResolverErrorSurfaces(t *testing.T) {
	provider := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{GiteaUsername: "alice"}},
	})
	client := &stubClient{current: nil}
	teamResolver := NewConfigTeamResolver(map[string]int64{"team-a": 42})

	gitResolver := &stubGitResolver{err: errors.New("tenant has no git_server_id")}
	svc := NewService(provider, gitResolver, teamResolver, zap.NewNop())
	svc.clientFactory = func(GitServerConfig) GiteaTeamMemberAPI { return client }

	_, err := svc.SyncTeam(context.Background(), "t-acme", "team-a")
	if err == nil {
		t.Fatalf("SyncTeam: got nil err, want resolver error")
	}
	if len(client.addCalls) != 0 || len(client.removeCalls) != 0 {
		t.Errorf("Gitea fired despite resolver miss: add=%v remove=%v",
			client.addCalls, client.removeCalls)
	}
}

// TestSyncTeam_NilGitResolverReturnsNilService verifies the
// feature-disabled signal: NewService with a nil GitServerResolver
// returns nil so cmd/api/main.go can skip wiring the handler.
func TestSyncTeam_NilGitResolverReturnsNilService(t *testing.T) {
	svc := NewService(NewStubProvider(), nil, NewConfigTeamResolver(nil), zap.NewNop())
	if svc != nil {
		t.Errorf("NewService with nil GitServerResolver: got non-nil Service, want nil")
	}
}
