// Package idp contains the server-side RPC client for cs-user's identity
// provider endpoints (Phase E2.6). The client fetches per-tenant IdP configs
// (including OAuth client_secret) needed to initiate and complete OAuth flows
// for any provider configured via cs-user's /api/idp-sources CRUD.
//
// All endpoints consumed here are behind cs-user's X-Internal-Token gate
// (RequireInternalToken middleware). Never expose the InternalIdPSourceView
// to a public route — Config contains raw secrets.
package idp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/logger"
)

var (
	// ErrNotConfigured means the client lacks baseURL or internalToken.
	ErrNotConfigured = errors.New("idp rpc client: not configured")
	// ErrRPCUnavailable covers transport failures, timeouts, and 5xx responses.
	ErrRPCUnavailable = errors.New("idp rpc client: upstream unavailable")
	// ErrIdPNotFound signals cs-user returned 404 for a single-IdP lookup.
	ErrIdPNotFound = errors.New("idp rpc client: idp source not found")
)

const defaultTimeout = 10 * time.Second

// InternalIdPSourceView mirrors cs-user's idp.InternalIdPSourceView. Config
// contains raw secrets (client_secret, bind_password, etc.) — handle with care
// and never serialize into a public API response or log line.
type InternalIdPSourceView struct {
	TenantID  string                 `json:"tenant_id"`
	Provider  string                 `json:"provider"`
	Config    map[string]interface{} `json:"config"`
	Scope     string                 `json:"scope"`
	Enabled   bool                   `json:"enabled"`
	Priority  int                    `json:"priority"`
	CreatedAt string                 `json:"created_at,omitempty"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
	CreatedBy string                 `json:"created_by,omitempty"`
	UpdatedBy string                 `json:"updated_by,omitempty"`
}

// RPCClient talks to cs-user's /api/internal/idp-sources/* endpoints.
// Construct with NewRPCClient.
type RPCClient struct {
	baseURL       string
	internalToken string
	httpClient    *http.Client
}

// NewRPCClient builds an RPCClient from config. Shares the same cs-user
// endpoint as the user RPC client — pass the same UserServiceConfig values.
func NewRPCClient(cfg config.UserServiceConfig) *RPCClient {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &RPCClient{
		baseURL:       strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		internalToken: strings.TrimSpace(cfg.InternalToken),
		httpClient:    &http.Client{Timeout: timeout},
	}
}

// Configured reports whether baseURL + internalToken are both set.
func (c *RPCClient) Configured() bool {
	return c != nil && c.baseURL != "" && c.internalToken != ""
}

// ListEnabledIdPs calls GET /api/internal/idp-sources/:tenant_id/enabled.
// Returns the tenant's enabled IdPs sorted by priority DESC with full configs
// (including secrets). Caller is responsible for not leaking Config publicly.
func (c *RPCClient) ListEnabledIdPs(ctx context.Context, tenantID string) ([]InternalIdPSourceView, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if tenantID == "" {
		return nil, fmt.Errorf("idp rpc client: empty tenant_id")
	}
	path := "/api/internal/idp-sources/" + url.PathEscape(tenantID) + "/enabled"
	var views []InternalIdPSourceView
	if err := c.do(ctx, http.MethodGet, path, nil, &views); err != nil {
		return nil, err
	}
	if views == nil {
		views = []InternalIdPSourceView{}
	}
	return views, nil
}

// GetIdP calls GET /api/internal/idp-sources/:tenant_id/:provider.
// Returns ErrIdPNotFound when cs-user responds 404.
func (c *RPCClient) GetIdP(ctx context.Context, tenantID, provider string) (*InternalIdPSourceView, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if tenantID == "" || provider == "" {
		return nil, fmt.Errorf("idp rpc client: tenant_id and provider required")
	}
	path := "/api/internal/idp-sources/" + url.PathEscape(tenantID) + "/" + url.PathEscape(provider)
	var view InternalIdPSourceView
	if err := c.do(ctx, http.MethodGet, path, nil, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// do issues an authenticated request and decodes JSON into out. HTTP 404 →
// ErrIdPNotFound; transport errors / 5xx → ErrRPCUnavailable.
func (c *RPCClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	timeout := c.httpClient.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("idp rpc client: build request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[idp-rpc] %s %s request failed: %v", method, path, err)
		return fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[idp-rpc] %s %s read body failed: %v", method, path, err)
		return fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrIdPNotFound
	case resp.StatusCode >= 400:
		logger.Warn("[idp-rpc] %s %s returned status %d: %s",
			method, path, resp.StatusCode, truncate(string(respBody), 200))
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, resp.StatusCode)
		}
		return fmt.Errorf("idp rpc client: status %d", resp.StatusCode)
	}

	if len(respBody) == 0 || string(respBody) == "null" {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		logger.Warn("[idp-rpc] %s %s decode failed: %v", method, path, err)
		return fmt.Errorf("idp rpc client: decode response: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
