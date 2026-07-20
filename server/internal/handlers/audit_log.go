// Audit-log list handlers (Phase C4.3 step 5).
//
// 2 thin handlers at /api/platform/audit-logs + /api/tenant/audit-logs that
// proxy to cs-user via RPCClient (the C4.3 step 4 client). The platform
// route is gated by middleware.RequirePlatformAdmin; the tenant route by
// middleware.RequireTenantAdmin. cs-user remains the sole owner of
// user_center_audit_log (ADR D1).
//
// Error mapping (translates RPCClient sentinels → HTTP):
//   - ErrRPCUnavailable / ErrNotConfigured → 502
//   - ErrAuditLogRPCBadRequest              → 400

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// PlatformAuditLogService is the platform-scope read surface the handler
// consumes. Declared as an interface so tests substitute a fake; production
// wires the *userpkg.RPCClient via UserModule.Reader cast (mirrors
// PlatformTenantAPI wiring in cmd/api/main.go).
type PlatformAuditLogService interface {
	ListAuditLogs(ctx context.Context, p userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error)
}

// TenantAuditLogService is the tenant-scope read surface. Same RPC client
// under the hood but a different method (cs-user forces tenant scope from
// the X-Tenant-Id header rather than the query string).
type TenantAuditLogService interface {
	ListAuditLogsForTenant(ctx context.Context, p userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error)
}

// PlatformAuditLogAPI is the receiver for the platform-scope list handler.
type PlatformAuditLogAPI struct {
	Svc PlatformAuditLogService
}

// TenantAuditLogAPI is the receiver for the tenant-scope list handler.
type TenantAuditLogAPI struct {
	Svc TenantAuditLogService
}

// parseAuditLogListQuery reads the shared query-string contract into
// userpkg.AuditLogListParams. trustTenantID controls whether tenant_id is
// plumbed through (platform) or stripped (tenant — cs-user forces scope).
//
// Bad from/to values return 400; everything else (missing/blank) falls
// through as zero values that the RPC client omits from the URL.
func parseAuditLogListQuery(c *gin.Context, trustTenantID bool) (userpkg.AuditLogListParams, bool) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))

	p := userpkg.AuditLogListParams{
		ActorSubjectID: c.Query("actor_subject_id"),
		Action:         c.Query("action"),
		TargetType:     c.Query("target_type"),
		TargetID:       c.Query("target_id"),
		Limit:          limit,
		Offset:         offset,
	}
	if trustTenantID {
		p.TenantID = c.Query("tenant_id")
	}

	if raw := c.Query("from"); raw != "" {
		t, ok := parseAuditLogQueryTime(c, raw, "from")
		if !ok {
			return userpkg.AuditLogListParams{}, false
		}
		p.From = t
	}
	if raw := c.Query("to"); raw != "" {
		t, ok := parseAuditLogQueryTime(c, raw, "to")
		if !ok {
			return userpkg.AuditLogListParams{}, false
		}
		p.To = t
	}
	return p, true
}

// parseAuditLogQueryTime accepts the same formats cs-user's handler accepts
// (RFC3339 + looser alternatives for manual curl testing). 400 on failure.
func parseAuditLogQueryTime(c *gin.Context, raw, field string) (time.Time, bool) {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return t, true
		}
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": field + " must be ISO8601 (e.g. 2026-07-01T12:00:00Z)"})
	return time.Time{}, false
}

