// Package middleware — server-side tenant slug resolver (Phase B3b.2a).
//
// ResolveTenantSlug extracts the per-request tenant slug via the §5
// three-layer fallback and stores it in the request context (tenant.WithSlug)
// so the cs-user RPC client can forward it as the X-Tenant-Id header.
//
// Unlike cs-user's ResolveTenant, the server does NO database lookup — it
// just extracts the slug and lets cs-user resolve it against the tenants
// table. This keeps tenant data ownership in cs-user (ADR D1).
//
// Resolution order (first hit wins, design §5.1):
//
//  1. X-Tenant-Id header — set by an upstream proxy / gateway that already
//     ran tenant resolution. Trusted because the server's own auth layer
//     (OptionalAuth / RequireAuth) runs after this middleware.
//  2. cs_tenant_slug cookie — browser sticky selection after the Try-3
//     picker. Same name + semantics as the cs-user middleware so a single
//     cookie drives both sides.
//  3. Host subdomain — acme.example.com → slug "acme". Only runs when
//     apexDomains is non-empty (disabled in local dev).
//
// On all-miss the middleware stores an empty slug (= "no signal") — RPC
// calls then omit X-Tenant-Id and cs-user falls back to its own default
// tenant. The middleware never aborts the chain.
package middleware

import (
	"net"
	"strings"

	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/gin-gonic/gin"
)

const (
	// TenantIDHeader is the trusted slug carrier for the resolved tenant.
	// Same name as cs-user's middleware so the server can pass the header
	// through to cs-user verbatim on outbound RPC calls.
	TenantIDHeader = "X-Tenant-Id"

	// TenantSlugCookie mirrors cs-user's cs_tenant_slug — single cookie
	// drives both sides.
	TenantSlugCookie = "cs_tenant_slug"
)

// ResolveTenantSlug returns a gin middleware that resolves the tenant slug
// via §5 fallback and stores it in the request context. apexDomains may be
// nil (disables Host subdomain layer — local dev default).
//
// The middleware never aborts; on all-miss it stores an empty slug which
// RPC forwarding interprets as "no signal" (omit X-Tenant-Id header).
func ResolveTenantSlug(apexDomains []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := resolveTenantSlugFromSignals(c, apexDomains)
		c.Request = c.Request.WithContext(tenant.WithSlug(c.Request.Context(), slug))
		c.Set("tenant_slug", slug)
		c.Next()
	}
}

// resolveTenantSlugFromSignals walks the three layers in §5 order. Returns
// "" when no layer produced a hit.
func resolveTenantSlugFromSignals(c *gin.Context, apexDomains []string) string {
	// Layer 1: X-Tenant-Id header.
	if raw := strings.TrimSpace(c.GetHeader(TenantIDHeader)); raw != "" {
		return raw
	}

	// Layer 2: cs_tenant_slug cookie.
	if raw, err := c.Cookie(TenantSlugCookie); err == nil {
		if v := strings.TrimSpace(raw); v != "" {
			return v
		}
	}

	// Layer 3: Host subdomain (only if apexDomains configured).
	if len(apexDomains) > 0 {
		if slug := slugFromHost(c.Request.Host, apexDomains); slug != "" {
			return slug
		}
	}

	return ""
}

// slugFromHost extracts the tenant slug from a Host header by stripping the
// matching apex domain. Returns "" if no apex matches or the host IS an
// apex (no subdomain).
//
// Mirrors cs-user/internal/tenant/resolver.go slugFromHost behavior:
//   - tolerates host:port via net.SplitHostPort
//   - tolerates FQDN trailing dot
//   - for nested subdomains (foo.acme.example.com with apex example.com),
//     returns the label IMMEDIATELY below apex ("acme") — matches cs-user's
//     LastIndex-based extraction so both sides agree on the slug.
func slugFromHost(host string, apexDomains []string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)

	for _, apex := range apexDomains {
		apex = strings.ToLower(strings.TrimSpace(apex))
		apex = strings.TrimSuffix(apex, ".")
		if a, _, err := net.SplitHostPort(apex); err == nil {
			apex = a
		}
		if apex == "" {
			continue
		}
		if host == apex {
			continue // host IS the apex — no subdomain
		}
		if strings.HasSuffix(host, "."+apex) {
			sub := strings.TrimSuffix(host, "."+apex)
			// Label immediately below apex = last segment of sub. For
			// "foo.acme" that's "acme"; for plain "acme" it's "acme".
			if i := strings.LastIndex(sub, "."); i >= 0 {
				return sub[i+1:]
			}
			return sub
		}
	}
	return ""
}
