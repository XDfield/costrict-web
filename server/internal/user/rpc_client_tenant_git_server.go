// rpc_client_tenant_git_server.go — Phase E3b.1.1 per-tenant Git server RPC.
//
// One method proxying to cs-user's /api/internal/tenants/:tenant_id/git-server:
//
//	GetTenantGitServer(ctx, tenantID) → GET /api/internal/tenants/:tenant_id/git-server
//
// Unlike the other tenant-scoped RPCs, the tenant_id is in the PATH (not the
// X-Tenant-Id header) because platform-admin may sync any tenant — the caller
// is not necessarily the tenant that owns the request. See the cs-user
// handler doc for the rationale.
//
// Sentinel mapping:
//
//	transport / 5xx / 503             → ErrRPCUnavailable
//	404 tenant not found              → ErrTenantNotFound
//	500 missing git_server_id         → ErrTenantMissingGitServer
//	500 git_server FK missing         → ErrGitServerNotFound
//	500 config malformed              → ErrGitServerConfigMalformed
//	503 git server disabled           → ErrGitServerDisabled
//	4xx (other)                       → ErrRPCUnavailable (operational)
//	ErrNotConfigured                  → RPC backend selected without URL/token
//
// Distinct from cs-user's gitserver.* per ADR D1 (type decoupling).

package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// TenantGitServerConfig is the local projection of cs-user's
// tenantGitServerResponse. Server-side type decoupled from cs-user per ADR D1.
//
// AdminUser / AdminPassword are optional credentials required for Gitea
// endpoints that reject admin PAT auth (notably POST /users/{name}/tokens —
// upstream Gitea's reqBasicOrRevProxyAuth middleware). cs-user returns them
// when the git_servers.config row carries them; absent fields mean the
// tenant's Gitea cannot provision bot PATs until ops adds the credentials.
type TenantGitServerConfig struct {
	ServerID      string `json:"server_id"`
	Kind          string `json:"kind"`
	Endpoint      string `json:"endpoint"`
	AdminToken    string `json:"admin_token"`
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
}

// Per-tenant git-server sentinel errors (server-side). Distinct from
// cs-user's gitserver.* per ADR D1. Prefixed ErrGitServer* to avoid
// collisions with platform-tenant's ErrTenantNotFound.
var (
	ErrGitServerTenantNotFound  = errors.New("tenant_git_server: tenant not found")
	ErrGitServerNoBinding       = errors.New("tenant_git_server: tenant has no git_server_id (bootstrap incomplete)")
	ErrGitServerRowMissing      = errors.New("tenant_git_server: git_server row missing (FK violation)")
	ErrGitServerConfigMalformed = errors.New("tenant_git_server: git_servers.config malformed or missing admin_token")
	ErrGitServerDisabled        = errors.New("tenant_git_server: git server disabled")
)

// GetTenantGitServer proxies GET /api/internal/tenants/:tenant_id/git-server.
// Returns the per-tenant Git server config (endpoint + admin_token).
//
// tenantID is the cs-user tenant_id (e.g. "t-acme"); passed in the URL path,
// NOT via X-Tenant-Id header — the sync may target any tenant, not just
// the caller's own.
func (c *RPCClient) GetTenantGitServer(ctx context.Context, tenantID string) (*TenantGitServerConfig, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(tenantID) == "" {
		return nil, ErrGitServerTenantNotFound
	}

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	path := fmt.Sprintf("/api/internal/tenants/%s/git-server", tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("user rpc client: build tenant-git-server request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	applyActorMetaHeaders(req, ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] tenant-git-server GET %s request failed: %v", path, err)
		return nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] tenant-git-server GET %s read body failed: %v", path, err)
		return nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		logger.Warn("[user-rpc] tenant-git-server GET %s returned %d: %s",
			path, resp.StatusCode, truncate(string(respBody), 200))
		return nil, mapTenantGitServerHTTPError(resp.StatusCode, string(respBody))
	}

	var cfg TenantGitServerConfig
	if err := json.Unmarshal(respBody, &cfg); err != nil {
		return nil, fmt.Errorf("user rpc client: decode tenant-git-server response: %w", err)
	}
	if cfg.Endpoint == "" || cfg.AdminToken == "" {
		// cs-user returned 200 but the body is incomplete — operator bug.
		return nil, fmt.Errorf("%w: endpoint or admin_token empty", ErrGitServerConfigMalformed)
	}
	return &cfg, nil
}

// mapTenantGitServerHTTPError translates cs-user's HTTP codes into
// server-side sentinels. Body-text matching against cs-user's exact
// error strings keeps the contract stable without coupling.
func mapTenantGitServerHTTPError(status int, body string) error {
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrGitServerTenantNotFound, truncate(body, 200))
	case http.StatusInternalServerError:
		switch {
		case containsSentinel(body, "no git_server_id"):
			return fmt.Errorf("%w: %s", ErrGitServerNoBinding, truncate(body, 200))
		case containsSentinel(body, "git_server row"):
			return fmt.Errorf("%w: %s", ErrGitServerRowMissing, truncate(body, 200))
		case containsSentinel(body, "config"):
			return fmt.Errorf("%w: %s", ErrGitServerConfigMalformed, truncate(body, 200))
		}
		// Unknown 500 — surface as RPC unavailable so callers retry / page.
		return fmt.Errorf("%w: status 500: %s", ErrRPCUnavailable, truncate(body, 200))
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %s", ErrGitServerDisabled, truncate(body, 200))
	default:
		if status >= 500 {
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, status)
		}
		// All other 4xx → 502-class operational fault.
		return fmt.Errorf("%w: upstream %d: %s", ErrRPCUnavailable, status, truncate(body, 200))
	}
}
