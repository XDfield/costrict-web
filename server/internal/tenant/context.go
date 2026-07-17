// Package tenant carries the per-request tenant slug AND tenant_id on the
// server side.
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
//   - middleware.TenantContext (Phase B4) extracts tenant_id from the JWT and
//     stores it via WithTenantID. The fallback "default" applies when the JWT
//     carries no tenant_id claim (pre-cutover Casdoor tokens).
//   - B5's tenantScope(ctx) helper will read TenantIDFromContext to scope
//     queries automatically (`WHERE tenant_id = ?`).
//   - middleware.TenantMatch (Phase B3b.2c) cross-checks the JWT's tenant_slug
//     claim against SlugFromContext for stolen-cookie detection.
package tenant

import "context"

type ctxKey struct{}

type tenantIDKey struct{}

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

// WithTenantID returns a new ctx carrying the tenant_id (the tenants table PK
// — e.g. "default" / "acme-corp"). Distinct from the slug (URL-friendly key)
// because B5's tenantScope helper needs the canonical identifier for
// `WHERE tenant_id = ?`, not the slug. Empty is allowed but the TenantContext
// middleware always falls back to "default" before storing.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// TenantIDFromContext returns the tenant_id stored by WithTenantID, or ""
// when absent. Callers wanting the production-safe value should fall back to
// "default" themselves — the middleware already does this so most call sites
// see a non-empty value, but defensive code (e.g. background tasks with no
// request ctx) may see "".
func TenantIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(tenantIDKey{}).(string)
	return v
}

// DefaultTenantID is the canonical ID of the bootstrap tenant created by
// cs-user's A6/B1 migration. Phase A and any unscoped (no JWT / no resolver
// signal) Phase B request resolves to this.
const DefaultTenantID = "default"
