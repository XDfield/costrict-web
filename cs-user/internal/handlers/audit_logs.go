// Phase C4.3 — audit-log list endpoints.
//
// Two endpoints sit on top of auditlog.Service.List (step 1 of this slice):
//
//	GET /api/internal/platform/audit-logs  — platform-admin, cross-tenant
//	GET /api/internal/tenants/audit-logs   — tenant-scoped via X-Tenant-Id
//
// Both honor the same query-string filters (action / actor_subject_id /
// target_type / target_id / from / to / limit / offset) and differ only in
// how tenant scope is established. The platform endpoint passes the
// caller-supplied tenant_id filter (if any) through to the service; the
// tenant endpoint IGNORES any client-supplied tenant_id and forces it from
// request ctx (resolved by middleware.ResolveTenant from X-Tenant-Id).
//
// Returns 200 with {logs, total, limit, offset}; never 404 on empty (Total=0
// is a valid answer). 500 only when the underlying service errors.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
)

// AuditLogListService is the read-side subset of *auditlog.Service the
// handlers need. Declared as an interface so handler tests substitute a
// fake and so app.go can wire a 503 fallback stub when Deps.AuditLog is nil.
type AuditLogListService interface {
	List(ctx context.Context, p auditlog.ListParams) (*auditlog.ListResult, error)
}

// parseAuditLogListParams translates the shared query-string contract into
// auditlog.ListParams. doesNotSetTenant=true skips the tenant_id dimension
// (platform endpoint trusts caller-supplied tenant_id; tenant endpoint
// forces its own from ctx after this returns).
//
// Bad limit/offset values fall through to the service's defaults (100/0) —
// we don't 400 on garbage because strconv.Atoi("") returns 0 which is the
// "use default" signal. Malformed timestamps DO return 400 because a silent
// zero-time filter would mean "no upper/lower bound" rather than an obvious
// client error.
func parseAuditLogListParams(c *gin.Context, trustTenantQuery bool) (auditlog.ListParams, bool) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))

	p := auditlog.ListParams{
		ActorSubjectID: c.Query("actor_subject_id"),
		Action:         c.Query("action"),
		TargetType:     c.Query("target_type"),
		TargetID:       c.Query("target_id"),
		Limit:          limit,
		Offset:         offset,
	}
	if trustTenantQuery {
		p.TenantID = c.Query("tenant_id")
	}

	if raw := c.Query("from"); raw != "" {
		t, ok := parseAuditLogTime(c, raw, "from")
		if !ok {
			return auditlog.ListParams{}, false
		}
		p.From = t
	}
	if raw := c.Query("to"); raw != "" {
		t, ok := parseAuditLogTime(c, raw, "to")
		if !ok {
			return auditlog.ListParams{}, false
		}
		p.To = t
	}
	return p, true
}

// parseAuditLogTime accepts RFC3339 + a handful of common alternatives so
// curl/browser manual testing doesn't require nanosecond precision. Failure
// returns false and writes a 400 — caller MUST short-circuit.
func parseAuditLogTime(c *gin.Context, raw, field string) (time.Time, bool) {
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

// respondAuditLogErr centralizes the sentinel→HTTP mapping. The list path
// has only one sentinel today (auditlog.ErrNilDB → 503) but the switch keeps
// the shape symmetric with other handlers so new sentinels land cleanly.
func respondAuditLogErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auditlog.ErrNilDB):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit log service unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// PlatformAuditLogsAPI exposes the cross-tenant audit-log list to the
// costrict-web server (which re-exposes it at /api/platform/audit-logs
// behind RequirePlatformAdmin). cs-user owns user_center_audit_log (ADR D1);
// this is the read surface that complements the C4.1 writer instrumented
// into PlatformTenantsAPI / UsersAPI / TenantConfigAPI / etc.
type PlatformAuditLogsAPI struct {
	Svc AuditLogListService
}

