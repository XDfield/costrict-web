package user

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/tenant"
)

// actorMetaStubServer captures all actor headers so tests can assert forwarding.
func actorMetaStubServer(t *testing.T) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotRole, gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRole = r.Header.Get(ActorRoleHeader)
		gotScope = r.Header.Get(ActorPlatformScopeHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant_id":"t-x","config_yaml":"","updated_at":"2026-07-17T00:00:00Z","created_at":"2026-07-17T00:00:00Z"}`))
	}))
	return srv, &gotRole, &gotScope
}

// TestRPCTenantConfig_ForwardsActorMetaHeaders verifies the Phase C4.1
// header forwarding: when ctx carries non-empty ActorMeta, both
// X-Actor-Tenant-Role and X-Actor-Platform-Scope are set on the outbound
// request.
func TestRPCTenantConfig_ForwardsActorMetaHeaders(t *testing.T) {
	srv, gotRole, gotScope := actorMetaStubServer(t)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	ctx = tenant.WithActorMeta(ctx, tenant.ActorMeta{Role: "owner", Scope: "full"})

	if _, err := c.GetTenantConfig(ctx); err != nil {
		t.Fatalf("GetTenantConfig: %v", err)
	}
	if *gotRole != "owner" {
		t.Errorf("X-Actor-Tenant-Role: got %q want owner", *gotRole)
	}
	if *gotScope != "full" {
		t.Errorf("X-Actor-Platform-Scope: got %q want full", *gotScope)
	}
}

// TestRPCTenantConfig_OmitsActorMetaHeadersWhenAbsent verifies the
// no-signal path: ctx with no ActorMeta produces no actor headers.
func TestRPCTenantConfig_OmitsActorMetaHeadersWhenAbsent(t *testing.T) {
	srv, gotRole, gotScope := actorMetaStubServer(t)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	// No WithActorMeta — should not set the headers.

	if _, err := c.GetTenantConfig(ctx); err != nil {
		t.Fatalf("GetTenantConfig: %v", err)
	}
	if *gotRole != "" {
		t.Errorf("X-Actor-Tenant-Role: got %q want empty", *gotRole)
	}
	if *gotScope != "" {
		t.Errorf("X-Actor-Platform-Scope: got %q want empty", *gotScope)
	}
}

// TestRPCTenantConfig_PartialActorMetaHeaders verifies partial meta (only
// Role set, Scope empty) forwards only the Role header.
func TestRPCTenantConfig_PartialActorMetaHeaders(t *testing.T) {
	srv, gotRole, gotScope := actorMetaStubServer(t)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	ctx = tenant.WithActorMeta(ctx, tenant.ActorMeta{Role: "tenant_admin"}) // Scope=""

	if _, err := c.GetTenantConfig(ctx); err != nil {
		t.Fatalf("GetTenantConfig: %v", err)
	}
	if *gotRole != "tenant_admin" {
		t.Errorf("X-Actor-Tenant-Role: got %q want tenant_admin", *gotRole)
	}
	if *gotScope != "" {
		t.Errorf("X-Actor-Platform-Scope: got %q want empty", *gotScope)
	}
}

// TestActorMetaCtx_RoundTrip is a small unit test on the ctx-carrier itself
// (mirrors tenant/context_test.go pattern) so the lookup path stays covered
// when the RPC layer is refactored.
func TestActorMetaCtx_RoundTrip(t *testing.T) {
	ctx := tenant.WithActorMeta(context.Background(), tenant.ActorMeta{Role: "owner", Scope: "full"})
	m := tenant.ActorMetaFromContext(ctx)
	if m.Role != "owner" || m.Scope != "full" {
		t.Errorf("round-trip: got %+v, want {owner,full}", m)
	}
}

// TestActorMetaCtx_NilSafe verifies the lookup never panics on nil ctx.
func TestActorMetaCtx_NilSafe(t *testing.T) {
	m := tenant.ActorMetaFromContext(nil)
	if m.Role != "" || m.Scope != "" {
		t.Errorf("nil ctx: got %+v, want zero-value", m)
	}
}
