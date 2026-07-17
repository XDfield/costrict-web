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

// providerMappingStubServer mirrors tenantConfigStubServer.
func providerMappingStubServer(t *testing.T, status int, bodyJSON string) (*httptest.Server, *string, *string, *string) {
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

func rpcBool(b bool) *bool { return &b }
func rpcInt(n int) *int    { return &n }

// ---------------- GetProviderMapping ----------------

func TestRPCProviderMapping_Get_HappyPath(t *testing.T) {
	body := `{"providers":{"ldap":{"enabled":true,"rank":200}}}`
	srv, gotMethod, gotTenantHeader, _ := providerMappingStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	got, err := c.GetProviderMapping(ctx)
	if err != nil {
		t.Fatalf("GetProviderMapping: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Errorf("providers: got %d want 1", len(got.Providers))
	}
	if *gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", *gotMethod)
	}
	if *gotTenantHeader != "acme" {
		t.Errorf("X-Tenant-Id: got %q want acme", *gotTenantHeader)
	}
}

func TestRPCProviderMapping_Get_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"})
	_, err := c.GetProviderMapping(context.Background())
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestRPCProviderMapping_Get_TransportError_Unavailable(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0")
	_, err := c.GetProviderMapping(context.Background())
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCProviderMapping_Get_5xx_Unavailable(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadGateway, `{"error":"boom"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetProviderMapping(context.Background())
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCProviderMapping_Get_4xx_Unavailable(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadRequest, `{"error":"random 400"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetProviderMapping(context.Background())
	if !errors.Is(err, ErrTenantConfigUnavailable) {
		t.Errorf("want ErrTenantConfigUnavailable, got %v", err)
	}
}

func TestRPCProviderMapping_Get_DecodeError(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusOK, `not-json`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetProviderMapping(context.Background())
	if err == nil {
		t.Errorf("want decode error, got nil")
	}
}

// ---------------- UpdateProviderMapping ----------------

func TestRPCProviderMapping_Update_HappyPath(t *testing.T) {
	body := `{"providers":{"ldap":{"enabled":true,"rank":200}}}`
	srv, gotMethod, gotTenantHeader, gotActorHeader := providerMappingStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	ctx := tenant.WithSlug(context.Background(), "acme")
	m := &ProviderMapping{Providers: map[string]Provider{
		"ldap": {Rank: rpcInt(200)},
	}}
	got, err := c.UpdateProviderMapping(ctx, m, "subj-1")
	if err != nil {
		t.Fatalf("UpdateProviderMapping: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Errorf("providers: got %d want 1", len(got.Providers))
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

func TestRPCProviderMapping_Update_EmptyActor_OmitsHeader(t *testing.T) {
	body := `{"providers":{}}`
	srv, _, _, gotActorHeader := providerMappingStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if err != nil {
		t.Fatalf("UpdateProviderMapping: %v", err)
	}
	if *gotActorHeader != "" {
		t.Errorf("X-Actor-Subject-Id: got %q want empty", *gotActorHeader)
	}
}

func TestRPCProviderMapping_Update_NilMapping_NormalizedToEmpty(t *testing.T) {
	body := `{"providers":{}}`
	srv, _, _, _ := providerMappingStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.UpdateProviderMapping(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("UpdateProviderMapping(nil): %v", err)
	}
	if got.Providers == nil {
		t.Errorf("expected non-nil Providers map")
	}
}

func TestRPCProviderMapping_Update_InvalidProviderName_400(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadRequest, `{"error":"invalid provider name"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrProviderNameInvalid) {
		t.Errorf("want ErrProviderNameInvalid, got %v", err)
	}
}

func TestRPCProviderMapping_Update_InvalidInterval_400(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadRequest, `{"error":"invalid enterprise_sync.interval"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrIntervalInvalid) {
		t.Errorf("want ErrIntervalInvalid, got %v", err)
	}
}

func TestRPCProviderMapping_Update_NegativeRank_400(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadRequest, `{"error":"rank must be non-negative"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrRankNegative) {
		t.Errorf("want ErrRankNegative, got %v", err)
	}
}

func TestRPCProviderMapping_Update_InvalidYAMLInBlob_400(t *testing.T) {
	// Rare: cs-user found a malformed stored blob.
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadRequest, `{"error":"invalid YAML"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrInvalidYAML) {
		t.Errorf("want ErrInvalidYAML, got %v", err)
	}
}

// Adversarial: 400 with body text that doesn't match any typed sentinel
// must NOT be misclassified. Surface as ErrTenantConfigUnavailable.
func TestRPCProviderMapping_Update_400NonMatchingBody_Unavailable(t *testing.T) {
	srv, _, _, _ := providerMappingStubServer(t, http.StatusBadRequest, `{"error":"some unrelated 400"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrTenantConfigUnavailable) {
		t.Errorf("want ErrTenantConfigUnavailable, got %v", err)
	}
	for _, target := range []error{ErrInvalidYAML, ErrProviderNameInvalid, ErrIntervalInvalid, ErrRankNegative} {
		if errors.Is(err, target) {
			t.Errorf("must NOT match typed sentinel %v — body wasn't on the contract list", target)
		}
	}
}

func TestRPCProviderMapping_Update_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"})
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestRPCProviderMapping_Update_TransportError_Unavailable(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0")
	_, err := c.UpdateProviderMapping(context.Background(), &ProviderMapping{}, "")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

// Sanity check that ProviderMapping JSON tags match what cs-user emits.
func TestProviderMapping_JSONRoundTrip(t *testing.T) {
	m := ProviderMapping{Providers: map[string]Provider{
		"ldap": {Enabled: rpcBool(true), Rank: rpcInt(200)},
	}}
	buf, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"providers":{"ldap":{"enabled":true,"rank":200}}}`
	if string(buf) != want {
		t.Errorf("JSON: got %s want %s", buf, want)
	}
}
