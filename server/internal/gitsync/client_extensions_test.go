package gitsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// dispatchServer routes per-path / per-method handlers so a single
// httptest.Server can stand in for the whole Gitea API surface in
// ProvisionBot-shaped tests.
type dispatch map[string]http.HandlerFunc

func newDispatchServer(t *testing.T, d dispatch) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		// Try exact match first.
		if h, ok := d[key]; ok {
			h(w, r)
			return
		}
		// Wildcard match: "METHOD /prefix/*" matches any path under /prefix/.
		for k, h := range d {
			if !strings.HasSuffix(k, "/*") {
				continue
			}
			prefix := strings.TrimSuffix(k, "/*") // "METHOD /prefix"
			// Split into method and path-prefix.
			sp := strings.IndexByte(prefix, ' ')
			if sp < 0 {
				continue
			}
			m, pathPrefix := prefix[:sp], prefix[sp+1:]
			if r.Method == m && strings.HasPrefix(r.URL.Path, pathPrefix+"/") {
				h(w, r)
				return
			}
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func respondJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func readBody(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// newClientWithHTTPCAndBasicAuth extends newClientWithHTTPC with admin user
// + password, required by token-mint endpoints (POST/DELETE /users/{name}/tokens).
func newClientWithHTTPCAndBasicAuth(baseURL, adminToken, adminUser, adminPassword string, hc *http.Client) *Client {
	c := newClientWithHTTPC(baseURL, adminToken, hc)
	if c == nil {
		return nil
	}
	c.adminUser = adminUser
	c.adminPassword = adminPassword
	return c
}

func TestClient_CreateUser_HappyPath(t *testing.T) {
	var capturedBody string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			respondJSON(t, w, http.StatusCreated, GiteaUser{ID: 100, Login: "bot-t-abc12345", Email: "bot+abc12345@costrict.internal"})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	u, err := c.CreateUser(context.Background(), CreateUserOptions{
		Login:    "bot-t-abc12345",
		Email:    "bot+abc12345@costrict.internal",
		FullName: "Bot for team abc12345",
		Password: "random-32-bytes",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID != 100 || u.Login != "bot-t-abc12345" {
		t.Errorf("got %+v", u)
	}
	if !strings.Contains(capturedBody, `"username":"bot-t-abc12345"`) {
		t.Errorf("body missing username: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"password":"random-32-bytes"`) {
		t.Errorf("body missing password: %s", capturedBody)
	}
}

func TestClient_CreateUser_UsernameConflictReturnsTaken(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"user already exists"}`, http.StatusConflict)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	_, err := c.CreateUser(context.Background(), CreateUserOptions{
		Login: "dup", Password: "x",
	})
	if !errors.Is(err, ErrGiteaUsernameTaken) {
		t.Errorf("got %v, want ErrGiteaUsernameTaken", err)
	}
}

func TestClient_GetUserByName_NotFoundReturnsGiteaNotFound(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/users/ghost": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"user not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	_, err := c.GetUserByName(context.Background(), "ghost")
	if !errors.Is(err, ErrGiteaTeamNotFound) {
		t.Errorf("got %v, want ErrGiteaTeamNotFound", err)
	}
}

func TestClient_CreateUserToken_HappyPath(t *testing.T) {
	var tokenIDCounter int64
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/users/bot-t-abc12345/tokens": func(w http.ResponseWriter, r *http.Request) {
			id := atomic.AddInt64(&tokenIDCounter, 1)
			respondJSON(t, w, http.StatusCreated, GiteaToken{
				ID:             id,
				Name:           "bot-pat",
				TokenPlaintext: " plaintext-token-XYZ",
			})
		},
	})
	c := newClientWithHTTPCAndBasicAuth(srv.URL, "tok", "admin", "pw", srv.Client())
	tok, err := c.CreateUserToken(context.Background(), "bot-t-abc12345", CreateUserTokenOptions{
		Name:   "bot-pat",
		Scopes: []string{"write:repository", "read:user"},
	})
	if err != nil {
		t.Fatalf("CreateUserToken: %v", err)
	}
	if tok.ID == 0 || tok.TokenPlaintext == "" {
		t.Errorf("got %+v", tok)
	}
}

func TestClient_DeleteUserToken_IdempotentOn404(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"DELETE /api/v1/users/bot-t-abc12345/tokens/999": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"token not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPCAndBasicAuth(srv.URL, "tok", "admin", "pw", srv.Client())
	if err := c.DeleteUserToken(context.Background(), "bot-t-abc12345", 999); err != nil {
		t.Errorf("DeleteUserToken on 404 should be nil (idempotent), got %v", err)
	}
}

// TestClient_CreateUserToken_MissingBasicAuth exercises the ErrGiteaBasicAuthRequired
// guard: a Client constructed with just an admin PAT (no admin user/password)
// cannot reach the token-mint endpoint and must surface a clear configuration
// error rather than an opaque 401.
func TestClient_CreateUserToken_MissingBasicAuth(t *testing.T) {
	c := newClientWithHTTPC("http://x", "tok", nil)
	_, err := c.CreateUserToken(context.Background(), "bot-x", CreateUserTokenOptions{Name: "n"})
	if !errors.Is(err, ErrGiteaBasicAuthRequired) {
		t.Errorf("got %v, want ErrGiteaBasicAuthRequired", err)
	}
}

func TestClient_CreateOrg_HappyPath(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/orgs": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusCreated, GiteaOrg{
				ID:       555,
				Name:     "t-abc12345",
				Username: "t-abc12345",
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	o, err := c.CreateOrg(context.Background(), CreateOrgOptions{
		Username:   "t-abc12345",
		FullName:   "Team abc12345",
		Visibility: "private",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if o.ID != 555 || o.Name != "t-abc12345" {
		t.Errorf("got %+v", o)
	}
}

func TestClient_CreateOrg_ConflictReturnsTaken(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/orgs": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"org already exists"}`, http.StatusConflict)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	_, err := c.CreateOrg(context.Background(), CreateOrgOptions{Username: "dup"})
	if !errors.Is(err, ErrGiteaUsernameTaken) {
		t.Errorf("got %v, want ErrGiteaUsernameTaken", err)
	}
}

func TestClient_ListOrgTeams_HappyPath(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/orgs/t-abc12345/teams": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, []GiteaTeam{
				{ID: 1, Name: "Owners", Permission: "admin"},
				{ID: 2, Name: "Members", Permission: "read"},
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())
	teams, err := c.ListOrgTeams(context.Background(), "t-abc12345")
	if err != nil {
		t.Fatalf("ListOrgTeams: %v", err)
	}
	if len(teams) != 2 || teams[0].Name != "Owners" {
		t.Errorf("got %+v", teams)
	}
}

func TestClient_CreateUser_RequiresLoginAndPassword(t *testing.T) {
	c := newClientWithHTTPC("http://x", "tok", nil)
	if _, err := c.CreateUser(context.Background(), CreateUserOptions{}); err == nil {
		t.Error("empty login/password: want error")
	}
}
