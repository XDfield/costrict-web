// Tenant-admin config read/write endpoints (Phase C3.2).
//
// /api/internal/tenant/config exposes the per-tenant YAML config blob to
// the costrict-web server, which re-exposes it under /api/tenant/config
// behind the C1 RequireTenantAdmin middleware. cs-user owns tenant data
// (ADR D1); this is the raw blob CRUD surface that C3.3's typed
// provider_mapping editor will build on top of.
//
// Two endpoints:
//
//	GET  /api/internal/tenant/config      → 200 + TenantConfig JSON
//	PUT  /api/internal/tenant/config      → 200 + updated TenantConfig JSON
//
// Tenant scoping comes from the ResolveTenant middleware: it pins the
// caller's tenant via the X-Tenant-Id header (which the server forwards
// from AuthClaims). The handler reads tenant off the gin context via
// middleware.TenantFromGin — there is no tenant_id path or body field
// the caller can spoof.
//
// Error mapping:
//
//	tenant unresolved          → 400 "tenant resolution required"
//	ErrInvalidYAML             → 400 "invalid YAML"
//	ErrYAMLTooLarge            → 413 "YAML exceeds size cap"
//	ErrEmptyTenantID           → 500 (programmer error — middleware bug)
//	other DB errors            → 500
//
// updated_by is taken from the X-Actor-Subject-Id header when present
// (the server forwards the caller's JWT subject_id so audit trails stay
// accurate). Absent header → nil (stored as NULL).

package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/middleware"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/gin-gonic/gin"
)

// TenantConfigAPI wraps a tenantconfig.Service. The dependency is an
// interface so unit tests can substitute a fake; production wires
// *tenantconfig.Service.
//
// Audit (Phase C4.1) is optional — nil skips the post-success audit-log
// write (test path / 503 fallback). When set, UpdateTenantConfig writes a
// tenant_config.update row to user_center_audit_log after the service
// commits. Payload captures the new YAML blob (post-state only — diff
// generation deferred per C4.1 known limitations).
type TenantConfigAPI struct {
	Svc   TenantConfigService
	Audit *auditlog.Service
}

// TenantConfigService is the read+write surface the handlers need.
// Declared as an interface so handler tests don't pull in the concrete
// type (and so unavailableTenantConfigService can substitute for the
// swagger-stub fallback in app.go).
type TenantConfigService interface {
	Get(ctx context.Context, tenantID string) (*models.TenantConfig, error)
	Update(ctx context.Context, p tenantconfig.UpdateParams) (*models.TenantConfig, error)
}

// actorSubjectIDHeader is the server-forwarded JWT subject_id. Picked
// over a body field so the audit trail can't drift from the auth claim
// (a misbehaving client can't lie about who they are).
const actorSubjectIDHeader = "X-Actor-Subject-Id"

// tenantConfigRequest is the PUT body shape. config_yaml is the raw blob
// — the service validates + normalizes.
type tenantConfigRequest struct {
	ConfigYAML string `json:"config_yaml" binding:"required"`
}

// GetTenantConfig godoc
//
//	@Summary		Read tenant config (tenant admin)
//	@Description	Returns the caller's tenant_configs row. Missing row returns a synthetic default {"config_yaml":"{}"} — every tenant implicitly has an empty config. Tenant scoping enforced by the ResolveTenant middleware via X-Tenant-Id.
//	@Tags			tenant-config
//	@Produce		json
//	@Security		InternalToken
//	@Success		200		{object}	models.TenantConfig
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/tenant/config [get]
func (a *TenantConfigAPI) GetTenantConfig(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenant config service unavailable"})
		return
	}
	tenantID, ok := requireTenantID(c)
	if !ok {
		return // requireTenantID already wrote the response
	}

	tc, err := a.Svc.Get(c.Request.Context(), tenantID)
	if err != nil {
		respondTenantConfigErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tc)
}

// UpdateTenantConfig godoc
//
//	@Summary		Update tenant config (tenant admin)
//	@Description	Replaces the caller's tenant_configs.config_yaml blob. Validates the YAML parses (schema-agnostic — C3.2 accepts any well-formed YAML; C3.3 will layer typed provider_mapping checks on top). Empty / whitespace-only body normalizes to "{}". 64 KiB cap. X-Actor-Subject-Id header (forwarded by server from JWT) stamps updated_by.
//	@Tags			tenant-config
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		handlers.tenantConfigRequest	true	"Raw YAML blob"
//	@Success		200		{object}	models.TenantConfig
//	@Failure		400		{object}	object{error=string}
//	@Failure		413		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/tenant/config [put]
func (a *TenantConfigAPI) UpdateTenantConfig(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenant config service unavailable"})
		return
	}
	tenantID, ok := requireTenantID(c)
	if !ok {
		return
	}

	var req tenantConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "config_yaml is required"})
		return
	}

	// Actor header is optional — nil when absent so the DB column stays
	// NULL (preserving the "no actor recorded" semantic).
	var actor *string
	if v := c.GetHeader(actorSubjectIDHeader); v != "" {
		actor = &v
	}

	tc, err := a.Svc.Update(c.Request.Context(), tenantconfig.UpdateParams{
		TenantID:   tenantID,
		ConfigYAML: req.ConfigYAML,
		UpdatedBy:  actor,
	})
	if err != nil {
		respondTenantConfigErr(c, err)
		return
	}
	recordAudit(a.Audit, c, models.ActionTenantConfigUpdate, models.TargetTypeTenantConfig,
		"tenant_config:"+tenantID, map[string]any{
			"bytes": len(req.ConfigYAML),
		})
	c.JSON(http.StatusOK, tc)
}

// requireTenantID pulls the resolved tenant off the gin context. Returns
// (tenantID, true) on success; on failure writes the response + returns
// ("", false) so callers can early-return.
//
// 400 (not 503) when ResolveTenant produced no tenant: this endpoint is
// tenant-scoped, so the only legitimate caller is the server proxying
// with X-Tenant-Id set; a missing tenant means the server forgot to
// forward, which is a programmer error surfacing as 400 is clearer than
// a generic 503.
func requireTenantID(c *gin.Context) (string, bool) {
	tn, err := middleware.TenantFromGin(c)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenant resolution failed"})
		return "", false
	}
	if tn == nil || tn.TenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant resolution required"})
		return "", false
	}
	return tn.TenantID, true
}

// respondTenantConfigErr maps tenantconfig service errors to HTTP.
func respondTenantConfigErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tenantconfig.ErrInvalidYAML):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid YAML"})
	case errors.Is(err, tenantconfig.ErrYAMLTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "YAML exceeds size cap"})
	case errors.Is(err, tenantconfig.ErrEmptyTenantID):
		// Programmer error (middleware should have caught this). 500
		// surfaces it loudly rather than masquerading as client fault.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "tenant resolution required"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
