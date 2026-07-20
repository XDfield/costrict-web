package giteasync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewClient_DisabledOnEmptyConfig verifies the feature-flag contract:
// empty baseURL or admin token returns nil Client so the boot wiring can
// treat nil as "feature disabled, skip provisioning entirely".
func TestNewClient_DisabledOnEmptyConfig(t *testing.T) {
	t.Parallel()
	if c := NewClient("", "tok"); c != nil {
		t.Errorf("NewClient with empty baseURL: got non-nil, want nil")
	}
	if c := NewClient("https://gitea.example.com", ""); c != nil {
		t.Errorf("NewClient with empty token: got non-nil, want nil")
	}
	if c := NewClient("https://gitea.example.com/", "tok"); c == nil {
		t.Errorf("NewClient with valid args: got nil, want non-nil")
	} else if c.baseURL != "https://gitea.example.com" {
		t.Errorf("baseURL trailing slash not stripped: got %q", c.baseURL)
	}
}

// stubGiteaServer returns a httptest.Server whose handler writes the given
// status + body on every request and records the request body for tests
// that want to assert on the Gitea-bound payload.
func stubGiteaServer(t *testing.T, status int, respBody string) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	return srv, &gotAuth, &gotBody
}

// TestProvisionGiteaUser_HappyPath verifies the 201 path returns a GiteaUser
// with ID populated, and that the Authorization header carries the admin
// token in Gitea's expected "token <PAT>" form.
func TestProvisionGiteaUser_HappyPath(t *testing.T) {
	t.Parallel()
	srv, gotAuth, gotBody := stubGiteaServer(t, http.StatusCreated,
		`{"id":42,"login":"u-alice","email":"alice@example.com"}`)
	defer srv.Close()

	c := NewClient(srv.URL, "secret-pat")
	u, err := c.ProvisionGiteaUser(context.Background(), GiteaUserParams{
		Username: "u-alice",
		Email:    "alice@example.com",
		Password: "throwaway-pw-12345",
		SourceID: 0,
	})
	if err != nil {
		t.Fatalf("ProvisionGiteaUser: %v", err)
	}
	if u.ID != 42 {
		t.Errorf("ID: got %d, want 42", u.ID)
	}
	if u.Username != "u-alice" {
		t.Errorf("Username: got %q, want u-alice", u.Username)
	}
	if *gotAuth != "token secret-pat" {
		t.Errorf("Authorization header: got %q, want 'token secret-pat'", *gotAuth)
	}
	if !strings.Contains(*gotBody, `"username":"u-alice"`) {
		t.Errorf("request body missing username: %s", *gotBody)
	}
	if !strings.Contains(*gotBody, `"must_change_password":false`) {
		t.Errorf("request body missing must_change_password=false: %s", *gotBody)
	}
}

// TestProvisionGiteaUser_409UserExists verifies the recovery sentinel — a
// 409 surfaces as ErrGiteaUserExists so the Service layer can switch to
// LookupUserByName + mark binding synced.
func TestProvisionGiteaUser_409UserExists(t *testing.T) {
	t.Parallel()
	srv, _, _ := stubGiteaServer(t, http.StatusConflict, `{"message":"user already exists"}`)
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.ProvisionGiteaUser(context.Background(), GiteaUserParams{
		Username: "u-bob", Email: "bob@example.com", Password: "pw",
	})
	if !errors.Is(err, ErrGiteaUserExists) {
		t.Errorf("err: got %v, want ErrGiteaUserExists", err)
	}
}

// TestProvisionGiteaUser_401Unauthorized verifies the config-error sentinel
// — 401/403 surfaces as ErrGiteaUnauthorized so ops see a distinct alert
// (not lumped with transient network failures).
func TestProvisionGiteaUser_401Unauthorized(t *testing.T) {
	t.Parallel()
	srv, _, _ := stubGiteaServer(t, http.StatusUnauthorized, `{"message":"bad token"}`)
	defer srv.Close()

	c := NewClient(srv.URL, "wrong-token")
	_, err := c.ProvisionGiteaUser(context.Background(), GiteaUserParams{
		Username: "u-bob", Email: "bob@example.com", Password: "pw",
	})
	if !errors.Is(err, ErrGiteaUnauthorized) {
		t.Errorf("err: got %v, want ErrGiteaUnauthorized", err)
	}
}

