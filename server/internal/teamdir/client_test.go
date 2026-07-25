package teamdir

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func stubServer(t *testing.T, status int, body string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces" {
			t.Errorf("unexpected path %q, want /api/workspaces", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
			t.Errorf("missing/invalid Authorization: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(Config{BaseURL: srv.URL, TimeoutSec: 5}), srv
}

func TestListUserTeams_HappyPath(t *testing.T) {
	cli, _ := stubServer(t, http.StatusOK,
		`[{"id":"tid-1","name":"Platform","slug":"platform"},`+
			`{"id":"tid-2","name":"Mobile"}]`)
	teams, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("len=%d, want 2", len(teams))
	}
	if teams[0].TeamID != "tid-1" || teams[0].DisplayName != "Platform" {
		t.Errorf("team[0] mismatch: %+v", teams[0])
	}
}

func TestListUserTeams_EmptyList_NotError(t *testing.T) {
	cli, _ := stubServer(t, http.StatusOK, `[]`)
	teams, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if teams == nil {
		t.Fatalf("expected non-nil empty slice")
	}
	if len(teams) != 0 {
		t.Errorf("len=%d, want 0", len(teams))
	}
}

func TestListUserTeams_NullBody_EmptySlice(t *testing.T) {
	// Backends sometimes emit null instead of []; must not panic.
	cli, _ := stubServer(t, http.StatusOK, `null`)
	teams, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if teams == nil {
		t.Fatalf("expected non-nil empty slice")
	}
}

func TestListUserTeams_401_FailClosed(t *testing.T) {
	cli, _ := stubServer(t, http.StatusUnauthorized, `{"error":"invalid token"}`)
	_, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable on 401, got %v", err)
	}
}

func TestListUserTeams_503(t *testing.T) {
	cli, _ := stubServer(t, http.StatusServiceUnavailable,
		`{"error":"org-team-service down"}`)
	_, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

func TestListUserTeams_EmptyToken(t *testing.T) {
	cli, _ := stubServer(t, http.StatusOK, `[]`)
	_, err := cli.ListUserTeams(context.Background(), "")
	if !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("expected ErrEmptyToken, got %v", err)
	}
}

func TestListUserTeams_NotConfigured(t *testing.T) {
	cli := NewClient(Config{}) // empty BaseURL
	_, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("expected ErrServiceUnavailable, got %v", err)
	}
}

func TestListUserTeams_TransportError(t *testing.T) {
	// Non-routable port → transport error, must propagate (not masked
	// as ErrServiceUnavailable — that would conflate network failure
	// with backend rejection).
	cli := NewClient(Config{BaseURL: "http://127.0.0.1:1", TimeoutSec: 1})
	_, err := cli.ListUserTeams(context.Background(), "test-jwt")
	if err == nil {
		t.Fatalf("expected transport err")
	}
	if errors.Is(err, ErrServiceUnavailable) {
		t.Errorf("transport failure must NOT mask as ErrServiceUnavailable")
	}
}
