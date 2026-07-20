package gitsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubGiteaServer spins up an httptest.Server whose handler is switched
// per-test via the mutate function. Tests assert on captured method/path.
func stubGiteaServer(t *testing.T, status int, respBody string) (*httptest.Server, *http.Request, chan struct{}) {
	t.Helper()
	var captured http.Request
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = *r
		done <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	return srv, &captured, done
}

func TestClient_NewClient_EmptyConfigReturnsNil(t *testing.T) {
	if c := NewClient("", "token"); c != nil {
		t.Errorf("expected nil client for empty baseURL, got %v", c)
	}
	if c := NewClient("https://gitea.example.com", ""); c != nil {
		t.Errorf("expected nil client for empty token, got %v", c)
	}
}

func TestClient_ListTeamMembers_HappyPath(t *testing.T) {
	body := `[{"id":1,"login":"alice","email":"a@x.com"},{"id":2,"login":"bob","email":"b@x.com"}]`
	srv, captured, _ := stubGiteaServer(t, http.StatusOK, body)
	defer srv.Close()

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	members, err := c.ListTeamMembers(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].Login != "alice" || members[1].Login != "bob" {
		t.Errorf("unexpected logins: %+v", members)
	}
	if captured.Method != http.MethodGet {
		t.Errorf("expected GET, got %s", captured.Method)
	}
	if !strings.HasSuffix(captured.URL.Path, "/api/v1/teams/42/members") {
		t.Errorf("unexpected path: %s", captured.URL.Path)
	}
	if captured.Header.Get("Authorization") != "token tok" {
		t.Errorf("expected token auth header, got %q", captured.Header.Get("Authorization"))
	}
}

func TestClient_AddTeamMember_PutRequest(t *testing.T) {
	srv, captured, _ := stubGiteaServer(t, http.StatusNoContent, "")
	defer srv.Close()

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	if err := c.AddTeamMember(context.Background(), 7, "carol"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.Method != http.MethodPut {
		t.Errorf("expected PUT, got %s", captured.Method)
	}
	if !strings.HasSuffix(captured.URL.Path, "/api/v1/teams/7/members/carol") {
		t.Errorf("unexpected path: %s", captured.URL.Path)
	}
}

func TestClient_RemoveTeamMember_DeleteRequest(t *testing.T) {
	srv, captured, _ := stubGiteaServer(t, http.StatusNoContent, "")
	defer srv.Close()

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	if err := c.RemoveTeamMember(context.Background(), 7, "carol"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.Method != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", captured.Method)
	}
	if !strings.HasSuffix(captured.URL.Path, "/api/v1/teams/7/members/carol") {
		t.Errorf("unexpected path: %s", captured.URL.Path)
	}
}

func TestClient_ListTeamMembers_404ReturnsTeamNotFound(t *testing.T) {
	srv, _, _ := stubGiteaServer(t, http.StatusNotFound, `{"message":"team not found"}`)
	defer srv.Close()

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	_, err := c.ListTeamMembers(context.Background(), 9999)
	if !errors.Is(err, ErrGiteaTeamNotFound) {
		t.Errorf("expected ErrGiteaTeamNotFound, got %v", err)
	}
}

func TestClient_AddTeamMember_401ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := stubGiteaServer(t, http.StatusUnauthorized, `{"message":"bad token"}`)
	defer srv.Close()

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	err := c.AddTeamMember(context.Background(), 7, "carol")
	if !errors.Is(err, ErrGiteaUnauthorized) {
		t.Errorf("expected ErrGiteaUnauthorized, got %v", err)
	}
}

func TestClient_ServerClosedReturnsUnreachable(t *testing.T) {
	srv, _, _ := stubGiteaServer(t, http.StatusOK, "[]")
	srv.Close() // shut before use

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	_, err := c.ListTeamMembers(context.Background(), 1)
	if !errors.Is(err, ErrGiteaUnreachable) {
		t.Errorf("expected ErrGiteaUnreachable, got %v", err)
	}
}

func TestClient_CtxDeadlineReturnsTimeout(t *testing.T) {
	srv, _, _ := stubGiteaServer(t, http.StatusOK, "[]")
	defer srv.Close()

	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	// Block until call cancelled before issuing — use a sub-200ms deadline
	// to keep the test fast but still trip ErrGiteaTimeout.
	// We use a server that sleeps past the deadline instead.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ListTeamMembers(ctx, 1)
	if !errors.Is(err, ErrGiteaTimeout) {
		t.Errorf("expected ErrGiteaTimeout, got %v", err)
	}
}

func TestClient_NilClientReturnsUnreachable(t *testing.T) {
	var c *Client
	_, err := c.ListTeamMembers(context.Background(), 1)
	if !errors.Is(err, ErrGiteaUnreachable) {
		t.Errorf("expected ErrGiteaUnreachable, got %v", err)
	}
}

func TestClient_InvalidTeamIDReturnsError(t *testing.T) {
	c := newClientWithHTTPC("https://x.example.com", "tok", nil)
	if err := c.AddTeamMember(context.Background(), 0, "carol"); err == nil {
		t.Errorf("expected error for team_id=0, got nil")
	}
	if err := c.AddTeamMember(context.Background(), 1, ""); err == nil {
		t.Errorf("expected error for empty username, got nil")
	}
}
