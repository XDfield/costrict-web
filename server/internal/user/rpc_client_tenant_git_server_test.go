package user

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
)

// tenantGitServerStubServer replays a status + body for the
// /tenants/:tenant_id/git-server path. Captures the inbound path so tests
// can assert the tenant_id appears in the URL (not in a header).
func tenantGitServerStubServer(t *testing.T, status int, bodyJSON string) (*httptest.Server, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(bodyJSON))
	}))
	return srv, &gotPath
}

func TestRPCTenantGitServer_HappyPath(t *testing.T) {
	body := `{"server_id":"gs-acme","kind":"gitea","endpoint":"https://gitea.acme.com","admin_token":"tok-acme-XYZ"}`
	srv, gotPath := tenantGitServerStubServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.GetTenantGitServer(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("GetTenantGitServer: %v", err)
	}
	if got.ServerID != "gs-acme" {
		t.Errorf("server_id: got %q", got.ServerID)
	}
	if got.Endpoint != "https://gitea.acme.com" {
		t.Errorf("endpoint: got %q", got.Endpoint)
	}
	if got.AdminToken != "tok-acme-XYZ" {
		t.Errorf("admin_token: got %q", got.AdminToken)
	}
	if got.Kind != "gitea" {
		t.Errorf("kind: got %q", got.Kind)
	}
	if !strings.HasSuffix(*gotPath, "/tenants/t-acme/git-server") {
		t.Errorf("path: got %q, want suffix /tenants/t-acme/git-server", *gotPath)
	}
}

func TestRPCTenantGitServer_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	_, err := c.GetTenantGitServer(context.Background(), "t-acme")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestRPCTenantGitServer_EmptyTenantID(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://example.invalid")
	_, err := c.GetTenantGitServer(context.Background(), "  ")
	if !errors.Is(err, ErrGitServerTenantNotFound) {
		t.Errorf("want ErrGitServerTenantNotFound, got %v", err)
	}
}

func TestRPCTenantGitServer_TransportError(t *testing.T) {
	c := newConfiguredRPCClient(t, "http://127.0.0.1:0")
	_, err := c.GetTenantGitServer(context.Background(), "t-acme")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantGitServer_404(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusNotFound, `{"error":"gitserver: tenant not found"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-ghost")
	if !errors.Is(err, ErrGitServerTenantNotFound) {
		t.Errorf("want ErrGitServerTenantNotFound, got %v", err)
	}
}

func TestRPCTenantGitServer_500_MissingGitServerID(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusInternalServerError,
		`{"error":"gitserver: tenant has no git_server_id (bootstrap incomplete)"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-orphan")
	if !errors.Is(err, ErrGitServerNoBinding) {
		t.Errorf("want ErrGitServerNoBinding, got %v", err)
	}
}

func TestRPCTenantGitServer_500_FKViolation(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusInternalServerError,
		`{"error":"gitserver: git_server row not found (FK violation)"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-bad")
	if !errors.Is(err, ErrGitServerRowMissing) {
		t.Errorf("want ErrGitServerRowMissing, got %v", err)
	}
}

func TestRPCTenantGitServer_500_ConfigMalformed(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusInternalServerError,
		`{"error":"gitserver: config JSON malformed or missing admin_token: server=gs-x"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-bad")
	if !errors.Is(err, ErrGitServerConfigMalformed) {
		t.Errorf("want ErrGitServerConfigMalformed, got %v", err)
	}
}

func TestRPCTenantGitServer_503_Disabled(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusServiceUnavailable,
		`{"error":"gitserver: git server is disabled"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-drained")
	if !errors.Is(err, ErrGitServerDisabled) {
		t.Errorf("want ErrGitServerDisabled, got %v", err)
	}
}

func TestRPCTenantGitServer_500_Unknown(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusInternalServerError,
		`{"error":"random 500"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-acme")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantGitServer_4xx_Operational(t *testing.T) {
	srv, _ := tenantGitServerStubServer(t, http.StatusTeapot, `{"error":"random 4xx"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-acme")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCTenantGitServer_Incomplete200Body(t *testing.T) {
	// 200 but missing endpoint — operator bug. Should surface as malformed.
	srv, _ := tenantGitServerStubServer(t, http.StatusOK,
		`{"server_id":"gs-x","kind":"gitea","endpoint":"","admin_token":"tok"}`)
	defer srv.Close()
	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetTenantGitServer(context.Background(), "t-acme")
	if !errors.Is(err, ErrGitServerConfigMalformed) {
		t.Errorf("want ErrGitServerConfigMalformed, got %v", err)
	}
}
