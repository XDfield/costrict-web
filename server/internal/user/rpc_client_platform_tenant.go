// rpc_client_platform_tenant.go — Phase C2 step C.
//
// 7 RPC methods on *RPCClient that proxy to cs-user's
// /api/internal/platform/tenants* (the C2 step B endpoints). The server's
// /api/platform/tenants* public surface (step D) uses these to forward
// platform-admin tenant CRUD operations without duplicating the tenants
// table (ADR D1 — cs-user owns tenant data).
//
// HTTP-to-Go-sentinel mapping (mirrors cs-user's
// respondPlatformTenantErr):
//   - 404 → ErrTenantNotFound
//   - 409 → ErrSlugTaken / ErrEmailDomainConflict / ErrInvalidStateTransition
//     (cs-user body's `error` string disambiguates; we string-match because
//     HTTP gives us nothing richer)
//   - 400 → ErrInvalidSlug / ErrInvalidEdition / ErrInvalidDisplayName /
//     ErrInvalidEmailDomains (same string-match)
//   - 5xx + transport errors → ErrRPCUnavailable (handler surfaces 502)
//
// All methods forward the X-Tenant-Id header from ctx (consistent with the
// read-path rpc_client_tenant.go forwarding — even though platform-admin
// calls are cross-tenant by design, cs-user's ResolveTenant middleware
// still runs on every request and an explicit slug preserves downstream
// trace context).

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
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// PlatformTenant mirrors the subset of cs-user's models.Tenant that the
// server consumes. Defined locally (not imported from cs-user) because the
// two services don't share a models package — ADR D1 keeps them decoupled
// at the type level so neither can accidentally acquire a foreign
// dependency.
type PlatformTenant struct {
	TenantID            string  `json:"tenant_id"`
	Slug                string  `json:"slug"`
	DisplayName         string  `json:"display_name"`
	Status              string  `json:"status"`
	Edition             string  `json:"edition"`
	EmailDomains        string  `json:"email_domains"`
	Features            string  `json:"features"`
	Limits              string  `json:"limits"`
	Settings            string  `json:"settings"`
	DeletionRequestedAt *string `json:"deletion_requested_at,omitempty"`
	CreatedAt           string  `json:"created_at,omitempty"`
	UpdatedAt           string  `json:"updated_at,omitempty"`
}

