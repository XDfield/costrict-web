package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/gin-gonic/gin"
)

// stubAuditLogListService captures the params each call saw so tests can
// assert filter plumbing. list field is required; nil panics like the
// platform_tenants stub pattern.
type stubAuditLogListService struct {
	list func(context.Context, auditlog.ListParams) (*auditlog.ListResult, error)
	last auditlog.ListParams
}

func (s *stubAuditLogListService) List(ctx context.Context, p auditlog.ListParams) (*auditlog.ListResult, error) {
	s.last = p
	if s.list == nil {
		panic("stubAuditLogListService.list not wired")
	}
	return s.list(ctx, p)
}

func newPlatformAuditLogsAPI(svc AuditLogListService) (*PlatformAuditLogsAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &PlatformAuditLogsAPI{Svc: svc}
	r.GET("/api/internal/platform/audit-logs", api.List)
	return api, r
}

func newTenantAuditLogsAPI(svc AuditLogListService, resolverFn func(*gin.Context)) (*TenantAuditLogsAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if resolverFn != nil {
		r.Use(func(c *gin.Context) { resolverFn(c); c.Next() })
	}
	api := &TenantAuditLogsAPI{Svc: svc}
	r.GET("/api/internal/tenants/audit-logs", api.List)
	return api, r
}

// ---- PlatformAuditLogsAPI.List ----

// TestPlatformAuditLogs_HappyPath verifies a 200 with the result body shaped
// as auditlog.ListResult JSON and the query params plumbed through.
func TestPlatformAuditLogs_HappyPath(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, p auditlog.ListParams) (*auditlog.ListResult, error) {
		return &auditlog.ListResult{
			Logs:   []*models.AuditLog{{ID: 42, Action: models.ActionTenantCreate}},
			Total:  1,
			Limit:  p.Limit,
			Offset: p.Offset,
		}, nil
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet,
		"/api/internal/platform/audit-logs?action=tenant.create&tenant_id=t-acme&limit=10&offset=5&from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := stub.last.Action; got != "tenant.create" {
		t.Errorf("Action = %q, want tenant.create", got)
	}
	if got := stub.last.TenantID; got != "t-acme" {
		t.Errorf("TenantID = %q, want t-acme", got)
	}
	if got := stub.last.Limit; got != 10 {
		t.Errorf("Limit = %d, want 10", got)
	}
	if got := stub.last.Offset; got != 5 {
		t.Errorf("Offset = %d, want 5", got)
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !stub.last.From.Equal(want) {
		t.Errorf("From = %v, want %v", stub.last.From, want)
	}
	if want := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC); !stub.last.To.Equal(want) {
		t.Errorf("To = %v, want %v", stub.last.To, want)
	}

	var body auditlog.ListResult
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if body.Total != 1 {
		t.Errorf("body.Total = %d, want 1", body.Total)
	}
	if len(body.Logs) != 1 || body.Logs[0].ID != 42 {
		t.Errorf("body.Logs = %+v, want one row id=42", body.Logs)
	}
}

// TestPlatformAuditLogs_EmptyResultNotError verifies Total=0 + Logs=[] is
// 200, not 404.
func TestPlatformAuditLogs_EmptyResultNotError(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return &auditlog.ListResult{Logs: []*models.AuditLog{}, Total: 0}, nil
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestPlatformAuditLogs_BadFromTime400 verifies malformed from/to returns 400
// and does NOT call the service.
func TestPlatformAuditLogs_BadFromTime400(t *testing.T) {
	called := false
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		called = true
		return nil, nil
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/platform/audit-logs?from=not-a-time", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if called {
		t.Errorf("service was called on bad input")
	}
}

// TestPlatformAuditLogs_BadToTime400 mirrors the above for the to param.
func TestPlatformAuditLogs_BadToTime400(t *testing.T) {
	called := false
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		called = true
		return nil, nil
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/platform/audit-logs?to=2026-99-99", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if called {
		t.Errorf("service was called on bad input")
	}
}

