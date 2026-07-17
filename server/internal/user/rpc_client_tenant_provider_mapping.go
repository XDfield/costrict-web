// rpc_client_tenant_provider_mapping.go — Phase C3.3 typed provider_mapping RPC.
//
// Two methods proxying to cs-user's /api/internal/tenant/provider-mapping:
//
//	GetProviderMapping(ctx)           → GET  /api/internal/tenant/provider-mapping
//	UpdateProviderMapping(ctx, body)  → PUT  /api/internal/tenant/provider-mapping
//
// Tenant scoping + actor forwarding are identical to the C3.2 raw-blob RPC
// (X-Tenant-Id from ctx, X-Actor-Subject-Id from caller). See
// rpc_client_tenant_config.go for the rationale.
//
// Sentinel mapping (server-side distinct from cs-user's per ADR D1):
//
//	transport / 5xx / 503        → ErrRPCUnavailable
//	4xx (non-typed-schema)       → ErrTenantConfigUnavailable (operational)
//	ErrInvalidYAML               → stored blob was malformed (rare; reuse C3.2 sentinel)
//	ErrProviderNameInvalid       → cs-user rejected provider name pattern
//	ErrIntervalInvalid           → cs-user rejected enterprise_sync.interval
//	ErrRankNegative              → cs-user rejected negative rank
//	ErrNotConfigured             → RPC backend selected without URL/token
//
// All three typed-schema sentinels surface as HTTP 400 from cs-user's
// handlers.respondProviderMappingErr. We disambiguate by echoing cs-user's
// exact body text ("invalid provider name" / "invalid enterprise_sync.interval"
// / "rank must be non-negative" / "invalid YAML"). Body-text matching keeps
// the contract stable without coupling the server to cs-user's error vars.

package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// ProviderMapping is the local projection of cs-user's
// tenantconfig.ProviderMapping. Declared locally per ADR D1 (type
// decoupling). Field set + JSON shape match cs-user's exactly so the
// server handler can echo it to the public API without translation.
type ProviderMapping struct {
	Providers map[string]Provider `json:"providers"`
}

// Provider mirrors cs-user's tenantconfig.Provider. Pointer fields
// preserve the "absent" vs "explicit zero" distinction through the
// JSON round-trip.
type Provider struct {
	Enabled        *bool             `json:"enabled,omitempty"`
	Rank           *int              `json:"rank,omitempty"`
	FieldMap       map[string]string `json:"field_map,omitempty"`
	EnterpriseSync *EnterpriseSync   `json:"enterprise_sync,omitempty"`
}

// EnterpriseSync mirrors cs-user's tenantconfig.EnterpriseSync.
type EnterpriseSync struct {
	Interval string `json:"interval,omitempty"`
}

// Provider_mapping sentinel errors (server-side). Distinct from cs-user's
// tenantconfig.ErrProviderNameInvalid / etc per ADR D1.
var (
	ErrProviderNameInvalid = errors.New("provider_mapping: invalid provider name")
	ErrIntervalInvalid     = errors.New("provider_mapping: invalid enterprise_sync.interval")
	ErrRankNegative        = errors.New("provider_mapping: rank must be non-negative")
)

const providerMappingPath = "/api/internal/tenant/provider-mapping"

// GetProviderMapping proxies GET /api/internal/tenant/provider-mapping.
// Returns the typed subsection; cs-user returns {"providers":{}} when the
// section is absent — this method never surfaces a 404.
func (c *RPCClient) GetProviderMapping(ctx context.Context) (*ProviderMapping, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	body, err := c.doProviderMapping(ctx, http.MethodGet, nil, "")
	if err != nil {
		return nil, err
	}
	var m ProviderMapping
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("user rpc client: decode provider-mapping response: %w", err)
	}
	return &m, nil
}

// UpdateProviderMapping proxies PUT /api/internal/tenant/provider-mapping.
// mapping is the typed body (full replace of the provider_mapping subtree).
// actorSubjectID forwards the JWT subject_id for the audit trail (empty
// string allowed → cs-user stores NULL).
//
// On success returns the typed mapping as cs-user wrote it (with defaults
// applied — e.g. Enabled→true when omitted).
func (c *RPCClient) UpdateProviderMapping(ctx context.Context, mapping *ProviderMapping, actorSubjectID string) (*ProviderMapping, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if mapping == nil {
		mapping = &ProviderMapping{Providers: map[string]Provider{}}
	}

	reqBody, err := json.Marshal(mapping)
	if err != nil {
		return nil, fmt.Errorf("user rpc client: marshal provider-mapping body: %w", err)
	}

	body, err := c.doProviderMapping(ctx, http.MethodPut, reqBody, actorSubjectID)
	if err != nil {
		return nil, err
	}
	var m ProviderMapping
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("user rpc client: decode provider-mapping response: %w", err)
	}
	return &m, nil
}

// doProviderMapping issues the request + maps the response. Returns the
// raw body on success so the caller can decode into the typed shape;
// returns a wrapped sentinel on error.
func (c *RPCClient) doProviderMapping(ctx context.Context, method string, body []byte, actorSubjectID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+providerMappingPath, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("user rpc client: build provider-mapping request: %w", err)
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
		logger.Warn("[user-rpc] provider-mapping %s %s request failed: %v", method, providerMappingPath, err)
		return nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] provider-mapping %s %s read body failed: %v", method, providerMappingPath, err)
		return nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		logger.Warn("[user-rpc] provider-mapping %s %s returned %d: %s",
			method, providerMappingPath, resp.StatusCode, truncate(string(respBody), 200))
		return nil, mapProviderMappingHTTPError(resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// mapProviderMappingHTTPError translates cs-user's HTTP codes into
// server-side sentinels. Body-text matching against cs-user's exact
// response strings (set in handlers.respondProviderMappingErr) keeps the
// contract stable.
func mapProviderMappingHTTPError(status int, body string) error {
	switch status {
	case http.StatusBadRequest:
		// 400 has four sub-cases on this surface, distinguished by body text:
		//   "invalid YAML"                    → ErrInvalidYAML (rare; stored blob malformed)
		//   "invalid provider name"           → ErrProviderNameInvalid
		//   "invalid enterprise_sync.interval"→ ErrIntervalInvalid
		//   "rank must be non-negative"       → ErrRankNegative
		// Anything else 400 is an operational fault → ErrTenantConfigUnavailable.
		switch {
		case containsSentinel(body, "invalid YAML"):
			return fmt.Errorf("%w: %s", ErrInvalidYAML, truncate(body, 200))
		case containsSentinel(body, "invalid provider name"):
			return fmt.Errorf("%w: %s", ErrProviderNameInvalid, truncate(body, 200))
		case containsSentinel(body, "invalid enterprise_sync.interval"):
			return fmt.Errorf("%w: %s", ErrIntervalInvalid, truncate(body, 200))
		case containsSentinel(body, "rank must be non-negative"):
			return fmt.Errorf("%w: %s", ErrRankNegative, truncate(body, 200))
		}
		return fmt.Errorf("%w: upstream 400: %s", ErrTenantConfigUnavailable, truncate(body, 200))
	default:
		if status >= 500 || status == http.StatusServiceUnavailable {
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, status)
		}
		// All other 4xx → 502-class operational fault.
		return fmt.Errorf("%w: upstream %d: %s", ErrTenantConfigUnavailable, status, truncate(body, 200))
	}
}
