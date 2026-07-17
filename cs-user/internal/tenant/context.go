// Package tenant — context helpers (B3b).
//
// These helpers let a gin middleware stash the resolved tenant into the
// request context and let downstream handlers pull it back out without
// reaching into gin-specific storage. The same context key type works for
// both net/http and gin (gin.Context implements the standard
// context.Context interface via its Request().Context()).
package tenant

import (
	"context"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// ctxKey is unexported so callers can't construct a colliding key. The
// stored value is always *models.Tenant (never nil — see WithTenant).
type ctxKey struct{}

// WithTenant returns a new context carrying the supplied tenant. Pass nil to
// explicitly mark "no tenant resolved" (downstream sees HasTenant false +
// FromContext returns nil) — useful for tests that want to exercise the
// "default tenant" fallback path without spinning up the resolver.
//
// A nil tenant here means "no signal" and is the same value the middleware
// sets when no X-Tenant-Id / cookie / subdomain signal was present. It does
// NOT mean "default tenant" — the middleware always falls back to the
// default tenant row before storing, so handlers in production see a real
// tenant pointer. Tests can opt into the "no signal" path explicitly.
func WithTenant(ctx context.Context, t *models.Tenant) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, t)
}

// FromContext returns the tenant stored in ctx, or nil if none. Combine
// with HasTenant when nil-vs-absent matters (rare — most callers just want
// the pointer and fall back to default-tenant when nil).
func FromContext(ctx context.Context) *models.Tenant {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKey{}).(*models.Tenant)
	return v
}

// HasTenant reports whether ctx carries a tenant. False means either "no
// middleware ran" or "middleware ran but couldn't resolve" — same outcome
// for callers: fall back to the default tenant.
func HasTenant(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(ctxKey{}).(*models.Tenant)
	return ok
}
