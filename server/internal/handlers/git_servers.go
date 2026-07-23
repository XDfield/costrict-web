// git_servers CRUD handlers (Git Ownership Refactor Phase 1).
//
// Four internal-only endpoints at /api/internal/git-servers that operate
// directly on the server's local git_servers table (server is the new owner;
// no RPC back to cs-user):
//
//	POST   /api/internal/git-servers                — create
//	GET    /api/internal/git-servers                — list
//	GET    /api/internal/git-servers/:server_id     — read
//	PUT    /api/internal/git-servers/:server_id     — update
//	DELETE /api/internal/git-servers/:server_id     — delete (soft-disabled)
//
// Auth: X-Internal-Token (middleware.InternalAuth), same as other
// /api/internal/* routes. Operators (bootstrap scripts, terraform, etc.)
// are the intended callers; no platform-admin role required at this layer.
//
// Error mapping:
//
//	validation error (missing kind/endpoint)        → 400
//	duplicate server_id                             → 409
//	not found                                       → 404
//	referenced by tenant_git_server_binding         → 409 (refuse delete)

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GitServerStore is the GORM surface the handler consumes. *gorm.DB
// satisfies it directly.
type GitServerStore interface {
	CreateGitServer(ctx context.Context, gs *models.GitServer) error
	ListGitServers(ctx context.Context) ([]models.GitServer, error)
	GetGitServer(ctx context.Context, serverID string) (*models.GitServer, error)
	UpdateGitServer(ctx context.Context, serverID string, updates map[string]any) (*models.GitServer, error)
	DeleteGitServer(ctx context.Context, serverID string) error
}

// GormGitServerStore is the production GitServerStore, wrapping *gorm.DB.
type GormGitServerStore struct {
	DB *gorm.DB
}

// NewGormGitServerStore binds a store to the supplied pool.
func NewGormGitServerStore(db *gorm.DB) *GormGitServerStore {
	return &GormGitServerStore{DB: db}
}

// CreateGitServer inserts a new row. Uses Select("*") to bypass GORM's
// zero-value skipping — otherwise a row created with Enabled=false would
// silently pick up the column's DEFAULT true.
func (s *GormGitServerStore) CreateGitServer(ctx context.Context, gs *models.GitServer) error {
	return s.DB.WithContext(ctx).Select("*").Create(gs).Error
}

