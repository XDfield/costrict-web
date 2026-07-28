package user

import (
	"context"
	"encoding/json"
	"errors"
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

// ---- Phase B3b.2b-step2b: ResolveTenantByEmail ----

// newResolveByEmailServer returns a test server that mirrors cs-user's
// /api/internal/tenants/resolve-by-email contract: always 200, three-state
// `status` discriminator. The handler captures the inbound request body +
// X-Tenant-Id header so tests can assert on them.
func newResolveByEmailServer(t *testing.T, status, slug, tenantID string, candidates []TenantEmailCandidate) (*httptest.Server, *string, *string, *[]byte) {
	t.Helper()
	var gotEmail, gotHeader string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenant-Id")
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		_ = json.Unmarshal(gotBody, &(map[string]any{}))
		// Decode just to capture email; re-marshal for assertion simplicity.
		var parsed struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal(gotBody, &parsed)
		gotEmail = parsed.Email
		w.Header().Set("Content-Type", "application/json")
		switch status {
		case "ok":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":    "ok",
				"slug":      slug,
				"tenant_id": tenantID,
			})
		case "ambiguous":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":     "ambiguous",
				"candidates": candidates,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "not_found"})
		}
	}))
	return srv, &gotEmail, &gotHeader, &gotBody
}

func TestRPCClient_ResolveTenantByEmail_UniqueHit(t *testing.T) {
	srv, gotEmail, _, _ := newResolveByEmailServer(t, "ok", "acme", "t-acme", nil)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	res, err := c.ResolveTenantByEmail(context.Background(), "alice@acme.example.com")
	if err != nil {
		t.Fatalf("ResolveTenantByEmail: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("Status: got %q, want ok", res.Status)
	}
	if res.Slug != "acme" {
		t.Errorf("Slug: got %q, want acme", res.Slug)
	}
	if res.TenantID != "t-acme" {
		t.Errorf("TenantID: got %q, want t-acme", res.TenantID)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("Candidates should be empty on ok, got %d", len(res.Candidates))
	}
	if *gotEmail != "alice@acme.example.com" {
		t.Errorf("email forwarded: got %q, want alice@acme.example.com", *gotEmail)
	}
}

func TestRPCClient_ResolveTenantByEmail_Ambiguous(t *testing.T) {
	cands := []TenantEmailCandidate{
		{Slug: "acme", TenantID: "t-acme", Name: "Acme Co"},
		{Slug: "acme-emea", TenantID: "t-acme-emea", Name: "Acme EMEA"},
	}
	srv, _, _, _ := newResolveByEmailServer(t, "ambiguous", "", "", cands)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	res, err := c.ResolveTenantByEmail(context.Background(), "alice@acme.example.com")
	if err != nil {
		t.Fatalf("ResolveTenantByEmail: %v", err)
	}
	if res.Status != "ambiguous" {
		t.Fatalf("Status: got %q, want ambiguous", res.Status)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("Candidates count: got %d, want 2", len(res.Candidates))
	}
	if res.Candidates[0].Slug != "acme" {
		t.Errorf("first candidate slug: got %q, want acme", res.Candidates[0].Slug)
	}
	if res.Candidates[1].Name != "Acme EMEA" {
		t.Errorf("second candidate name: got %q, want 'Acme EMEA'", res.Candidates[1].Name)
	}
}

func TestRPCClient_ResolveTenantByEmail_NotFound(t *testing.T) {
	srv, _, _, _ := newResolveByEmailServer(t, "not_found", "", "", nil)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	res, err := c.ResolveTenantByEmail(context.Background(), "alice@nowhere.example.com")
	if err != nil {
		t.Fatalf("ResolveTenantByEmail: %v", err)
	}
	if res.Status != "not_found" {
		t.Errorf("Status: got %q, want not_found", res.Status)
	}
	if res.Slug != "" || res.TenantID != "" || len(res.Candidates) != 0 {
		t.Errorf("not_found resolution should have all empty fields: %+v", res)
	}
}

func TestRPCClient_ResolveTenantByEmail_EmptyEmailIsNoSignal(t *testing.T) {
	// Empty email short-circuits before any HTTP call — the handler treats
	// this as "Try 2 miss, fall through" without burning a round trip.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	res, err := c.ResolveTenantByEmail(context.Background(), "   ")
	if err != nil {
		t.Fatalf("ResolveTenantByEmail on empty email: %v", err)
	}
	if res.Status != "not_found" {
		t.Errorf("Status: got %q, want not_found (no signal)", res.Status)
	}
	if calls != 0 {
		t.Errorf("expected zero HTTP calls for empty email, got %d", calls)
	}
}

func TestRPCClient_ResolveTenantByEmail_5xxIsErrRPCUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"cs-user db down"}`))
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ResolveTenantByEmail(context.Background(), "alice@acme.example.com")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("err: got %v, want errors.Is ErrRPCUnavailable", err)
	}
}

func TestRPCClient_ResolveTenantByEmail_404IsErrRPCUnavailable(t *testing.T) {
	// cs-user deliberately returns 200 + status="not_found" at the
	// application layer. An HTTP 404 here means routing/deployment is
	// broken (older cs-user build, proxy mis-route) — treat as upstream-
	// unavailable rather than masking it as a benign miss.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ResolveTenantByEmail(context.Background(), "alice@acme.example.com")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("err: got %v, want errors.Is ErrRPCUnavailable", err)
	}
}

func TestRPCClient_ResolveTenantByEmail_ForwardsExistingSlug(t *testing.T) {
	// Even though Try 2 typically runs because Try 1 missed (no slug),
	// the client must still forward any slug that IS in ctx — uniform
	// contract across all cs-user RPCs.
	srv, _, gotHeader, _ := newResolveByEmailServer(t, "ok", "acme", "t-acme", nil)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "already-set")
	if _, err := c.ResolveTenantByEmail(ctx, "alice@acme.example.com"); err != nil {
		t.Fatalf("ResolveTenantByEmail: %v", err)
	}
	if *gotHeader != "already-set" {
		t.Errorf("X-Tenant-Id forwarded: got %q, want 'already-set'", *gotHeader)
	}
}

func TestRPCClient_ResolveTenantByEmail_Unconfigured(t *testing.T) {
	c := &RPCClient{} // baseURL + internalToken both empty → Configured()==false
	_, err := c.ResolveTenantByEmail(context.Background(), "alice@acme.example.com")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err: got %v, want errors.Is ErrNotConfigured", err)
	}
}

func TestRPCClient_ResolveTenantByEmail_UnknownStatusIsErrRPCUnavailable(t *testing.T) {
	// If cs-user adds a new status string this client doesn't recognize,
	// fail loud rather than silently dropping into "not_found".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"weird-new-state"}`))
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ResolveTenantByEmail(context.Background(), "alice@acme.example.com")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("err: got %v, want errors.Is ErrRPCUnavailable (unknown status)", err)
	}
}

// TestRPCClient_SatisfiesTenantResolverInterface confirms *RPCClient
// satisfies Module.TenantResolver (Phase B3b.2b-step2b wiring).
func TestRPCClient_SatisfiesTenantResolverInterface(t *testing.T) {
	var _ TenantResolver = (*RPCClient)(nil)
}
