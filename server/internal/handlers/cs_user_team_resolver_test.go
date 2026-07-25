package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// stubRPCServer mounts a single response handler at /api/workspaces so we
// can drive the real RPCClient through its HTTP path without spinning up
// multica. It asserts the caller forwarded the Casdoor JWT via
// Authorization: Bearer.
func stubRPCServer(t *testing.T, status int, body string) (*user.RPCClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces" {
			t.Errorf("unexpected path %q, want /api/workspaces", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
			t.Errorf("missing/invalid Authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cli := user.NewRPCClient(config.UserServiceConfig{BaseURL: srv.URL, InternalToken: "tok", TimeoutSec: 5})
	return cli, srv
}

// newGinCtx builds a gin.Context carrying a fake Casdoor JWT in the
// Authorization header, mirroring how kb/ensure's handler sees the
// request after middleware.RequireAuth.
func newGinCtx() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer test-jwt")
	return c
}

func TestCSUserTeamResolver_HappyPath_MapsFields(t *testing.T) {
	// multica returns a top-level array of workspace objects.
	cli, _ := stubRPCServer(t, http.StatusOK,
		`[{"id":"tid-1","name":"Platform","slug":"platform"},`+
			`{"id":"tid-2","name":"Mobile","slug":"mobile"}]`)
	r := &CSUserTeamResolver{Client: cli}
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
	// Role is intentionally blank — multica's /api/workspaces doesn't
	// surface membership role.
	if teams[0].Role != "" {
		t.Errorf("team[0].role = %q, want empty", teams[0].Role)
	}
}

func TestCSUserTeamResolver_EmptyList_NotError(t *testing.T) {
	// Empty list is the legitimate "no team" state — KBEnsure maps to 403.
	cli, _ := stubRPCServer(t, http.StatusOK, `[]`)
	r := &CSUserTeamResolver{Client: cli}
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

func TestCSUserTeamResolver_503_MapsToSentinel(t *testing.T) {
	cli, _ := stubRPCServer(t, http.StatusServiceUnavailable,
		`{"error":"org-team-service down","error_code":"ORG_TEAM_SERVICE_UNAVAILABLE"}`)
	r := &CSUserTeamResolver{Client: cli}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("expected ErrOrgTeamServiceUnavailable, got %v", err)
	}
}

func TestCSUserTeamResolver_401_MapsToSentinel(t *testing.T) {
	// JWT rejected by multica → fail closed as service-unavailable so
	// kb/ensure returns a single 503 code instead of leaking auth state.
	cli, _ := stubRPCServer(t, http.StatusUnauthorized, `{"error":"invalid token"}`)
	r := &CSUserTeamResolver{Client: cli}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("expected ErrOrgTeamServiceUnavailable on 401, got %v", err)
	}
}

func TestCSUserTeamResolver_TransportError_Propagates(t *testing.T) {
	// Point at a non-routable port to force transport error.
	cli := user.NewRPCClient(config.UserServiceConfig{BaseURL: "http://127.0.0.1:1", InternalToken: "tok", TimeoutSec: 1})
	r := &CSUserTeamResolver{Client: cli}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if err == nil {
		t.Fatalf("expected transport err, got nil")
	}
	if errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Errorf("transport failure must NOT mask as service-unavailable")
	}
}

func TestCSUserTeamResolver_NilClient_ReturnsSentinel(t *testing.T) {
	r := &CSUserTeamResolver{Client: nil}
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("nil client must surface ErrOrgTeamServiceUnavailable, got %v", err)
	}
}

func TestCSUserTeamResolver_NilReceiver_ReturnsSentinel(t *testing.T) {
	var r *CSUserTeamResolver
	_, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if !errors.Is(err, ErrOrgTeamServiceUnavailable) {
		t.Fatalf("nil receiver must surface ErrOrgTeamServiceUnavailable, got %v", err)
	}
}

// Compile-time guarantee the test's context usage doesn't get pruned.
var _ = context.Background
