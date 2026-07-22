package gitsync

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// newBotService wires a Service whose clientFactory + botAccountClientFactory
// both return a *Client pointed at the supplied httptest.Server. Tests inject
// this to drive ProvisionBot / RevokeBot / RotateBot through real Client code
// paths without touching the network. Basic-auth credentials are populated
// because ProvisionBot / RotateBot hit the token-mint endpoint, which sits
// behind Gitea's reqBasicOrRevProxyAuth middleware.
func newBotService(t *testing.T, srvURL string) *Service {
	t.Helper()
	resolver := &stubGitResolver{cfg: &GitServerConfig{ServerID: "test", Kind: "gitea", Endpoint: srvURL, AdminToken: "tok"}}
	s := &Service{
		gitResolver: resolver,
		logger:      nil,
		botAccountClientFactory: func(GitServerConfig) *Client {
			return newClientWithHTTPCAndBasicAuth(srvURL, "tok", "admin", "pw", nil)
		},
	}
	return s
}

func TestProvisionBot_HappyPath_CreatesUserAddsToOwnersMintsToken(t *testing.T) {
	var gotPaths []string
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/users/bot-t-abc12345": func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, "GET user")
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
		"POST /api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, "POST user")
			respondJSON(t, w, http.StatusCreated, GiteaUser{ID: 200, Login: "bot-t-abc12345"})
		},
		"GET /api/v1/orgs/t-abc12345/teams": func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, "GET teams")
			respondJSON(t, w, http.StatusOK, []GiteaTeam{{ID: 7, Name: "Owners"}})
		},
		"PUT /api/v1/teams/7/members/bot-t-abc12345": func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, "PUT member")
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /api/v1/users/bot-t-abc12345/tokens": func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, "POST token")
			respondJSON(t, w, http.StatusCreated, GiteaToken{ID: 999, TokenPlaintext: "tok-XYZ"})
		},
	})
	s := newBotService(t, srv.URL)

	creds, err := s.ProvisionBot(context.Background(), "default", "team-1", "abc12345", "t-abc12345")
	if err != nil {
		t.Fatalf("ProvisionBot: %v", err)
	}
	if creds.GiteaUsername != "bot-t-abc12345" {
		t.Errorf("username: got %q", creds.GiteaUsername)
	}
	if creds.GiteaUserID != 200 || creds.GiteaTokenID != 999 {
		t.Errorf("ids: %+v", creds)
	}
	if creds.TokenPlaintext != "tok-XYZ" {
		t.Errorf("token: got %q", creds.TokenPlaintext)
	}
	if creds.TokenSHA256 == "" || len(creds.TokenSHA256) != 64 {
		t.Errorf("sha256: got %q (want 64 hex chars)", creds.TokenSHA256)
	}
	// Sanity: all expected paths hit.
	for _, want := range []string{"GET user", "POST user", "GET teams", "PUT member", "POST token"} {
		if !contains(gotPaths, want) {
			t.Errorf("missing path step %q; got %v", want, gotPaths)
		}
	}
}

func TestProvisionBot_ReusesExistingUser(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/users/bot-t-abc12345": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, GiteaUser{ID: 200, Login: "bot-t-abc12345"})
		},
		"GET /api/v1/orgs/t-abc12345/teams": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, []GiteaTeam{{ID: 7, Name: "Owners"}})
		},
		"PUT /api/v1/teams/7/members/bot-t-abc12345": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /api/v1/users/bot-t-abc12345/tokens": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusCreated, GiteaToken{ID: 1000, TokenPlaintext: "fresh-tok"})
		},
	})
	s := newBotService(t, srv.URL)

	creds, err := s.ProvisionBot(context.Background(), "default", "team-1", "abc12345", "t-abc12345")
	if err != nil {
		t.Fatalf("ProvisionBot: %v", err)
	}
	if creds.GiteaUserID != 200 {
		t.Errorf("user id: got %d, want 200 (reused)", creds.GiteaUserID)
	}
}

func TestProvisionBot_MissingArgs(t *testing.T) {
	srv := newDispatchServer(t, dispatch{})
	s := newBotService(t, srv.URL)
	cases := []struct{ tenant, team, short, org string }{
		{"", "team-1", "abc12345", "t-abc12345"},
		{"default", "", "abc12345", "t-abc12345"},
		{"default", "team-1", "", "t-abc12345"},
		{"default", "team-1", "abc12345", ""},
	}
	for i, c := range cases {
		if _, err := s.ProvisionBot(context.Background(), c.tenant, c.team, c.short, c.org); !errors.Is(err, ErrBotAccountMissing) {
			t.Errorf("case %d: got %v, want ErrBotAccountMissing", i, err)
		}
	}
}

func TestRevokeBot_IdempotentOn404(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"DELETE /api/v1/users/bot-t-abc12345/tokens/42": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	s := newBotService(t, srv.URL)
	if err := s.RevokeBot(context.Background(), "default", "bot-t-abc12345", 42); err != nil {
		t.Errorf("RevokeBot 404 should be nil, got %v", err)
	}
}

func TestRevokeBot_MissingArgs(t *testing.T) {
	srv := newDispatchServer(t, dispatch{})
	s := newBotService(t, srv.URL)
	if err := s.RevokeBot(context.Background(), "default", "", 1); !errors.Is(err, ErrBotAccountMissing) {
		t.Errorf("empty bot: got %v, want ErrBotAccountMissing", err)
	}
	if err := s.RevokeBot(context.Background(), "default", "bot", 0); !errors.Is(err, ErrBotAccountMissing) {
		t.Errorf("zero token id: got %v, want ErrBotAccountMissing", err)
	}
}

func TestRotateBot_MintsNewAndRevokesOld(t *testing.T) {
	var calls []string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/users/bot-t-abc12345/tokens": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "mint")
			respondJSON(t, w, http.StatusCreated, GiteaToken{ID: 1001, TokenPlaintext: "new-tok"})
		},
		"DELETE /api/v1/users/bot-t-abc12345/tokens/999": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "revoke")
			w.WriteHeader(http.StatusNoContent)
		},
		"GET /api/v1/users/bot-t-abc12345": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "lookup")
			respondJSON(t, w, http.StatusOK, GiteaUser{ID: 200, Login: "bot-t-abc12345"})
		},
	})
	s := newBotService(t, srv.URL)

	creds, err := s.RotateBot(context.Background(), "default", "bot-t-abc12345", 999)
	if err != nil {
		t.Fatalf("RotateBot: %v", err)
	}
	if creds.GiteaTokenID != 1001 || creds.TokenPlaintext != "new-tok" {
		t.Errorf("got %+v", creds)
	}
	// Sanity: both mint and revoke happened.
	if !contains(calls, "mint") {
		t.Error("mint not called")
	}
	if !contains(calls, "revoke") {
		t.Error("revoke not called")
	}
}

func TestProvisionBot_OwnersTeamMissingFails(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/users/bot-t-abc12345": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
		"POST /api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusCreated, GiteaUser{ID: 200, Login: "bot-t-abc12345"})
		},
		"GET /api/v1/orgs/t-abc12345/teams": func(w http.ResponseWriter, r *http.Request) {
			// Empty team list — no Owners team.
			respondJSON(t, w, http.StatusOK, []GiteaTeam{})
		},
	})
	s := newBotService(t, srv.URL)
	_, err := s.ProvisionBot(context.Background(), "default", "team-1", "abc12345", "t-abc12345")
	if err == nil || !strings.Contains(err.Error(), "Owners") {
		t.Errorf("got %v, want error mentioning Owners", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
