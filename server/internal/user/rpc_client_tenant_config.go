// rpc_client_tenant_config.go — Phase C3.2 tenant-admin config CRUD.
//
// Two methods proxying to cs-user's /api/internal/tenant/config:
//
//	GetTenantConfig(ctx)    → GET  /api/internal/tenant/config
//	UpdateTenantConfig(...) → PUT  /api/internal/tenant/config
//
// Tenant scoping is via X-Tenant-Id (forwarded from ctx by the same
// tenantSlugFromContext helper every other tenant-scoped RPC method uses).
// The server handler injects the JWT's TenantSlug claim into ctx before
// calling us, so cs-user's ResolveTenant middleware pins the row to the
// caller's tenant — there is no tenant_id path or body field the caller
// can spoof.
//
// updated_by (the JWT subject_id of the editing tenant_admin) is forwarded
// via X-Actor-Subject-Id so cs-user's audit trail records who changed the
// YAML. Empty actor (e.g. a service-account caller) is allowed; cs-user
// stores NULL.
//
// Sentinel mapping (server-side sentinels distinct from cs-user's per ADR D1):
//
//	transport / 5xx / 503  → ErrRPCUnavailable
//	4xx from upstream      → ErrTenantConfigUnavailable (RPC client mangled
//	                         the request — the only legitimate 4xx source
//	                         for this surface is the server handler)
//	ErrNotConfigured       → RPC backend selected without URL/token
//	ErrInvalidYAML         → cs-user rejected the YAML body (400 echo)
//	ErrYAMLTooLarge        → cs-user rejected the YAML as too big (413 echo)

package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// TenantConfig is the local read projection of cs-user's
// models.TenantConfig. Locally declared per ADR D1 (type decoupling).
// Field set matches the JSON shape cs-user emits exactly.
type TenantConfig struct {
	TenantID   string  `json:"tenant_id"`
	ConfigYAML string  `json:"config_yaml"`
	UpdatedBy  *string `json:"updated_by"`
	UpdatedAt  string  `json:"updated_at"`
	CreatedAt  string  `json:"created_at"`
}

// TenantConfig sentinel errors. Distinct from cs-user's so the server
// doesn't couple to cs-user's error vars (ADR D1).
var (
	ErrTenantConfigUnavailable = errors.New("tenant config service unavailable")
	ErrInvalidYAML             = errors.New("tenant config: invalid YAML")
	ErrYAMLTooLarge            = errors.New("tenant config: YAML exceeds size cap")
)

const (
	tenantConfigPath = "/api/internal/tenant/config"
	// ActorSubjectIDHeader forwards the JWT subject_id of the editing
	// tenant_admin so cs-user can stamp updated_by. Picked over a body
	// field so the audit trail can't drift from the auth claim.
	ActorSubjectIDHeader = "X-Actor-Subject-Id"
)

// GetTenantConfig proxies GET /api/internal/tenant/config. Returns the
// caller's tenant_configs row (cs-user returns a synthetic default {} on
// missing row, so this method never surfaces a 404).
func (c *RPCClient) GetTenantConfig(ctx context.Context) (*TenantConfig, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	body, err := c.doTenantConfig(ctx, http.MethodGet, nil, "")
	if err != nil {
		return nil, err
	}
	var tc TenantConfig
	if err := json.Unmarshal(body, &tc); err != nil {
		return nil, fmt.Errorf("user rpc client: decode tenant-config response: %w", err)
	}
	return &tc, nil
}

// UpdateTenantConfig proxies PUT /api/internal/tenant/config. yamlStr is
// the raw blob (server handler validates HTTP body shape before calling
// us; cs-user validates that it parses as YAML). actorSubjectID is the
// JWT subject_id of the editing tenant_admin — empty string is allowed
// (cs-user stores NULL updated_by).
//
// On success returns the row as written (with refreshed updated_at).
func (c *RPCClient) UpdateTenantConfig(ctx context.Context, yamlStr, actorSubjectID string) (*TenantConfig, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	reqBody, err := encodeTenantConfigRequest(yamlStr)
	if err != nil {
		return nil, err
	}

	body, err := c.doTenantConfig(ctx, http.MethodPut, reqBody, actorSubjectID)
	if err != nil {
		return nil, err
	}
	var tc TenantConfig
	if err := json.Unmarshal(body, &tc); err != nil {
		return nil, fmt.Errorf("user rpc client: decode tenant-config response: %w", err)
	}
	return &tc, nil
}

// doTenantConfig issues the request + maps the response. Returns the raw
// body on success so the caller can decode into the typed shape; returns
// a wrapped sentinel on error.
func (c *RPCClient) doTenantConfig(ctx context.Context, method string, body []byte, actorSubjectID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+tenantConfigPath, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("user rpc client: build tenant-config request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}
	if actorSubjectID != "" {
		req.Header.Set(ActorSubjectIDHeader, actorSubjectID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] tenant-config %s %s request failed: %v", method, tenantConfigPath, err)
		return nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] tenant-config %s %s read body failed: %v", method, tenantConfigPath, err)
		return nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		logger.Warn("[user-rpc] tenant-config %s %s returned %d: %s",
			method, tenantConfigPath, resp.StatusCode, truncate(string(respBody), 200))
		return nil, mapTenantConfigHTTPError(resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// mapTenantConfigHTTPError translates cs-user's HTTP codes into server-side
// sentinels. Echoes cs-user's exact sentinel text in the body (set by
// handlers.respondTenantConfigErr) so callers can route by sentinel
// without coupling to a free-text error message.
func mapTenantConfigHTTPError(status int, body string) error {
	switch status {
	case http.StatusBadRequest:
		// 400 has two sub-cases: "invalid YAML" (cs-user service rejected)
		// and "tenant resolution required" (cs-user middleware bug; the
		// server handler should have caught this). The former is the
		// expected one and surfaces as ErrInvalidYAML.
		if containsSentinel(body, "invalid YAML") {
			return fmt.Errorf("%w: %s", ErrInvalidYAML, truncate(body, 200))
		}
		return fmt.Errorf("%w: upstream 400: %s", ErrTenantConfigUnavailable, truncate(body, 200))
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %s", ErrYAMLTooLarge, truncate(body, 200))
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%w: status %d", ErrRPCUnavailable, status)
	default:
		if status >= 500 {
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, status)
		}
		// Any other 4xx (e.g. 401 from a misconfigured internal token, 404
		// if cs-user's route table isn't wired) — surface as 502-class
		// "tenant config service unavailable" since these all indicate
		// operational problems, not client-fixable ones.
		return fmt.Errorf("%w: upstream %d: %s", ErrTenantConfigUnavailable, status, truncate(body, 200))
	}
}

// containsSentinel does a case-sensitive substring check. Used to disambiguate
// cs-user's body-text sentinels ("invalid YAML" vs "tenant resolution
// required") rather than relying on JSON parsing — cs-user's response is
// {"error": "..."} but the inner text is the stable contract.
func containsSentinel(body, sentinel string) bool {
	return strings.Contains(body, sentinel)
}

// encodeTenantConfigRequest marshals the PUT body shape.
func encodeTenantConfigRequest(yamlStr string) ([]byte, error) {
	return json.Marshal(map[string]any{"config_yaml": yamlStr})
}
