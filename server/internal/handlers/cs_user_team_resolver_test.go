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

// stubRPCServer mounts a single response handler at
// /api/internal/users/:subject_id/teams so we can drive the real RPCClient
// through its HTTP path without spinning up cs-user.
func stubRPCServer(t *testing.T, status int, body string) (*user.RPCClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "tok" {
			t.Errorf("missing X-Internal-Token header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cli := user.NewRPCClient(config.UserServiceConfig{BaseURL: srv.URL, InternalToken: "tok", TimeoutSec: 5})
	return cli, srv
}

func newGinCtx() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c
}

func TestCSUserTeamResolver_HappyPath_MapsFields(t *testing.T) {
	cli, _ := stubRPCServer(t, http.StatusOK,
		`{"teams":[{"team_id":"tid-1","display_name":"Platform","role":"owner"},`+
			`{"team_id":"tid-2","display_name":"Mobile","role":"member"}]}`)
	r := &CSUserTeamResolver{Client: cli}
	teams, err := r.ResolveCurrentUserTeams(newGinCtx(), "user-1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("len=%d, want 2", len(teams))
	}
	if teams[0].TeamID != "tid-1" || teams[0].DisplayName != "Platform" || teams[0].Role != "owner" {
		t.Errorf("team[0] mismatch: %+v", teams[0])
	}
}

func TestCSUserTeamResolver_EmptyList_NotError(t *testing.T) {
	// Empty list is the legitimate "no team" state — KBEnsure maps to 403.
	cli, _ := stubRPCServer(t, http.StatusOK, `{"teams":[]}`)
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
