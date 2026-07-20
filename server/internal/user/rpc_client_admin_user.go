// rpc_client_admin_user.go — admin-user-migration slice RPC methods.
//
// Four methods proxying to cs-user's /api/internal/users/* admin surface:
//
//	ListUsers(ctx, params)         → GET  /api/internal/users/list?...
//	SetUserStatus(ctx, sid, s, op) → POST /api/internal/users/:subject_id/status
//	ListOrganizations(ctx)         → GET  /api/internal/users/organizations
//	GetUserProfile(ctx, sid)       → GET  /api/internal/users/:subject_id/profile
//
// cs-user is the single source of truth for user identity + status
// (admin-user-migration slice, option A full migration). @server's
// adminuser.Module swaps from local DB queries to these RPCs (Commit 7);
// activity counts (capability_items, item_distributions) stay local to
// @server because those tables live in costrict_db, not cs_user.
//
// Sentinel mapping:
//
//	transport / 5xx / 503             → ErrRPCUnavailable
//	400 invalid status                → ErrAdminUserRPCInvalidStatus
//	404 unknown subject_id            → ErrAdminUserRPCNotFound
//	409 self-lock                     → ErrAdminUserRPCCannotChangeOwn
//	ErrNotConfigured                  → RPC backend selected without URL/token
//
// Distinct from cs-user's admin_service sentinels per ADR D1 (type
// decoupling between the two modules).

package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// AdminUser is the local read projection of cs-user's adminUserListItem.
// Locally declared (not imported from cs-user) per ADR D1.
type AdminUser struct {
	SubjectID    string  `json:"subject_id"`
	Username     string  `json:"username"`
	DisplayName  *string `json:"display_name,omitempty"`
	Email        *string `json:"email,omitempty"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	Organization *string `json:"organization,omitempty"`
	Status       string  `json:"status"`
	IsActive     bool    `json:"is_active"`
	CreatedAt    string  `json:"created_at"`
}

// AdminUserListParams narrows the admin user list query. Same shape as
// cs-user's ListUsersParams but as a server-side type.
type AdminUserListParams struct {
	Keyword      string
	Organization string
	Status       string
	Page         int
	PageSize     int
}

// AdminUserListResult is the decoded envelope from cs-user's /list.
type AdminUserListResult struct {
	Users []AdminUser `json:"users"`
	Total int64       `json:"total"`
	Page  int         `json:"page"`
	Size  int         `json:"page_size"`
}

// AdminOrganization is the local read projection of cs-user's
// OrganizationCount. Note the camelCase memberCount tag — mirrors cs-user's
// JSON tag exactly so decoding lines up.
type AdminOrganization struct {
	Organization string `json:"organization"`
	MemberCount  int64  `json:"memberCount"`
}

// AdminUserProfile is the local read projection of cs-user's
// adminUserProfileDTO. Mirrors the privacy-scoped field set cs-user emits
// (no external_key, casdoor_*, provider_user_id).
type AdminUserProfile struct {
	SubjectID    string  `json:"subject_id"`
	Username     string  `json:"username"`
	DisplayName  *string `json:"display_name,omitempty"`
	Email        *string `json:"email,omitempty"`
	Phone        *string `json:"phone,omitempty"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	AuthProvider *string `json:"auth_provider,omitempty"`
	Organization *string `json:"organization,omitempty"`
	Status       string  `json:"status"`
	IsActive     bool    `json:"is_active"`
	LastLoginAt  *string `json:"last_login_at,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

// AdminSetUserStatusResult is the local projection of cs-user's
// SetUserStatus response ({from_status, to_status}).
type AdminSetUserStatusResult struct {
	FromStatus string `json:"from_status"`
	ToStatus   string `json:"to_status"`
}

// Admin-user sentinel errors. Distinct from cs-user's so the server doesn't
// couple to cs-user's error vars (ADR D1). Prefixed ErrAdminUserRPC* to
// avoid collisions with the existing local admin_service.go sentinels
// (which will be removed in Commit 8 once the local path is decommissioned).
var (
	ErrAdminUserRPCInvalidStatus   = errors.New("admin user rpc: invalid status")
	ErrAdminUserRPCNotFound        = errors.New("admin user rpc: not found")
	ErrAdminUserRPCCannotChangeOwn = errors.New("admin user rpc: cannot change own status")
)

const (
	adminUsersListPath          = "/api/internal/users/list"
	adminUsersOrganizationsPath = "/api/internal/users/organizations"
)

// ListUsers proxies GET /api/internal/users/list. Forwards keyword /
// organization / status / page / page_size as query params and the
// caller's tenant slug via X-Tenant-Id (cs-user's ResolveTenant pins
// tenant.Scope(ctx) so the query is scoped to that tenant).
func (c *RPCClient) ListUsers(ctx context.Context, p AdminUserListParams) (*AdminUserListResult, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	q := url.Values{}
	if p.Keyword != "" {
		q.Set("keyword", p.Keyword)
	}
	if p.Organization != "" {
		q.Set("organization", p.Organization)
	}
	if p.Status != "" {
		q.Set("status", p.Status)
	}
	if p.Page > 0 {
		q.Set("page", strconv.Itoa(p.Page))
	}
	if p.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(p.PageSize))
	}
	path := adminUsersListPath
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	statusCode, body, err := c.adminUserDo(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		return nil, mapAdminUserHTTPError(statusCode, string(body))
	}

	var out AdminUserListResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("user rpc client: decode admin-users-list response: %w", err)
	}
	if out.Users == nil {
		out.Users = []AdminUser{}
	}
	return &out, nil
}

// SetUserStatus proxies POST /api/internal/users/:subject_id/status. The
// operator_id is forwarded in the body so cs-user can run the self-lock
// check and attribute the audit row. Returns the before/after status so
// the caller (@server adminuser handler) can render or further audit the
// transition.
func (c *RPCClient) SetUserStatus(ctx context.Context, subjectID, status, operatorID string) (*AdminSetUserStatusResult, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	bodyJSON, err := json.Marshal(map[string]string{
		"status":      status,
		"operator_id": operatorID,
	})
	if err != nil {
		return nil, fmt.Errorf("user rpc client: marshal set-status body: %w", err)
	}
	path := fmt.Sprintf("/api/internal/users/%s/status", url.PathEscape(subjectID))

	statusCode, body, err := c.adminUserDo(ctx, http.MethodPost, path, bodyJSON)
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		return nil, mapAdminUserHTTPError(statusCode, string(body))
	}

	var out AdminSetUserStatusResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("user rpc client: decode set-status response: %w", err)
	}
	return &out, nil
}

// ListOrganizations proxies GET /api/internal/users/organizations. Returns
// the per-tenant org roll-up (busiest first); nil-safe — cs-user always
// returns a JSON array, never null.
func (c *RPCClient) ListOrganizations(ctx context.Context) ([]AdminOrganization, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	statusCode, body, err := c.adminUserDo(ctx, http.MethodGet, adminUsersOrganizationsPath, nil)
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		return nil, mapAdminUserHTTPError(statusCode, string(body))
	}

	var env struct {
		Organizations []AdminOrganization `json:"organizations"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("user rpc client: decode admin-orgs response: %w", err)
	}
	if env.Organizations == nil {
		env.Organizations = []AdminOrganization{}
	}
	return env.Organizations, nil
}

