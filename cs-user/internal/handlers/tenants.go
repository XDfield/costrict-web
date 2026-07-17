// Package handlers — tenant resolution RPC endpoints (Phase B3b.2b-step2).
//
// /api/internal/tenants/resolve-by-email lets the costrict-web server ask
// cs-user "which tenant does this email belong to?" without duplicating the
// tenants table (ADR D1 — cs-user owns tenant data). The server's OAuth
// callback uses this for the §5 Try 2 layer: subdomain (Try 1) already ran
// in middleware; if it missed, the callback now has user claims and can
// resolve by email domain.
//
// Response shapes (design §5.1):
//
//   - exactly one match → 200 {"status":"ok","slug":"acme","tenant_id":"t-..."}
//   - zero matches      → 200 {"status":"not_found"} (NOT a 404 — Try 3
//     picker is a UI flow, the callback proceeds)
//   - two or more       → 200 {"status":"ambiguous","candidates":[{...}]}
//
// We deliberately return 200 across all three outcomes so the server's
// RPCWriter can map a single transport outcome (200) to three semantic
// states via the `status` field. A 4xx would force the writer to inspect
// both status code AND body to disambiguate.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
)

// TenantsAPI wraps a tenant resolver. The dependency is an interface so
// unit tests can substitute a fake; production wires *tenant.Resolver.
type TenantsAPI struct {
	Resolver TenantResolverService
}

// TenantResolverService is the read-side subset of *tenant.Resolver the
// handlers need. Declared as an interface so handler tests don't pull in
// the concrete type (and so unavailableTenantResolver can substitute for
// the swagger-stub fallback in app.go).
type TenantResolverService interface {
	// ResolveByEmail returns the active tenant whose email_domains array
	// contains the domain extracted from the supplied email. See
	// tenant.Resolver.ResolveByEmail for the full contract — returns
	// ErrTenantNotFound on zero hits and ErrAmbiguousTenant on two or more.
	ResolveByEmail(ctx context.Context, email string) (*models.Tenant, error)

	// ListByEmailDomain returns every active tenant whose email_domains
	// array contains the domain extracted from the supplied email. Used to
	// populate the picker candidates when ResolveByEmail returns
	// ErrAmbiguousTenant. Empty list on zero hits.
	ListByEmailDomain(ctx context.Context, email string) ([]*models.Tenant, error)
}

// resolveByEmailRequest is the body shape for POST
// /api/internal/tenants/resolve-by-email.
type resolveByEmailRequest struct {
	Email string `json:"email" binding:"required"`
}

// tenantCandidate is the per-tenant shape in the ambiguous-response array.
// Only public, non-sensitive fields — server uses this to render a picker
// UI without needing another round trip.
type tenantCandidate struct {
	Slug     string `json:"slug"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// ResolveByEmail godoc
//
//	@Summary		Resolve tenant by email domain (Try 2)
//	@Description	Looks up the active tenant whose email_domains array contains the domain extracted from the supplied email. Returns slug on unique hit, candidates on ambiguity, not_found on miss. Always 200 — semantic state lives in the `status` field so the caller (server RPCWriter) can branch without parsing both status code and body.
//	@Tags			tenants
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		resolveByEmailRequest	true	"Email to resolve"
//	@Success		200		{object}	object{status=string,slug=string,tenant_id=string,candidates=[]handlers.tenantCandidate}
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/tenants/resolve-by-email [post]
func (a *TenantsAPI) ResolveByEmail(c *gin.Context) {
	var req resolveByEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email is required"})
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email is required"})
		return
	}

	tn, err := a.Resolver.ResolveByEmail(c.Request.Context(), email)
	if err == nil && tn != nil {
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"slug":      tn.Slug,
			"tenant_id": tn.TenantID,
		})
		return
	}
	if errors.Is(err, tenant.ErrAmbiguousTenant) {
		candidates := listAmbiguousCandidates(c.Request.Context(), a.Resolver, email)
		c.JSON(http.StatusOK, gin.H{
			"status":     "ambiguous",
			"candidates": candidates,
		})
		return
	}
	// ErrTenantNotFound OR (nil, nil) — both map to "no signal from this
	// layer". The server's OAuth callback will fall through to Try 3
	// (picker) or default tenant.
	c.JSON(http.StatusOK, gin.H{"status": "not_found"})
}

// listAmbiguousCandidates re-runs the email-domain scan via
// ListByEmailDomain to collect every match for the picker UI. On any error
// returns nil — by this point we've committed to the ambiguous branch, and
// a 500 would just confuse the caller.
func listAmbiguousCandidates(ctx context.Context, svc TenantResolverService, email string) []tenantCandidate {
	matches, err := svc.ListByEmailDomain(ctx, email)
	if err != nil || len(matches) == 0 {
		return nil
	}
	out := make([]tenantCandidate, 0, len(matches))
	for _, tn := range matches {
		if tn == nil {
			continue
		}
		out = append(out, tenantCandidate{
			Slug:     tn.Slug,
			TenantID: tn.TenantID,
			Name:     tn.DisplayName,
		})
	}
	return out
}
