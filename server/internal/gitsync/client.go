// Package gitsync implements Phase E3b.1's @server-side Gitea team
// membership synchronization framework.
//
// Per ADR-3 v3 (TEAM_ORG_UNIFICATION), team-level Gitea operations belong
// to @server (cs-user owns only user-level provisioning, done in E3a.1).
// This package establishes the outbound HTTP client pattern for team ops
// on @server, mirroring the cs-user giteasync.GiteaClient shape.
//
// Three layers:
//
//   - Client (this file) — thin HTTP wrapper around Gitea team-member API.
//     Mockable via the GiteaTeamMemberAPI interface.
//
//   - Provider (provider.go) — source-of-truth for expected team membership.
//     StubTeamProvider returns hardcoded data for MVP; real providers
//     (cs-user team RPC, org-team-service webhook) swap behind the same
//     interface in future slices.
//
//   - Service (service.go) — owns the full-reconcile diff/apply loop and
//     the SyncResult shape returned to callers.
package gitsync

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
// to decide whether a sync failure is retryable (unreachable/timeout) or
// fatal (unauthorized/team-not-found).
var (
	// ErrGiteaTeamNotFound — HTTP 404 from a team endpoint. Either the
	// gitea_team_id mapping is wrong or the team was deleted out-of-band.
	ErrGiteaTeamNotFound = errors.New("gitsync: gitea team not found")

	// ErrGiteaUnauthorized — HTTP 401/403. Admin token is wrong or missing
	// scope; configuration error, not transient.
	ErrGiteaUnauthorized = errors.New("gitsync: unauthorized (check admin token)")

	// ErrGiteaUnreachable — network error / 5xx / malformed response.
	// Transient; reconciliation cron (E3b.2) will retry.
	ErrGiteaUnreachable = errors.New("gitsync: gitea unreachable")

	// ErrGiteaTimeout — ctx deadline exceeded before Gitea responded.
	// Transient; retry is safe (team-member ops are idempotent).
	ErrGiteaTimeout = errors.New("gitsync: gitea request timed out")

	// ErrGiteaBasicAuthRequired — the caller invoked a Gitea endpoint that
	// requires HTTP Basic auth (POST /users/{name}/tokens, etc.) without
	// supplying admin user/password. Configuration error, not transient.
	ErrGiteaBasicAuthRequired = errors.New("gitsync: basic auth credentials required for this endpoint (set git_server admin_user/admin_password)")
)

// defaultTimeout caps a single Gitea API call when the caller's ctx has no
// deadline. Defensive only — the Service layer wraps each sync with its
// own bounded timeout.
const defaultTimeout = 10 * time.Second

// GiteaMember is the minimal slice of Gitea's team-member payload that the
// sync loop consumes (only login is load-bearing — used as the add/remove
// key on PUT/DELETE /teams/:id/members/:username).
type GiteaMember struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
}

// GiteaTeamMemberAPI is the team-membership surface the Service layer
// depends on. Declared as an interface so tests can inject a stub without
// spinning up a real Gitea instance.
type GiteaTeamMemberAPI interface {
	// ListTeamMembers calls GET /teams/:id/members.
	ListTeamMembers(ctx context.Context, giteaTeamID int64) ([]GiteaMember, error)

	// AddTeamMember calls PUT /teams/:id/members/:username (idempotent —
	// adding an existing member returns 204 without error).
	AddTeamMember(ctx context.Context, giteaTeamID int64, username string) error

	// RemoveTeamMember calls DELETE /teams/:id/members/:username (idempotent —
	// removing a non-member returns 204 without error on most Gitea versions).
	RemoveTeamMember(ctx context.Context, giteaTeamID int64, username string) error
}

// Client is the production GiteaTeamMemberAPI. Construct via NewClient.
//
// Field exposure: baseURL / adminToken are read-only after construction;
// httpClient defaults to a stdlib http.Client but can be substituted in
// tests via the unexported newClientWithHTTPC constructor.
type Client struct {
	baseURL    string
	adminToken string
	// adminUser / adminPassword are populated when the caller needs to hit
	// Gitea endpoints that reject admin PAT (notably POST /users/{name}/tokens,
	// which sits behind reqBasicOrRevProxyAuth in upstream Gitea). Empty =
	// not configured; calls to those endpoints will surface a clear error.
	adminUser     string
	adminPassword string
	httpClient    *http.Client
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

// NewClientWithBasicAuth extends NewClient with admin username + password,
// required for the token-mint endpoints (see Client.adminUser doc). Empty
// user OR password returns nil — partial credentials are a config error.
func NewClientWithBasicAuth(baseURL, adminToken, adminUser, adminPassword string) *Client {
	if baseURL == "" || adminToken == "" {
		return nil
	}
	if adminUser == "" || adminPassword == "" {
		return NewClient(baseURL, adminToken)
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		adminToken:    adminToken,
		adminUser:     adminUser,
		adminPassword: adminPassword,
		httpClient:    &http.Client{Timeout: defaultTimeout},
	}
}

// newClientWithHTTPC is the test-only constructor that lets tests inject
// a stub *http.Client (or rely on httptest.Server's default client).
func newClientWithHTTPC(baseURL, adminToken string, hc *http.Client) *Client {
	if baseURL == "" || adminToken == "" {
		return nil
	}
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		adminToken: adminToken,
		httpClient: hc,
	}
}

