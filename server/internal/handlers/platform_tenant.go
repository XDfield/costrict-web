// Platform-tenant CRUD handlers (Phase C2 step D).
//
// 7 thin handlers at /api/platform/tenants* that proxy to cs-user's
// /api/internal/platform/tenants* via RPCClient. The route group is gated
// by middleware.RequirePlatformAdmin (first real consumer of the C1
// middleware — previously test-only). cs-user remains the sole owner of
// tenant data (ADR D1).
//
// Error mapping (translates RPCClient sentinels → HTTP):
//   - ErrRPCUnavailable / ErrNotConfigured → 502
//   - ErrTenantNotFound                    → 404
//   - ErrSlugTaken / ErrEmailDomainConflict / ErrInvalidStateTransition → 409
//   - ErrInvalidSlug / ErrInvalidEdition / ErrInvalidDisplayName /
//     ErrInvalidEmailDomains → 400

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// PlatformTenantService is the write/lifecycle surface on *RPCClient that
// the handlers consume. Declared as an interface so tests can substitute a
// fake; production wires the *userpkg.RPCClient via Module.TenantResolver
// (which already holds the RPC client in rpc mode).
type PlatformTenantService interface {
	ListTenants(ctx context.Context, limit, offset int, status string) (*userpkg.PlatformTenantListResult, error)
	GetTenant(ctx context.Context, idOrSlug string) (*userpkg.PlatformTenant, error)
	CreateTenant(ctx context.Context, p userpkg.PlatformTenantCreateParams) (*userpkg.PlatformTenant, error)
	UpdateTenant(ctx context.Context, idOrSlug string, p userpkg.PlatformTenantUpdateParams) (*userpkg.PlatformTenant, error)
	SuspendTenant(ctx context.Context, idOrSlug string) (*userpkg.PlatformTenant, error)
	RestoreTenant(ctx context.Context, idOrSlug string) (*userpkg.PlatformTenant, error)
	DeleteTenant(ctx context.Context, idOrSlug string) (*userpkg.PlatformTenant, error)
}

// PlatformTenantAPI is the receiver for the 7 handlers. Svc is injected
// per-test; production wires it from UserModule.TenantResolver cast to
// *RPCClient (or a wrapper).
type PlatformTenantAPI struct {
	Svc PlatformTenantService
}

// --- request body shapes (server-side; mirror RPC client params) ---

type platformCreateRequest struct {
	Slug         string   `json:"slug" binding:"required"`
	DisplayName  string   `json:"display_name" binding:"required"`
	Edition      string   `json:"edition,omitempty"`
	EmailDomains []string `json:"email_domains,omitempty"`
	Features     string   `json:"features,omitempty"`
	Limits       string   `json:"limits,omitempty"`
	Settings     string   `json:"settings,omitempty"`
}

// platformUpdateRequest mirrors userpkg.PlatformTenantUpdateParams —
// every field is a pointer so absent means "leave as-is".
type platformUpdateRequest struct {
	DisplayName  *string   `json:"display_name,omitempty"`
	Edition      *string   `json:"edition,omitempty"`
	EmailDomains *[]string `json:"email_domains,omitempty"`
	Features     *string   `json:"features,omitempty"`
	Limits       *string   `json:"limits,omitempty"`
	Settings     *string   `json:"settings,omitempty"`
}

// --- handlers ---

// PlatformListTenants godoc
// @Summary      List tenants (platform admin)
// @Description  Paginated list of all tenants. Wrapper over cs-user's /api/internal/platform/tenants; returns 502 when the server runs in local backend mode (no tenant data on this side per ADR D1).
// @Tags         platform-tenants
// @Produce      json
// @Security     BearerAuth
// @Param        limit   query     int     false  "Page size (default 100, max 500)"
// @Param        offset  query     int     false  "Page offset (default 0)"
// @Param        status  query     string  false  "Filter by status: active | suspended | deleted"
// @Success      200     {object}  userpkg.PlatformTenantListResult
// @Failure      401     {object}  object{error=string}
// @Failure      403     {object}  object{error=string}
// @Failure      502     {object}  object{error=string}
// @Router       /api/platform/tenants [get]
func (a *PlatformTenantAPI) PlatformListTenants(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	status := c.Query("status")

	res, err := a.Svc.ListTenants(c.Request.Context(), limit, offset, status)
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// PlatformGetTenant godoc
// @Summary      Get tenant by id or slug (platform admin)
// @Description  Returns a single tenant. :id accepts either tenant_id OR slug.
// @Tags         platform-tenants
// @Produce      json
// @Security     BearerAuth
// @Param        id   path       string  true  "Tenant ID or slug"
// @Success      200  {object}   userpkg.PlatformTenant
// @Failure      401  {object}   object{error=string}
// @Failure      403  {object}   object{error=string}
// @Failure      404  {object}   object{error=string}
// @Failure      502  {object}   object{error=string}
// @Router       /api/platform/tenants/{id} [get]
func (a *PlatformTenantAPI) PlatformGetTenant(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	id := c.Param("id")
	tn, err := a.Svc.GetTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// PlatformCreateTenant godoc
// @Summary      Create tenant (platform admin)
// @Description  Creates a new tenant. cs-user validates slug format + uniqueness, edition enum, and email_domains non-overlap.
// @Tags         platform-tenants
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body       platformCreateRequest  true  "Tenant to create"
// @Success      201   {object}   userpkg.PlatformTenant
// @Failure      400   {object}   object{error=string}
// @Failure      401   {object}   object{error=string}
// @Failure      403   {object}   object{error=string}
// @Failure      409   {object}   object{error=string}
// @Failure      502   {object}   object{error=string}
// @Router       /api/platform/tenants [post]
func (a *PlatformTenantAPI) PlatformCreateTenant(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	var req platformCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug and display_name are required"})
		return
	}
	tn, err := a.Svc.CreateTenant(c.Request.Context(), userpkg.PlatformTenantCreateParams{
		Slug:         req.Slug,
		DisplayName:  req.DisplayName,
		Edition:      req.Edition,
		EmailDomains: req.EmailDomains,
		Features:     req.Features,
		Limits:       req.Limits,
		Settings:     req.Settings,
	})
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, tn)
}

