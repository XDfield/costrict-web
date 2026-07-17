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