// TestPlatformAuditLogs_NilDBReturns503 verifies auditlog.ErrNilDB surfaces
// as 503 (the handler can't serve when Deps.AuditLog is nil).
func TestPlatformAuditLogs_NilDBReturns503(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return nil, auditlog.ErrNilDB
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestPlatformAuditLogs_GenericError500 verifies a non-sentinel error
// surfaces as 500 without leaking the underlying string to clientside
// consumers (handler returns err.Error() but ops should ideally wrap).
func TestPlatformAuditLogs_GenericError500(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return nil, errors.New("disk on fire")
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// TestPlatformAuditLogs_DateOnlyFilterAccepted verifies the looser timestamp
// formats (date-only) parse without 400 — supports manual curl testing.
func TestPlatformAuditLogs_DateOnlyFilterAccepted(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, p auditlog.ListParams) (*auditlog.ListResult, error) {
		return &auditlog.ListResult{}, nil
	}}
	_, r := newPlatformAuditLogsAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/platform/audit-logs?from=2026-07-01", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !stub.last.From.Equal(want) {
		t.Errorf("From = %v, want %v", stub.last.From, want)
	}
}

// ---- TenantAuditLogsAPI.List ----

// TestTenantAuditLogs_ForcesTenantFromCtx verifies a client-supplied
// tenant_id query param is ignored and the ctx-resolved tenant wins.
// This is the cross-tenant spoofing protection.
func TestTenantAuditLogs_ForcesTenantFromCtx(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return &auditlog.ListResult{}, nil
	}}
	resolvedTenant := &models.Tenant{TenantID: "t-real"}
	_, r := newTenantAuditLogsAPI(stub, func(c *gin.Context) {
		// Simulate middleware.ResolveTenant stashing the resolved tenant.
		ctx := tenant.WithTenant(c.Request.Context(), resolvedTenant)
		c.Request = c.Request.WithContext(ctx)
	})

	// Client attempts to spoof a different tenant.
	req := httptest.NewRequest(http.MethodGet, "/api/internal/tenants/audit-logs?tenant_id=t-forged", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := stub.last.TenantID; got != "t-real" {
		t.Errorf("TenantID = %q, want t-real (ctx must override query)", got)
	}
}

// TestTenantAuditLogs_DefaultsWhenNoTenantResolved verifies that when ctx
// carries no tenant (middleware ran but couldn't resolve — operator misconfig),
// the handler falls back to DefaultTenantID rather than 4xx. Keeps the
// endpoint 200-stable during pre-cutover windows.
func TestTenantAuditLogs_DefaultsWhenNoTenantResolved(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return &auditlog.ListResult{}, nil
	}}
	_, r := newTenantAuditLogsAPI(stub, nil /*no resolver installed*/)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/tenants/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := stub.last.TenantID; got != tenant.DefaultTenantID {
		t.Errorf("TenantID = %q, want %q", got, tenant.DefaultTenantID)
	}
}

// TestTenantAuditLogs_HappyPathWithFilters verifies non-tenant filters
// (action/actor/target) flow through unchanged alongside the forced tenant.
func TestTenantAuditLogs_HappyPathWithFilters(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return &auditlog.ListResult{Logs: []*models.AuditLog{}, Total: 0}, nil
	}}
	resolvedTenant := &models.Tenant{TenantID: "t-acme"}
	_, r := newTenantAuditLogsAPI(stub, func(c *gin.Context) {
		ctx := tenant.WithTenant(c.Request.Context(), resolvedTenant)
		c.Request = c.Request.WithContext(ctx)
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/internal/tenants/audit-logs?action=user.status_changed&actor_subject_id=u-admin&target_type=user&target_id=user:u-victim&limit=20", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.last.Action != "user.status_changed" {
		t.Errorf("Action = %q", stub.last.Action)
	}
	if stub.last.ActorSubjectID != "u-admin" {
		t.Errorf("ActorSubjectID = %q", stub.last.ActorSubjectID)
	}
	if stub.last.TargetType != "user" {
		t.Errorf("TargetType = %q", stub.last.TargetType)
	}
	if stub.last.TargetID != "user:u-victim" {
		t.Errorf("TargetID = %q", stub.last.TargetID)
	}
	if stub.last.Limit != 20 {
		t.Errorf("Limit = %d", stub.last.Limit)
	}
	if stub.last.TenantID != "t-acme" {
		t.Errorf("TenantID = %q, want t-acme", stub.last.TenantID)
	}
}

// TestTenantAuditLogs_NilDBReturns503 mirrors the platform path.
func TestTenantAuditLogs_NilDBReturns503(t *testing.T) {
	stub := &stubAuditLogListService{list: func(_ context.Context, _ auditlog.ListParams) (*auditlog.ListResult, error) {
		return nil, auditlog.ErrNilDB
	}}
	_, r := newTenantAuditLogsAPI(stub, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/tenants/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
