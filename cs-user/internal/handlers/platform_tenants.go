// Platform-admin tenant CRUD endpoints (Phase C2).
//
// /api/internal/platform/tenants* exposes the full tenant lifecycle to the
// costrict-web server (which re-exposes them under /api/platform/tenants*
// behind the C1 RequirePlatformAdmin middleware). cs-user owns the tenants
// table (ADR D1); this is the write surface that complements the read-side
// Resolver / handlers.TenantsAPI shipped in Phase B.
//
// 7 endpoints — list / get / create / patch / suspend / restore / delete.
// State-machine transitions return 409 (ErrInvalidStateTransition); slug /
// domain conflicts return 409 (ErrSlugTaken / ErrEmailDomainConflict);
// validation failures return 400 (ErrInvalidSlug / ErrInvalidEdition /
// ErrInvalidDisplayName / ErrInvalidEmailDomains); misses return 404
// (ErrTenantNotFound).
//
// The :id path parameter accepts either tenant_id OR slug — mirrors
// Resolver.ResolveBySlug's `WHERE tenant_id = ? OR slug = ?` semantics so
// callers can use whichever they happen to have on hand.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
)

// PlatformTenantsAPI wraps a tenant.Admin. The dependency is an interface so
// unit tests can substitute a fake; production wires *tenant.Admin.
//
// Audit (Phase C4.1) is optional — nil skips the post-success audit-log write
// (test path / 503 fallback). When set, the four write/lifecycle handlers
// (Create / Suspend / Restore / DeleteTenant) call Audit.Record after the
// service returns. UpdateTenant is intentionally not audited in C4.1 (low
// compliance value; see progress doc).
type PlatformTenantsAPI struct {
	Svc   PlatformTenantService
	Audit *auditlog.Service
}

// PlatformTenantService is the write/lifecycle subset of *tenant.Admin the
// handlers need. Declared as an interface so handler tests don't pull in the
// concrete type (and so unavailablePlatformTenantService can substitute for
// the swagger-stub fallback in app.go).
type PlatformTenantService interface {
	CreateTenant(ctx context.Context, p tenant.CreateParams) (*models.Tenant, error)
	ListTenants(ctx context.Context, p tenant.ListParams) (*tenant.ListResult, error)
	GetTenant(ctx context.Context, idOrSlug string) (*models.Tenant, error)
	UpdateTenant(ctx context.Context, idOrSlug string, p tenant.UpdateParams) (*models.Tenant, error)
	SuspendTenant(ctx context.Context, idOrSlug string) (*models.Tenant, error)
	RestoreTenant(ctx context.Context, idOrSlug string) (*models.Tenant, error)
	RequestDeletion(ctx context.Context, idOrSlug string) (*models.Tenant, error)
}

// --- request body shapes ---

type platformCreateTenantRequest struct {
	Slug         string   `json:"slug" binding:"required"`
	DisplayName  string   `json:"display_name" binding:"required"`
	Edition      string   `json:"edition,omitempty"`
	EmailDomains []string `json:"email_domains,omitempty"`
	Features     string   `json:"features,omitempty"`
	Limits       string   `json:"limits,omitempty"`
	Settings     string   `json:"settings,omitempty"`
}

// platformUpdateTenantRequest mirrors tenant.UpdateParams — every field is a
// pointer so absent fields mean "leave as-is" (true PATCH semantics). slug +
// tenant_id + status are intentionally NOT accepted here — slug/tenant_id are
// immutable; status changes only via the suspend/restore/delete endpoints.
type platformUpdateTenantRequest struct {
	DisplayName  *string   `json:"display_name,omitempty"`
	Edition      *string   `json:"edition,omitempty"`
	EmailDomains *[]string `json:"email_domains,omitempty"`
	Features     *string   `json:"features,omitempty"`
	Limits       *string   `json:"limits,omitempty"`
	Settings     *string   `json:"settings,omitempty"`
}

// --- handlers ---

// CreateTenant godoc
//
//	@Summary		Create tenant
//	@Description	Creates a new tenant. Validates slug format + uniqueness, edition enum, and email_domains non-overlap with existing tenants. status defaults to active; tenant_id is server-minted UUID.
//	@Tags			platform-tenants
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		platformCreateTenantRequest	true	"Tenant to create"
//	@Success		201		{object}	models.Tenant
//	@Failure		400		{object}	object{error=string}
//	@Failure		409		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/platform/tenants [post]
func (a *PlatformTenantsAPI) CreateTenant(c *gin.Context) {
	var req platformCreateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug and display_name are required"})
		return
	}
	tn, err := a.Svc.CreateTenant(c.Request.Context(), tenant.CreateParams{
		Slug:         req.Slug,
		DisplayName:  req.DisplayName,
		Edition:      req.Edition,
		EmailDomains: req.EmailDomains,
		Features:     req.Features,
		Limits:       req.Limits,
		Settings:     req.Settings,
	})
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	recordAudit(a.Audit, c, models.ActionTenantCreate, models.TargetTypeTenant,
		"tenant:"+tn.TenantID, map[string]any{
			"slug":          tn.Slug,
			"edition":       tn.Edition,
			"email_domains": req.EmailDomains,
		})
	c.JSON(http.StatusCreated, tn)
}

