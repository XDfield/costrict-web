// Package tenant carries the per-request tenant slug on the server side.
//
// Mirror of cs-user/internal/tenant/context.go but stores only the SLUG (not
// a full *models.Tenant pointer) — the server deliberately doesn't duplicate
// the tenants table; it forwards the slug to cs-user as the X-Tenant-Id header
// and lets cs-user do the DB lookup. This keeps tenant data ownership in one
// place (ADR D1 — cs-user owns users + tenants).
//
// Lifecycle:
//   - middleware.ResolveTenantSlug extracts the slug from X-Tenant-Id header /
//     cs_tenant_slug cookie / Host subdomain and stores it via WithSlug.
//   - user.RPCClient.do / RPCWriter.doCapture read it via SlugFromContext and
//     set the X-Tenant-Id outbound header so cs-user sees the same signal.
//   - Phase B3b.2c (cross-tenant mismatch detection) will add JWT tenant_id
//     comparison against the cookie slug — for now we just forward.
package tenant

import "context"

type ctxKey struct{}

// WithSlug returns a new ctx carrying the tenant slug. Empty slug is allowed
// and represents "no signal" (the middleware uses it when no layer matched).
func WithSlug(ctx context.Context, slug string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, slug)
}

// SlugFromContext returns the tenant slug stored by WithSlug, or "" if absent.
// Returns "" for nil ctx (defensive — gin sometimes passes nil c.Request).
func SlugFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

// HasSlug reports whether the ctx carries a non-empty tenant slug.
func HasSlug(ctx context.Context) bool {
	return SlugFromContext(ctx) != ""
}