// GetUserProfile proxies GET /api/internal/users/:subject_id/profile.
// Returns the identity + status slice; @server supplements with locally-
// computed activity counts before returning to the admin UI.
func (c *RPCClient) GetUserProfile(ctx context.Context, subjectID string) (*AdminUserProfile, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	path := fmt.Sprintf("/api/internal/users/%s/profile", url.PathEscape(subjectID))

	statusCode, body, err := c.adminUserDo(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		return nil, mapAdminUserHTTPError(statusCode, string(body))
	}

	var out AdminUserProfile
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("user rpc client: decode admin-profile response: %w", err)
	}
	return &out, nil
}

// adminUserDo is the shared HTTP executor for admin-user RPCs. Centralised
// here so all four methods share the same header plumbing (X-Internal-Token,
// X-Tenant-Id forwarding, actor-meta headers, timeout) and error mapping.
// Returns (statusCode, body, err) — err is non-nil only for transport faults
// (mapped to ErrRPCUnavailable); 4xx/5xx are returned via statusCode so the
// caller can run mapAdminUserHTTPError.
func (c *RPCClient) adminUserDo(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, fmt.Errorf("user rpc client: build admin-user request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}
	applyActorMetaHeaders(req, ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] admin-user %s %s request failed: %v", method, path, err)
		return 0, nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		logger.Warn("[user-rpc] admin-user %s %s read body failed: %v", method, path, readErr)
		return 0, nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, readErr)
	}
	return resp.StatusCode, respBody, nil
}

// mapAdminUserHTTPError translates cs-user's HTTP codes into server-side
// sentinels. Body-text matching against cs-user's exact error strings
// keeps the contract stable without coupling.
func mapAdminUserHTTPError(status int, body string) error {
	switch status {
	case http.StatusBadRequest:
		// cs-user emits "status must be one of active|disabled|banned" for
		// invalid status, "subject_id is required", or "invalid body: ...".
		// All are caller bugs; surface as ErrAdminUserRPCInvalidStatus.
		return fmt.Errorf("%w: %s", ErrAdminUserRPCInvalidStatus, truncate(body, 200))
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrAdminUserRPCNotFound, truncate(body, 200))
	case http.StatusConflict:
		// cs-user emits "user: cannot change own status" for the self-lock.
		return fmt.Errorf("%w: %s", ErrAdminUserRPCCannotChangeOwn, truncate(body, 200))
	default:
		if status >= 500 || status == http.StatusServiceUnavailable {
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, status)
		}
		// Other 4xx — surface as RPC unavailable (operational fault; the
		// server-side handler is the only legitimate 4xx source here).
		return fmt.Errorf("%w: upstream %d", ErrRPCUnavailable, status)
	}
}
