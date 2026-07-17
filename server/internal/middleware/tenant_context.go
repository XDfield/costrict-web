// Package middleware — request-ctx tenant_id hydration (Phase B4).
//
// TenantContext reads the tenant_id claim from AuthClaims (populated by
// OptionalAuth / RequireAuth) and stores it in the request context via
// tenant.WithTenantID. Downstream consumers — most importantly B5's
// tenantScope(ctx) query helper — can then read tenant.TenantIDFromContext
// to scope queries automatically without re-parsing the JWT or carrying the
// gin context through the GORM layer.
//
// Fallback semantics: when AuthClaimsKey is absent (no token supplied), or
// AuthClaims.TenantID is empty (Casdoor-issued pre-cutover token), the
// middleware falls back to tenant.DefaultTenantID. This keeps Phase A
// single-tenant behavior correct without forcing every caller to handle the
// "no tenant signal" case.
//
// Must run AFTER RequireAuth/OptionalAuth (which populates AuthClaimsKey).
// Ordering relative to TenantMatch does not matter — they read disjoint
// fields (TenantID vs TenantSlug) — but both are typically registered
// together right after the auth layer.
package middleware

import (
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

// TenantContext returns a gin middleware that hydrates tenant_id from the
// JWT claims into the request context. See package doc for fallback rules.
func TenantContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := tenant.DefaultTenantID
		if raw, exists := c.Get(AuthClaimsKey); exists {
			if ac, ok := raw.(AuthClaims); ok && ac.TenantID != "" {
				tenantID = ac.TenantID
			}
		}
		ctx := tenant.WithTenantID(c.Request.Context(), tenantID)
		c.Request = c.Request.WithContext(ctx)
		c.Set("tenant_id", tenantID)
		c.Next()
	}
}
