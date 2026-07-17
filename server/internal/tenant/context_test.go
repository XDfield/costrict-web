package tenant

import (
	"context"
	"testing"
)

func TestWithSlug_RoundTrip(t *testing.T) {
	ctx := WithSlug(context.Background(), "acme")
	if got := SlugFromContext(ctx); got != "acme" {
		t.Errorf("SlugFromContext: got %q, want acme", got)
	}
	if !HasSlug(ctx) {
		t.Errorf("HasSlug: got false, want true")
	}
}

func TestSlugFromContext_Empty(t *testing.T) {
	if got := SlugFromContext(context.Background()); got != "" {
		t.Errorf("SlugFromContext(empty): got %q, want empty", got)
	}
	if HasSlug(context.Background()) {
		t.Errorf("HasSlug(empty): got true, want false")
	}
}

func TestSlugFromContext_NilCtx(t *testing.T) {
	if got := SlugFromContext(nil); got != "" {
		t.Errorf("SlugFromContext(nil): got %q, want empty", got)
	}
}

func TestWithSlug_NilCtxUsesBackground(t *testing.T) {
	ctx := WithSlug(nil, "acme")
	if got := SlugFromContext(ctx); got != "acme" {
		t.Errorf("WithSlug(nil,...) round-trip: got %q, want acme", got)
	}
}

func TestWithSlug_EmptySlugIsNoSignal(t *testing.T) {
	// Empty slug is permitted (middleware uses it for "no signal") but
	// HasSlug must report false so downstream RPC calls skip X-Tenant-Id.
	ctx := WithSlug(context.Background(), "")
	if HasSlug(ctx) {
		t.Errorf("HasSlug after empty-set: got true, want false")
	}
}

func TestWithTenantID_RoundTrip(t *testing.T) {
	ctx := WithTenantID(context.Background(), "acme-corp")
	if got := TenantIDFromContext(ctx); got != "acme-corp" {
		t.Errorf("TenantIDFromContext: got %q, want acme-corp", got)
	}
}

func TestTenantIDFromContext_Empty(t *testing.T) {
	if got := TenantIDFromContext(context.Background()); got != "" {
		t.Errorf("TenantIDFromContext(empty): got %q, want empty", got)
	}
}

func TestTenantIDFromContext_NilCtx(t *testing.T) {
	if got := TenantIDFromContext(nil); got != "" {
		t.Errorf("TenantIDFromContext(nil): got %q, want empty", got)
	}
}

func TestWithTenantID_NilCtxUsesBackground(t *testing.T) {
	ctx := WithTenantID(nil, "acme-corp")
	if got := TenantIDFromContext(ctx); got != "acme-corp" {
		t.Errorf("WithTenantID(nil,...) round-trip: got %q, want acme-corp", got)
	}
}

// TestWithSlugAndTenantID_DoNotClobber confirms the two keys are independent —
// storing one must not corrupt the other. Critical because both ride on the
// same ctx in production (ResolveTenantSlug sets slug, TenantContext sets id).
func TestWithSlugAndTenantID_DoNotClobber(t *testing.T) {
	ctx := WithSlug(context.Background(), "acme")
	ctx = WithTenantID(ctx, "acme-corp")
	if got := SlugFromContext(ctx); got != "acme" {
		t.Errorf("slug after id-set: got %q, want acme", got)
	}
	if got := TenantIDFromContext(ctx); got != "acme-corp" {
		t.Errorf("id after slug-set: got %q, want acme-corp", got)
	}
}

func TestDefaultTenantIDConstant(t *testing.T) {
	if DefaultTenantID != "default" {
		t.Errorf("DefaultTenantID = %q, want 'default'", DefaultTenantID)
	}
}
