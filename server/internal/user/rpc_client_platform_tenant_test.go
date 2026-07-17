package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
)

// platformTenantStubServer returns a test server that always replies with
// the supplied status code + body for any path under
// /api/internal/platform/tenants. Captures the inbound method + path +
// X-Tenant-Id header so tests can assert on them.
func platformTenantStubServer(t *testing.T, status int, respBody any) (*httptest.Server, *string, *string, *string) {
	t.Helper()
	var gotMethod, gotPath, gotTenantHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotTenantHeader = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if respBody != nil {
			_ = json.NewEncoder(w).Encode(respBody)
		}
	}))
	return srv, &gotMethod, &gotPath, &gotTenantHeader
}

func TestRPCPlatform_ListTenants_HappyPath(t *testing.T) {
	want := PlatformTenantListResult{
		Tenants: []PlatformTenant{{TenantID: "t-1", Slug: "acme"}},
		Total:   1, Limit: 10, Offset: 0,
	}
	srv, gotMethod, gotPath, _ := platformTenantStubServer(t, http.StatusOK, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListTenants(context.Background(), 10, 0, "active")
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if got.Total != 1 || len(got.Tenants) != 1 {
		t.Errorf("body: %+v", got)
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", *gotMethod)
	}
	if *gotPath != platformTenantsPath {
		t.Errorf("path: got %q", *gotPath)
	}
	// Query params are appended to the path — verify they round-trip into
	// the request URL by re-issuing with a capturing server.
	var gotQuery string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PlatformTenantListResult{})
	}))
	defer srv2.Close()
	c2 := newConfiguredRPCClient(t, srv2.URL)
	if _, err := c2.ListTenants(context.Background(), 25, 50, "suspended"); err != nil {
		t.Fatalf("ListTenants(2): %v", err)
	}
	if gotQuery != "limit=25&offset=50&status=suspended" {
		t.Errorf("query: got %q", gotQuery)
	}
}

