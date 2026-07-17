// Package tenant implements cs-user's tenant-resolution logic (Phase B3).
//
// The Resolver exposes the three primitives behind the §5 three-layer
// fallback documented in docs/identity-tenant/MULTI_TENANCY_DESIGN.md:
//
//  1. ResolveBySlug        — subdomain path (Try 1)
//  2. ResolveByEmailDomain — email-domain path (Try 2)
//  3. (explicit selection) — Try 3 is a UI flow; Resolver returns
//     ErrTenantNotFound / ErrAmbiguousTenant so the caller knows to fall
//     through. The Resolver itself does not own UI.
//
// Scope of B3 first iteration: pure read-side logic + email_domains typed
// reader (B1 left email_domains as an opaque TEXT-holding JSON string;
// B3 is where that field gets a typed parser per the B1 known-limitations
// note). HTTP middleware / cookie / session / Casdoor redirect wiring is
// deferred to B3b.
//
// All queries filter status='active' — suspended / deleted tenants never
// resolve, matching §5.1 "WHERE slug = 'acme' AND status = 'active'".
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// Sentinel errors. Callers (handlers / middleware, landed in B3b) translate
// these to HTTP behavior:
//
//   - ErrTenantNotFound   → fall through to next Try layer, eventually 200
//     with empty body so the frontend tenant picker
//     (Try 3) takes over.
//   - ErrAmbiguousTenant  → same fall-through; design §5.3 场景 B calls for
//     the picker UI in this case.
var (
	ErrTenantNotFound  = errors.New("tenant: not found")
	ErrAmbiguousTenant = errors.New("tenant: ambiguous (multiple tenants match)")
)

// Resolver answers "which tenant does this request belong to?" given the
// three §5 inputs (host, email, explicit slug). It owns no state — safe to
// share across goroutines once constructed.
type Resolver struct {
	db *gorm.DB
}

// NewResolver binds a Resolver to the supplied gorm pool. Callers own the
// pool's lifecycle.
func NewResolver(db *gorm.DB) *Resolver {
	return &Resolver{db: db}
}

// ResolveBySlug returns the active tenant whose slug matches. Slug is the
// URL-safe identifier carried in the subdomain (Try 1) and the explicit
// picker (Try 3). Lookup is case-insensitive on the caller side: this
// method does NOT lowercase — slugs are stored lowercased by convention
// (B1 schema comment: "global URL-safe slug [a-z0-9-]{3,32}"), so callers
// should normalize before calling.
func (r *Resolver) ResolveBySlug(ctx context.Context, slug string) (*models.Tenant, error) {
	if r == nil || r.db == nil {
		return nil, ErrTenantNotFound
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrTenantNotFound
	}
	var tn models.Tenant
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? OR slug = ?", slug, slug).
		Where("status = ?", "active").
		First(&tn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant: query by slug %q: %w", slug, err)
	}
	return &tn, nil
}

