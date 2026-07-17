package handlers

import (
	"errors"
	"net/http"
	"strings"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// tenantCandidateDTO mirrors userpkg.TenantEmailCandidate over the wire so the
// frontend picker UI doesn't reach into the RPC client package. Field names
// stay identical (slug / tenant_id / name) to match what cs-user already
// returns internally — keeps the contract uniform across the stack.
type tenantCandidateDTO struct {
	Slug     string `json:"slug"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// tenantSuggestResponse is the JSON shape returned to the frontend picker UI.
// Status is one of "ok" / "ambiguous" / "not_found" — same discriminator
// cs-user uses on its internal RPC — so the frontend can branch directly
// without learning a third vocabulary.
//
//   - ok         → Slug + TenantID populated; auto-redirect candidate
//   - ambiguous  → Candidates populated; render picker
//   - not_found  → all fields empty; fall back to default tenant
type tenantSuggestResponse struct {
	Status     string               `json:"status"`
	Slug       string               `json:"slug,omitempty"`
	TenantID   string               `json:"tenant_id,omitempty"`
	Candidates []tenantCandidateDTO `json:"candidates,omitempty"`
}

// SuggestTenant godoc
// @Summary      Suggest tenants for an email domain
// @Description  Resolve which tenant(s) an email address belongs to by domain lookup against cs-user. Used by the login picker UI when subdomain resolution (Layer 1) and the OAuth callback's automatic resolution (Layer 2) come back ambiguous. Returns a three-state body — ok / ambiguous / not_found — so the frontend can auto-redirect, render a picker, or fall back to the default tenant respectively. Unavailable when the server runs in local backend mode (no tenant data on this side per ADR D1).
// @Tags         tenants
// @Produce      json
// @Param        email  query     string  true  "User email address; domain is extracted and matched against tenant email_domains"
// @Success      200    {object}  handlers.tenantSuggestResponse
// @Failure      400    {object}  object{error=string}
// @Failure      503    {object}  object{error=string}
// @Router       /tenants/suggest [get]
func SuggestTenant(c *gin.Context) {
	email := strings.TrimSpace(c.Query("email"))
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email parameter is required"})
		return
	}

	// Local-mode guard: the server deliberately does not duplicate cs-user's
	// tenants table (ADR D1), so TenantResolver is nil when Backend=local.
	// 503 rather than 404 so the frontend can distinguish "feature disabled
	// here" from "endpoint moved".
	if UserModule == nil || UserModule.TenantResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenant resolution unavailable"})
		return
	}

	res, err := UserModule.TenantResolver.ResolveTenantByEmail(c.Request.Context(), email)
	if err != nil {
		// Transport failures surface as ErrRPCUnavailable; everything else is
		// unexpected. Either way we don't leak the underlying cause over the
		// wire — clients get a generic 502 and ops gets the server log.
		if errors.Is(err, userpkg.ErrRPCUnavailable) {
			c.JSON(http.StatusBadGateway, gin.H{"error": "tenant service unavailable"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant resolution failed"})
		return
	}
	if res == nil {
		// Defensive — ResolveTenantByEmail's contract says it never returns
		// (nil, nil), but we don't trust the wire shape across a service
		// boundary.
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant resolution returned empty"})
		return
	}

	resp := tenantSuggestResponse{Status: res.Status}
	switch res.Status {
	case "ok":
		resp.Slug = res.Slug
		resp.TenantID = res.TenantID
	case "ambiguous":
		resp.Candidates = make([]tenantCandidateDTO, 0, len(res.Candidates))
		for _, cand := range res.Candidates {
			resp.Candidates = append(resp.Candidates, tenantCandidateDTO{
				Slug:     cand.Slug,
				TenantID: cand.TenantID,
				Name:     cand.Name,
			})
		}
	case "not_found":
		// nothing else to fill
	default:
		// Unknown status string — the cs-user contract changed without this
		// handler catching up. Treat as not_found so the frontend falls back
		// to the default tenant rather than dead-ending on an unrecognised
		// discriminator.
		resp.Status = "not_found"
	}
	c.JSON(http.StatusOK, resp)
}
