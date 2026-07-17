// Tenant-admin provider_mapping typed-edit handlers (Phase C3.3).
//
// C3 sub-slice 3 typed counterpart to /api/tenant/config. Two public
// endpoints:
//
//	GET  /api/tenant/provider-mapping   → ProviderMapping JSON
//	PUT  /api/tenant/provider-mapping   → typed PUT (full replace of the
//	                                      provider_mapping subtree)
//
// Auth + tenant scoping identical to /api/tenant/config (C3.2):
// middleware.RequireTenantAdmin enforces the role gate; this handler
// derives X-Tenant-Id from AuthClaims.TenantSlug (falling back to
// TenantID for legacy tokens) and forwards AuthClaims.Sub as
// X-Actor-Subject-Id for the audit trail.
//
// Error mapping (extends C3.2):
//
//	ErrRPCUnavailable / ErrTenantConfigUnavailable / ErrNotConfigured → 502
//	ErrInvalidYAML           → 400 (rare: stored blob malformed)
//	ErrProviderNameInvalid   → 400 "invalid provider name"
//	ErrIntervalInvalid       → 400 "invalid enterprise_sync.interval"
//	ErrRankNegative          → 400 "rank must be non-negative"
//	missing claims           → 401
//	empty tenant binding     → 403

package handlers

import (
	"context"
	"errors"
	"net/http"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// TenantProviderMappingService is the cs-user-side typed surface. Declared
// as interface for test substitution; production wires *userpkg.RPCClient.
type TenantProviderMappingService interface {
	GetProviderMapping(ctx context.Context) (*userpkg.ProviderMapping, error)
	UpdateProviderMapping(ctx context.Context, mapping *userpkg.ProviderMapping, actorSubjectID string) (*userpkg.ProviderMapping, error)
}

// TenantProviderMappingAPI is the receiver for typed provider_mapping handlers.
type TenantProviderMappingAPI struct {
	Svc TenantProviderMappingService
}

// GetProviderMapping godoc
// @Summary      Read my tenant's provider_mapping (tenant admin)
// @Description  Returns the typed provider_mapping subsection. Missing subsection returns {"providers":{}} — every tenant implicitly has an empty mapping. Tenant scoping is enforced by cs-user's ResolveTenant middleware via the forwarded X-Tenant-Id header.
// @Tags         tenant-provider-mapping
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  userpkg.ProviderMapping
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      502  {object}  object{error=string}
// @Router       /api/tenant/provider-mapping [get]
func (a *TenantProviderMappingAPI) GetProviderMapping(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "provider_mapping service unavailable"})
		return
	}
	ctx, ok := prepareTenantConfigCtx(c)
	if !ok {
		return
	}

	m, err := a.Svc.GetProviderMapping(ctx)
	if err != nil {
		respondProviderMappingErr(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// UpdateProviderMapping godoc
// @Summary      Replace my tenant's provider_mapping (tenant admin)
// @Description  PUT (full replace) of the provider_mapping subsection. Sibling YAML sections (employment_providers, features, etc.) are preserved by cs-user. Provider names must match ^[a-z0-9_]{1,64}$; rank non-negative; enterprise_sync.interval a positive Go duration capped at 30 days. Enabled defaults to true when omitted. The JWT subject_id is forwarded as X-Actor-Subject-Id.
// @Tags         tenant-provider-mapping
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      userpkg.ProviderMapping  true  "Typed provider_mapping"
// @Success      200   {object}  userpkg.ProviderMapping
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      502   {object}  object{error=string}
// @Router       /api/tenant/provider-mapping [put]
func (a *TenantProviderMappingAPI) UpdateProviderMapping(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "provider_mapping service unavailable"})
		return
	}

	var m userpkg.ProviderMapping
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider_mapping body"})
		return
	}

	ctx, ok := prepareTenantConfigCtx(c)
	if !ok {
		return
	}

	// Forward JWT subject_id for cs-user's audit trail. Empty when the JWT
	// doesn't carry one — cs-user stores NULL updated_by.
	ac, _ := readTenantConfigClaims(c)
	actor := ""
	if ac.Sub != "" {
		actor = ac.Sub
	}

	out, err := a.Svc.UpdateProviderMapping(ctx, &m, actor)
	if err != nil {
		respondProviderMappingErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// respondProviderMappingErr maps RPC client errors to HTTP.
func respondProviderMappingErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userpkg.ErrInvalidYAML):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid YAML"})
	case errors.Is(err, userpkg.ErrProviderNameInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider name"})
	case errors.Is(err, userpkg.ErrIntervalInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid enterprise_sync.interval"})
	case errors.Is(err, userpkg.ErrRankNegative):
		c.JSON(http.StatusBadRequest, gin.H{"error": "rank must be non-negative"})
	case errors.Is(err, userpkg.ErrRPCUnavailable),
		errors.Is(err, userpkg.ErrTenantConfigUnavailable),
		errors.Is(err, userpkg.ErrNotConfigured):
		c.JSON(http.StatusBadGateway, gin.H{"error": "provider_mapping service unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
