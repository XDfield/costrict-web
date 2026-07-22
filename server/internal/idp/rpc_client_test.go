package idp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
)

func newTestClient(t *testing.T, srv *httptest.Server) *RPCClient {
	t.Helper()
	return &RPCClient{
		baseURL:       srv.URL,
		internalToken: "test-token",
		httpClient:    srv.Client(),
	}
}

func TestRPCClient_NotConfigured(t *testing.T) {
	c := &RPCClient{}
	if _, err := c.ListEnabledIdPs(context.Background(), "t1"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
	if _, err := c.GetIdP(context.Background(), "t1", "github"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}

func TestListEnabledIdPs_Success(t *testing.T) {
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Internal-Token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]InternalIdPSourceView{
			{Provider: "github", Config: map[string]interface{}{"client_secret": "shh"}, Priority: 100},
			{Provider: "google", Config: map[string]interface{}{"client_secret": "secret"}, Priority: 50},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	views, err := c.ListEnabledIdPs(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("ListEnabledIdPs: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("expected 2 views, got %d", len(views))
	}
	if views[0].Provider != "github" {
		t.Errorf("first provider: got %s want github", views[0].Provider)
	}
	// Secrets must come through raw
	if views[0].Config["client_secret"] != "shh" {
		t.Errorf("expected raw secret, got %v", views[0].Config["client_secret"])
	}
	if gotPath != "/api/internal/idp-sources/t-acme/enabled" {
		t.Errorf("path: got %s", gotPath)
	}
	if gotToken != "test-token" {
		t.Errorf("token header missing or wrong: %q", gotToken)
	}
}

func TestListEnabledIdPs_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	views, err := c.ListEnabledIdPs(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("ListEnabledIdPs: %v", err)
	}
	if views == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(views) != 0 {
		t.Errorf("expected 0 items, got %d", len(views))
	}
}

func TestListEnabledIdPs_EmptyTenantID(t *testing.T) {
	c := &RPCClient{baseURL: "http://x", internalToken: "t"}
	_, err := c.ListEnabledIdPs(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty tenant_id") {
		t.Errorf("expected empty tenant_id error, got %v", err)
	}
}

func TestListEnabledIdPs_5xx_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.ListEnabledIdPs(context.Background(), "t-acme")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("expected ErrRPCUnavailable, got %v", err)
	}
}

func TestGetIdP_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetIdP(context.Background(), "t-acme", "github")
	if !errors.Is(err, ErrIdPNotFound) {
		t.Errorf("expected ErrIdPNotFound, got %v", err)
	}
}

func TestGetIdP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/t-acme/github") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(InternalIdPSourceView{
			Provider: "github",
			Config:   map[string]interface{}{"client_id": "id", "client_secret": "sec"},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	view, err := c.GetIdP(context.Background(), "t-acme", "github")
	if err != nil {
		t.Fatalf("GetIdP: %v", err)
	}
	if view.Provider != "github" {
		t.Errorf("provider: got %s", view.Provider)
	}
	if view.Config["client_secret"] != "sec" {
		t.Errorf("expected raw secret, got %v", view.Config["client_secret"])
	}
}

// Smoke test the constructor produces a Configured() client when given full cfg.
func TestNewRPCClient_Configured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{
		BaseURL:       "http://localhost:8081",
		InternalToken: "tok",
		TimeoutSec:    5,
	})
	if !c.Configured() {
		t.Error("expected Configured() true")
	}
}