// ListTenants godoc
//
//	@Summary		List tenants (paginated)
//	@Description	Returns a paginated list of tenants (newest first). Optional status filter narrows to one of active / suspended / deleted. Limit defaults to 100 (capped at 500); offset defaults to 0.
//	@Tags			platform-tenants
//	@Produce		json
//	@Security		InternalToken
//	@Param			limit	query		int		false	"Page size (default 100, max 500)"
//	@Param			offset	query		int		false	"Page offset (default 0)"
//	@Param			status	query		string	false	"Filter by status: active | suspended | deleted"
//	@Success		200		{object}	tenant.ListResult
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/platform/tenants [get]
func (a *PlatformTenantsAPI) ListTenants(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	status := c.Query("status")

	res, err := a.Svc.ListTenants(c.Request.Context(), tenant.ListParams{
		Limit:  limit,
		Offset: offset,
		Status: status,
	})
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// GetTenant godoc
//
//	@Summary		Get tenant by id or slug
//	@Description	Returns a single tenant. The :id path param accepts either tenant_id OR slug — whichever matches.
//	@Tags			platform-tenants
//	@Produce		json
//	@Security		InternalToken
//	@Param			id	path		string	true	"Tenant ID or slug"
//	@Success		200	{object}	models.Tenant
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/api/internal/platform/tenants/{id} [get]
func (a *PlatformTenantsAPI) GetTenant(c *gin.Context) {
	id := c.Param("id")
	tn, err := a.Svc.GetTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// UpdateTenant godoc
//
//	@Summary		Partially update tenant
//	@Description	Updates mutable fields (display_name / edition / email_domains / features / limits / settings). Absent fields are left untouched (true PATCH semantics). slug + tenant_id are immutable and silently ignored if supplied; status changes only via /suspend / /restore / /delete.
//	@Tags			platform-tenants
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			id		path		string						true	"Tenant ID or slug"
//	@Param			body	body		platformUpdateTenantRequest	true	"Fields to update"
//	@Success		200		{object}	models.Tenant
//	@Failure		400		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		409		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/platform/tenants/{id} [patch]
func (a *PlatformTenantsAPI) UpdateTenant(c *gin.Context) {
	id := c.Param("id")
	var req platformUpdateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tn, err := a.Svc.UpdateTenant(c.Request.Context(), id, tenant.UpdateParams{
		DisplayName:  req.DisplayName,
		Edition:      req.Edition,
		EmailDomains: req.EmailDomains,
		Features:     req.Features,
		Limits:       req.Limits,
		Settings:     req.Settings,
	})
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tn)
}

// SuspendTenant godoc
//
//	@Summary		Suspend tenant
//	@Description	Transitions tenant status from active → suspended. Blocks logins at the resolver layer (status check).
//	@Tags			platform-tenants
//	@Produce		json
//	@Security		InternalToken
//	@Param			id	path		string	true	"Tenant ID or slug"
//	@Success		200	{object}	models.Tenant
//	@Failure		404	{object}	object{error=string}
//	@Failure		409	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/api/internal/platform/tenants/{id}/suspend [post]
func (a *PlatformTenantsAPI) SuspendTenant(c *gin.Context) {
	id := c.Param("id")
	tn, err := a.Svc.SuspendTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	recordAudit(a.Audit, c, models.ActionTenantSuspend, models.TargetTypeTenant,
		"tenant:"+tn.TenantID, map[string]any{"slug": tn.Slug})
	c.JSON(http.StatusOK, tn)
}

// RestoreTenant godoc
//
//	@Summary		Restore suspended tenant
//	@Description	Transitions tenant status from suspended → active.
//	@Tags			platform-tenants
//	@Produce		json
//	@Security		InternalToken
//	@Param			id	path		string	true	"Tenant ID or slug"
//	@Success		200	{object}	models.Tenant
//	@Failure		404	{object}	object{error=string}
//	@Failure		409	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/api/internal/platform/tenants/{id}/restore [post]
func (a *PlatformTenantsAPI) RestoreTenant(c *gin.Context) {
	id := c.Param("id")
	tn, err := a.Svc.RestoreTenant(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	recordAudit(a.Audit, c, models.ActionTenantRestore, models.TargetTypeTenant,
		"tenant:"+tn.TenantID, map[string]any{"slug": tn.Slug})
	c.JSON(http.StatusOK, tn)
}

// DeleteTenant godoc
//
//	@Summary		Request tenant deletion (30-day grace)
//	@Description	Transitions status from active|suspended → deleted and stamps deletion_requested_at = now. The actual row purge is performed by a 30-day grace cron (out of scope — separate PR with runbook).
//	@Tags			platform-tenants
//	@Produce		json
//	@Security		InternalToken
//	@Param			id	path		string	true	"Tenant ID or slug"
//	@Success		200	{object}	models.Tenant
//	@Failure		404	{object}	object{error=string}
//	@Failure		409	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/api/internal/platform/tenants/{id}/delete [post]
func (a *PlatformTenantsAPI) DeleteTenant(c *gin.Context) {
	id := c.Param("id")
	tn, err := a.Svc.RequestDeletion(c.Request.Context(), id)
	if err != nil {
		respondPlatformTenantErr(c, err)
		return
	}
	recordAudit(a.Audit, c, models.ActionTenantDeletionRequested, models.TargetTypeTenant,
		"tenant:"+tn.TenantID, map[string]any{"slug": tn.Slug})
	c.JSON(http.StatusOK, tn)
}

// respondPlatformTenantErr maps tenant.Admin sentinel errors to HTTP codes.
// Centralized so the seven handlers stay declarative — adds a case here when
// a new sentinel lands.
func respondPlatformTenantErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tenant.ErrTenantNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
	case errors.Is(err, tenant.ErrSlugTaken),
		errors.Is(err, tenant.ErrEmailDomainConflict),
		errors.Is(err, tenant.ErrInvalidStateTransition):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, tenant.ErrInvalidSlug),
		errors.Is(err, tenant.ErrInvalidEdition),
		errors.Is(err, tenant.ErrInvalidDisplayName),
		errors.Is(err, tenant.ErrInvalidEmailDomains):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
