package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/tenant"
)

// tenantUserStubServer returns a test server that replays the supplied
// status + body for any path. Captures inbound method, raw path (with
// query), and X-Tenant-Id header so tests can assert forwarding.
func tenantUserStubServer(t *testing.T, status int, bodyJSON string) (*httptest.Server, *string, *string, *string) {
	t.Helper()
	var gotMethod, gotPath, gotTenantHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotTenantHeader = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(bodyJSON))
	}))
	return srv, &gotMethod, &gotPath, &gotTenantHeader
}

func TestRPCTenantUser_List_HappyPath_Envelope(t *testing.T) {
	body := `{"users":[{"subject_id":"s-1","username":"alice","display_name":"Alice","email":"a@x.com","is_active":true,"tenant_id":"t-1"}]}`
	srv, gotMethod, gotPath, gotTenantHeader := tenantUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	// Inject tenant slug via the same ctx key production uses (server
	// handler sets this from AuthClaims.TenantSlug before calling us).
	ctx := tenant.WithSlug(context.Background(), "acme")
	got, err := c.ListTenantUsers(ctx, "ali", 25)
	if err != nil {
		t.Fatalf("ListTenantUsers: %v", err)
	}
	if len(got) != 1 || got[0].SubjectID != "s-1" || got[0].Username != "alice" {
		t.Errorf("body: %+v", got)
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", *gotMethod)
	}
	if !strings.Contains(*gotPath, "keyword=ali") || !strings.Contains(*gotPath, "limit=25") {
		t.Errorf("query: got %q", *gotPath)
	}
	if *gotTenantHeader != "acme" {
		t.Errorf("X-Tenant-Id: got %q want acme", *gotTenantHeader)
	}
}

func TestRPCTenantUser_List_HappyPath_BareArray(t *testing.T) {
	// Defensive: tolerate a bare array as well as the {users: [...]}
	// envelope so a future cs-user refactor doesn't break us.
	body := `[{"subject_id":"s-2","username":"bob","is_active":true}]`
	srv, _, _, _ := tenantUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListTenantUsers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("ListTenantUsers: %v", err)
	}
	if len(got) != 1 || got[0].Username != "bob" {
		t.Errorf("body: %+v", got)
	}
}

func TestRPCTenantUser_List_EmptyKeywordAndLimit(t *testing.T) {
	body := `{"users":[]}`
	srv, _, gotPath, _ := tenantUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListTenantUsers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("ListTenantUsers: %v", err)
	}
	// Empty keyword + limit=0 → query string should be empty (no params
	// set), so gotPath ends with the bare path + trailing "?".
	if !strings.HasPrefix(*gotPath, tenantUsersSearchPath) {
		t.Errorf("path: got %q", *gotPath)
	}
	// No keyword / limit in the URL.
	if strings.Contains(*gotPath, "keyword=") || strings.Contains(*gotPath, "limit=") {
		t.Errorf("expected no keyword/limit params; got %q", *gotPath)
	}
}

func TestRPCTenantUser_List_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	_, err := c.ListTenantUsers(context.Background(), "", 0)
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestRPCTenantUser_List_TransportError_Unavailable(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0") // nothing listening
	_, err := c.ListTenantUsers(context.Background(), "x", 5)
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantUser_List_5xx_Unavailable(t *testing.T) {
	srv, _, _, _ := tenantUserStubServer(t, http.StatusBadGateway, `{"error":"boom"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListTenantUsers(context.Background(), "x", 5)
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantUser_List_4xx_Unavailable(t *testing.T) {
	// 4xx from cs-user means the RPC client mangled the request (handler
	// is the only legitimate 4xx source); surface as a 502-class
	// "tenant user service unavailable" rather than passing 4xx through.
	srv, _, _, _ := tenantUserStubServer(t, http.StatusBadRequest, `{"error":"bad limit"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListTenantUsers(context.Background(), "x", -1)
	if !errors.Is(err, ErrTenantUserUnavailable) {
		t.Errorf("want ErrTenantUserUnavailable, got %v", err)
	}
}

func TestRPCTenantUser_List_DecodeError(t *testing.T) {
	srv, _, _, _ := tenantUserStubServer(t, http.StatusOK, `not-json`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListTenantUsers(context.Background(), "", 0)
	if err == nil {
		t.Errorf("want decode error, got nil")
	}
}

// TestRPCTenantUser_List_NoSlugInContext — production handler should
// always set the slug, but defensively the RPC client must not crash
// when it's absent; the request still goes out without X-Tenant-Id and
// cs-user's ResolveTenant middleware will fail downstream with a 503
// (surfaced as ErrRPCUnavailable here).
func TestRPCTenantUser_List_NoSlugInContext(t *testing.T) {
	body := `{"users":[]}`
	srv, _, _, gotTenantHeader := tenantUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListTenantUsers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("ListTenantUsers: %v", err)
	}
	if *gotTenantHeader != "" {
		t.Errorf("X-Tenant-Id: got %q want empty when ctx has no slug", *gotTenantHeader)
	}
}

// Sanity check that the TenantUser JSON tags match what cs-user emits.
func TestTenantUser_JSONRoundTrip(t *testing.T) {
	u := TenantUser{
		SubjectID: "s-1", Username: "alice", DisplayName: "Alice",
		Email: "a@x.com", IsActive: true, TenantID: "t-1",
	}
	buf, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"subject_id":"s-1","username":"alice","display_name":"Alice","email":"a@x.com","is_active":true,"tenant_id":"t-1"}`
	if string(buf) != want {
		t.Errorf("JSON: got %s want %s", buf, want)
	}
}
