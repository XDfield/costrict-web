// rpc_client_tenant_user.go — Phase C3.1 tenant-admin user listing.
//
// C3 sub-slice 1 (本 tenant 用户列表): tenant_admin lists users within
// their own tenant. cs-user already exposes
// GET /api/internal/users/search (InternalAuth-gated + auto-scoped to
// tenant_id via tenant.Scope(ctx) — the ResolveTenant middleware reads
// X-Tenant-Id from the inbound RPC and pins the query). So this RPC
// method is a thin proxy: forward keyword + limit, forward X-Tenant-Id
// (server's RequireTenantAdmin middleware guarantees the JWT-carrying
// tenant_admin is who they say they are, and the handler injects the
// JWT's tenant_slug claim as X-Tenant-Id before this call), and decode
// cs-user's {users: [...]} envelope into a typed slice.
//
// Why no cs-user-side changes: the existing endpoint already does
// exactly what tenant_admin needs (active-only, tenant-scoped, paginated
// via limit). The gap was server-side — no JWT-gated public path existed.
// C3.1 closes that gap with a single public route + this RPC method.
//
// Future: inactive-user inclusion (suspended members), pagination
// cursor, role filter — all deferred to C3.2 / a later iteration.

package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// TenantUser is the read projection of cs-user's models.User that this
// RPC method returns. Locally declared (not imported from cs-user) per
// ADR D1 — the two services stay type-decoupled so neither accidentally
// acquires a foreign dependency. The field set is the minimum
// tenant_admin UI needs to render a user list; structured fields like
// roles live in separate RPC calls.
type TenantUser struct {
	SubjectID   string `json:"subject_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	IsActive    bool   `json:"is_active"`
	TenantID    string `json:"tenant_id"`
}

// TenantUser sentinel errors. Distinct from cs-user's so the server
// doesn't couple to cs-user's error vars (ADR D1).
var (
	ErrTenantUserUnavailable = errors.New("tenant user service unavailable")
)

const tenantUsersSearchPath = "/api/internal/users/search"

// ListTenantUsers proxies GET /api/internal/users/search with the
// caller's tenant slug forwarded via X-Tenant-Id (set by the server
// handler from AuthClaims). cs-user's ResolveTenant middleware reads
// that header and pins tenant.Scope(ctx) so SearchUsers returns only
// users in that tenant.
//
// keyword == "" returns all active users in the tenant (cs-user's
// SearchUsers behavior). limit ≤ 0 falls back to cs-user's default
// (50); values > 200 are clamped server-side by cs-user.
func (c *RPCClient) ListTenantUsers(ctx context.Context, keyword string, limit int) ([]TenantUser, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	q := url.Values{}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := tenantUsersSearchPath
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("user rpc client: build tenant-users request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	// Forward the caller's tenant slug so cs-user's ResolveTenant
	// middleware pins the query. The server handler derives this from
	// AuthClaims.TenantSlug (JWT claim) before calling us; if absent
	// (e.g. platform_admin token without tenant binding), the request
	// fails downstream at cs-user's tenant.Scope(ctx) returning 503.
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] tenant-users GET %s request failed: %v", path, err)
		return nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] tenant-users GET %s read body failed: %v", path, err)
		return nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		logger.Warn("[user-rpc] tenant-users GET %s returned %d: %s",
			path, resp.StatusCode, truncate(string(body), 200))
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusServiceUnavailable {
			return nil, fmt.Errorf("%w: status %d", ErrRPCUnavailable, resp.StatusCode)
		}
		// 4xx (most likely 400 for bad limit) — surface as generic 502
		// to the tenant_admin since the server-side handler is the only
		// place that should 4xx; cs-user 4xx here means our RPC client
		// mangled the request.
		return nil, fmt.Errorf("%w: upstream 4xx: %s", ErrTenantUserUnavailable, truncate(string(body), 200))
	}

	// cs-user returns {"users": [...]}; tolerate either that envelope or
	// a bare array (defensive — cs-user currently emits the envelope).
	var users []TenantUser
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		if err := json.Unmarshal(body, &users); err != nil {
			return nil, fmt.Errorf("user rpc client: decode tenant-users array: %w", err)
		}
		return users, nil
	}
	var env struct {
		Users []TenantUser `json:"users"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("user rpc client: decode tenant-users envelope: %w", err)
	}
	return env.Users, nil
}
