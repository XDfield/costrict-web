package tenant

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

func TestWithTenant_FromContext(t *testing.T) {
	t2 := &models.Tenant{TenantID: "t-acme", Slug: "acme"}
	ctx := WithTenant(context.Background(), t2)
	got := FromContext(ctx)
	if got == nil || got.TenantID != "t-acme" {
		t.Errorf("FromContext: got %+v, want t-acme", got)
	}
	if !HasTenant(ctx) {
		t.Errorf("HasTenant: got false, want true")
	}
}

func TestFromContext_EmptyContext(t *testing.T) {
	if got := FromContext(context.Background()); got != nil {
		t.Errorf("FromContext(empty): got %v, want nil", got)
	}
	if HasTenant(context.Background()) {
		t.Errorf("HasTenant(empty): got true, want false")
	}
}

func TestFromContext_NilCtx(t *testing.T) {
	if got := FromContext(nil); got != nil {
		t.Errorf("FromContext(nil): got %v, want nil", got)
	}
	if HasTenant(nil) {
		t.Errorf("HasTenant(nil): got true, want false")
	}
}

func TestWithTenant_NilCtxUsesBackground(t *testing.T) {
	// WithTenant must tolerate nil ctx (defensive — gin handlers sometimes
	// pass nil c.Request.Context()).
	t2 := &models.Tenant{TenantID: "t-x"}
	ctx := WithTenant(nil, t2)
	if got := FromContext(ctx); got == nil || got.TenantID != "t-x" {
		t.Errorf("WithTenant(nil,...): round-trip got %v", got)
	}
}

func TestWithTenant_NilValueMeansNoSignal(t *testing.T) {
	// WithTenant(ctx, nil) is the explicit "no signal" path the middleware
	// tests use. FromContext returns nil; HasTenant returns true because the
	// key IS present (the value just happens to be nil-typed).
	ctx := WithTenant(context.Background(), nil)
	if got := FromContext(ctx); got != nil {
		t.Errorf("FromContext after nil-set: got %v, want nil", got)
	}
}

func TestIDFromContext_ResolvesTenantID(t *testing.T) {
	t2 := &models.Tenant{TenantID: "t-acme", Slug: "acme"}
	ctx := WithTenant(context.Background(), t2)
	if got := IDFromContext(ctx); got != "t-acme" {
		t.Errorf("IDFromContext: got %q, want t-acme", got)
	}
}

func TestIDFromContext_FallsBackToDefault(t *testing.T) {
	// Empty ctx and nil-tenant ctx both fall back to DefaultTenantID — the
	// single-tenant safety property (B5).
	if got := IDFromContext(context.Background()); got != DefaultTenantID {
		t.Errorf("IDFromContext(empty): got %q, want %q", got, DefaultTenantID)
	}
	if got := IDFromContext(nil); got != DefaultTenantID {
		t.Errorf("IDFromContext(nil): got %q, want %q", got, DefaultTenantID)
	}
	ctx := WithTenant(context.Background(), nil)
	if got := IDFromContext(ctx); got != DefaultTenantID {
		t.Errorf("IDFromContext(nil-tenant): got %q, want %q", got, DefaultTenantID)
	}
}

func TestIDFromContext_EmptyTenantIDFallsBack(t *testing.T) {
	// Defensive: a tenant row with empty TenantID (shouldn't happen in prod
	// but possible in tests) must still produce a usable scope value.
	t2 := &models.Tenant{TenantID: ""}
	ctx := WithTenant(context.Background(), t2)
	if got := IDFromContext(ctx); got != DefaultTenantID {
		t.Errorf("IDFromContext(empty-id tenant): got %q, want %q", got, DefaultTenantID)
	}
}

func TestDefaultTenantIDConstant(t *testing.T) {
	if DefaultTenantID != "default" {
		t.Errorf("DefaultTenantID = %q, want 'default'", DefaultTenantID)
	}
}
