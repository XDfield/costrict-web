package user

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/tenant"
)

// adminUserStubServer returns a test server that captures inbound method,
// path (with query), body, and X-Tenant-Id header. Replays the supplied
// status + body. One helper covers all four admin-user RPC methods.
func adminUserStubServer(t *testing.T, status int, bodyJSON string) (*httptest.Server, *string, *string, *[]byte, *string) {
	t.Helper()
	var gotMethod, gotPath, gotTenant string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if r.URL.RawQuery != "" {
			gotPath += "?" + r.URL.RawQuery
		}
		gotTenant = r.Header.Get("X-Tenant-Id")
		if r.Body != nil {
			gotBody, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(bodyJSON))
	}))
	return srv, &gotMethod, &gotPath, &gotBody, &gotTenant
}

// --- ListUsers ---

func TestRPCAdminUser_ListUsers_HappyPath(t *testing.T) {
	body := `{"users":[{"subject_id":"s-1","username":"alice","status":"active","is_active":true,"created_at":"2026-01-01T00:00:00Z"}],"total":1,"page":2,"page_size":10}`
	srv, gotMethod, gotPath, _, gotTenant := adminUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	got, err := c.ListUsers(ctx, AdminUserListParams{
		Keyword: "ali", Organization: "Eng", Status: "active", Page: 2, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if got.Total != 1 || len(got.Users) != 1 || got.Users[0].SubjectID != "s-1" {
		t.Errorf("unexpected body: %+v", got)
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", *gotMethod)
	}
	for _, want := range []string{"keyword=ali", "organization=Eng", "status=active", "page=2", "page_size=10"} {
		if !strings.Contains(*gotPath, want) {
			t.Errorf("path missing %q: %s", want, *gotPath)
		}
	}
	if *gotTenant != "acme" {
		t.Errorf("X-Tenant-Id: got %q want acme", *gotTenant)
	}
}

func TestRPCAdminUser_ListUsers_EmptyResult(t *testing.T) {
	body := `{"users":[],"total":0,"page":1,"page_size":20}`
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListUsers(context.Background(), AdminUserListParams{})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if got.Users == nil {
		t.Errorf("Users should be non-nil empty slice, got nil")
	}
	if len(got.Users) != 0 {
		t.Errorf("expected 0 users, got %d", len(got.Users))
	}
}

func TestRPCAdminUser_ListUsers_5xxReturnsRPCUnavailable(t *testing.T) {
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusInternalServerError, `{"error":"db down"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListUsers(context.Background(), AdminUserListParams{})
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("expected ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCAdminUser_ListUsers_NotConfigured(t *testing.T) {
	c := newConfiguredRPCClient(t, "") // empty URL → not configured
	_, err := c.ListUsers(context.Background(), AdminUserListParams{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}

// --- SetUserStatus ---

func TestRPCAdminUser_SetUserStatus_HappyPath(t *testing.T) {
	srv, gotMethod, gotPath, gotBody, _ := adminUserStubServer(t, http.StatusOK, `{"from_status":"active","to_status":"banned"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.SetUserStatus(context.Background(), "subj-1", "banned", "admin-007")
	if err != nil {
		t.Fatalf("SetUserStatus: %v", err)
	}
	if got.FromStatus != "active" || got.ToStatus != "banned" {
		t.Errorf("unexpected: %+v", got)
	}
	if *gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", *gotMethod)
	}
	if !strings.HasSuffix(*gotPath, "/users/subj-1/status") {
		t.Errorf("path: %s", *gotPath)
	}
	var decoded map[string]string
	if err := json.Unmarshal(*gotBody, &decoded); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if decoded["status"] != "banned" || decoded["operator_id"] != "admin-007" {
		t.Errorf("body: %s", string(*gotBody))
	}
}

func TestRPCAdminUser_SetUserStatus_SelfLockReturns409(t *testing.T) {
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusConflict, `{"error":"user: cannot change own status"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.SetUserStatus(context.Background(), "self", "disabled", "self")
	if !errors.Is(err, ErrAdminUserRPCCannotChangeOwn) {
		t.Errorf("expected ErrAdminUserRPCCannotChangeOwn, got %v", err)
	}
}

func TestRPCAdminUser_SetUserStatus_NotFoundReturns404(t *testing.T) {
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusNotFound, `{"error":"user: admin target not found"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.SetUserStatus(context.Background(), "ghost", "disabled", "admin-1")
	if !errors.Is(err, ErrAdminUserRPCNotFound) {
		t.Errorf("expected ErrAdminUserRPCNotFound, got %v", err)
	}
}

func TestRPCAdminUser_SetUserStatus_InvalidStatusReturns400(t *testing.T) {
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusBadRequest, `{"error":"status must be one of active|disabled|banned"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.SetUserStatus(context.Background(), "subj-1", "quarantined", "admin-1")
	if !errors.Is(err, ErrAdminUserRPCInvalidStatus) {
		t.Errorf("expected ErrAdminUserRPCInvalidStatus, got %v", err)
	}
}

// --- ListOrganizations ---

func TestRPCAdminUser_ListOrganizations_HappyPath(t *testing.T) {
	body := `{"organizations":[{"organization":"Eng","memberCount":42},{"organization":"Ops","memberCount":7}]}`
	srv, gotMethod, gotPath, _, _ := adminUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	if len(got) != 2 || got[0].Organization != "Eng" || got[0].MemberCount != 42 {
		t.Errorf("unexpected: %+v", got)
	}
	if *gotMethod != http.MethodGet || !strings.HasSuffix(*gotPath, "/users/organizations") {
		t.Errorf("method/path: %s %s", *gotMethod, *gotPath)
	}
}

func TestRPCAdminUser_ListOrganizations_Empty(t *testing.T) {
	body := `{"organizations":[]}`
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("expected non-nil empty slice, got %v", got)
	}
}

// --- GetUserProfile ---

func TestRPCAdminUser_GetUserProfile_HappyPath(t *testing.T) {
	body := `{"subject_id":"subj-1","username":"alice","status":"active","is_active":true,"created_at":"2026-01-01T00:00:00Z","display_name":"Alice","email":"a@x.com"}`
	srv, gotMethod, gotPath, _, _ := adminUserStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.GetUserProfile(context.Background(), "subj-1")
	if err != nil {
		t.Fatalf("GetUserProfile: %v", err)
	}
	if got.SubjectID != "subj-1" || got.Username != "alice" || got.Status != "active" {
		t.Errorf("unexpected: %+v", got)
	}
	if *gotMethod != http.MethodGet || !strings.HasSuffix(*gotPath, "/users/subj-1/profile") {
		t.Errorf("method/path: %s %s", *gotMethod, *gotPath)
	}
}

func TestRPCAdminUser_GetUserProfile_NotFoundReturns404(t *testing.T) {
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusNotFound, `{"error":"user not found"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetUserProfile(context.Background(), "ghost")
	if !errors.Is(err, ErrAdminUserRPCNotFound) {
		t.Errorf("expected ErrAdminUserRPCNotFound, got %v", err)
	}
}

func TestRPCAdminUser_GetUserProfile_5xxReturnsRPCUnavailable(t *testing.T) {
	srv, _, _, _, _ := adminUserStubServer(t, http.StatusBadGateway, `{"error":"gateway"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetUserProfile(context.Background(), "subj-1")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("expected ErrRPCUnavailable, got %v", err)
	}
}
