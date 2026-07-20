package user

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/tenant"
)

// auditLogStubServer captures inbound method/path/tenant and replays the
// supplied status + body. Used by every test in this file.
func auditLogStubServer(t *testing.T, status int, bodyJSON string) (*httptest.Server, *string, *string, *string) {
	t.Helper()
	var gotMethod, gotPath, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if r.URL.RawQuery != "" {
			gotPath += "?" + r.URL.RawQuery
		}
		gotTenant = r.Header.Get("X-Tenant-Id")
		if r.Body != nil {
			_, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(bodyJSON))
	}))
	return srv, &gotMethod, &gotPath, &gotTenant
}

// --- ListAuditLogs (platform) ---

func TestRPCAuditLog_List_HappyPath(t *testing.T) {
	body := `{"logs":[{"id":42,"action":"tenant.create","tenant_id":"t-acme","created_at":"2026-07-01T12:00:00Z"}],"total":1,"limit":10,"offset":5}`
	srv, gotMethod, gotPath, gotTenant := auditLogStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	got, err := c.ListAuditLogs(ctx, AuditLogListParams{
		TenantID: "t-acme", Action: "tenant.create",
		Limit: 10, Offset: 5,
		From: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	if got.Total != 1 || len(got.Logs) != 1 || got.Logs[0].ID != 42 {
		t.Errorf("unexpected body: %+v", got)
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method = %q want GET", *gotMethod)
	}
	if !strings.HasPrefix(*gotPath, "/api/internal/platform/audit-logs") {
		t.Errorf("path = %q want platform prefix", *gotPath)
	}
	// tenant_id should be in the query (trustTenantID=true on platform method).
	if !strings.Contains(*gotPath, "tenant_id=t-acme") {
		t.Errorf("path missing tenant_id query: %q", *gotPath)
	}
	if !strings.Contains(*gotPath, "action=tenant.create") {
		t.Errorf("path missing action query: %q", *gotPath)
	}
	if !strings.Contains(*gotPath, "limit=10") || !strings.Contains(*gotPath, "offset=5") {
		t.Errorf("path missing pagination: %q", *gotPath)
	}
	if !strings.Contains(*gotPath, "from=") || !strings.Contains(*gotPath, "to=") {
		t.Errorf("path missing time bounds: %q", *gotPath)
	}
	if *gotTenant != "acme" {
		t.Errorf("X-Tenant-Id = %q want acme", *gotTenant)
	}
}

func TestRPCAuditLog_List_EmptyResultOK(t *testing.T) {
	body := `{"logs":[],"total":0,"limit":100,"offset":0}`
	srv, _, _, _ := auditLogStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListAuditLogs(context.Background(), AuditLogListParams{})
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	if got.Total != 0 || len(got.Logs) != 0 {
		t.Errorf("unexpected body: %+v", got)
	}
}

func TestRPCAuditLog_List_OmitsZeroValueFilters(t *testing.T) {
	body := `{"logs":[],"total":0,"limit":100,"offset":0}`
	srv, _, gotPath, _ := auditLogStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListAuditLogs(context.Background(), AuditLogListParams{})
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	// Empty params should send no filters and no pagination — cs-user
	// applies its own defaults.
	if strings.Contains(*gotPath, "?") {
		t.Errorf("path should have no query: %q", *gotPath)
	}
}

func TestRPCAuditLog_List_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	if _, err := c.ListAuditLogs(context.Background(), AuditLogListParams{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestRPCAuditLog_List_5xxReturnsRPCUnavailable(t *testing.T) {
	srv, _, _, _ := auditLogStubServer(t, http.StatusBadGateway, `{"error":"upstream down"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListAuditLogs(context.Background(), AuditLogListParams{})
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Fatalf("err = %v, want ErrRPCUnavailable", err)
	}
}

func TestRPCAuditLog_List_4xxReturnsBadRequest(t *testing.T) {
	srv, _, _, _ := auditLogStubServer(t, http.StatusBadRequest, `{"error":"from must be ISO8601"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListAuditLogs(context.Background(), AuditLogListParams{})
	if !errors.Is(err, ErrAuditLogRPCBadRequest) {
		t.Fatalf("err = %v, want ErrAuditLogRPCBadRequest", err)
	}
}

// --- ListAuditLogsForTenant (tenant-scoped) ---

func TestRPCAuditLog_ListForTenant_HappyPath(t *testing.T) {
	body := `{"logs":[{"id":7,"action":"user.status_changed","created_at":"2026-07-03T15:00:00Z"}],"total":1,"limit":50,"offset":0}`
	srv, _, gotPath, gotTenant := auditLogStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "globex")
	got, err := c.ListAuditLogsForTenant(ctx, AuditLogListParams{
		Action: "user.status_changed", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListAuditLogsForTenant: %v", err)
	}
	if got.Total != 1 || len(got.Logs) != 1 || got.Logs[0].ID != 7 {
		t.Errorf("unexpected body: %+v", got)
	}
	if !strings.HasPrefix(*gotPath, "/api/internal/tenants/audit-logs") {
		t.Errorf("path = %q want tenant prefix", *gotPath)
	}
	// tenant_id must NOT be in the query — cs-user forces scope from ctx.
	if strings.Contains(*gotPath, "tenant_id=") {
		t.Errorf("tenant-scope endpoint should not send tenant_id query: %q", *gotPath)
	}
	if *gotTenant != "globex" {
		t.Errorf("X-Tenant-Id = %q want globex (slug forwarded for upstream ctx resolution)", *gotTenant)
	}
}

func TestRPCAuditLog_ListForTenant_OmitsTenantIDEvenWhenSupplied(t *testing.T) {
	body := `{"logs":[],"total":0,"limit":100,"offset":0}`
	srv, _, gotPath, _ := auditLogStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	// Caller passes TenantID anyway — client must strip it so cs-user's
	// ctx-derived scope is the only authority.
	_, err := c.ListAuditLogsForTenant(context.Background(), AuditLogListParams{
		TenantID: "t-forged",
	})
	if err != nil {
		t.Fatalf("ListAuditLogsForTenant: %v", err)
	}
	if strings.Contains(*gotPath, "tenant_id=") {
		t.Errorf("tenant_id leaked into tenant-scoped query: %q", *gotPath)
	}
}

func TestRPCAuditLog_ListForTenant_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	if _, err := c.ListAuditLogsForTenant(context.Background(), AuditLogListParams{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// --- buildAuditLogQuery (unit-level) ---

func TestBuildAuditLogQuery_TrustTenantTrueIncludesIt(t *testing.T) {
	q := buildAuditLogQuery(AuditLogListParams{
		TenantID:       "t-x",
		ActorSubjectID: "u-admin",
		Action:         "tenant.create",
		TargetType:     "tenant",
		TargetID:       "tenant:t-x",
		From:           time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:             time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
		Limit:          25,
		Offset:         50,
	}, true)
	if got := q.Get("tenant_id"); got != "t-x" {
		t.Errorf("tenant_id = %q", got)
	}
	if got := q.Get("actor_subject_id"); got != "u-admin" {
		t.Errorf("actor_subject_id = %q", got)
	}
	if got := q.Get("from"); !strings.HasPrefix(got, "2026-07-01") {
		t.Errorf("from = %q", got)
	}
}

func TestBuildAuditLogQuery_TrustTenantFalseOmitsIt(t *testing.T) {
	q := buildAuditLogQuery(AuditLogListParams{
		TenantID: "t-ignored",
		Action:   "user.status_changed",
	}, false)
	if _, ok := q["tenant_id"]; ok {
		t.Errorf("tenant_id should be absent: %+v", q)
	}
	if got := q.Get("action"); got != "user.status_changed" {
		t.Errorf("action = %q", got)
	}
}

func TestBuildAuditLogQuery_SkipsZeroValues(t *testing.T) {
	q := buildAuditLogQuery(AuditLogListParams{}, true)
	if len(q) > 0 {
		t.Errorf("empty params should produce empty query: %+v", q)
	}
}
