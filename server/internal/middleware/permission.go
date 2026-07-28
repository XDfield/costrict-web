// Package middleware — permission middlewares (Phase C1).
//
// Three gate middlewares that read AuthClaims (populated by RequireAuth /
// OptionalAuth from the JWT) and translate the Phase C1 claims
// (platform_admin / platform_scope / tenant_roles) into HTTP allow/deny:
//
//   - RequirePlatformAdmin(scope...) — caller must have platform_admin=true
//     AND, when scope args are passed, a scope that appears in the args.
//   - RequireTenantAdmin(roles...)   — caller must have at least one of the
//     named roles in their tenant_roles claim. Platform admins short-circuit
//     to allowed (platform scope is super-tenant by design — §14.3).
//   - RequireTenantMember            — caller must have a non-empty TenantID
//     (every authenticated cs-user token carries it; Casdoor pre-cutover
//     tokens fall back to "default" via TenantContext). Used as the baseline
//     "any tenant member" gate below admin tiers.
//
// Auth contract:
//
//   - Missing AuthClaimsKey entry → 401 (treat as unauthenticated, even if
//     the route is mounted under OptionalAuth — caller of an admin route
//     must have authenticated).
//   - AuthClaims exists but required claim absent → 403 (authenticated but
//     insufficient). Distinguishes "log in" from "you can't do this".
//
// Pre-cutover behavior: Casdoor-issued tokens carry none of the Phase C1
// claims, so all three middlewares deny. Mount admin routes behind these
// middlewares ONLY post-cutover, or pair with a feature-flag bypass during
// the 灰度 window (A8).

package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequirePlatformAdmin returns a middleware that allows only platform admins.
// When scopeArgs is non-empty, the caller's PlatformScope must appear in the
// list (e.g. RequirePlatformAdmin("full","support") excludes "read_only").
// Empty scopeArgs means "any platform admin regardless of scope".
//
// Use this for cross-tenant admin endpoints (e.g. /api/platform/users).
func RequirePlatformAdmin(scopeArgs ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(scopeArgs))
	for _, s := range scopeArgs {
		allowed[s] = struct{}{}
	}
	return func(c *gin.Context) {
		ac, ok := authClaimsFromGin(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if !ac.PlatformAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "platform admin required"})
			return
		}
		if len(allowed) > 0 {
			if _, ok := allowed[ac.PlatformScope]; !ok {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "platform scope insufficient"})
				return
			}
		}
		c.Next()
	}
}

// RequireTenantAdmin returns a middleware that allows callers holding at
// least one of the named roles on their current tenant. Platform admins
// (PlatformAdmin=true) short-circuit to allowed — platform scope is
// super-tenant per §14.3, so the platform-admin path is intentional.
//
// Typical usage:
//
//	adminGroup := r.Group("/", RequireTenantAdmin("owner", "admin"))
//
// Pass no role args to mean "any tenant_admin role is fine" (i.e. the
// caller's TenantRoles is non-empty).
func RequireTenantAdmin(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(c *gin.Context) {
		ac, ok := authClaimsFromGin(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		// Platform admins bypass the tenant-admin check.
		if ac.PlatformAdmin {
			c.Next()
			return
		}
		if len(ac.TenantRoles) == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "tenant admin required"})
			return
		}
		if len(allowed) == 0 {
			// No role args → any tenant_admin role is fine.
			c.Next()
			return
		}
		for _, r := range ac.TenantRoles {
			if _, ok := allowed[r]; ok {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "tenant role insufficient"})
	}
}

// RequireTenantMember returns a middleware that allows any authenticated
// caller scoped to a tenant. Useful as the baseline gate below admin tiers
// — every cs-user-signed token carries tenant_id, so this is mostly a
// belt-and-braces guard against accidentally mounting a tenant-scoped route
// outside the authenticated chain.
func RequireTenantMember() gin.HandlerFunc {
	return func(c *gin.Context) {
		ac, ok := authClaimsFromGin(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if ac.TenantID == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "tenant membership required"})
			return
		}
		c.Next()
	}
}

// authClaimsFromGin reads the AuthClaims entry set by RequireAuth /
// OptionalAuth. Returns (claims, true) on hit, (zero, false) when the entry
// is absent (unauthenticated request) or holds an unexpected type
// (defensive — never panic on a context-shape mismatch).
func authClaimsFromGin(c *gin.Context) (AuthClaims, bool) {
	raw, exists := c.Get(AuthClaimsKey)
	if !exists {
		return AuthClaims{}, false
	}
	ac, ok := raw.(AuthClaims)
	return ac, ok
}
