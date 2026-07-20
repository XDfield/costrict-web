// Package giteasync implements Phase E3a.1's Gitea user auto-provisioning.
//
// Two layers:
//
//   - Client (this file) — thin HTTP wrapper around Gitea admin API.
//     Establishes the outbound-HTTP pattern for cs-user (no prior net/http
//     client existed); mockable via the GiteaUserProvisioner interface.
//
//   - Service (service.go) — owns the binding-table state machine + the
//     best-effort "never-fail-the-signup" contract; called from
//     user.Service.GetOrCreateUser after a new user row commits.
//
// Design sources:
//   - USER_CENTER_DESIGN §11.1 (eager mode: 注册即创建 Gitea 账号)
//   - USER_CENTER_DESIGN §4.4 (user_gitea_binding table + state machine)
//   - ADR-3 v3 (TEAM_ORG_UNIFICATION): cs-user does user-level only;
//     team-level team_user sync belongs to @server.
package giteasync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors surfaced by Client. The Service layer switches on these
// to drive the binding-table state machine (e.g. ErrGiteaUserExists →
// lookup-then-mark-synced recovery path).
var (
	// ErrGiteaUserExists — HTTP 409 from POST /admin/users. Caller should
	// GET /users/{name} to recover the existing UID and mark binding synced.
	ErrGiteaUserExists = errors.New("giteasync: gitea user already exists")

	// ErrGiteaUnauthorized — HTTP 401/403. Admin token is wrong or missing
	// scope; this is a configuration error, not transient.
	ErrGiteaUnauthorized = errors.New("giteasync: unauthorized (check admin token)")

	// ErrGiteaUnreachable — network error / 5xx / malformed response.
	// Transient; reconciliation cron (E3a.2) will retry.
	ErrGiteaUnreachable = errors.New("giteasync: gitea unreachable")

	// ErrGiteaTimeout — ctx deadline exceeded before Gitea responded.
	// Transient; retry is safe (POST /admin/users is idempotent on username).
	ErrGiteaTimeout = errors.New("giteasync: gitea request timed out")

	// ErrGiteaNotFound — HTTP 404 from GET /users/{name}. Used by
	// LookupUserByName when the 409-recovery path finds nothing.
	ErrGiteaNotFound = errors.New("giteasync: gitea user not found")
)

// defaultTimeout caps a single Gitea API call when the caller's ctx has no
// deadline. The caller (giteasync.Service) always wraps with a 5s timeout,
// so this is defensive only.
const defaultTimeout = 10 * time.Second

// GiteaUserParams is the input shape for ProvisionGiteaUser. Field
// semantics:
//
//   - Username (required) — becomes Gitea login name. Must be globally
//     unique within the Gitea instance.
//   - Email (required) — Gitea requires it; set to the user's primary email.
//   - Password (required) — Gitea requires non-empty even when
//     must_change_password=false. The auto-provisioning flow never uses
//     password auth (Gitea JWT middleware is the auth path, E3a.3), so the
//     caller generates a high-entropy random string that is discarded.
//   - SourceID — 0 (local source). Future IdP-backed provisioning (LDAP /
//     OIDC sources configured in Gitea) sets this to the source's ID; out
//     of scope for E3a.1.
//   - MustChangePassword — false (we never want the user prompted; the
//     password is throwaway).
type GiteaUserParams struct {
	Username           string
	Email              string
	Password           string
	SourceID           int64
	MustChangePassword bool
}

// GiteaUser is the minimal slice of Gitea's user payload that the
// provisioning flow consumes (ID is the only load-bearing field — written
// to user_gitea_binding.gitea_uid).
type GiteaUser struct {
	ID       int64  `json:"id"`
	Username string `json:"login"`
	Email    string `json:"email"`
}

