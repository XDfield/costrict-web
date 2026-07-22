// Package handlers exposes cs-user's IdP sources REST endpoints.
package handlers

import (
	"context"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/idp"
	"github.com/gin-gonic/gin"
)

// IdPService is the interface for IdP source operations.
// This allows both the real service and stub implementations to be used.
type IdPService interface {
	Create(ctx context.Context, p idp.CreateParams) (*idp.IdPSourceView, error)
	Get(ctx context.Context, tenantID, provider string) (*idp.IdPSourceView, error)
	List(ctx context.Context, tenantID string) ([]idp.IdPSourceView, error)
	Update(ctx context.Context, p idp.UpdateParams) (*idp.IdPSourceView, error)
	Delete(ctx context.Context, tenantID, provider string) error
	GetTenantIdPs(ctx context.Context, tenantID string, tenantConfig idp.TenantConfigProvider) ([]idp.IdPSourceView, error)
}

// IdPInternalService is the server-to-server variant returning raw configs
// (including secrets). Handlers in this interface MUST only be mounted behind
// the RequireInternalToken middleware — never on a public route.
type IdPInternalService interface {
	GetTenantIdPsInternal(ctx context.Context, tenantID string, tenantConfig idp.TenantConfigProvider) ([]idp.InternalIdPSourceView, error)
	GetInternal(ctx context.Context, tenantID, provider string) (*idp.InternalIdPSourceView, error)
}

// IdPSourcesAPI wraps an IdPService to expose CRUD operations.
type IdPSourcesAPI struct {
	Svc          IdPService
	InternalSvc  IdPInternalService // required for /api/internal/idp-sources/* routes
	TenantConfig idp.TenantConfigProvider
}

// CreateIdPSource godoc
// @Summary Create an IdP source for a tenant
// @Description Creates a new identity provider source configuration for a tenant
// @Tags idp-sources
// @Accept json
// @Produce json
// @Param request body idp.CreateParams true "IdP source creation parameters"
// @Success 200 {object} idp.IdPSourceView
// @Failure 400 {object}ErrorResponse
// @Router /api/idp-sources [post]
func (h *IdPSourcesAPI) CreateIdPSource(c *gin.Context) {
	var req idp.CreateParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	view, err := h.Svc.Create(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, view)
}

// GetIdPSource godoc
// @Summary Get an IdP source
// @Description Retrieves a specific IdP source by tenant and provider
// @Tags idp-sources
// @Accept json
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Param provider path string true "Provider name"
// @Success 200 {object} idp.IdPSourceView
// @Failure 404 {object}ErrorResponse
// @Router /api/idp-sources/{tenant_id}/{provider} [get]
func (h *IdPSourcesAPI) GetIdPSource(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	provider := c.Param("provider")

	view, err := h.Svc.Get(c.Request.Context(), tenantID, provider)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if view == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "IdP source not found"})
		return
	}

	c.JSON(http.StatusOK, view)
}

// ListIdPSources godoc
// @Summary List all IdP sources for a tenant
// @Description Retrieves all IdP source configurations for a tenant
// @Tags idp-sources
// @Accept json
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Success 200 {array} idp.IdPSourceView
// @Router /api/idp-sources/{tenant_id} [get]
func (h *IdPSourcesAPI) ListIdPSources(c *gin.Context) {
	tenantID := c.Param("tenant_id")

	views, err := h.Svc.List(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, views)
}

// UpdateIdPSource godoc
// @Summary Update an IdP source
// @Description Updates an existing IdP source configuration
// @Tags idp-sources
// @Accept json
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Param provider path string true "Provider name"
// @Param request body idp.UpdateParams true "IdP source update parameters"
// @Success 200 {object} idp.IdPSourceView
// @Failure 400 {object}ErrorResponse
// @Failure 404 {object}ErrorResponse
// @Router /api/idp-sources/{tenant_id}/{provider} [put]
func (h *IdPSourcesAPI) UpdateIdPSource(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	provider := c.Param("provider")

	var req idp.UpdateParams
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Override path parameters
	req.TenantID = tenantID
	req.Provider = provider

	view, err := h.Svc.Update(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, view)
}

// DeleteIdPSource godoc
// @Summary Delete an IdP source
// @Description Removes an IdP source configuration
// @Tags idp-sources
// @Accept json
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Param provider path string true "Provider name"
// @Success 204
// @Failure 400 {object}ErrorResponse
// @Failure 404 {object}ErrorResponse
// @Router /api/idp-sources/{tenant_id}/{provider} [delete]
func (h *IdPSourcesAPI) DeleteIdPSource(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	provider := c.Param("provider")

	if err := h.Svc.Delete(c.Request.Context(), tenantID, provider); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetTenantIdPs godoc
// @Summary Get enabled IdP sources for login
// @Description Returns all enabled IdP sources for a tenant, filtered by provider_mapping
// @Tags idp-sources
// @Accept json
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Success 200 {array} idp.IdPSourceView
// @Router /api/idp-sources/{tenant_id}/enabled [get]
func (h *IdPSourcesAPI) GetTenantIdPs(c *gin.Context) {
	tenantID := c.Param("tenant_id")

	// Get tenant config from context (injected by middleware)
	// For now, pass nil to skip provider_mapping filtering
	views, err := h.Svc.GetTenantIdPs(c.Request.Context(), tenantID, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, views)
}

// GetTenantIdPsInternal godoc
// @Summary Get enabled IdP sources WITH secrets (internal only)
// @Description Server-to-server endpoint. Returns raw config including client_secret, bind_password, etc. Must be behind X-Internal-Token gate.
// @Tags idp-sources-internal
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Success 200 {array} idp.InternalIdPSourceView
// @Router /api/internal/idp-sources/{tenant_id}/enabled [get]
func (h *IdPSourcesAPI) GetTenantIdPsInternal(c *gin.Context) {
	if h.InternalSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "internal IdP service not configured"})
		return
	}
	tenantID := c.Param("tenant_id")
	views, err := h.InternalSvc.GetTenantIdPsInternal(c.Request.Context(), tenantID, h.TenantConfig)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, views)
}

// GetIdPSourceInternal godoc
// @Summary Get single IdP source WITH secrets (internal only)
// @Description Server-to-server endpoint. Returns raw config including client_secret. Must be behind X-Internal-Token gate.
// @Tags idp-sources-internal
// @Produce json
// @Param tenant_id path string true "Tenant identifier"
// @Param provider path string true "Provider name"
// @Success 200 {object} idp.InternalIdPSourceView
// @Failure 404 {object} ErrorResponse
// @Router /api/internal/idp-sources/{tenant_id}/{provider} [get]
func (h *IdPSourcesAPI) GetIdPSourceInternal(c *gin.Context) {
	if h.InternalSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "internal IdP service not configured"})
		return
	}
	tenantID := c.Param("tenant_id")
	provider := c.Param("provider")

	view, err := h.InternalSvc.GetInternal(c.Request.Context(), tenantID, provider)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if view == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "IdP source not found"})
		return
	}
	c.JSON(http.StatusOK, view)
}
