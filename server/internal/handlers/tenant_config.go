// Tenant-admin config read/write handlers (Phase C3.2).
//
// C3 sub-slice 2 (tenant 配置 CRUD): tenant_admin reads / replaces the
// per-tenant YAML config blob. Two public endpoints:
//
//	GET  /api/tenant/config   → TenantConfig JSON
//	PUT  /api/tenant/config   → upsert; body {config_yaml: "..."}
//
// Auth: Bearer JWT carrying tenant_admin role (owner / admin) on
// AuthClaims.TenantID. middleware.RequireTenantAdmin enforces the role
// gate; this handler derives X-Tenant-Id from AuthClaims.TenantSlug
// (falling back to TenantID for legacy tokens) so the RPC client
// forwards it and cs-user's ResolveTenant middleware pins the row.
//
// Actor forwarding: the handler reads AuthClaims.Sub (JWT subject_id)
// and forwards it as X-Actor-Subject-Id so cs-user's audit trail
// (tenant_configs.updated_by) records the editing tenant_admin.
//
// Error mapping:
//
//	ErrRPCUnavailable / ErrTenantConfigUnavailable / ErrNotConfigured → 502
//	ErrInvalidYAML    → 400 (echo cs-user's validation)
//	ErrYAMLTooLarge   → 413
//	missing claims    → 401 (Auth middleware misconfigured)
//	empty tenant      → 403 (defensive — RequireTenantAdmin should catch)

package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/tenant"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// TenantConfigService is the cs-user-side surface the handler consumes.
// Declared as interface for test substitution; production wires the
// *userpkg.RPCClient.
type TenantConfigService interface {
	GetTenantConfig(ctx context.Context) (*userpkg.TenantConfig, error)
	UpdateTenantConfig(ctx context.Context, yamlStr, actorSubjectID string) (*userpkg.TenantConfig, error)
}

// TenantConfigAPI is the receiver for tenant-admin config handlers.
type TenantConfigAPI struct {
	Svc TenantConfigService
}

// tenantConfigUpdateRequest is the PUT body shape.
type tenantConfigUpdateRequest struct {
	ConfigYAML string `json:"config_yaml" binding:"required"`
}

// GetTenantConfig godoc
// @Summary      Read my tenant's config (tenant admin)
// @Description  Returns the caller's tenant_configs row. Missing row returns a synthetic default {"config_yaml":"{}"} — every tenant implicitly has an empty config. Tenant scoping is enforced by cs-user's ResolveTenant middleware via the forwarded X-Tenant-Id header — caller cannot escape their own tenant.
// @Tags         tenant-config
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  userpkg.TenantConfig
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      502  {object}  object{error=string}
// @Router       /api/tenant/config [get]
func (a *TenantConfigAPI) GetTenantConfig(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant config service unavailable"})
		return
	}
	ctx, ok := prepareTenantConfigCtx(c)
	if !ok {
		return // prepareTenantConfigCtx already wrote the response
	}

	tc, err := a.Svc.GetTenantConfig(ctx)
	if err != nil {
		respondTenantConfigErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tc)
}

// UpdateTenantConfig godoc
// @Summary      Replace my tenant's config (tenant admin)
// @Description  Replaces the caller's tenant_configs.config_yaml blob. Validates the YAML parses (schema-agnostic — C3.2 accepts any well-formed YAML; C3.3 will layer typed provider_mapping checks on top). Empty / whitespace-only body normalizes to "{}". 64 KiB cap. The JWT subject_id is forwarded as X-Actor-Subject-Id so cs-user stamps updated_by.
// @Tags         tenant-config
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      handlers.tenantConfigUpdateRequest  true  "Raw YAML blob"
// @Success      200   {object}  userpkg.TenantConfig
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      413   {object}  object{error=string}
// @Failure      502   {object}  object{error=string}
// @Router       /api/tenant/config [put]
func (a *TenantConfigAPI) UpdateTenantConfig(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant config service unavailable"})
		return
	}

	var req tenantConfigUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "config_yaml is required"})
		return
	}

	ctx, ok := prepareTenantConfigCtx(c)
	if !ok {
		return
	}

	// Forward the JWT subject_id so cs-user's audit trail records the
	// editor. Sub is the JWT `sub` claim; empty when the JWT doesn't
	// carry one (e.g. a service-account token), which is fine — cs-user
	// stores NULL updated_by in that case.
	ac, _ := readTenantConfigClaims(c)
	actor := ""
	if ac.Sub != "" {
		actor = ac.Sub
	}

	tc, err := a.Svc.UpdateTenantConfig(ctx, req.ConfigYAML, actor)
	if err != nil {
		respondTenantConfigErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tc)
}

// prepareTenantConfigCtx validates auth claims, derives the tenant slug,
// and returns a ctx with the slug + actor meta (Phase C4.1) injected so the
// RPC client forwards X-Tenant-Id + X-Actor-Tenant-Role / X-Actor-Platform-
// Scope. Returns (ctx, true) on success; on failure writes the response +
// returns (nil, false).
func prepareTenantConfigCtx(c *gin.Context) (context.Context, bool) {
	ac, ok := readTenantConfigClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return nil, false
	}
	slug := ac.TenantSlug
	if slug == "" {
		slug = ac.TenantID
	}
	if slug == "" {
		// Should be impossible behind RequireTenantAdmin, but defensive —
		// 403 not 500 to keep the contract obvious.
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant binding required"})
		return nil, false
	}
	ctx := tenant.WithSlug(c.Request.Context(), slug)
	ctx = tenant.WithActorMeta(ctx, actorMetaFromClaims(ac))
	return ctx, true
}

// actorMetaFromClaims extracts the audit-log actor meta from JWT claims. The
// first TenantRoles entry wins (sufficient for compliance; full role-list
// audit deferred per C4.1 known limitations). PlatformScope passes through
// unchanged.
func actorMetaFromClaims(ac middleware.AuthClaims) tenant.ActorMeta {
	m := tenant.ActorMeta{Scope: ac.PlatformScope}
	if len(ac.TenantRoles) > 0 {
		m.Role = ac.TenantRoles[0]
	}
	return m
}

// readTenantConfigClaims pulls the middleware.AuthClaims value from the
// gin context. Reuses the same shape as tenant_user.go's readAuthClaims.
func readTenantConfigClaims(c *gin.Context) (middleware.AuthClaims, bool) {
	v, exists := c.Get(middleware.AuthClaimsKey)
	if !exists {
		return middleware.AuthClaims{}, false
	}
	ac, ok := v.(middleware.AuthClaims)
	return ac, ok
}

// respondTenantConfigErr maps RPC client errors to HTTP.
func respondTenantConfigErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userpkg.ErrInvalidYAML):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid YAML"})
	case errors.Is(err, userpkg.ErrYAMLTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "YAML exceeds size cap"})
	case errors.Is(err, userpkg.ErrRPCUnavailable),
		errors.Is(err, userpkg.ErrTenantConfigUnavailable),
		errors.Is(err, userpkg.ErrNotConfigured):
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant config service unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