// GiteaUserProvisioner is the per-user provisioning surface the Service
// layer depends on. Declared as an interface so tests can inject a stub
// without spinning up a real Gitea instance.
type GiteaUserProvisioner interface {
	// ProvisionGiteaUser calls POST /admin/users.
	// Returns ErrGiteaUserExists on 409; caller does recovery via
	// LookupUserByName.
	ProvisionGiteaUser(ctx context.Context, p GiteaUserParams) (*GiteaUser, error)

	// LookupUserByName calls GET /users/{name} to recover UID for an
	// existing Gitea user (used by the 409 recovery path).
	LookupUserByName(ctx context.Context, username string) (*GiteaUser, error)
}

// Client is the production GiteaUserProvisioner. Construct via NewClient.
//
// Field exposure: baseURL / adminToken are read-only after construction
// (no setter); httpClient defaults to a stdlib http.Client but can be
// substituted in tests via the unexported newClientWithHTTPC constructor.
type Client struct {
	baseURL    string
	adminToken string
	httpClient *http.Client
}

// NewClient returns a Client bound to the supplied base URL and admin
// token. baseURL should be the Gitea root (e.g. https://gitea.example.com);
// the /api/v1 prefix is added internally. Trailing slashes are stripped.
//
// Empty baseURL or token returns nil — caller (cmd/api/main.go) treats nil
// as "feature disabled" and does not construct the Service.
func NewClient(baseURL, adminToken string) *Client {
	if baseURL == "" || adminToken == "" {
		return nil
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		adminToken: adminToken,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// ProvisionGiteaUser implements GiteaUserProvisioner.
func (c *Client) ProvisionGiteaUser(ctx context.Context, p GiteaUserParams) (*GiteaUser, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if p.Username == "" || p.Email == "" || p.Password == "" {
		return nil, fmt.Errorf("giteasync: username, email, and password are required")
	}

	body := struct {
		Username           string `json:"username"`
		Email              string `json:"email"`
		Password           string `json:"password"`
		MustChangePassword bool   `json:"must_change_password"`
		SourceID           int64  `json:"source_id"`
		LoginName          string `json:"login_name"`
		SendNotify         bool   `json:"send_notify"`
	}{
		Username:           p.Username,
		Email:              p.Email,
		Password:           p.Password,
		MustChangePassword: p.MustChangePassword,
		SourceID:           p.SourceID,
		SendNotify:         false,
	}

	resp, err := c.doJSON(ctx, http.MethodPost, "/api/v1/admin/users", body, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var u GiteaUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &u, nil
}

// LookupUserByName implements GiteaUserProvisioner.
func (c *Client) LookupUserByName(ctx context.Context, username string) (*GiteaUser, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if username == "" {
		return nil, fmt.Errorf("giteasync: username is required")
	}

	resp, err := c.doJSON(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(username), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var u GiteaUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &u, nil
}

// doJSON executes an authenticated JSON request against the Gitea API and
// returns the raw response on success. expectedStatus drives the error
// mapping; any other status code becomes ErrGiteaUnreachable (or
// ErrGiteaUnauthorized / ErrGiteaUserExists / ErrGiteaNotFound for the
// known non-2xx cases).
//
// body may be nil for GET requests.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, expectedStatus int) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("giteasync: marshal body: %w", err)
		}
		bodyReader = strings.NewReader(string(raw))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrGiteaUnreachable, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Gitea accepts either "Authorization: Basic base64(user:token)" or
	// "Authorization: token <token>" for admin PATs. The token form is
	// simpler and avoids embedding a username.
	req.Header.Set("Authorization", "token "+c.adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Distinguish ctx-deadline from generic network failure so the
		// service layer can choose different state-machine transitions
		// (timeout → keep pending; network → mark error).
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrGiteaTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrGiteaUnreachable, err)
	}

	// ctx cancelled mid-flight (response came back but caller gave up).
	if ctx.Err() != nil {
		_ = resp.Body.Close()
		return nil, ErrGiteaTimeout
	}

	if resp.StatusCode == expectedStatus {
		return resp, nil
	}

	// Drain + close on error paths so the connection can be reused.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnauthorized, resp.StatusCode, snippet)
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: status=409 body=%s", ErrGiteaUserExists, snippet)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: status=404 body=%s", ErrGiteaNotFound, snippet)
	default:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnreachable, resp.StatusCode, snippet)
	}
}
