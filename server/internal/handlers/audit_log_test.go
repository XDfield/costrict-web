package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// --- stubs ---

type stubPlatformAuditLogSvc struct {
	list func(context.Context, userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error)
	last userpkg.AuditLogListParams
}

func (s *stubPlatformAuditLogSvc) ListAuditLogs(ctx context.Context, p userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
	s.last = p
	if s.list == nil {
		panic("stubPlatformAuditLogSvc.list not wired")
	}
	return s.list(ctx, p)
}

type stubTenantAuditLogSvc struct {
	list func(context.Context, userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error)
	last userpkg.AuditLogListParams
}

func (s *stubTenantAuditLogSvc) ListAuditLogsForTenant(ctx context.Context, p userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
	s.last = p
	if s.list == nil {
		panic("stubTenantAuditLogSvc.list not wired")
	}
	return s.list(ctx, p)
}

func newPlatformAuditLogAPI(svc PlatformAuditLogService) (*PlatformAuditLogAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &PlatformAuditLogAPI{Svc: svc}
	r.GET("/api/platform/audit-logs", api.PlatformListAuditLogs)
	return api, r
}

func newTenantAuditLogAPI(svc TenantAuditLogService) (*TenantAuditLogAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &TenantAuditLogAPI{Svc: svc}
	r.GET("/api/tenant/audit-logs", api.TenantListAuditLogs)
	return api, r
}

// --- PlatformListAuditLogs ---

func TestPlatformAuditLog_HappyPath(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, p userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return &userpkg.AuditLogListResult{
			Logs:   []userpkg.AuditLogEntry{{ID: 99, Action: "tenant.create"}},
			Total:  1,
			Limit:  p.Limit,
			Offset: p.Offset,
		}, nil
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet,
		"/api/platform/audit-logs?action=tenant.create&tenant_id=t-acme&limit=10&offset=5&from=2026-07-01T00:00:00Z&to=2026-07-02T00:00:00Z", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.last.Action != "tenant.create" {
		t.Errorf("Action = %q", stub.last.Action)
	}
	if stub.last.TenantID != "t-acme" {
		t.Errorf("TenantID = %q, want t-acme (platform trusts query)", stub.last.TenantID)
	}
	if stub.last.Limit != 10 || stub.last.Offset != 5 {
		t.Errorf("Limit/Offset = %d/%d", stub.last.Limit, stub.last.Offset)
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !stub.last.From.Equal(want) {
		t.Errorf("From = %v, want %v", stub.last.From, want)
	}

	var body userpkg.AuditLogListResult
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 1 || len(body.Logs) != 1 || body.Logs[0].ID != 99 {
		t.Errorf("body = %+v", body)
	}
}

func TestPlatformAuditLog_EmptyResultOK(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return &userpkg.AuditLogListResult{Logs: []userpkg.AuditLogEntry{}, Total: 0}, nil
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestPlatformAuditLog_BadFrom400(t *testing.T) {
	called := false
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		called = true
		return nil, nil
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs?from=not-a-time", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if called {
		t.Error("service called on bad input")
	}
}

func TestPlatformAuditLog_RPCUnavailable502(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return nil, userpkg.ErrRPCUnavailable
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestPlatformAuditLog_NotConfigured502(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return nil, userpkg.ErrNotConfigured
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (NotConfigured maps to 502)", w.Code)
	}
}

func TestPlatformAuditLog_RPCBadRequest400(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return nil, userpkg.ErrAuditLogRPCBadRequest
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPlatformAuditLog_GenericError500(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return nil, errors.New("unexpected")
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestPlatformAuditLog_NilSvc502(t *testing.T) {
	_, r := newPlatformAuditLogAPI(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (nil Svc stub)", w.Code)
	}
}

func TestPlatformAuditLog_DateOnlyFilterAccepted(t *testing.T) {
	stub := &stubPlatformAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return &userpkg.AuditLogListResult{}, nil
	}}
	_, r := newPlatformAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/platform/audit-logs?from=2026-07-01", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !stub.last.From.Equal(want) {
		t.Errorf("From = %v, want %v", stub.last.From, want)
	}
}

// --- TenantListAuditLogs ---

func TestTenantAuditLog_HappyPath(t *testing.T) {
	stub := &stubTenantAuditLogSvc{list: func(_ context.Context, p userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return &userpkg.AuditLogListResult{
			Logs:  []userpkg.AuditLogEntry{{ID: 7, Action: "user.status_changed"}},
			Total: 1,
			Limit: p.Limit,
		}, nil
	}}
	_, r := newTenantAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet,
		"/api/tenant/audit-logs?action=user.status_changed&limit=20", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.last.Action != "user.status_changed" {
		t.Errorf("Action = %q", stub.last.Action)
	}
	if stub.last.Limit != 20 {
		t.Errorf("Limit = %d", stub.last.Limit)
	}
}

// TestTenantAuditLog_StripsTenantIDQuery verifies the @server handler
// strips tenant_id from the params before forwarding — defense-in-depth on
// top of cs-user's own ignore-tenant_id-query behavior. Tenant scope must
// come ONLY from the X-Tenant-Id header (set by middleware.ResolveTenant /
// JWT slug).
func TestTenantAuditLog_StripsTenantIDQuery(t *testing.T) {
	stub := &stubTenantAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return &userpkg.AuditLogListResult{}, nil
	}}
	_, r := newTenantAuditLogAPI(stub)

	// Caller attempts to spoof a different tenant via query.
	req := httptest.NewRequest(http.MethodGet, "/api/tenant/audit-logs?tenant_id=t-forged", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if stub.last.TenantID != "" {
		t.Errorf("TenantID = %q, want empty (handler must strip query tenant_id)", stub.last.TenantID)
	}
}

func TestTenantAuditLog_NilSvc502(t *testing.T) {
	_, r := newTenantAuditLogAPI(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestTenantAuditLog_RPCUnavailable502(t *testing.T) {
	stub := &stubTenantAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		return nil, userpkg.ErrRPCUnavailable
	}}
	_, r := newTenantAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestTenantAuditLog_BadTo400(t *testing.T) {
	called := false
	stub := &stubTenantAuditLogSvc{list: func(_ context.Context, _ userpkg.AuditLogListParams) (*userpkg.AuditLogListResult, error) {
		called = true
		return nil, nil
	}}
	_, r := newTenantAuditLogAPI(stub)

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/audit-logs?to=2026-13-99", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if called {
		t.Error("service called on bad input")
	}
}