// List godoc
//
//	@Summary		List audit-log entries (platform-admin, cross-tenant)
//	@Description	Returns paginated audit-log entries. Platform-admin scope sees all tenants; pass tenant_id to narrow. Filters: action / actor_subject_id / target_type / target_id / from / to (ISO8601) / limit (default 100, cap 500) / offset (default 0). Newest first.
//	@Tags			platform-audit-logs
//	@Produce		json
//	@Security		InternalToken
//	@Param			tenant_id			query		string	false	"Exact tenant_id match"
//	@Param			actor_subject_id	query		string	false	"Exact actor_subject_id match"
//	@Param			action				query		string	false	"Exact action match (e.g. tenant.create, user.status_changed)"
//	@Param			target_type			query		string	false	"Exact target_type match (e.g. tenant, user)"
//	@Param			target_id			query		string	false	"Exact target_id match"
//	@Param			from				query		string	false	"ISO8601 lower bound on created_at (inclusive)"
//	@Param			to					query		string	false	"ISO8601 upper bound on created_at (inclusive)"
//	@Param			limit				query		int		false	"Page size (default 100, cap 500)"
//	@Param			offset				query		int		false	"Page offset (default 0)"
//	@Success		200					{object}	auditlog.ListResult
//	@Failure		400					{object}	object{error=string}
//	@Failure		500					{object}	object{error=string}
//	@Failure		503					{object}	object{error=string}
//	@Router			/api/internal/platform/audit-logs [get]
func (a *PlatformAuditLogsAPI) List(c *gin.Context) {
	p, ok := parseAuditLogListParams(c, true /*trustTenantQuery*/)
	if !ok {
		return // 400 already written by parser
	}
	res, err := a.Svc.List(c.Request.Context(), p)
	if err != nil {
		respondAuditLogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// TenantAuditLogsAPI exposes a tenant-scoped audit-log list (only rows
// belonging to the resolved tenant). cs-user's middleware.ResolveTenant
// populates ctx with the tenant resolved from X-Tenant-Id header; the
// handler forces that tenant_id into the filter and silently ignores any
// client-supplied tenant_id query param — cross-tenant spoofing protection
// is at the handler, not the service layer.
type TenantAuditLogsAPI struct {
	Svc AuditLogListService
}

// List godoc
//
//	@Summary		List audit-log entries (tenant-scoped)
//	@Description	Returns paginated audit-log entries scoped to the request's tenant (resolved from X-Tenant-Id header). Client-supplied tenant_id is ignored. Filters: action / actor_subject_id / target_type / target_id / from / to (ISO8601) / limit (default 100, cap 500) / offset (default 0). Newest first.
//	@Tags			tenant-audit-logs
//	@Produce		json
//	@Security		InternalToken
//	@Param			actor_subject_id	query		string	false	"Exact actor_subject_id match"
//	@Param			action				query		string	false	"Exact action match (e.g. tenant_config.update, user.status_changed)"
//	@Param			target_type			query		string	false	"Exact target_type match"
//	@Param			target_id			query		string	false	"Exact target_id match"
//	@Param			from				query		string	false	"ISO8601 lower bound on created_at (inclusive)"
//	@Param			to					query		string	false	"ISO8601 upper bound on created_at (inclusive)"
//	@Param			limit				query		int		false	"Page size (default 100, cap 500)"
//	@Param			offset				query		int		false	"Page offset (default 0)"
//	@Success		200					{object}	auditlog.ListResult
//	@Failure		400					{object}	object{error=string}
//	@Failure		500					{object}	object{error=string}
//	@Failure		503					{object}	object{error=string}
//	@Router			/api/internal/tenants/audit-logs [get]
func (a *TenantAuditLogsAPI) List(c *gin.Context) {
	p, ok := parseAuditLogListParams(c, false /*tenant scope forced below*/)
	if !ok {
		return // 400 already written by parser
	}
	// Force tenant scope from ctx; ignore any client-supplied tenant_id.
	// IDFromContext falls back to DefaultTenantID when ctx carries no
	// tenant — for the tenant-scoped endpoint that's an operator
	// misconfiguration (X-Tenant-Id missing), but defaulting keeps the
	// handler 200-stable rather than 4xx on resolver gaps.
	p.TenantID = tenant.IDFromContext(c.Request.Context())

	res, err := a.Svc.List(c.Request.Context(), p)
	if err != nil {
		respondAuditLogErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
