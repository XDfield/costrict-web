package gitsync

import (
	"testing"
)

func TestDefaultGitServerFactory_KindGitea(t *testing.T) {
	cfg := GitServerConfig{
		ServerID:   "srv-1",
		Kind:       GitServerKindGitea,
		Endpoint:   "http://127.0.0.1:3000",
		AdminToken: "tok-abc",
	}
	gs := DefaultGitServerFactory(cfg)
	if gs == nil {
		t.Fatal("expected non-nil GitServer for kind=gitea")
	}
	if _, ok := gs.(*Client); !ok {
		t.Fatalf("expected *Client impl, got %T", gs)
	}
}

func TestDefaultGitServerFactory_EmptyKindBackwardCompat(t *testing.T) {
	// Pre-Kind tenants: RPC returned Kind="" — must still resolve to Gitea.
	cfg := GitServerConfig{
		ServerID:   "srv-1",
		Kind:       "",
		Endpoint:   "http://127.0.0.1:3000",
		AdminToken: "tok-abc",
	}
	gs := DefaultGitServerFactory(cfg)
	if gs == nil {
		t.Fatal("expected non-nil GitServer for empty Kind (backward compat)")
	}
}

func TestDefaultGitServerFactory_UnknownKind(t *testing.T) {
	cfg := GitServerConfig{
		ServerID:   "srv-1",
		Kind:       "gitlab", // not yet implemented
		Endpoint:   "https://gitlab.example.com",
		AdminToken: "tok-abc",
	}
	gs := DefaultGitServerFactory(cfg)
	if gs != nil {
		t.Fatalf("expected nil GitServer for unknown Kind, got %T", gs)
	}
}

func TestDefaultGitServerFactory_EmptyEndpoint(t *testing.T) {
	// NewClient returns nil for empty baseURL/token; factory must propagate
	// so caller surfaces ErrTenantGitServerUnresolved rather than panic on
	// a nil method call.
	cfg := GitServerConfig{
		ServerID:   "srv-1",
		Kind:       GitServerKindGitea,
		Endpoint:   "",
		AdminToken: "tok-abc",
	}
	if DefaultGitServerFactory(cfg) != nil {
		t.Fatal("expected nil for empty Endpoint")
	}
}

func TestDefaultGitServerFactory_EmptyToken(t *testing.T) {
	cfg := GitServerConfig{
		ServerID:   "srv-1",
		Kind:       GitServerKindGitea,
		Endpoint:   "http://127.0.0.1:3000",
		AdminToken: "",
	}
	if DefaultGitServerFactory(cfg) != nil {
		t.Fatal("expected nil for empty AdminToken")
	}
}