// respondAuditLogRPCErr centralizes the sentinel → HTTP mapping for both
// audit-log handlers.
func respondAuditLogRPCErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userpkg.ErrAuditLogRPCBadRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": "audit log request rejected upstream"})
	case errors.Is(err, userpkg.ErrRPCUnavailable), errors.Is(err, userpkg.ErrNotConfigured):
		c.JSON(http.StatusBadGateway, gin.H{"error": "audit log service unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// auditLogUnavailable is the 502 stub used when Svc is nil (route wired but
// no RPC backend — e.g. local-backend dev mode). Mirrors platformTenantUnavailable.
func auditLogUnavailable(c *gin.Context) {
	c.JSON(http.StatusBadGateway, gin.H{"error": "audit log service unavailable"})
}

// PlatformListAuditLogs godoc
//
//	@Summary		List audit-log entries (platform admin, cross-tenant)
//	@Description	Paginated audit-log list across all tenants. Wrapper over cs-user's /api/internal/platform/audit-logs; returns 502 when the server runs in local backend mode (no audit-log data on this side per ADR D1). Filters: tenant_id / actor_subject_id / action / target_type / target_id / from / to (ISO8601) / limit (default 100, cap 500) / offset (default 0). Newest first.
//	@Tags			platform-audit-logs
//	@Produce		json
//	@Security		BearerAuth
//	@Param			tenant_id			query		string	false	"Exact tenant_id match (cross-tenant narrowing)"
//	@Param			actor_subject_id	query		string	false	"Exact actor_subject_id match"
//	@Param			action				query		string	false	"Exact action match (e.g. tenant.create, user.status_changed)"
//	@Param			target_type			query		string	false	"Exact target_type match (e.g. tenant, user)"
//	@Param			target_id			query		string	false	"Exact target_id match"
//	@Param			from				query		string	false	"ISO8601 lower bound on created_at (inclusive)"
//	@Param			to					query		string	false	"ISO8601 upper bound on created_at (inclusive)"
//	@Param			limit				query		int		false	"Page size (default 100, cap 500)"
//	@Param			offset				query		int		false	"Page offset (default 0)"
//	@Success		200					{object}	userpkg.AuditLogListResult
//	@Failure		400					{object}	object{error=string}
//	@Failure		401					{object}	object{error=string}
//	@Failure		403					{object}	object{error=string}
//	@Failure		502					{object}	object{error=string}
//	@Router			/api/platform/audit-logs [get]
func (a *PlatformAuditLogAPI) PlatformListAuditLogs(c *gin.Context) {
	if a.Svc == nil {
		auditLogUnavailable(c)
		return
	}
	p, ok := parseAuditLogListQuery(c, true /*trustTenantID*/)
	if !ok {
		return
	}
	ctx := platformActorCtx(c)
	res, err := a.Svc.ListAuditLogs(ctx, p)
	if err != nil {
		respondAuditLogRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// TenantListAuditLogs godoc
//
//	@Summary		List audit-log entries (tenant admin, this tenant only)
//	@Description	Paginated audit-log list scoped to the caller's tenant. Tenant scope is forced by cs-user from the X-Tenant-Id header; client-supplied tenant_id is ignored. Wrapper over cs-user's /api/internal/tenants/audit-logs; returns 502 when the server runs in local backend mode. Filters: actor_subject_id / action / target_type / target_id / from / to (ISO8601) / limit (default 100, cap 500) / offset (default 0). Newest first.
//	@Tags			tenant-audit-logs
//	@Produce		json
//	@Security		BearerAuth
//	@Param			actor_subject_id	query		string	false	"Exact actor_subject_id match"
//	@Param			action				query		string	false	"Exact action match (e.g. tenant_config.update, user.status_changed)"
//	@Param			target_type			query		string	false	"Exact target_type match"
//	@Param			target_id			query		string	false	"Exact target_id match"
//	@Param			from				query		string	false	"ISO8601 lower bound on created_at (inclusive)"
//	@Param			to					query		string	false	"ISO8601 upper bound on created_at (inclusive)"
//	@Param			limit				query		int		false	"Page size (default 100, cap 500)"
//	@Param			offset				query		int		false	"Page offset (default 0)"
//	@Success		200					{object}	userpkg.AuditLogListResult
//	@Failure		400					{object}	object{error=string}
//	@Failure		401					{object}	object{error=string}
//	@Failure		403					{object}	object{error=string}
//	@Failure		502					{object}	object{error=string}
//	@Router			/api/tenant/audit-logs [get]
func (a *TenantAuditLogAPI) TenantListAuditLogs(c *gin.Context) {
	if a.Svc == nil {
		auditLogUnavailable(c)
		return
	}
	p, ok := parseAuditLogListQuery(c, false /*tenant scope forced upstream*/)
	if !ok {
		return
	}
	// Actor context (role/scope) forwarded for cs-user's audit-log reads —
	// not strictly required on the read path (cs-user doesn't write a new
	// audit row when serving the list), but consistent with sibling handlers
	// and zero-cost via the platformActorCtx helper.
	ctx := platformActorCtx(c)
	res, err := a.Svc.ListAuditLogsForTenant(ctx, p)
	if err != nil {
		respondAuditLogRPCErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// Compile-time assertions that *userpkg.RPCClient satisfies both service
// interfaces. Drift in the RPC client signatures surfaces at build time
// rather than at first request.
var (
	_ PlatformAuditLogService = (*userpkg.RPCClient)(nil)
	_ TenantAuditLogService   = (*userpkg.RPCClient)(nil)
)
