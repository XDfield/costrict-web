// Package tenant — context helpers (B3b) + query scope primitive (B5).
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
	"gorm.io/gorm"
)

// DefaultTenantID is the canonical ID of the bootstrap tenant created by
// cs-user's A6/B1 migration. Phase A and any unscoped (no resolver signal)
// request resolves to this. Read paths fall back to this when ctx carries
// no tenant — keeps single-tenant behavior correct during the pre-cutover
// window and for background tasks that have no request ctx.
const DefaultTenantID = "default"

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

// IDFromContext returns the canonical tenant_id (tenants.tenant_id PK) for
// the tenant resolved in ctx, or DefaultTenantID when ctx carries no tenant.
// This is the helper B5 query scopes call — it always returns a non-empty
// ID, so `WHERE tenant_id = ?` clauses get a real value and Phase A
// single-tenant behavior (all rows = "default") stays correct without
// forcing every caller to handle the "no signal" case.
func IDFromContext(ctx context.Context) string {
	if t := FromContext(ctx); t != nil && t.TenantID != "" {
		return t.TenantID
	}
	return DefaultTenantID
}

// Scope returns a GORM scope function that applies `WHERE tenant_id = ?`
// using the tenant resolved in ctx (IDFromContext, with DefaultTenantID
// fallback). Pass to `db.Scopes(tenant.Scope(ctx))` on tenant-scoped tables
// (users / user_auth_identities / employment_identities — anything carrying
// the tenant_id column added by B2).
//
// Single-tenant safety: when no middleware populated the tenant (background
// tasks, tests, pre-cutover requests), the scope falls back to
// DefaultTenantID — querying returns the same set as the unscoped pre-B5
// behavior, so this is safe to apply incrementally without a flag.
//
// Cross-tenant queries (platform_admin) must NOT use this scope — they need
// an explicit `CrossTenantScope` or a SECURITY DEFINER function per §10.2.
// That's a follow-up tracked under B6 / platform_admin API.
func Scope(ctx context.Context) func(*gorm.DB) *gorm.DB {
	tenantID := IDFromContext(ctx)
	return func(db *gorm.DB) *gorm.DB {
		return db.Where("tenant_id = ?", tenantID)
	}
}