// ResolveByEmailDomain returns the active tenant(s) whose email_domains JSON
// array contains the supplied domain. Behavior:
//
//   - exactly one match  → that tenant, nil
//   - zero matches       → nil, ErrTenantNotFound (caller falls to Try 3)
//   - two or more        → nil, ErrAmbiguousTenant (caller falls to Try 3;
//     design §5.3 场景 B "请选择您所属组织")
//
// email_domains is stored as a TEXT column holding a JSON array-of-strings
// (B1 schema decision; opaque until B3). We can't push the contains check
// into Postgres with a typed query because the column is JSON text, not
// JSONB / TEXT[]. Instead we load all active tenants and filter in Go via
// ParseEmailDomains. This is acceptable at B3 scale (tenant count is in the
// 10s, not 10k+); if the table grows large, a B-later migration converts
// email_domains to JSONB / TEXT[] and this method becomes a single WHERE
// clause (MULTI_TENANCY_DESIGN §6.5.1 / B1 known-limitations note).
func (r *Resolver) ResolveByEmailDomain(ctx context.Context, domain string) (*models.Tenant, error) {
	if r == nil || r.db == nil {
		return nil, ErrTenantNotFound
	}
	domain = normalizeEmailDomain(domain)
	if domain == "" {
		return nil, ErrTenantNotFound
	}
	var all []models.Tenant
	if err := r.db.WithContext(ctx).
		Where("status = ?", "active").
		Find(&all).Error; err != nil {
		return nil, fmt.Errorf("tenant: query for email-domain lookup: %w", err)
	}
	var matches []*models.Tenant
	for i := range all {
		for _, d := range ParseEmailDomains(&all[i]) {
			if d == domain {
				tn := all[i]
				matches = append(matches, &tn)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, ErrTenantNotFound
	case 1:
		return matches[0], nil
	default:
		return nil, ErrAmbiguousTenant
	}
}

// ResolveByEmail extracts the domain from an email address and delegates to
// ResolveByEmailDomain. Malformed / empty email → ErrTenantNotFound (no
// fallthrough error, just "no signal from this layer").
func (r *Resolver) ResolveByEmail(ctx context.Context, email string) (*models.Tenant, error) {
	domain := domainFromEmail(email)
	if domain == "" {
		return nil, ErrTenantNotFound
	}
	return r.ResolveByEmailDomain(ctx, domain)
}

// ListByEmailDomain returns every active tenant whose email_domains array
// contains the domain extracted from the supplied email. Used by the
// /api/internal/tenants/resolve-by-email handler (B3b.2b-step2) to populate
// the picker candidates when ResolveByEmail returns ErrAmbiguousTenant.
//
// Empty / malformed email → empty list, nil error. The handler treats
// "empty list" the same as ErrTenantNotFound (Try 2 miss → fall through).
func (r *Resolver) ListByEmailDomain(ctx context.Context, email string) ([]*models.Tenant, error) {
	domain := domainFromEmail(email)
	if domain == "" {
		return nil, nil
	}
	if r == nil || r.db == nil {
		return nil, nil
	}
	var all []models.Tenant
	if err := r.db.WithContext(ctx).
		Where("status = ?", "active").
		Find(&all).Error; err != nil {
		return nil, fmt.Errorf("tenant: query for list-by-email-domain: %w", err)
	}
	var matches []*models.Tenant
	for i := range all {
		for _, d := range ParseEmailDomains(&all[i]) {
			if d == domain {
				tn := all[i]
				matches = append(matches, &tn)
				break
			}
		}
	}
	return matches, nil
}

// ResolveFromHost extracts the first subdomain segment of host (relative to
// one of apexDomains) and calls ResolveBySlug. host may be in "host:port"
// form (port stripped). apexDomains is the list of bare domains the
// deployment serves — e.g. {"cs-user.example.com"} or {"localhost:8080"}.
//
// Behavior:
//
//   - host has port        → port stripped before parsing
//   - host ends with ".<apex>" → segment immediately before apex is slug
//   - host equals an apex  → no subdomain signal → ErrTenantNotFound
//   - host matches no apex → ErrTenantNotFound (cross-deployment request)
//
// Trailing dots (FQDN form like "acme.cs-user.example.com.") are tolerated.
func (r *Resolver) ResolveFromHost(ctx context.Context, host string, apexDomains []string) (*models.Tenant, error) {
	slug := slugFromHost(host, apexDomains)
	if slug == "" {
		return nil, ErrTenantNotFound
	}
	return r.ResolveBySlug(ctx, slug)
}

// ParseEmailDomains decodes the JSON-array-of-strings stored in the
// email_domains TEXT column. B1 stored it as opaque JSON text; B3 introduces
// this typed reader (see B1 known-limitations note "B2/B3 引入 typed
// reader"). Malformed JSON → empty slice (graceful degradation — same
// convention as EmploymentIdentity.Attributes: the column is opaque at the
// DB layer, app layer tolerates any string).
func ParseEmailDomains(t *models.Tenant) []string {
	if t == nil {
		return nil
	}
	raw := strings.TrimSpace(t.EmailDomains)
	if raw == "" {
		return nil
	}
	// Default '[]' from B1 schema → empty slice, no error.
	if raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	// Lowercase + trim each entry defensively; tenants CRUD (Phase C) will
	// normalize at write time but readers stay defensive.
	for i, d := range out {
		out[i] = normalizeEmailDomain(d)
	}
	return out
}

// normalizeEmailDomain lowercases + trims a domain. Empty input → "".
func normalizeEmailDomain(d string) string {
	return strings.ToLower(strings.TrimSpace(d))
}

// domainFromEmail extracts the lowercased domain from an email address.
// Returns "" for malformed input. Conservative: relies on strings.Split
// rather than a regex, since the email format we accept is the simple
// "local@domain" shape; weird but RFC-valid addresses (quoted local parts,
// multiple @) are not supported — they'd come from admin/IdP flows that
// pre-validate.
func domainFromEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ""
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	return email[at+1:]
}

// slugFromHost extracts the subdomain slug from host relative to one of the
// apexDomains. Returns "" when host has no subdomain signal or matches no
// apex. See ResolveFromHost for the full contract.
func slugFromHost(host string, apexDomains []string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	// Strip port if present. net.SplitHostPort tolerates plain hosts without
	// ports by returning the input + error; we fall back to the raw host in
	// that case.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".") // tolerate FQDN trailing dot

	for _, apex := range apexDomains {
		apex = strings.ToLower(strings.TrimSpace(apex))
		if apex == "" {
			continue
		}
		// Strip port from apex too (caller may pass "cs-user.example.com:8080").
		if a, _, err := net.SplitHostPort(apex); err == nil {
			apex = a
		}
		apex = strings.TrimSuffix(apex, ".")
		if host == apex {
			return "" // bare apex, no subdomain signal
		}
		suffix := "." + apex
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		head := strings.TrimSuffix(host, suffix)
		// head may itself contain dots (e.g. "login.acme" for
		// login.acme.cs-user.example.com); the slug is the LAST segment of
		// head, since slugs are single-label by B1 convention.
		if idx := strings.LastIndex(head, "."); idx >= 0 {
			head = head[idx+1:]
		}
		if head == "" {
			return ""
		}
		return head
	}
	return ""
}