func TestRPCPlatform_GetTenant_HappyPath(t *testing.T) {
	want := PlatformTenant{TenantID: "t-1", Slug: "acme", DisplayName: "Acme"}
	srv, _, gotPath, _ := platformTenantStubServer(t, http.StatusOK, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.GetTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.Slug != "acme" {
		t.Errorf("slug: got %q", got.Slug)
	}
	if *gotPath != platformTenantsPath+"/acme" {
		t.Errorf("path: got %q", *gotPath)
	}
}

func TestRPCPlatform_GetTenant_NotFound(t *testing.T) {
	srv, _, _, _ := platformTenantStubServer(t, http.StatusNotFound, map[string]string{"error": "tenant not found"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenant(context.Background(), "nope")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Errorf("want ErrTenantNotFound, got %v", err)
	}
}

func TestRPCPlatform_CreateTenant_HappyPath(t *testing.T) {
	want := PlatformTenant{TenantID: "t-1", Slug: "acme"}
	srv, gotMethod, _, _ := platformTenantStubServer(t, http.StatusCreated, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.CreateTenant(context.Background(), PlatformTenantCreateParams{
		Slug: "acme", DisplayName: "Acme",
	})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if got.TenantID != "t-1" {
		t.Errorf("id: got %q", got.TenantID)
	}
	if *gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", *gotMethod)
	}
}

func TestRPCPlatform_CreateTenant_SlugTaken(t *testing.T) {
	srv, _, _, _ := platformTenantStubServer(t, http.StatusConflict, map[string]string{"error": "tenant: slug already taken"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.CreateTenant(context.Background(), PlatformTenantCreateParams{
		Slug: "acme", DisplayName: "Acme",
	})
	if !errors.Is(err, ErrSlugTaken) {
		t.Errorf("want ErrSlugTaken, got %v", err)
	}
}

func TestRPCPlatform_CreateTenant_EmailDomainConflict(t *testing.T) {
	// Stub uses the EXACT string cs-user's ErrEmailDomainConflict emits
	// (underscore-joined — derived from the email_domains column name).
	// A space-formatted stub would mask a matcher bug.
	srv, _, _, _ := platformTenantStubServer(t, http.StatusConflict, map[string]string{"error": "tenant: email_domain overlaps an existing tenant"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.CreateTenant(context.Background(), PlatformTenantCreateParams{
		Slug: "second", DisplayName: "Second",
	})
	if !errors.Is(err, ErrEmailDomainConflict) {
		t.Errorf("want ErrEmailDomainConflict, got %v", err)
	}
}

func TestRPCPlatform_CreateTenant_InvalidDisplayName(t *testing.T) {
	// Same pattern: stub uses cs-user's real ErrInvalidDisplayName text
	// (underscore-joined display_name, not space).
	srv, _, _, _ := platformTenantStubServer(t, http.StatusBadRequest, map[string]string{"error": "tenant: display_name is required"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.CreateTenant(context.Background(), PlatformTenantCreateParams{
		Slug: "acme", DisplayName: "   ",
	})
	if !errors.Is(err, ErrInvalidDisplayName) {
		t.Errorf("want ErrInvalidDisplayName, got %v", err)
	}
}

func TestRPCPlatform_CreateTenant_InvalidEmailDomains(t *testing.T) {
	// Stub uses cs-user's real ErrInvalidEmailDomains text.
	srv, _, _, _ := platformTenantStubServer(t, http.StatusBadRequest, map[string]string{"error": "tenant: invalid email_domains"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.CreateTenant(context.Background(), PlatformTenantCreateParams{
		Slug: "acme", DisplayName: "Acme", EmailDomains: []string{"not-a-domain"},
	})
	if !errors.Is(err, ErrInvalidEmailDomains) {
		t.Errorf("want ErrInvalidEmailDomains, got %v", err)
	}
}

func TestRPCPlatform_CreateTenant_InvalidSlug(t *testing.T) {
	srv, _, _, _ := platformTenantStubServer(t, http.StatusBadRequest, map[string]string{"error": "tenant: invalid slug"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.CreateTenant(context.Background(), PlatformTenantCreateParams{
		Slug: "AB", DisplayName: "Ab",
	})
	if !errors.Is(err, ErrInvalidSlug) {
		t.Errorf("want ErrInvalidSlug, got %v", err)
	}
}

func TestRPCPlatform_UpdateTenant_HappyPath(t *testing.T) {
	want := PlatformTenant{TenantID: "t-1", Slug: "acme", DisplayName: "New"}
	srv, gotMethod, gotPath, _ := platformTenantStubServer(t, http.StatusOK, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	newName := "New"
	got, err := c.UpdateTenant(context.Background(), "acme", PlatformTenantUpdateParams{
		DisplayName: &newName,
	})
	if err != nil {
		t.Fatalf("UpdateTenant: %v", err)
	}
	if got.DisplayName != "New" {
		t.Errorf("name: got %q", got.DisplayName)
	}
	if *gotMethod != http.MethodPatch {
		t.Errorf("method: got %q want PATCH", *gotMethod)
	}
	if *gotPath != platformTenantsPath+"/acme" {
		t.Errorf("path: got %q", *gotPath)
	}
}

func TestRPCPlatform_SuspendTenant_HappyPath(t *testing.T) {
	want := PlatformTenant{TenantID: "t-1", Slug: "acme", Status: "suspended"}
	srv, _, gotPath, _ := platformTenantStubServer(t, http.StatusOK, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.SuspendTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("SuspendTenant: %v", err)
	}
	if got.Status != "suspended" {
		t.Errorf("status: got %q", got.Status)
	}
	if *gotPath != platformTenantsPath+"/acme/suspend" {
		t.Errorf("path: got %q", *gotPath)
	}
}

func TestRPCPlatform_SuspendTenant_InvalidTransition(t *testing.T) {
	srv, _, _, _ := platformTenantStubServer(t, http.StatusConflict, map[string]string{"error": "tenant: invalid state transition"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.SuspendTenant(context.Background(), "acme")
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("want ErrInvalidStateTransition, got %v", err)
	}
}

func TestRPCPlatform_RestoreTenant_HappyPath(t *testing.T) {
	want := PlatformTenant{TenantID: "t-1", Slug: "acme", Status: "active"}
	srv, _, _, _ := platformTenantStubServer(t, http.StatusOK, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.RestoreTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("RestoreTenant: %v", err)
	}
	if got.Status != "active" {
		t.Errorf("status: got %q", got.Status)
	}
}

func TestRPCPlatform_DeleteTenant_HappyPath(t *testing.T) {
	want := PlatformTenant{TenantID: "t-1", Slug: "acme", Status: "deleted"}
	srv, _, _, _ := platformTenantStubServer(t, http.StatusOK, want)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.DeleteTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
	if got.Status != "deleted" {
		t.Errorf("status: got %q", got.Status)
	}
}

// Transport / 5xx → ErrRPCUnavailable (one test per failure mode).

func TestRPCPlatform_TransportError_Unavailable(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0") // nothing listening
	_, err := c.ListTenants(context.Background(), 10, 0, "")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCPlatform_5xx_Unavailable(t *testing.T) {
	srv, _, _, _ := platformTenantStubServer(t, http.StatusInternalServerError, map[string]string{"error": "boom"})
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenant(context.Background(), "acme")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCPlatform_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	_, err := c.ListTenants(context.Background(), 10, 0, "")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}
