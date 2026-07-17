// rpc_client_tenant.go — Phase B3b.2b-step2b.
//
// ResolveTenantByEmail wraps cs-user's POST /api/internal/tenants/resolve-by-email.
// The server's OAuth callback uses it for the §5 Try 2 layer: when middleware's
// subdomain lookup (Try 1) missed, the callback now has user claims and can ask
// cs-user "which tenant does this email belong to?" without duplicating the
// tenants table (ADR D1 — cs-user owns tenant data).
//
// The RPC returns 200 across all three semantic states; the discriminator lives
// in the `status` field of the body so this client maps a single transport
// outcome (200) to three Go-level outcomes via Resolution.Status.

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

// TenantEmailResolution is the Go-level mirror of cs-user's three-state
// response. Status is one of:
//   - "ok"         — Slug + TenantID populated, Candidates empty
//   - "ambiguous"  — Candidates populated (may be empty if the secondary
//     ListByEmailDomain scan raced to zero — handler still
//     treats it as ambiguous and surfaces a picker)
//   - "not_found"  — Slug/TenantID/Candidates all empty
type TenantEmailResolution struct {
	Status     string                 // "ok" / "ambiguous" / "not_found"
	Slug       string                 // populated when Status == "ok"
	TenantID   string                 // populated when Status == "ok"
	Candidates []TenantEmailCandidate // populated when Status == "ambiguous"
}

// TenantEmailCandidate is a single tenant row in the ambiguous-response array.
// Mirrors handlers.tenantCandidate on cs-user (Slug / TenantID / Name).
type TenantEmailCandidate struct {
	Slug     string `json:"slug"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// ResolveTenantByEmail asks cs-user to resolve a tenant by email domain.
//
// Empty / whitespace-only email returns (&TenantEmailResolution{Status: "not_found"}, nil)
// — the handler treats this as "Try 2 miss, fall through to default tenant",
// the same outcome cs-user would return for an unrecognized domain.
//
// Network/transport errors / 5xx → wrapped ErrRPCUnavailable so callers can
// branch on errors.Is(err, ErrRPCUnavailable) without parsing strings.
// 4xx (other than 404) is treated the same way — cs-user only emits 400 for
// malformed body, which shouldn't happen from this client, so a 4xx here is
// unexpected and safer to surface as upstream-unavailable than silently
// drop.
func (c *RPCClient) ResolveTenantByEmail(ctx context.Context, email string) (*TenantEmailResolution, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	email = strings.TrimSpace(email)
	if email == "" {
		// No signal from this layer; let the caller proceed with whatever
		// tenant context (if any) was already established upstream.
		return &TenantEmailResolution{Status: "not_found"}, nil
	}

	reqBody, err := json.Marshal(struct {
		Email string `json:"email"`
	}{Email: email})
	if err != nil {
		return nil, fmt.Errorf("user rpc client: marshal resolve-by-email request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/internal/tenants/resolve-by-email", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("user rpc client: build resolve-by-email request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	// B3b.2a: forward the tenant slug so cs-user's middleware resolves
	// against the same tenant on the WRITE-path side of any downstream
	// cascade. For this read-only resolution call the slug is usually empty
	// (Try 1 missed, that's why we're here), but forwarding is harmless and
	// keeps the contract uniform across all cs-user RPCs.
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] resolve-by-email request failed: %v", err)
		return nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] resolve-by-email read body failed: %v", err)
		return nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		logger.Warn("[user-rpc] resolve-by-email returned status %d: %s",
			resp.StatusCode, truncate(string(body), 200))
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
			// 5xx and 404 both mean "upstream can't answer right now" —
			// cs-user deliberately returns 200 for not_found at the
			// application layer, so an HTTP 404 here is a routing problem
			// (older cs-user build, proxy mis-route) rather than a real
			// miss. Treat both as ErrRPCUnavailable so the handler falls
			// through to default tenant rather than potentially masking
			// a deployment issue.
			return nil, fmt.Errorf("%w: status %d", ErrRPCUnavailable, resp.StatusCode)
		}
		return nil, fmt.Errorf("user rpc client: resolve-by-email status %d", resp.StatusCode)
	}

	// Decode defensively — cs-user returns 200 + a status discriminator,
	// but we never trust the wire shape across a service boundary.
	var raw struct {
		Status     string                 `json:"status"`
		Slug       string                 `json:"slug"`
		TenantID   string                 `json:"tenant_id"`
		Candidates []TenantEmailCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		logger.Warn("[user-rpc] resolve-by-email decode failed: %v", err)
		return nil, fmt.Errorf("user rpc client: decode resolve-by-email response: %w", err)
	}

	res := &TenantEmailResolution{Status: raw.Status}
	switch raw.Status {
	case "ok":
		res.Slug = raw.Slug
		res.TenantID = raw.TenantID
	case "ambiguous":
		res.Candidates = raw.Candidates
	case "not_found":
		// nothing else to fill
	default:
		// Unknown status string — log and surface as ambiguous upstream-unavailable
		// so the handler falls through. Unexpected status values mean the
		// cs-user contract changed without this client catching up.
		logger.Warn("[user-rpc] resolve-by-email returned unknown status %q (body: %s)",
			raw.Status, truncate(string(body), 200))
		return nil, fmt.Errorf("%w: unknown status %q", ErrRPCUnavailable, raw.Status)
	}
	return res, nil
}

// errTenantResolutionNoSignal is a sentinel for handlers that want to
// distinguish "Try 2 made no decision" from real errors. Currently unused —
// the handler treats both as fall-through — but reserved here so future
// telemetry can hook the distinction without an API change.
var errTenantResolutionNoSignal = errors.New("tenant resolution: no signal")
