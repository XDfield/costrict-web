// Package handlers — audit.go provides the Phase C4.1 helper that captures
// audit-log context from a gin.Context: the actor identity (forwarded by
// server's RPC client as headers), the tenant context (set by middleware),
// and the network context (gin c.ClientIP / User-Agent).
//
// The handler layer is the right home for audit orchestration: it already
// knows the action (the route), the target (URL param or response body), and
// the actor meta (from headers). The service layer stays focused on business
// logic; audit is a cross-cutting concern that wraps the service call.
//
// All audit calls are best-effort — see auditlog.Service.Record doc. A nil
// audit service (test path / 503 fallback) makes recordAudit a no-op so
// handler tests don't need to inject one unless they explicitly assert on it.
package handlers

import (
	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/middleware"
	"github.com/gin-gonic/gin"
)

// Standard header names forwarded by server's RPC client (mirror of the
// server-side constants in server/internal/user/rpc_client_*.go). Extracted
// as constants so a rename here is one diff, not three.
const (
	actorTenantRoleHeader    = "X-Actor-Tenant-Role"
	actorPlatformScopeHeader = "X-Actor-Platform-Scope"
)

// captureAuditMeta pulls the actor + tenant + network context off a gin
// request and returns a partially-filled RecordParams. The caller fills in
// Action / TargetType / TargetID / Payload (operation-specific bits) and
// passes the result to recordAudit.
//
// TenantID is best-effort: if middleware's tenant context is missing (e.g.
// platform-level endpoints without X-Tenant-Id), TenantID stays empty →
// auditlog writes NULL. Same for actor headers — absent headers mean NULL
// columns, which is the documented semantics for system / unauthenticated
// callers.
func captureAuditMeta(c *gin.Context) auditlog.RecordParams {
	out := auditlog.RecordParams{
		ActorTenantRole:    c.GetHeader(actorTenantRoleHeader),
		ActorPlatformScope: c.GetHeader(actorPlatformScopeHeader),
		IP:                 c.ClientIP(),
		UserAgent:          c.GetHeader("User-Agent"),
	}
	// Actor subject id is set by the same server RPC client under
	// actorSubjectIDHeader (declared in tenant_config.go). Re-using the
	// existing constant avoids drift.
	out.ActorSubjectID = c.GetHeader(actorSubjectIDHeader)

	// Tenant context is set by middleware.TenantFromGin via the
	// tenant_resolve_middleware chain. Missing tenant context on a
	// tenant-scoped endpoint is a programmer error (caught by requireTenantID);
	// on platform-level endpoints it's expected (NULL tenant_id audit row).
	tn, err := middleware.TenantFromGin(c)
	if err == nil && tn != nil && tn.TenantID != "" {
		out.TenantID = tn.TenantID
	}
	return out
}

// recordAudit is the post-success audit wrapper. nil-safe: if svc is nil
// (test stub / 503 fallback) the call is a no-op so callers don't need to
// guard. Errors are swallowed per the best-effort contract — the user op
// has already succeeded and audit failure must not change that.
func recordAudit(svc *auditlog.Service, c *gin.Context, action, targetType, targetID string, payload map[string]any) {
	if svc == nil {
		return
	}
	p := captureAuditMeta(c)
	p.Action = action
	p.TargetType = targetType
	p.TargetID = targetID
	p.Payload = payload
	_ = svc.Record(c.Request.Context(), p)
}
