package internal

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestResolve_NoNacos(t *testing.T) {
	resolver := NewEndpointResolver()
	cfg := &Config{Endpoint: "http://gateway.example.com:8081"}

	got, err := resolver.Resolve(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != cfg.Endpoint {
		t.Fatalf("expected endpoint %q, got %q", cfg.Endpoint, got)
	}
}

func TestResolve_NilConfig(t *testing.T) {
	resolver := NewEndpointResolver()

	_, err := resolver.Resolve(nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
	if !strings.Contains(err.Error(), "config is nil") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestResolve_NacosSuccess(t *testing.T) {
	expectedEndpoint := "http://nacos-resolved.example.com:8081"
	var capturedDataID, capturedGroup, capturedTenant string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nacos/v1/cs/configs" {
			http.NotFound(w, r)
			return
		}
		capturedDataID = r.URL.Query().Get("dataId")
		capturedGroup = r.URL.Query().Get("group")
		capturedTenant = r.URL.Query().Get("tenant")

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, expectedEndpoint)
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		Endpoint: "http://static.example.com:8081",
		Nacos: NacosConfig{
			ServerAddr:  server.URL,
			DataID:      "gateway-endpoint",
			Group:       "CUSTOM_GROUP",
			NamespaceID: "ns-123",
			TimeoutMs:   5000,
		},
	}

	got, err := resolver.Resolve(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != expectedEndpoint {
		t.Fatalf("expected endpoint %q, got %q", expectedEndpoint, got)
	}
	if capturedDataID != cfg.Nacos.DataID {
		t.Fatalf("expected dataId %q, got %q", cfg.Nacos.DataID, capturedDataID)
	}
	if capturedGroup != cfg.Nacos.Group {
		t.Fatalf("expected group %q, got %q", cfg.Nacos.Group, capturedGroup)
	}
	if capturedTenant != cfg.Nacos.NamespaceID {
		t.Fatalf("expected tenant %q, got %q", cfg.Nacos.NamespaceID, capturedTenant)
	}
}

func TestResolve_NacosNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "config data not found", http.StatusNotFound)
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		Endpoint: "http://static.example.com:8081",
		Nacos: NacosConfig{
			ServerAddr: server.URL,
			DataID:     "gateway-endpoint",
			Group:      "DEFAULT_GROUP",
			TimeoutMs:  5000,
		},
	}

	_, err := resolver.Resolve(cfg)
	if err == nil {
		t.Fatal("expected error when Nacos returns 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected error to contain 404, got: %v", err)
	}
}

func TestResolve_NacosEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "   ")
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		Endpoint: "http://static.example.com:8081",
		Nacos: NacosConfig{
			ServerAddr: server.URL,
			DataID:     "gateway-endpoint",
			Group:      "DEFAULT_GROUP",
			TimeoutMs:  5000,
		},
	}

	_, err := resolver.Resolve(cfg)
	if err == nil {
		t.Fatal("expected error when Nacos returns empty endpoint")
	}
	if !strings.Contains(err.Error(), "empty endpoint") {
		t.Fatalf("expected error to mention empty endpoint, got: %v", err)
	}
}

func TestResolve_NacosDefaultTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "http://resolved.example.com:8081")
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		Endpoint: "http://static.example.com:8081",
		Nacos: NacosConfig{
			ServerAddr: server.URL,
			DataID:     "gateway-endpoint",
			Group:      "DEFAULT_GROUP",
			TimeoutMs:  0,
		},
	}

	_, err := resolver.Resolve(cfg)
	if err != nil {
		t.Fatalf("unexpected error with default timeout: %v", err)
	}
}

func TestResolve_NacosServerAddrVariants(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "http://resolved.example.com:8081")
	}))
	defer server.Close()

	// httptest always exposes an explicit port, so we can only verify that
	// the known-good URL is accepted. The real default-port logic is exercised
	// by integration; here we ensure the resolver handles the supplied host
	// without mangling it.
	cases := []struct {
		name       string
		serverAddr string
	}{
		{name: "host_port", serverAddr: server.URL},
		{name: "http_scheme_host_port", serverAddr: strings.TrimPrefix(server.URL, "http://")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewEndpointResolverWithClient(server.Client())
			cfg := &Config{
				Endpoint: "http://static.example.com:8081",
				Nacos: NacosConfig{
					ServerAddr: tc.serverAddr,
					DataID:     "gateway-endpoint",
					Group:      "DEFAULT_GROUP",
					TimeoutMs:  1000,
				},
			}

			_, err := resolver.Resolve(cfg)
			if err != nil {
				t.Fatalf("unexpected error for serverAddr %q: %v", tc.serverAddr, err)
			}
		})
	}
}

func TestResolve_NacosInvalidEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		response string
	}{
		{name: "missing scheme", response: "gateway.example.com"},
		{name: "invalid scheme", response: "ftp://gateway.example.com"},
		{name: "missing host", response: "http://"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			resolver := NewEndpointResolverWithClient(server.Client())
			cfg := &Config{
				Endpoint: "http://static.example.com:8081",
				Nacos: NacosConfig{
					ServerAddr: server.URL,
					DataID:     "gateway-endpoint",
					Group:      "DEFAULT_GROUP",
					TimeoutMs:  5000,
				},
			}

			_, err := resolver.Resolve(cfg)
			if err == nil {
				t.Fatal("expected error for invalid endpoint")
			}
		})
	}
}

func TestResolve_NacosAuthParams(t *testing.T) {
	var captured url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "http://resolved.example.com:8081")
	}))
	defer server.Close()

	resolver := NewEndpointResolverWithClient(server.Client())
	cfg := &Config{
		Endpoint: "http://static.example.com:8081",
		Nacos: NacosConfig{
			ServerAddr:  server.URL,
			DataID:      "gateway-endpoint",
			Group:       "DEFAULT_GROUP",
			TimeoutMs:   5000,
			Username:    "user",
			Password:    "pass",
			AccessToken: "token123",
		},
	}

	_, err := resolver.Resolve(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := captured.Get("username"); got != "user" {
		t.Fatalf("expected username %q, got %q", "user", got)
	}
	if got := captured.Get("password"); got != "pass" {
		t.Fatalf("expected password %q, got %q", "pass", got)
	}
	if got := captured.Get("accessToken"); got != "token123" {
		t.Fatalf("expected accessToken %q, got %q", "token123", got)
	}
}

func TestResolve_NacosDefaultPort(t *testing.T) {
	transport := &captureTransport{}
	resolver := NewEndpointResolverWithClient(&http.Client{Transport: transport}).(*defaultEndpointResolver)
	cfg := &Config{
		Endpoint: "http://static.example.com:8081",
		Nacos: NacosConfig{
			ServerAddr: "nacos.example.com",
			DataID:     "gateway-endpoint",
			Group:      "DEFAULT_GROUP",
			TimeoutMs:  1000,
		},
	}

	_, err := resolver.Resolve(cfg)
	if err == nil {
		t.Fatal("expected error because captureTransport does not return a response")
	}

	gotURL := transport.lastURL
	if gotURL == "" {
		t.Fatal("expected a request URL to be captured")
	}
	if !strings.Contains(gotURL, "nacos.example.com:8848") {
		t.Fatalf("expected default port 8848 in URL %q", gotURL)
	}
}

type captureTransport struct {
	lastURL string
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.lastURL = req.URL.String()
	return nil, fmt.Errorf("intentional transport failure")
}
