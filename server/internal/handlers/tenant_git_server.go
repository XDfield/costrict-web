// Tenant→git_server binding handlers (Git Ownership Refactor Phase 1).
//
// Three internal endpoints at /api/internal/tenants/:tenant_id/git-server:
//
//	PUT    /api/internal/tenants/:tenant_id/git-server  — upsert binding
//	GET    /api/internal/tenants/:tenant_id/git-server  — read binding
//	DELETE /api/internal/tenants/:tenant_id/git-server  — unbind
//
// These are server-local (no RPC to cs-user). tenant_id is the cs-user
// tenants.tenant_id; server doesn't validate the tenant exists (Phase 4 may
// add an existence check, but for now binding can be created ahead of the
// tenant row to support bootstrap ordering).
//
// Error mapping:
//
//	empty tenant_id                          → 400
//	git_server_id missing / unknown          → 400 / 404
//	internal store error                     → 500
//
// Note: 200 on PUT covers both insert and update; the response body records
// the bound git_server_id + bound_at timestamp so the caller can distinguish.

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// TenantGitServerBindingStore is the GORM surface for tenant_git_server_binding.
type TenantGitServerBindingStore interface {
	UpsertBinding(ctx context.Context, tenantID, gitServerID string) (*models.TenantGitServerBinding, error)
	GetBinding(ctx context.Context, tenantID string) (*models.TenantGitServerBinding, error)
	DeleteBinding(ctx context.Context, tenantID string) error
}

// GormTenantGitServerBindingStore is the production impl.
type GormTenantGitServerBindingStore struct {
	DB *gorm.DB
}

// NewGormTenantGitServerBindingStore binds a store.
func NewGormTenantGitServerBindingStore(db *gorm.DB) *GormTenantGitServerBindingStore {
	return &GormTenantGitServerBindingStore{DB: db}
}

// UpsertBinding inserts or updates the binding. Validates that gitServerID
// exists in git_servers (FK enforced at the DB level, but we surface a
// friendlier 404 here before INSERT).
func (s *GormTenantGitServerBindingStore) UpsertBinding(ctx context.Context, tenantID, gitServerID string) (*models.TenantGitServerBinding, error) {
	// Validate git_server exists (Otherwise we'd get a raw FK error).
	var gs models.GitServer
	if err := s.DB.WithContext(ctx).Select("server_id").First(&gs, "server_id = ?", gitServerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errGitServerNotFound
		}
		return nil, err
	}

	// Try INSERT first; on PK conflict, UPDATE.
	binding := &models.TenantGitServerBinding{
		TenantID:    tenantID,
		GitServerID: gitServerID,
		BoundAt:     time.Now(),
		UpdatedAt:   time.Now(),
	}
	tx := s.DB.WithContext(ctx).Create(binding)
	if tx.Error == nil {
		return binding, nil
	}
	if !isDuplicateErr(tx.Error) {
		return nil, tx.Error
	}
	// Existing row — update.
	if err := s.DB.WithContext(ctx).Model(&models.TenantGitServerBinding{}).
		Where("tenant_id = ?", tenantID).
		Updates(map[string]any{
			"git_server_id": gitServerID,
			"updated_at":    time.Now(),
		}).Error; err != nil {
		return nil, err
	}
	return s.GetBinding(ctx, tenantID)
}

// GetBinding returns the binding or gorm.ErrRecordNotFound.
func (s *GormTenantGitServerBindingStore) GetBinding(ctx context.Context, tenantID string) (*models.TenantGitServerBinding, error) {
	var b models.TenantGitServerBinding
	if err := s.DB.WithContext(ctx).First(&b, "tenant_id = ?", tenantID).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

// DeleteBinding removes the binding row. Idempotent — deleting a non-existent
// row is a 204 (caller intent satisfied).
func (s *GormTenantGitServerBindingStore) DeleteBinding(ctx context.Context, tenantID string) error {
	return s.DB.WithContext(ctx).Where("tenant_id = ?", tenantID).
		Delete(&models.TenantGitServerBinding{}).Error
}

// errGitServerNotFound — upsert refused because gitServerID doesn't resolve.
var errGitServerNotFound = errors.New("git_server not found")

// TenantGitServerBindingAPI is the receiver for tenant binding handlers.
type TenantGitServerBindingAPI struct {
	Store TenantGitServerBindingStore
}

// tenantBindingRequest is the PUT body shape. git_server_id is required.
type tenantBindingRequest struct {
	GitServerID string `json:"git_server_id" binding:"required"`
}

// tenantBindingResponse is the JSON shape returned to callers.
type tenantBindingResponse struct {
	TenantID    string `json:"tenant_id"`
	GitServerID string `json:"git_server_id"`
	BoundAt     string `json:"bound_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toTenantBindingResponse(b *models.TenantGitServerBinding) tenantBindingResponse {
	return tenantBindingResponse{
		TenantID:    b.TenantID,
		GitServerID: b.GitServerID,
		BoundAt:     b.BoundAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// BindTenantGitServer godoc
// @Summary  Bind a tenant to a git_server (upsert)
// @Tags     tenant-git-server
// @Accept   json
// @Produce  json
// @Security InternalToken
// @Param    tenant_id  path  string  true  "Tenant ID"
// @Param    body       body  handlers.tenantBindingRequest  true  "git_server_id to bind"
// @Success  200  {object}  handlers.tenantBindingResponse
// @Failure  400  {object}  object{error=string}
// @Failure  404  {object}  object{error=string}
// @Router   /api/internal/tenants/{tenant_id}/git-server [put]
func (a *TenantGitServerBindingAPI) BindTenantGitServer(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "binding store unavailable"})
		return
	}
	tenantID := strings.TrimSpace(c.Param("tenant_id"))
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
		return
	}
	var req tenantBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "git_server_id is required"})
		return
	}
	b, err := a.Store.UpsertBinding(c.Request.Context(), tenantID, req.GitServerID)
	if err != nil {
		if errors.Is(err, errGitServerNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "git_server not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, toTenantBindingResponse(b))
}

// GetTenantGitServerBinding godoc
// @Summary  Read a tenant's git_server binding
// @Tags     tenant-git-server
// @Produce  json
// @Security InternalToken
// @Param    tenant_id  path  string  true  "Tenant ID"
// @Success  200  {object}  handlers.tenantBindingResponse
// @Failure  404  {object}  object{error=string}
// @Router   /api/internal/tenants/{tenant_id}/git-server [get]
func (a *TenantGitServerBindingAPI) GetTenantGitServerBinding(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "binding store unavailable"})
		return
	}
	tenantID := strings.TrimSpace(c.Param("tenant_id"))
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
		return
	}
	b, err := a.Store.GetBinding(c.Request.Context(), tenantID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "tenant has no git_server binding"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, toTenantBindingResponse(b))
}

// UnbindTenantGitServer godoc
// @Summary  Unbind a tenant from its git_server (idempotent)
// @Tags     tenant-git-server
// @Produce  json
// @Security InternalToken
// @Param    tenant_id  path  string  true  "Tenant ID"
// @Success  204  {string}  string  "No Content"
// @Router   /api/internal/tenants/{tenant_id}/git-server [delete]
func (a *TenantGitServerBindingAPI) UnbindTenantGitServer(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "binding store unavailable"})
		return
	}
	tenantID := strings.TrimSpace(c.Param("tenant_id"))
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
		return
	}
	if err := a.Store.DeleteBinding(c.Request.Context(), tenantID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
