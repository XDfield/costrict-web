package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/teamdir"
	"github.com/gin-gonic/gin"
)

// newGinCtx carries a fake Casdoor JWT in Authorization, mirroring how
// kb/ensure sees the request after middleware.RequireAuth.
func newGinCtx() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer test-jwt")
	return c
}

// stubBackend mounts the given status+body at /api/workspaces so we can
// drive the real *teamdir.Client through HTTP. Asserts the JWT is
// forwarded as Authorization: Bearer.
func stubBackend(t *testing.T, status int, body string) *teamdir.Client {
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
	return teamdir.NewClient(teamdir.Config{BaseURL: srv.URL, TimeoutSec: 5})
}

func TestTeamDirectoryResolver_HappyPath(t *testing.T) {
	cli := stubBackend(t, http.StatusOK,
		`[{"id":"tid-1","name":"Platform","slug":"platform"},`+
			`{"id":"tid-2","name":"Mobile"}]`)
	r := &TeamDirectoryResolver{Client: cli}
	teams, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
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

func TestTeamDirectoryResolver_EmptyList_NotError(t *testing.T) {
	cli := stubBackend(t, http.StatusOK, `[]`)
	r := &TeamDirectoryResolver{Client: cli}
	teams, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
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

func TestTeamDirectoryResolver_503_MapsToSentinel(t *testing.T) {
	cli := stubBackend(t, http.StatusServiceUnavailable,
		`{"error":"org-team-service down"}`)
	r := &TeamDirectoryResolver{Client: cli}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("expected ErrOrgTeamServiceUnavailable, got %v", err)
	}
}

func TestTeamDirectoryResolver_401_FailsClosed(t *testing.T) {
	// JWT rejected by backend — surface as service-unavailable to avoid
	// leaking auth state through kb/ensure's response shape.
	cli := stubBackend(t, http.StatusUnauthorized, `{"error":"invalid token"}`)
	r := &TeamDirectoryResolver{Client: cli}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("expected ErrOrgTeamServiceUnavailable on 401, got %v", err)
	}
}

func TestTeamDirectoryResolver_NilClient_ReturnsSentinel(t *testing.T) {
	r := &TeamDirectoryResolver{Client: nil}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("nil client must surface ErrOrgTeamServiceUnavailable, got %v", err)
	}
}

func TestTeamDirectoryResolver_NilReceiver_ReturnsSentinel(t *testing.T) {
	var r *TeamDirectoryResolver
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("nil receiver must surface ErrOrgTeamServiceUnavailable, got %v", err)
	}
}
