// Tenant-admin provider_mapping typed-edit endpoints (Phase C3.3).
//
// /api/internal/tenant/provider-mapping is the typed counterpart to
// /api/internal/tenant/config (C3.2 raw blob CRUD). Same tenant-scoped
// shape: ResolveTenant middleware pins the caller via X-Tenant-Id;
// X-Actor-Subject-Id forwards the JWT subject_id for the audit trail.
//
// Two endpoints:
//
//	GET  /api/internal/tenant/provider-mapping  → 200 + ProviderMapping JSON
//	PUT  /api/internal/tenant/provider-mapping  → 200 + updated ProviderMapping JSON
//
// PUT semantics: the provider_mapping subtree is fully replaced; sibling
// top-level sections in config_yaml (employment_providers, features, etc.)
// are preserved verbatim. See service.UpdateProviderMapping + the
// mergeProviderMappingSection helper.
//
// Error mapping (extends C3.2's set):
//
//	tenant unresolved              → 400 "tenant resolution required"
//	ErrInvalidYAML                → 400 "invalid YAML"
//	ErrProviderNameInvalid        → 400 "invalid provider name"
//	ErrIntervalInvalid            → 400 "invalid enterprise_sync.interval"
//	ErrRankNegative               → 400 "rank must be non-negative"
//	ErrEmptyTenantID              → 500 (programmer error)
//	other DB / serialization errs → 500

package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/gin-gonic/gin"
)

// TenantProviderMappingAPI wraps the typed provider_mapping surface.
// Svc is an interface so handler tests substitute a fake; production
// wires *tenantconfig.Service (which satisfies both TenantConfigService
// and TenantProviderMappingService).
type TenantProviderMappingAPI struct {
	Svc TenantProviderMappingService
}

// TenantProviderMappingService is the typed read+write surface the
// handlers need. Declared as an interface so handler tests don't pull in
// the concrete type (and so unavailableTenantProviderMappingService can
// substitute for the swagger-stub fallback in app.go).
type TenantProviderMappingService interface {
	GetProviderMapping(ctx context.Context, tenantID string) (*tenantconfig.ProviderMapping, error)
	UpdateProviderMapping(ctx context.Context, p tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error)
}

// GetProviderMapping godoc
//
//	@Summary		Read provider_mapping (tenant admin)
//	@Description	Returns the typed provider_mapping subsection of the caller's tenant_configs.config_yaml. Missing subsection returns {"providers":{}} — every tenant implicitly has an empty mapping. Sibling YAML keys are not echoed here; use /api/internal/tenant/config for the raw blob.
//	@Tags			tenant-provider-mapping
//	@Produce		json
//	@Security		InternalToken
//	@Success		200		{object}	tenantconfig.ProviderMapping
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/tenant/provider-mapping [get]
func (a *TenantProviderMappingAPI) GetProviderMapping(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider_mapping service unavailable"})
		return
	}
	tenantID, ok := requireTenantID(c)
	if !ok {
		return
	}

	m, err := a.Svc.GetProviderMapping(c.Request.Context(), tenantID)
	if err != nil {
		respondProviderMappingErr(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// UpdateProviderMapping godoc
//
//	@Summary		Update provider_mapping (tenant admin)
//	@Description	PUT (full replace) of the provider_mapping subsection. Sibling YAML sections (employment_providers, features, etc.) are preserved verbatim via yaml.v3 Node-based merge. Provider names must match ^[a-z0-9_]{1,64}$; rank non-negative; enterprise_sync.interval a positive Go duration capped at 30 days. Enabled defaults to true when omitted.
//	@Tags			tenant-provider-mapping
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		tenantconfig.ProviderMapping	true	"Typed provider_mapping"
//	@Success		200		{object}	tenantconfig.ProviderMapping
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/tenant/provider-mapping [put]
func (a *TenantProviderMappingAPI) UpdateProviderMapping(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider_mapping service unavailable"})
		return
	}
	tenantID, ok := requireTenantID(c)
	if !ok {
		return
	}

	var m tenantconfig.ProviderMapping
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider_mapping body"})
		return
	}

	// Actor header is optional — nil when absent so the DB column stays
	// NULL (preserving the "no actor recorded" semantic).
	var actor *string
	if v := c.GetHeader(actorSubjectIDHeader); v != "" {
		actor = &v
	}

	out, err := a.Svc.UpdateProviderMapping(c.Request.Context(), tenantconfig.UpdateProviderMappingParams{
		TenantID:  tenantID,
		Mapping:   &m,
		UpdatedBy: actor,
	})
	if err != nil {
		respondProviderMappingErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// respondProviderMappingErr maps provider_mapping service errors to HTTP.
// Typed-schema errors (name/interval/rank) get 400 because they are
// request-body validation failures, not state-machine violations.
func respondProviderMappingErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tenantconfig.ErrInvalidYAML):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid YAML"})
	case errors.Is(err, tenantconfig.ErrProviderNameInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider name"})
	case errors.Is(err, tenantconfig.ErrIntervalInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid enterprise_sync.interval"})
	case errors.Is(err, tenantconfig.ErrRankNegative):
		c.JSON(http.StatusBadRequest, gin.H{"error": "rank must be non-negative"})
	case errors.Is(err, tenantconfig.ErrEmptyTenantID):
		c.JSON(http.StatusInternalServerError, gin.H{"error": "tenant resolution required"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
