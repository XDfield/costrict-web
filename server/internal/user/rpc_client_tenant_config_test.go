package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/tenant"
)

// tenantConfigStubServer replays a status + body for any path/method.
// Captures inbound method, X-Tenant-Id, and X-Actor-Subject-Id so tests
// can assert forwarding.
func tenantConfigStubServer(t *testing.T, status int, bodyJSON string) (*httptest.Server, *string, *string, *string) {
	t.Helper()
	var gotMethod, gotTenantHeader, gotActorHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotTenantHeader = r.Header.Get("X-Tenant-Id")
		gotActorHeader = r.Header.Get(ActorSubjectIDHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(bodyJSON))
	}))
	return srv, &gotMethod, &gotTenantHeader, &gotActorHeader
}

// ---------------- GetTenantConfig ----------------

func TestRPCTenantConfig_Get_HappyPath(t *testing.T) {
	body := `{"tenant_id":"t-acme","config_yaml":"key: value","updated_by":"subj-1","updated_at":"2026-07-17T00:00:00Z","created_at":"2026-07-17T00:00:00Z"}`
	srv, gotMethod, gotTenantHeader, _ := tenantConfigStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	got, err := c.GetTenantConfig(ctx)
	if err != nil {
		t.Fatalf("GetTenantConfig: %v", err)
	}
	if got.TenantID != "t-acme" || got.ConfigYAML != "key: value" {
		t.Errorf("body: %+v", got)
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", *gotMethod)
	}
	if *gotTenantHeader != "acme" {
		t.Errorf("X-Tenant-Id: got %q want acme", *gotTenantHeader)
	}
}

func TestRPCTenantConfig_Get_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	_, err := c.GetTenantConfig(context.Background())
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestRPCTenantConfig_Get_TransportError_Unavailable(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0")
	_, err := c.GetTenantConfig(context.Background())
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantConfig_Get_5xx_Unavailable(t *testing.T) {
	srv, _, _, _ := tenantConfigStubServer(t, http.StatusBadGateway, `{"error":"boom"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantConfig(context.Background())
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantConfig_Get_4xx_Unavailable(t *testing.T) {
	srv, _, _, _ := tenantConfigStubServer(t, http.StatusBadRequest, `{"error":"random 400"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantConfig(context.Background())
	if !errors.Is(err, ErrTenantConfigUnavailable) {
		t.Errorf("want ErrTenantConfigUnavailable, got %v", err)
	}
}

func TestRPCTenantConfig_Get_DecodeError(t *testing.T) {
	srv, _, _, _ := tenantConfigStubServer(t, http.StatusOK, `not-json`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantConfig(context.Background())
	if err == nil {
		t.Errorf("want decode error, got nil")
	}
}

// ---------------- UpdateTenantConfig ----------------

func TestRPCTenantConfig_Update_HappyPath(t *testing.T) {
	body := `{"tenant_id":"t-acme","config_yaml":"key: value","updated_by":"subj-1","updated_at":"2026-07-17T00:00:00Z","created_at":"2026-07-17T00:00:00Z"}`
	srv, gotMethod, gotTenantHeader, gotActorHeader := tenantConfigStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	got, err := c.UpdateTenantConfig(ctx, "key: value", "subj-1")
	if err != nil {
		t.Fatalf("UpdateTenantConfig: %v", err)
	}
	if got.ConfigYAML != "key: value" {
		t.Errorf("body: %+v", got)
	}
	if *gotMethod != http.MethodPut {
		t.Errorf("method: got %q want PUT", *gotMethod)
	}
	if *gotTenantHeader != "acme" {
		t.Errorf("X-Tenant-Id: got %q want acme", *gotTenantHeader)
	}
	if *gotActorHeader != "subj-1" {
		t.Errorf("X-Actor-Subject-Id: got %q want subj-1", *gotActorHeader)
	}
}

func TestRPCTenantConfig_Update_EmptyActor_OmitsHeader(t *testing.T) {
	body := `{"tenant_id":"t-acme","config_yaml":"{}"}`
	srv, _, _, gotActorHeader := tenantConfigStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateTenantConfig(context.Background(), "{}", "")
	if err != nil {
		t.Fatalf("UpdateTenantConfig: %v", err)
	}
	if *gotActorHeader != "" {
		t.Errorf("X-Actor-Subject-Id: got %q want empty", *gotActorHeader)
	}
}

func TestRPCTenantConfig_Update_InvalidYAML_400(t *testing.T) {
	// cs-user's exact sentinel text — must match what handlers.respondTenantConfigErr
	// emits so the RPC client's body-text matcher can route correctly.
	srv, _, _, _ := tenantConfigStubServer(t, http.StatusBadRequest, `{"error":"invalid YAML"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateTenantConfig(context.Background(), "bad", "subj-1")
	if !errors.Is(err, ErrInvalidYAML) {
		t.Errorf("want ErrInvalidYAML, got %v", err)
	}
}

func TestRPCTenantConfig_Update_TooLarge_413(t *testing.T) {
	srv, _, _, _ := tenantConfigStubServer(t, http.StatusRequestEntityTooLarge, `{"error":"YAML exceeds size cap"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateTenantConfig(context.Background(), "x", "")
	if !errors.Is(err, ErrYAMLTooLarge) {
		t.Errorf("want ErrYAMLTooLarge, got %v", err)
	}
}

func TestRPCTenantConfig_Update_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"})
	_, err := c.UpdateTenantConfig(context.Background(), "x", "")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestRPCTenantConfig_Update_TransportError_Unavailable(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0")
	_, err := c.UpdateTenantConfig(context.Background(), "x", "")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

// Adversarial: cs-user returns a 400 but the body text is NOT the
// "invalid YAML" sentinel (e.g. an unrelated 400 from a future code path).
// Should surface as ErrTenantConfigUnavailable, NOT ErrInvalidYAML.
func TestRPCTenantConfig_Update_400NonYAMLBody_UnavailableNotInvalid(t *testing.T) {
	srv, _, _, _ := tenantConfigStubServer(t, http.StatusBadRequest, `{"error":"some other 400 reason"}`)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateTenantConfig(context.Background(), "x", "")
	if !errors.Is(err, ErrTenantConfigUnavailable) {
		t.Errorf("want ErrTenantConfigUnavailable, got %v", err)
	}
	if errors.Is(err, ErrInvalidYAML) {
		t.Errorf("must NOT be ErrInvalidYAML — body wasn't the YAML sentinel")
	}
}

// Sanity check that the TenantConfig JSON tags match what cs-user emits.
func TestTenantConfig_JSONRoundTrip(t *testing.T) {
	tc := TenantConfig{
		TenantID:   "t-1",
		ConfigYAML: "key: value",
	}
	buf, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"tenant_id":"t-1","config_yaml":"key: value","updated_by":null,"updated_at":"","created_at":""}`
	if string(buf) != want {
		t.Errorf("JSON: got %s want %s", buf, want)
	}
}