// ListGitServers returns all rows ordered by created_at desc.
func (s *GormGitServerStore) ListGitServers(ctx context.Context) ([]models.GitServer, error) {
	var rows []models.GitServer
	if err := s.DB.WithContext(ctx).Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetGitServer returns one row or gorm.ErrRecordNotFound.
func (s *GormGitServerStore) GetGitServer(ctx context.Context, serverID string) (*models.GitServer, error) {
	var gs models.GitServer
	if err := s.DB.WithContext(ctx).First(&gs, "server_id = ?", serverID).Error; err != nil {
		return nil, err
	}
	return &gs, nil
}

// UpdateGitServer applies the supplied updates and returns the new row.
func (s *GormGitServerStore) UpdateGitServer(ctx context.Context, serverID string, updates map[string]any) (*models.GitServer, error) {
	if err := s.DB.WithContext(ctx).Model(&models.GitServer{}).
		Where("server_id = ?", serverID).
		Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetGitServer(ctx, serverID)
}

// DeleteGitServer removes the row. Refuses (returns errGitServerInUse) if
// any tenant_git_server_binding still references it.
func (s *GormGitServerStore) DeleteGitServer(ctx context.Context, serverID string) error {
	var count int64
	if err := s.DB.WithContext(ctx).Model(&models.TenantGitServerBinding{}).
		Where("git_server_id = ?", serverID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return errGitServerInUse
	}
	return s.DB.WithContext(ctx).Where("server_id = ?", serverID).
		Delete(&models.GitServer{}).Error
}

// errGitServerInUse — DELETE refused because a tenant_git_server_binding
// still references the row. Operator must unbind first.
var errGitServerInUse = errors.New("git_server is bound to one or more tenants; unbind before deleting")

// GitServerAPI is the receiver for git_servers CRUD handlers.
type GitServerAPI struct {
	Store GitServerStore
}

// gitServerCreateRequest is the POST body shape. Config is a raw JSON
// object passed through to the JSONB column.
type gitServerCreateRequest struct {
	Kind        string          `json:"kind" binding:"required"`
	Endpoint    string          `json:"endpoint" binding:"required"`
	DisplayName string          `json:"display_name" binding:"required"`
	Config      json.RawMessage `json:"config,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
}

// gitServerUpdateRequest is the PUT body shape. All fields optional; only
// supplied fields are updated (nil-config = leave alone, {} = clear).
type gitServerUpdateRequest struct {
	Kind        *string          `json:"kind,omitempty"`
	Endpoint    *string          `json:"endpoint,omitempty"`
	DisplayName *string          `json:"display_name,omitempty"`
	Config      *json.RawMessage `json:"config,omitempty"`
	Enabled     *bool            `json:"enabled,omitempty"`
}

// gitServerResponse is the JSON shape returned to callers. Sensitive config
// is echoed verbatim — callers are internal and already authorized.
type gitServerResponse struct {
	ServerID    string    `json:"server_id"`
	Kind        string    `json:"kind"`
	Endpoint    string    `json:"endpoint"`
	DisplayName string    `json:"display_name"`
	Config      string    `json:"config"`
	IsTemplate  bool      `json:"is_template"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

func toGitServerResponse(gs *models.GitServer) gitServerResponse {
	return gitServerResponse{
		ServerID:    gs.ServerID,
		Kind:        gs.Kind,
		Endpoint:    gs.Endpoint,
		DisplayName: gs.DisplayName,
		Config:      gs.Config,
		IsTemplate:  gs.IsTemplate,
		Enabled:     gs.Enabled,
		CreatedAt:   gs.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   gs.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// normalizeConfig normalizes the raw JSON input to a storable string. Empty
// or absent → "{}". Invalid JSON → error.
func normalizeConfig(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}", nil
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("config must be a valid JSON object: %w", err)
	}
	// Re-marshal to canonicalize whitespace.
	out, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// CreateGitServer godoc
// @Summary  Create a git_server row
// @Tags     git-servers
// @Accept   json
// @Produce  json
// @Security InternalToken
// @Param    body  body      handlers.gitServerCreateRequest  true  "git_server fields"
// @Success  201   {object}  handlers.gitServerResponse
// @Failure  400   {object}  object{error=string}
// @Failure  409   {object}  object{error=string}
// @Router   /api/internal/git-servers [post]
func (a *GitServerAPI) CreateGitServer(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git-server store unavailable"})
		return
	}
	var req gitServerCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind, endpoint, and display_name are required"})
		return
	}
	if req.Kind != models.GitServerKindGitea {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported kind %q (only %q supported)", req.Kind, models.GitServerKindGitea)})
		return
	}
	cfgStr, err := normalizeConfig(req.Config)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	gs := &models.GitServer{
		ServerID:    "gs-" + uuid.NewString(),
		Kind:        req.Kind,
		Endpoint:    strings.TrimRight(req.Endpoint, "/"),
		DisplayName: req.DisplayName,
		Config:      cfgStr,
		IsTemplate:  false,
		Enabled:     enabled,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := a.Store.CreateGitServer(c.Request.Context(), gs); err != nil {
		if isDuplicateErr(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "server_id already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, toGitServerResponse(gs))
}

// ListGitServers godoc
// @Summary  List all git_server rows
// @Tags     git-servers
// @Produce  json
// @Security InternalToken
// @Success  200  {array}   handlers.gitServerResponse
// @Router   /api/internal/git-servers [get]
func (a *GitServerAPI) ListGitServers(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git-server store unavailable"})
		return
	}
	rows, err := a.Store.ListGitServers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gitServerResponse, 0, len(rows))
	for i := range rows {
		out = append(out, toGitServerResponse(&rows[i]))
	}
	c.JSON(http.StatusOK, out)
}

// GetGitServer godoc
// @Summary  Read one git_server row
// @Tags     git-servers
// @Produce  json
// @Security InternalToken
// @Param    server_id  path  string  true  "git_servers.server_id"
// @Success  200  {object}  handlers.gitServerResponse
// @Failure  404  {object}  object{error=string}
// @Router   /api/internal/git-servers/{server_id} [get]
func (a *GitServerAPI) GetGitServer(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git-server store unavailable"})
		return
	}
	serverID := strings.TrimSpace(c.Param("server_id"))
	if serverID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server_id required"})
		return
	}
	gs, err := a.Store.GetGitServer(c.Request.Context(), serverID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "git_server not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, toGitServerResponse(gs))
}

// UpdateGitServer godoc
// @Summary  Update a git_server row
// @Tags     git-servers
// @Accept   json
// @Produce  json
// @Security InternalToken
// @Param    server_id  path  string  true  "git_servers.server_id"
// @Param    body       body  handlers.gitServerUpdateRequest  true  "fields to update"
// @Success  200  {object}  handlers.gitServerResponse
// @Failure  400  {object}  object{error=string}
// @Failure  404  {object}  object{error=string}
// @Router   /api/internal/git-servers/{server_id} [put]
func (a *GitServerAPI) UpdateGitServer(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git-server store unavailable"})
		return
	}
	serverID := strings.TrimSpace(c.Param("server_id"))
	if serverID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server_id required"})
		return
	}
	var req gitServerUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// PUT with empty body is valid (no-op); only fail on malformed JSON.
		if !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if req.Kind != nil && *req.Kind != models.GitServerKindGitea {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported kind %q", *req.Kind)})
		return
	}
	updates := map[string]any{}
	if req.Kind != nil {
		updates["kind"] = *req.Kind
	}
	if req.Endpoint != nil {
		updates["endpoint"] = strings.TrimRight(*req.Endpoint, "/")
	}
	if req.DisplayName != nil {
		updates["display_name"] = *req.DisplayName
	}
	if req.Config != nil {
		cfgStr, err := normalizeConfig(*req.Config)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates["config"] = cfgStr
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) == 0 {
		// No fields to update — return current row for caller convenience.
		gs, err := a.Store.GetGitServer(c.Request.Context(), serverID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "git_server not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, toGitServerResponse(gs))
		return
	}
	gs, err := a.Store.UpdateGitServer(c.Request.Context(), serverID, updates)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "git_server not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, toGitServerResponse(gs))
}

// DeleteGitServer godoc
// @Summary  Delete a git_server row (refuses if tenant bindings exist)
// @Tags     git-servers
// @Produce  json
// @Security InternalToken
// @Param    server_id  path  string  true  "git_servers.server_id"
// @Success  204  {string}  string  "No Content"
// @Failure  404  {object}  object{error=string}
// @Failure  409  {object}  object{error=string}
// @Router   /api/internal/git-servers/{server_id} [delete]
func (a *GitServerAPI) DeleteGitServer(c *gin.Context) {
	if a == nil || a.Store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git-server store unavailable"})
		return
	}
	serverID := strings.TrimSpace(c.Param("server_id"))
	if serverID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server_id required"})
		return
	}
	if err := a.Store.DeleteGitServer(c.Request.Context(), serverID); err != nil {
		if errors.Is(err, errGitServerInUse) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "git_server not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// isDuplicateErr sniffs duplicate-key errors driver-agnostically. Mirrors
// the heuristic in cs-user's giteasync package.
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "23505")
}