// TestProvisionGiteaUser_ServerUnreachable verifies the network-failure
// path — closing the server mid-test produces a dial error that wraps
// ErrGiteaUnreachable.
func TestProvisionGiteaUser_ServerUnreachable(t *testing.T) {
	t.Parallel()
	srv, _, _ := stubGiteaServer(t, http.StatusOK, "")
	srv.Close() // close before request → dial fails

	c := NewClient(srv.URL, "tok")
	_, err := c.ProvisionGiteaUser(context.Background(), GiteaUserParams{
		Username: "u-bob", Email: "bob@example.com", Password: "pw",
	})
	if !errors.Is(err, ErrGiteaUnreachable) {
		t.Errorf("err: got %v, want ErrGiteaUnreachable", err)
	}
}

// TestProvisionGiteaUser_CtxTimeout verifies the ctx-deadline path
// surfaces as ErrGiteaTimeout (distinct from generic network failure so
// the Service layer can keep the binding in 'pending' rather than 'error').
func TestProvisionGiteaUser_CtxTimeout(t *testing.T) {
	t.Parallel()
	// Handler that never responds — sleeps past the caller's ctx deadline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := c.ProvisionGiteaUser(ctx, GiteaUserParams{
		Username: "u-bob", Email: "bob@example.com", Password: "pw",
	})
	if !errors.Is(err, ErrGiteaTimeout) {
		t.Errorf("err: got %v, want ErrGiteaTimeout", err)
	}
}

// TestLookupUserByName_HappyPath verifies the 409-recovery path's GET
// returns a GiteaUser with ID populated.
func TestLookupUserByName_HappyPath(t *testing.T) {
	t.Parallel()
	srv, gotAuth, _ := stubGiteaServer(t, http.StatusOK,
		`{"id":77,"login":"u-carol","email":"carol@example.com"}`)
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	u, err := c.LookupUserByName(context.Background(), "u-carol")
	if err != nil {
		t.Fatalf("LookupUserByName: %v", err)
	}
	if u.ID != 77 {
		t.Errorf("ID: got %d, want 77", u.ID)
	}
	if *gotAuth != "token tok" {
		t.Errorf("Authorization: got %q, want 'token tok'", *gotAuth)
	}
}

// TestLookupUserByName_404NotFound verifies the missing-user path.
func TestLookupUserByName_404NotFound(t *testing.T) {
	t.Parallel()
	srv, _, _ := stubGiteaServer(t, http.StatusNotFound, `{"message":"user not found"}`)
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.LookupUserByName(context.Background(), "ghost")
	if !errors.Is(err, ErrGiteaNotFound) {
		t.Errorf("err: got %v, want ErrGiteaNotFound", err)
	}
}

// TestProvisionGiteaUser_MissingParams is a guard against caller
// programming errors — empty username / email / password returns an error
// without hitting the network.
func TestProvisionGiteaUser_MissingParams(t *testing.T) {
	t.Parallel()
	c := NewClient("https://gitea.example.com", "tok")
	cases := []struct {
		name string
		p    GiteaUserParams
	}{
		{"empty username", GiteaUserParams{Email: "a@b.c", Password: "pw"}},
		{"empty email", GiteaUserParams{Username: "u", Password: "pw"}},
		{"empty password", GiteaUserParams{Username: "u", Email: "a@b.c"}},
	}
	for _, tc := range cases {
		if _, err := c.ProvisionGiteaUser(context.Background(), tc.p); err == nil {
			t.Errorf("%s: got nil err, want non-nil", tc.name)
		}
	}
}