// PlatformUpdateTenant godoc
// @Summary      Partially update tenant (platform admin)
// @Description  Updates mutable fields. slug + tenant_id are immutable (silently ignored if supplied). Status changes only via suspend / restore / delete endpoints.
// @Tags         platform-tenants
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path       string                 true  "Tenant ID or slug"
// @Param        body  body       platformUpdateRequest  true  "Fields to update"
// @Success      200   {object}   userpkg.PlatformTenant
// @Failure      400   {object}   object{error=string}
// @Failure      401   {object}   object{error=string}
// @Failure      403   {object}   object{error=string}
// @Failure      404   {object}   object{error=string}
// @Failure      409   {object}   object{error=string}
// @Failure      502   {object}   object{error=string}
// @Router       /api/platform/tenants/{id} [patch]
func (a *PlatformTenantAPI) PlatformUpdateTenant(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	id := c.Param("id")
	var req platformUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tn, err := a.Svc.UpdateTenant(c.Request.Context(), id, userpkg.PlatformTenantUpdateParams{
		DisplayName:  req.DisplayName,
		Edition:      req.Edition,
		EmailDomains: req.EmailDomains,
		Features:     req.Features,
		Limits:       req.Limits,
		Settings:     req.Settings,
	})
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// PlatformSuspendTenant godoc
// @Summary      Suspend tenant (platform admin)
// @Description  Transitions tenant status active → suspended.
// @Tags         platform-tenants
// @Produce      json
// @Security     BearerAuth
// @Param        id   path       string  true  "Tenant ID or slug"
// @Success      200  {object}   userpkg.PlatformTenant
// @Failure      401  {object}   object{error=string}
// @Failure      403  {object}   object{error=string}
// @Failure      404  {object}   object{error=string}
// @Failure      409  {object}   object{error=string}
// @Failure      502  {object}   object{error=string}
// @Router       /api/platform/tenants/{id}/suspend [post]
func (a *PlatformTenantAPI) PlatformSuspendTenant(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	id := c.Param("id")
	tn, err := a.Svc.SuspendTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// PlatformRestoreTenant godoc
// @Summary      Restore tenant (platform admin)
// @Description  Transitions tenant status suspended → active.
// @Tags         platform-tenants
// @Produce      json
// @Security     BearerAuth
// @Param        id   path       string  true  "Tenant ID or slug"
// @Success      200  {object}   userpkg.PlatformTenant
// @Failure      401  {object}   object{error=string}
// @Failure      403  {object}   object{error=string}
// @Failure      404  {object}   object{error=string}
// @Failure      409  {object}   object{error=string}
// @Failure      502  {object}   object{error=string}
// @Router       /api/platform/tenants/{id}/restore [post]
func (a *PlatformTenantAPI) PlatformRestoreTenant(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	id := c.Param("id")
	tn, err := a.Svc.RestoreTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// PlatformDeleteTenant godoc
// @Summary      Request tenant deletion (platform admin)
// @Description  Transitions status to deleted and stamps deletion_requested_at = now. The 30-day grace hard-delete is a separate cron (out of scope).
// @Tags         platform-tenants
// @Produce      json
// @Security     BearerAuth
// @Param        id   path       string  true  "Tenant ID or slug"
// @Success      200  {object}   userpkg.PlatformTenant
// @Failure      401  {object}   object{error=string}
// @Failure      403  {object}   object{error=string}
// @Failure      404  {object}   object{error=string}
// @Failure      409  {object}   object{error=string}
// @Failure      502  {object}   object{error=string}
// @Router       /api/platform/tenants/{id}/delete [post]
func (a *PlatformTenantAPI) PlatformDeleteTenant(c *gin.Context) {
	if a.Svc == nil {
		platformTenantUnavailable(c)
		return
	}
	id := c.Param("id")
	tn, err := a.Svc.DeleteTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// respondPlatformTenantRPCErr maps RPCClient sentinels to HTTP codes.
// Transport / config failures → 502 (Bad Gateway); semantic failures pass
// through with their natural code.
func respondPlatformTenantRPCErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userpkg.ErrRPCUnavailable), errors.Is(err, userpkg.ErrNotConfigured):
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant service unavailable"})
	case errors.Is(err, userpkg.ErrTenantNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
	case errors.Is(err, userpkg.ErrSlugTaken),
		errors.Is(err, userpkg.ErrEmailDomainConflict),
		errors.Is(err, userpkg.ErrInvalidStateTransition):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, userpkg.ErrInvalidSlug),
		errors.Is(err, userpkg.ErrInvalidEdition),
		errors.Is(err, userpkg.ErrInvalidDisplayName),
		errors.Is(err, userpkg.ErrInvalidEmailDomains):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// platformTenantUnavailable is the 502 path when the server runs in local
// backend mode (UserModule.TenantResolver == nil per ADR D1 — no tenant
// data on this side).
func platformTenantUnavailable(c *gin.Context) {
	c.JSON(http.StatusBadGateway, gin.H{"error": "tenant service unavailable"})
}