// ListTeamMembers implements GiteaTeamMemberAPI.
func (c *Client) ListTeamMembers(ctx context.Context, giteaTeamID int64) ([]GiteaMember, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if giteaTeamID <= 0 {
		return nil, fmt.Errorf("gitsync: gitea_team_id must be positive")
	}

	path := fmt.Sprintf("/api/v1/teams/%d/members", giteaTeamID)
	resp, err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var members []GiteaMember
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return members, nil
}

// AddTeamMember implements GiteaTeamMemberAPI.
func (c *Client) AddTeamMember(ctx context.Context, giteaTeamID int64, username string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if giteaTeamID <= 0 {
		return fmt.Errorf("gitsync: gitea_team_id must be positive")
	}
	if username == "" {
		return fmt.Errorf("gitsync: username is required")
	}

	path := fmt.Sprintf("/api/v1/teams/%d/members/%s", giteaTeamID, url.PathEscape(username))
	resp, err := c.doJSON(ctx, http.MethodPut, path, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// RemoveTeamMember implements GiteaTeamMemberAPI.
func (c *Client) RemoveTeamMember(ctx context.Context, giteaTeamID int64, username string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if giteaTeamID <= 0 {
		return fmt.Errorf("gitsync: gitea_team_id must be positive")
	}
	if username == "" {
		return fmt.Errorf("gitsync: username is required")
	}

	path := fmt.Sprintf("/api/v1/teams/%d/members/%s", giteaTeamID, url.PathEscape(username))
	resp, err := c.doJSON(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// doJSON executes an authenticated JSON request against the Gitea API and
// returns the raw response on success. expectedStatuses drives the error
// mapping; any status code not in the set becomes ErrGiteaUnreachable (or
// ErrGiteaUnauthorized / ErrGiteaTeamNotFound for the known non-2xx cases).
//
// Variadic so callers that accept multiple success codes (e.g. WriteFile
// accepts both 201 Created and 200 OK) can pass them inline; existing
// single-status callers are unaffected.
//
// body may be nil for GET / PUT / DELETE requests (team-member endpoints
// take no body).
func (c *Client) doJSON(ctx context.Context, method, path string, body any, expectedStatuses ...int) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gitsync: marshal body: %w", err)
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
	// Gitea accepts "Authorization: token <token>" for admin PATs — same
	// convention as cs-user giteasync.GiteaClient.
	req.Header.Set("Authorization", "token "+c.adminToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrGiteaTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrGiteaUnreachable, err)
	}

	if ctx.Err() != nil {
		_ = resp.Body.Close()
		return nil, ErrGiteaTimeout
	}

	if len(expectedStatuses) == 0 {
		// Default: only 2xx-class success. Caller left the contract
		// implicit; treat any 2xx as success.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
	} else {
		for _, code := range expectedStatuses {
			if resp.StatusCode == code {
				return resp, nil
			}
		}
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnauthorized, resp.StatusCode, snippet)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: status=404 body=%s", ErrGiteaTeamNotFound, snippet)
	default:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnreachable, resp.StatusCode, snippet)
	}
}

// doJSONBasicAuth mirrors doJSON but sends HTTP Basic auth (admin_user +
// admin_password) instead of the admin PAT. Required by Gitea's
// reqBasicOrRevProxyAuth-protected endpoints (notably POST /users/{name}/tokens).
// Returns ErrGiteaBasicAuthRequired if admin credentials aren't configured.
func (c *Client) doJSONBasicAuth(ctx context.Context, method, path string, body any, expectedStatuses ...int) (*http.Response, error) {
	if c.adminUser == "" || c.adminPassword == "" {
		return nil, ErrGiteaBasicAuthRequired
	}
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gitsync: marshal body: %w", err)
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
	req.SetBasicAuth(c.adminUser, c.adminPassword)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrGiteaTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrGiteaUnreachable, err)
	}

	if ctx.Err() != nil {
		_ = resp.Body.Close()
		return nil, ErrGiteaTimeout
	}

	if len(expectedStatuses) == 0 {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
	} else {
		for _, code := range expectedStatuses {
			if resp.StatusCode == code {
				return resp, nil
			}
		}
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnauthorized, resp.StatusCode, snippet)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: status=404 body=%s", ErrGiteaTeamNotFound, snippet)
	default:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnreachable, resp.StatusCode, snippet)
	}
}
