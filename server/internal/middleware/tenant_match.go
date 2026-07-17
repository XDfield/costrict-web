// Package middleware — cross-tenant mismatch detection (Phase B3b.2c).
//
// TenantMatch compares the tenant_slug claim embedded in the cs-user-signed JWT
// (Phase A7) against the runtime-resolved slug stored in the request context
// by ResolveTenantSlug. When both are populated and differ, the request is
// treated as a cross-tenant access attempt (cookie/JWT stolen from another
// tenant) and aborted with HTTP 401.
//
// Pre-cutover behavior: Casdoor-issued JWTs do NOT carry the tenant_slug claim,
// so AuthClaims.TenantSlug is empty. The middleware skips comparison in that
// case (graceful — same as "no runtime signal"). This keeps the gate dormant
// until cs-user token issuance is fully lit up.
//
// Skip conditions (both must hold for the gate to fire):
//   - JWT carries a non-empty tenant_slug claim (cs-user-signed).
//   - Request ctx carries a non-empty slug (subdomain/cookie/header hit).
//
// When either is empty, the middleware is a no-op. This matches the "empty
// slug = no signal" contract used throughout the tenant resolver chain.
package middleware

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

// TenantMatch returns a gin middleware that enforces JWT-vs-runtime tenant slug
// parity. Must run AFTER RequireAuth/OptionalAuth (which populates AuthClaimsKey
// on the ctx) and AFTER ResolveTenantSlug (which populates the slug ctx value).
//
// The middleware reads:
//   - AuthClaims.TenantSlug via the AuthClaimsKey gin-context entry.
//   - Runtime slug via tenant.SlugFromContext(c.Request.Context()).
//
// On mismatch it clears the auth cookie (force re-login) and aborts with 401.
// On any skip condition (either slug empty, no auth claims at all) it is a
// pass-through.
func TenantMatch() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, exists := c.Get(AuthClaimsKey)
		if !exists {
			c.Next()
			return
		}
		authClaims, ok := raw.(AuthClaims)
		if !ok {
			c.Next()
			return
		}
		jwtSlug := authClaims.TenantSlug
		if jwtSlug == "" {
			// Pre-cutover Casdoor token or A7 token issued without a slug
			// (e.g. server ran without runtime slug signal at reissue time).
			// Skip — comparison impossible without both sides populated.
			c.Next()
			return
		}
		runtimeSlug := tenant.SlugFromContext(c.Request.Context())
		if runtimeSlug == "" {
			// No resolver signal (apexDomains unset + no cookie/header).
			// Skip — runtime has no opinion to disagree with.
			c.Next()
			return
		}
		if jwtSlug != runtimeSlug {
			// Cross-tenant access: JWT was issued for one tenant but the
			// request is hitting another tenant's surface. Treat as stolen
			// token — clear cookie so the browser re-authenticates.
			ClearAuthCookie(c)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "tenant mismatch",
			})
			return
		}
		c.Next()
	}
}