// PlatformTenantListResult mirrors cs-user's tenant.ListResult.
type PlatformTenantListResult struct {
	Tenants []PlatformTenant `json:"tenants"`
	Total   int64            `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

// PlatformTenantCreateParams mirrors cs-user's tenant.CreateParams.
type PlatformTenantCreateParams struct {
	Slug         string   `json:"slug"`
	DisplayName  string   `json:"display_name"`
	Edition      string   `json:"edition,omitempty"`
	EmailDomains []string `json:"email_domains,omitempty"`
	Features     string   `json:"features,omitempty"`
	Limits       string   `json:"limits,omitempty"`
	Settings     string   `json:"settings,omitempty"`
}

// PlatformTenantUpdateParams mirrors cs-user's tenant.UpdateParams — every
// field is a pointer so absent means "leave as-is" (true PATCH semantics).
type PlatformTenantUpdateParams struct {
	DisplayName  *string   `json:"display_name,omitempty"`
	Edition      *string   `json:"edition,omitempty"`
	EmailDomains *[]string `json:"email_domains,omitempty"`
	Features     *string   `json:"features,omitempty"`
	Limits       *string   `json:"limits,omitempty"`
	Settings     *string   `json:"settings,omitempty"`
}

// Platform-admin tenant RPC sentinels — distinct from cs-user's so the
// server can map its own HTTP codes without coupling to cs-user's error
// vars. The handler in step D translates these to HTTP.
var (
	ErrTenantNotFound         = errors.New("platform tenant: not found")
	ErrSlugTaken              = errors.New("platform tenant: slug already taken")
	ErrEmailDomainConflict    = errors.New("platform tenant: email domain conflict")
	ErrInvalidStateTransition = errors.New("platform tenant: invalid state transition")
	ErrInvalidSlug            = errors.New("platform tenant: invalid slug")
	ErrInvalidEdition         = errors.New("platform tenant: invalid edition")
	ErrInvalidDisplayName     = errors.New("platform tenant: invalid display name")
	ErrInvalidEmailDomains    = errors.New("platform tenant: invalid email domains")
)

const platformTenantsPath = "/api/internal/platform/tenants"

// doPlatformTenantRequest is the shared transport for all 7 platform-tenant
// RPC methods. It owns: timeout ctx, header injection (X-Internal-Token /
// X-Tenant-Id / Content-Type / Accept), HTTP-status → Go-sentinel mapping,
// and JSON decode into the supplied out. Method + path + body are the
// per-call inputs.
//
// Returns ErrRPCUnavailable for transport / 5xx; the 4xx mapping is in
// mapPlatformTenantHTTPError.
func (c *RPCClient) doPlatformTenantRequest(ctx context.Context, method, path string, body any, out any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("user rpc client: marshal platform-tenant request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("user rpc client: build platform-tenant request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Forward the caller's tenant slug so cs-user's ResolveTenant middleware
	// sees the same context (mirrors B3b.2a read-path forwarding).
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] platform-tenant %s %s request failed: %v", method, path, err)
		return fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] platform-tenant %s %s read body failed: %v", method, path, err)
		return fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		if resp.StatusCode >= 500 {
			logger.Warn("[user-rpc] platform-tenant %s %s returned %d: %s",
				method, path, resp.StatusCode, truncate(string(respBody), 200))
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, resp.StatusCode)
		}
		return mapPlatformTenantHTTPError(resp.StatusCode, respBody)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		logger.Warn("[user-rpc] platform-tenant %s %s decode failed: %v", method, path, err)
		return fmt.Errorf("user rpc client: decode platform-tenant response: %w", err)
	}
	return nil
}

// mapPlatformTenantHTTPError translates cs-user's 4xx + body into a
// Go-sentinel. The body's `error` field carries the cs-user sentinel
// message (e.g. "tenant: slug already taken") which we string-match —
// HTTP alone can't distinguish ErrSlugTaken from ErrEmailDomainConflict
// (both 409). The matching is intentionally tolerant: `strings.Contains`
// against a stable substring rather than full equality, so cs-user can
// reword the surrounding text without breaking the client.
func mapPlatformTenantHTTPError(status int, body []byte) error {
	msg := ""
	var raw struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &raw); err == nil {
		msg = strings.ToLower(raw.Error)
	}

	switch status {
	case http.StatusNotFound:
		return ErrTenantNotFound
	case http.StatusConflict:
		switch {
		case strings.Contains(msg, "slug already taken"):
			return ErrSlugTaken
		case strings.Contains(msg, "email domain"):
			return ErrEmailDomainConflict
		case strings.Contains(msg, "state transition"):
			return ErrInvalidStateTransition
		}
		return fmt.Errorf("platform tenant: 409 conflict: %s", truncate(string(body), 200))
	case http.StatusBadRequest:
		switch {
		case strings.Contains(msg, "slug"):
			return ErrInvalidSlug
		case strings.Contains(msg, "edition"):
			return ErrInvalidEdition
		case strings.Contains(msg, "display name"):
			return ErrInvalidDisplayName
		case strings.Contains(msg, "email domain"):
			return ErrInvalidEmailDomains
		}
		return fmt.Errorf("platform tenant: 400 bad request: %s", truncate(string(body), 200))
	}
	return fmt.Errorf("platform tenant: upstream status %d", status)
}

// ListTenants proxies GET /api/internal/platform/tenants.
func (c *RPCClient) ListTenants(ctx context.Context, limit, offset int, status string) (*PlatformTenantListResult, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if status != "" {
		q.Set("status", status)
	}
	path := platformTenantsPath
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out PlatformTenantListResult
	if err := c.doPlatformTenantRequest(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTenant proxies GET /api/internal/platform/tenants/:id (id OR slug).
func (c *RPCClient) GetTenant(ctx context.Context, idOrSlug string) (*PlatformTenant, error) {
	var out PlatformTenant
	if err := c.doPlatformTenantRequest(ctx, http.MethodGet, platformTenantsPath+"/"+url.PathEscape(idOrSlug), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTenant proxies POST /api/internal/platform/tenants.
func (c *RPCClient) CreateTenant(ctx context.Context, p PlatformTenantCreateParams) (*PlatformTenant, error) {
	var out PlatformTenant
	if err := c.doPlatformTenantRequest(ctx, http.MethodPost, platformTenantsPath, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateTenant proxies PATCH /api/internal/platform/tenants/:id.
func (c *RPCClient) UpdateTenant(ctx context.Context, idOrSlug string, p PlatformTenantUpdateParams) (*PlatformTenant, error) {
	var out PlatformTenant
	if err := c.doPlatformTenantRequest(ctx, http.MethodPatch, platformTenantsPath+"/"+url.PathEscape(idOrSlug), p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SuspendTenant proxies POST /api/internal/platform/tenants/:id/suspend.
func (c *RPCClient) SuspendTenant(ctx context.Context, idOrSlug string) (*PlatformTenant, error) {
	var out PlatformTenant
	if err := c.doPlatformTenantRequest(ctx, http.MethodPost, platformTenantsPath+"/"+url.PathEscape(idOrSlug)+"/suspend", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestoreTenant proxies POST /api/internal/platform/tenants/:id/restore.
func (c *RPCClient) RestoreTenant(ctx context.Context, idOrSlug string) (*PlatformTenant, error) {
	var out PlatformTenant
	if err := c.doPlatformTenantRequest(ctx, http.MethodPost, platformTenantsPath+"/"+url.PathEscape(idOrSlug)+"/restore", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTenant proxies POST /api/internal/platform/tenants/:id/delete.
func (c *RPCClient) DeleteTenant(ctx context.Context, idOrSlug string) (*PlatformTenant, error) {
	var out PlatformTenant
	if err := c.doPlatformTenantRequest(ctx, http.MethodPost, platformTenantsPath+"/"+url.PathEscape(idOrSlug)+"/delete", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
