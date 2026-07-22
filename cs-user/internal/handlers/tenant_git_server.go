// Tenant Git server lookup RPC (Phase E3b.1.1).
//
// GET /api/internal/tenants/:tenant_id/git-server lets @server's gitsync
// resolve the per-tenant Gitea endpoint + admin_token without direct DB
// access. Same internal-token gating as other /api/internal/* endpoints.
//
// Path-based tenant_id (vs. X-Tenant-Id header) because platform-admin may
// sync any tenant, not just the caller's own. Future tenant_admin role
// would constrain to own tenant_id from ctx.
//
// Error mapping:
//
//	missing / empty tenant_id            → 400
//	ErrTenantNotFound                    → 404
//	ErrTenantMissingGitServer            → 500 (operator: bootstrap incomplete)
//	ErrGitServerNotFound                 → 500 (FK violation — should be impossible)
//	ErrGitServerDisabled                 → 503 (drained)
//	ErrConfigMalformed                   → 500 (operator: fix the config blob)

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/cs-user/internal/gitserver"
	"github.com/gin-gonic/gin"
)

// TenantGitServerAPI exposes the per-tenant git-server lookup. Svc is an
// interface so handler tests substitute a fake.
type TenantGitServerAPI struct {
	Svc TenantGitServerResolver
}

// TenantGitServerResolver mirrors gitserver.Resolver. Declared as a
// handler-side interface so tests inject a stub without pulling the
// resolver's *gorm.DB dependency into the test surface.
type TenantGitServerResolver interface {
	Resolve(ctx context.Context, tenantID string) (*gitserver.Config, error)
}

// tenantGitServerResponse is the JSON body returned to the caller.
//
//	admin_token / admin_password are sensitive — the internal network is
//	trusted (already used for user-gitea-binding reads), but the handler
//	must NEVER log them. admin_user / admin_password are optional; absent
//	when the tenant's git_servers.config doesn't carry them (the calling
//	gitsync.Client falls back to admin-token-only auth, which suffices for
//	every endpoint except the token-mint paths).
type tenantGitServerResponse struct {
	ServerID      string `json:"server_id"`
	Kind          string `json:"kind"`
	Endpoint      string `json:"endpoint"`
	AdminToken    string `json:"admin_token"`
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
}

// GetTenantGitServer godoc
//
//	@Summary		Read a tenant's Git server config (internal RPC)
//	@Description	Returns the endpoint + admin_token bound to the supplied tenant. Internal-token gated; consumed by @server gitsync.
//	@Tags			tenant-git-server
//	@Produce		json
//	@Security		InternalToken
//	@Param			tenant_id	path		string	true	"Tenant ID"
//	@Success		200			{object}	tenantGitServerResponse
//	@Failure		400			{object}	object{error=string}
//	@Failure		404			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Failure		503			{object}	object{error=string}
//	@Router			/api/internal/tenants/{tenant_id}/git-server [get]
func (a *TenantGitServerAPI) GetTenantGitServer(c *gin.Context) {
	if a == nil || a.Svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git-server service unavailable"})
		return
	}
	tenantID := strings.TrimSpace(c.Param("tenant_id"))
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
		return
	}

	cfg, err := a.Svc.Resolve(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(mapTenantGitServerError(err), gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, tenantGitServerResponse{
		ServerID:      cfg.ServerID,
		Kind:          cfg.Kind,
		Endpoint:      cfg.Endpoint,
		AdminToken:    cfg.AdminToken,
		AdminUser:     cfg.AdminUser,
		AdminPassword: cfg.AdminPassword,
	})
}

// mapTenantGitServerError translates the resolver's sentinel vocabulary to
// HTTP status codes. Kept in sync with gitserver/resolver.go's sentinel set.
func mapTenantGitServerError(err error) int {
	switch {
	case errors.Is(err, gitserver.ErrTenantNotFound):
		return http.StatusNotFound
	case errors.Is(err, gitserver.ErrGitServerDisabled):
		return http.StatusServiceUnavailable
	case errors.Is(err, gitserver.ErrTenantMissingGitServer),
		errors.Is(err, gitserver.ErrGitServerNotFound),
		errors.Is(err, gitserver.ErrConfigMalformed):
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}
