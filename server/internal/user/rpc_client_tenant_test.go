package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/tenant"
)

// TestRPCClient_ForwardsTenantSlugHeader asserts that GetUserByID forwards
// the tenant slug stored in ctx as the X-Tenant-Id header on the outbound
// RPC call. This is the read-path half of B3b.2a's slug forwarding.
func TestRPCClient_ForwardsTenantSlugHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.User{SubjectID: "u1"})
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	if _, err := c.GetUserByID(ctx, "u1"); err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if gotHeader != "acme" {
		t.Errorf("X-Tenant-Id header: got %q, want acme", gotHeader)
	}
}

// TestRPCClient_OmitsTenantSlugHeaderWhenAbsent asserts that no X-Tenant-Id
// header is sent when the ctx carries no slug — cs-user then falls back to
// its default tenant (B3b.2a "no signal" contract).
func TestRPCClient_OmitsTenantSlugHeaderWhenAbsent(t *testing.T) {
	var gotHeader string
	var headerPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenant-Id")
		headerPresent = r.Header.Get("X-Tenant-Id") != ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.User{SubjectID: "u1"})
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	if _, err := c.GetUserByID(context.Background(), "u1"); err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if headerPresent {
		t.Errorf("X-Tenant-Id header unexpectedly set: %q (want absent)", gotHeader)
	}
}

// TestRPCClient_OmitsTenantSlugHeaderWhenEmpty covers the WithSlug(ctx, "")
// "explicit no-signal" path. Middleware uses this when all three fallback
// layers miss.
func TestRPCClient_OmitsTenantSlugHeaderWhenEmpty(t *testing.T) {
	var gotHeader string
	var headerPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenant-Id")
		headerPresent = r.Header.Get("X-Tenant-Id") != ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.User{SubjectID: "u1"})
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "")
	if _, err := c.GetUserByID(ctx, "u1"); err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if headerPresent {
		t.Errorf("X-Tenant-Id header unexpectedly set: %q (want absent for empty slug)", gotHeader)
	}
}
