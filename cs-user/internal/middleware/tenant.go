// Package middleware — tenant resolver (Phase B3b.1).
//
// ResolveTenant runs early in the gin chain and pins the request to a single
// *models.Tenant via the §5 three-layer fallback. The resolved tenant is
// stashed in the request context (tenant.WithTenant) so handlers can pull it
// via tenant.FromContext / tenant.FromGin without reaching into gin-specific
// storage or DB layer.
//
// Resolution order (first hit wins, design §5.1):
//
//  1. X-Tenant-Id header — internal RPC carrier (server → cs-user). Trusted
//     because the route group is already gated by RequireInternalToken.
//  2. cs_tenant_slug cookie — browser-side sticky selection after the user
//     picked a tenant via the Try-3 picker. The cookie holds the slug, NOT
//     the tenant_id, because the slug is what the subdomain also resolves
//     to and remains stable across tenant_id migration.
//  3. Host subdomain — acme.cs-user.example.com → slug "acme". Only runs
//     when cfg.Tenant.ApexDomains is non-empty (disabled in local dev).
//
// On all-miss the middleware falls back to the default tenant row
// (slug="default"). This matches the design's "default-tenant fallback"
// directive so unauthenticated traffic (JWKS, healthz, swagger) and any path
// the middleware runs on keeps working even if upstream signals are absent.
//
// On DB error during fallback (e.g. default tenant row missing — bootstrap
// failure), the middleware records the error on the gin context key
// "tenant_resolve_error" and continues with no tenant stored. Handlers that
// require a tenant should check tenant.HasTenant and surface a 503.
package middleware

import (
	"errors"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	// TenantIDHeader is the trusted internal carrier for the resolved
	// tenant_id. Costrict-web sets this after running its own resolution
	// (subdomain + JWT tenant claim) so cs-user doesn't have to re-derive.
	TenantIDHeader = "X-Tenant-Id"

	// TenantSlugCookie is the browser-side carrier for the slug the user
	// picked via the Try-3 tenant picker. Set by the server's frontend
	// after picker submit; SameSite=Lax so cross-site IdP redirects carry
	// it through the OAuth callback chain (B3b.2 will enforce that).
	TenantSlugCookie = "cs_tenant_slug"

	// DefaultTenantSlug is the slug of the always-present fallback tenant
	// (bootstrapped by Phase A6 migration).
	DefaultTenantSlug = "default"
)

// ResolveTenant returns a gin middleware that resolves the request to a
// tenant via §5 fallback and stores it in the request context. resolver must
// be non-nil; apexDomains may be nil (disables Host subdomain resolution).
//
// The middleware never aborts the chain — even on hard DB error it records
// the error via c.Set("tenant_resolve_error", err) and continues, so a
// missing default-tenant row degrades to 503 at the handler layer rather
// than mysterious 404s from gin's NoRoute handler.
func ResolveTenant(resolver *tenant.Resolver, apexDomains []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			// No resolver wired — leave context empty, let handlers
			// fall back to default-tenant themselves or surface 503.
			c.Next()
			return
		}

		ctx := c.Request.Context()
		resolved := resolveTenantFromSignals(c, resolver, apexDomains)
		if resolved == nil {
			// All three signals missed OR returned ErrTenantNotFound —
			// fall back to the default tenant row. Hard DB error OR
			// missing default row both surface via context for the
			// handler to translate to 503.
			t, err := resolver.ResolveBySlug(ctx, DefaultTenantSlug)
			if err != nil {
				c.Set("tenant_resolve_error", err)
				c.Next()
				return
			}
			resolved = t
		} else {
			// A later layer succeeded — any tenant_resolve_error set by an
			// earlier layer's hard-DB-error path is now stale (the request
			// DID resolve). Clear it so TenantFromGin doesn't surface a
			// misleading (tenant, err) pair.
			c.Set("tenant_resolve_error", nil)
		}

		// Stash into the request context (so net/http handlers also see
		// it via tenant.FromContext) and re-inject the augmented context.
		c.Request = c.Request.WithContext(tenant.WithTenant(ctx, resolved))
		c.Set("tenant", resolved)
		c.Next()
	}
}

// TenantFromGin returns the resolved tenant stored by ResolveTenant, or nil
// if the middleware didn't run / didn't find a tenant. Handlers should use
// this rather than tenant.FromContext(c) directly so they don't have to
// import two packages.
//
// Returns (tenant, err) where err is any non-ErrTenantNotFound error the
// middleware surfaced via the "tenant_resolve_error" context key. Handlers
// that require a tenant should treat nil+nil-err as "default missing" and
// return 503 themselves; nil+err as 503 with the err logged.
func TenantFromGin(c *gin.Context) (*models.Tenant, error) {
	if c == nil {
		return nil, nil
	}
	v, _ := c.Get("tenant")
	t, _ := v.(*models.Tenant)
	if e, ok := c.Get("tenant_resolve_error"); ok {
		if err, _ := e.(error); err != nil {
			return t, err
		}
	}
	return t, nil
}

// resolveTenantFromSignals walks the three layers in §5 order. Returns nil
// when no layer produced a hit (caller falls back to default tenant).
//
// Each layer swallows ErrTenantNotFound silently (that's the "no signal
// from this layer" sentinel) but surfaces other errors via c.Set so the
// handler can return 503 instead of silently defaulting.
func resolveTenantFromSignals(c *gin.Context, resolver *tenant.Resolver, apexDomains []string) *models.Tenant {
	ctx := c.Request.Context()

	// Layer 1: X-Tenant-Id header.
	if raw := c.GetHeader(TenantIDHeader); raw != "" {
		t, err := resolver.ResolveBySlug(ctx, raw)
		if err == nil {
			return t
		}
		if !errors.Is(err, tenant.ErrTenantNotFound) && !errors.Is(err, gorm.ErrRecordNotFound) {
			c.Set("tenant_resolve_error", err)
		}
	}

	// Layer 2: cs_tenant_slug cookie.
	if raw, err := c.Cookie(TenantSlugCookie); err == nil && raw != "" {
		t, err := resolver.ResolveBySlug(ctx, raw)
		if err == nil {
			return t
		}
		if !errors.Is(err, tenant.ErrTenantNotFound) && !errors.Is(err, gorm.ErrRecordNotFound) {
			c.Set("tenant_resolve_error", err)
		}
	}

	// Layer 3: Host subdomain (only if apexDomains configured).
	if len(apexDomains) > 0 {
		t, err := resolver.ResolveFromHost(ctx, c.Request.Host, apexDomains)
		if err == nil {
			return t
		}
		if !errors.Is(err, tenant.ErrTenantNotFound) && !errors.Is(err, tenant.ErrAmbiguousTenant) {
			c.Set("tenant_resolve_error", err)
		}
		// ErrAmbiguousTenant on subdomain is unusual (means the slug
		// itself was matched ambiguously by tenant_id/slug OR) but
		// tolerated — fall through to default-tenant.
	}

	return nil
}
