// rpc_client_audit_log.go — Phase C4.3 step A.
//
// 2 RPC methods on *RPCClient that proxy to cs-user's
// /api/internal/platform/audit-logs and /api/internal/tenants/audit-logs
// (the C4.3 step 2 endpoints). The server's /api/platform/audit-logs and
// /api/tenant/audit-logs public surfaces (step B) use these to forward
// audit-log list operations without duplicating user_center_audit_log
// (ADR D1 — cs-user owns identity + audit data).
//
// HTTP-to-Go-sentinel mapping is intentionally minimal:
//   - 5xx + transport errors → ErrRPCUnavailable (handler surfaces 502)
//   - 4xx → unlikely from cs-user (list endpoints only return 4xx for bad
//     timestamps, and our types guarantee parse before send), but a
//     generic wrap surfaces them so handlers don't crash on the unexpected
//
// Both methods forward X-Tenant-Id + actor-meta headers via the shared
// do() helpers (consistent with platform_tenant / tenant_config / etc).
// The tenant-scoped method relies on cs-user's ResolveTenant middleware to
// honor the X-Tenant-Id header and pin the query — the client itself does
// not need to know whether it's calling the platform or tenant endpoint
// from a header perspective.

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
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// AuditLogEntry mirrors the subset of cs-user's models.AuditLog that the
// server consumes. Local type — ADR D1 keeps the two services type-decoupled.
// Pointer-encoded fields in cs-user (TenantID, ActorSubjectID, ...) become
// plain strings here; absent values serialize as JSON null thanks to
// omitempty, matching cs-user's wire shape.
type AuditLogEntry struct {
	ID                 int64           `json:"id"`
	TenantID           string          `json:"tenant_id,omitempty"`
	ActorSubjectID     string          `json:"actor_subject_id,omitempty"`
	ActorTenantRole    string          `json:"actor_tenant_role,omitempty"`
	ActorPlatformScope string          `json:"actor_platform_scope,omitempty"`
	Action             string          `json:"action"`
	TargetType         string          `json:"target_type,omitempty"`
	TargetID           string          `json:"target_id,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	IP                 string          `json:"ip,omitempty"`
	UserAgent          string          `json:"user_agent,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

// AuditLogListResult mirrors cs-user's auditlog.ListResult.
type AuditLogListResult struct {
	Logs   []AuditLogEntry `json:"logs"`
	Total  int64           `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// AuditLogListParams is the shared input shape for both list methods.
// From/To use time.Time zero value to mean "no bound"; the URL builder
// skips them when IsZero. TenantID is honored only by the platform method
// (cs-user's tenant endpoint ignores client-supplied tenant_id and forces
// its own from request ctx — see C4.3 step 2 design notes).
type AuditLogListParams struct {
	TenantID       string
	ActorSubjectID string
	Action         string
	TargetType     string
	TargetID       string
	From           time.Time
	To             time.Time
	Limit          int
	Offset         int
}

const (
	platformAuditLogsPath = "/api/internal/platform/audit-logs"
	tenantAuditLogsPath   = "/api/internal/tenants/audit-logs"
)

// ErrAuditLogRPCBadRequest is the sentinel for 4xx from cs-user. Surfaces
// in handler as 400 (likely a serialization bug — caller-side types
// validated the inputs already, so a 4xx here means upstream validation
// diverged or a malformed filter slipped through).
var ErrAuditLogRPCBadRequest = errors.New("audit log rpc: bad request")

// ListAuditLogs proxies GET /api/internal/platform/audit-logs (platform
// scope, cross-tenant). The caller is responsible for ensuring the
// authenticated subject has platform-admin scope; this method just forwards.
func (c *RPCClient) ListAuditLogs(ctx context.Context, p AuditLogListParams) (*AuditLogListResult, error) {
	return c.doAuditLogList(ctx, platformAuditLogsPath, p, true /*trustTenantID*/)
}

// ListAuditLogsForTenant proxies GET /api/internal/tenants/audit-logs
// (tenant-scoped via X-Tenant-Id header). trustTenantID=false means the
// URL builder omits the tenant_id query param — cs-user forces tenant scope
// from the request ctx resolved by its middleware.ResolveTenant.
func (c *RPCClient) ListAuditLogsForTenant(ctx context.Context, p AuditLogListParams) (*AuditLogListResult, error) {
	return c.doAuditLogList(ctx, tenantAuditLogsPath, p, false /*tenant scope forced by upstream*/)
}

// doAuditLogList is the shared transport for both list methods. Builds the
// query string, fires the GET, decodes the response.
func (c *RPCClient) doAuditLogList(ctx context.Context, path string, p AuditLogListParams, trustTenantID bool) (*AuditLogListResult, error) {
	q := buildAuditLogQuery(p, trustTenantID)
	full := path
	if encoded := q.Encode(); encoded != "" {
		full += "?" + encoded
	}
	var out AuditLogListResult
	if err := c.doAuditLogRequest(ctx, http.MethodGet, full, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// buildAuditLogQuery translates AuditLogListParams into url.Values. Empty
// strings / zero times are skipped so the URL only carries filters the
// caller explicitly set. Limit/Offset are sent only when positive; cs-user
// applies its own defaults (100 / 0) when they're absent.
func buildAuditLogQuery(p AuditLogListParams, trustTenantID bool) url.Values {
	q := url.Values{}
	if trustTenantID && p.TenantID != "" {
		q.Set("tenant_id", p.TenantID)
	}
	if p.ActorSubjectID != "" {
		q.Set("actor_subject_id", p.ActorSubjectID)
	}
	if p.Action != "" {
		q.Set("action", p.Action)
	}
	if p.TargetType != "" {
		q.Set("target_type", p.TargetType)
	}
	if p.TargetID != "" {
		q.Set("target_id", p.TargetID)
	}
	if !p.From.IsZero() {
		q.Set("from", p.From.UTC().Format(time.RFC3339Nano))
	}
	if !p.To.IsZero() {
		q.Set("to", p.To.UTC().Format(time.RFC3339Nano))
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Offset > 0 {
		q.Set("offset", strconv.Itoa(p.Offset))
	}
	return q
}

// doAuditLogRequest is a thin transport — same shape as the per-resource
// helpers in other rpc_client_*.go files but inline because there's no
// POST/PATCH/DELETE path to share with. Forwards the same headers every
// other RPC method does (X-Internal-Token, X-Tenant-Id, actor-meta).
func (c *RPCClient) doAuditLogRequest(ctx context.Context, method, path string, out any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("user rpc client: build audit-log request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}
	applyActorMetaHeaders(req, ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] audit-log %s %s request failed: %v", method, path, err)
		return fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] audit-log %s %s read body failed: %v", method, path, err)
		return fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	if resp.StatusCode >= 400 {
		if resp.StatusCode >= 500 {
			logger.Warn("[user-rpc] audit-log %s %s returned %d: %s",
				method, path, resp.StatusCode, truncate(string(respBody), 200))
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, resp.StatusCode)
		}
		// 4xx: most likely 400 from a bad timestamp filter our side didn't
		// pre-validate. Surface as ErrAuditLogRPCBadRequest so the handler
		// maps to 400 rather than the generic 502.
		logger.Warn("[user-rpc] audit-log %s %s returned %d: %s",
			method, path, resp.StatusCode, truncate(string(respBody), 200))
		return fmt.Errorf("%w: status %d: %s", ErrAuditLogRPCBadRequest, resp.StatusCode, truncate(string(respBody), 120))
	}

	if err := json.Unmarshal(respBody, out); err != nil {
		logger.Warn("[user-rpc] audit-log %s %s decode failed: %v", method, path, err)
		return fmt.Errorf("user rpc client: decode audit-log response: %w", err)
	}
	return nil
}
