// RPCClient implements UserReader by calling the cs-user microservice over HTTP.
// It is the "rpc" backend selected by USER_SERVICE_BACKEND=rpc; the default
// "local" backend uses *UserService against this process's own DB.
//
// cs-user authenticates server-to-server traffic with a shared X-Internal-Token
// header and signals not-found via HTTP 404 (translated here to gorm.ErrRecordNotFound
// so handler-level errors.Is(err, gorm.ErrRecordNotFound) checks keep working).
// Other transport/timeout/5xx failures map to ErrRPCUnavailable so handlers can
// surface a 503 cleanly.
package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

var (
	// ErrNotConfigured means USER_SERVICE_BACKEND=rpc was selected without both
	// USER_SERVICE_URL and USER_SERVICE_INTERNAL_TOKEN. main.go fails fast at
	// boot to prevent this in production, but the check is repeated here so a
	// misconfigured runtime path (e.g. a test) still degrades safely.
	ErrNotConfigured = errors.New("user rpc client: not configured")
	// ErrRPCUnavailable covers any transport failure, timeout, or 5xx response
	// from cs-user. Handlers translate it to HTTP 503.
	ErrRPCUnavailable = errors.New("user rpc client: upstream unavailable")
	// ErrUserNotFound signals cs-user returned no rows for a lookup. Used by
	// UserRef.employee_number path (team-namespace doc v1.1 §5.2); handlers
	// translate it to HTTP 404.
	ErrUserNotFound = errors.New("user rpc client: user not found")
)

const defaultTimeout = 10 * time.Second

// RPCClient talks to cs-user over HTTP. Construct with NewRPCClient.
type RPCClient struct {
	baseURL       string
	internalToken string
	httpClient    *http.Client
}

// NewRPCClient builds an RPCClient from config. The httpClient timeout is taken
// from cfg.TimeoutSec (default 10s). The client is usable even when Configured()
// is false — methods then return ErrNotConfigured — so this constructor never
// fails and tests can inject a stub http.Client before flipping Configured().
func NewRPCClient(cfg config.UserServiceConfig) *RPCClient {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &RPCClient{
		baseURL:       strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		internalToken: strings.TrimSpace(cfg.InternalToken),
		httpClient:    &http.Client{Timeout: timeout},
	}
}

// Configured reports whether both baseURL and internalToken are set. The
// Reader methods short-circuit with ErrNotConfigured when this is false.
func (c *RPCClient) Configured() bool {
	return c != nil && c.baseURL != "" && c.internalToken != ""
}

// GetUserByID calls GET /api/internal/users/:subject_id. HTTP 404 → gorm.ErrRecordNotFound.
func (c *RPCClient) GetUserByID(ctx context.Context, userID string) (*models.User, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	path := "/api/internal/users/" + url.PathEscape(userID)
	var user models.User
	if err := c.do(ctx, http.MethodGet, path, nil, &user, decodeBareUser); err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUsersByIDs calls POST /api/internal/users/by-ids with {"ids": [...]}.
// Returns the bare map cs-user emits; missing IDs simply don't appear.
func (c *RPCClient) GetUsersByIDs(ctx context.Context, userIDs []string) (map[string]*models.User, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	reqBody, err := json.Marshal(struct {
		IDs []string `json:"ids"`
	}{IDs: userIDs})
	if err != nil {
		return nil, fmt.Errorf("user rpc client: marshal by-ids request: %w", err)
	}
	var resp struct {
		Users map[string]*models.User `json:"users"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/internal/users/by-ids", reqBody, &resp, decodeUsersMap); err != nil {
		return nil, err
	}
	if resp.Users == nil {
		return map[string]*models.User{}, nil
	}
	return resp.Users, nil
}

// SearchUsers calls GET /api/internal/users/search?keyword=...&limit=...
func (c *RPCClient) SearchUsers(ctx context.Context, keyword string, limit int) ([]*models.User, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	q := url.Values{}
	q.Set("keyword", keyword)
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp struct {
		Users []*models.User `json:"users"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/internal/users/search?"+q.Encode(), nil, &resp, decodeUsersList); err != nil {
		return nil, err
	}
	if resp.Users == nil {
		return []*models.User{}, nil
	}
	return resp.Users, nil
}

// SearchByEmployeeNumber calls GET /api/internal/users/search?employee_number=...&limit=1.
// Backs the UserRef.employee_number path (team-namespace doc v1.1 §5.2).
// Returns ErrUserNotFound when cs-user returns no rows — handler maps that
// to HTTP 404 to match the doc's INVALID_USER_REF / USER_NOT_FOUND shape.
func (c *RPCClient) SearchByEmployeeNumber(ctx context.Context, employeeNumber string) (*models.User, error) {
	users, err := c.SearchByEmployeeNumberN(ctx, employeeNumber, 1)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, ErrUserNotFound
	}
	return users[0], nil
}

// SearchByEmployeeNumberN is the multi-row variant — exposed for tests and
// future callers that need to enumerate ambiguous matches.
func (c *RPCClient) SearchByEmployeeNumberN(ctx context.Context, employeeNumber string, limit int) ([]*models.User, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	q := url.Values{}
	q.Set("employee_number", employeeNumber)
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp struct {
		Users []*models.User `json:"users"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/internal/users/search?"+q.Encode(), nil, &resp, decodeUsersList); err != nil {
		return nil, err
	}
	if resp.Users == nil {
		return []*models.User{}, nil
	}
	return resp.Users, nil
}

// ListUserIdentities calls GET /api/internal/users/:subject_id/auth-identities.
func (c *RPCClient) ListUserIdentities(ctx context.Context, userSubjectID string) ([]*models.UserAuthIdentity, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	path := "/api/internal/users/" + url.PathEscape(userSubjectID) + "/auth-identities"
	var resp struct {
		Identities []*models.UserAuthIdentity `json:"identities"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp, decodeIdentitiesList); err != nil {
		return nil, err
	}
	if resp.Identities == nil {
		return []*models.UserAuthIdentity{}, nil
	}
	return resp.Identities, nil
}

// decodeStrategy tells do() how to interpret the response body. Three shapes:
//   - bareUser: body is the bare model.User object
//   - usersMap / usersList / identitiesList: body is a wrapper object
type decodeStrategy int

const (
	decodeBareUser decodeStrategy = iota
	decodeUsersMap
	decodeUsersList
	decodeIdentitiesList
)

// do issues an authenticated request and decodes the response into out using the
// given strategy. HTTP 404 → gorm.ErrRecordNotFound; transport errors, timeouts,
// and 5xx → ErrRPCUnavailable. The ctx is bounded by the configured per-request
// timeout (NOT context.Background — see plan deviation note vs deptsync).
func (c *RPCClient) do(ctx context.Context, method, path string, body []byte, out any, strategy decodeStrategy) error {
	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("user rpc client: build request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Phase B3b.2a: forward the tenant slug (if any) so cs-user's ResolveTenant
	// middleware resolves against the same tenant. Empty slug = no signal —
	// cs-user falls back to default tenant.
	if slug := tenantSlugFromContext(ctx); slug != "" {
		req.Header.Set("X-Tenant-Id", slug)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc] %s %s request failed: %v", method, path, err)
		return fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("[user-rpc] %s %s read body failed: %v", method, path, err)
		return fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return gorm.ErrRecordNotFound
	case resp.StatusCode >= 400:
		logger.Warn("[user-rpc] %s %s returned status %d: %s",
			method, path, resp.StatusCode, truncate(string(respBody), 200))
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: status %d", ErrRPCUnavailable, resp.StatusCode)
		}
		return fmt.Errorf("user rpc client: status %d", resp.StatusCode)
	}

	if err := decodeByStrategy(respBody, out, strategy); err != nil {
		logger.Warn("[user-rpc] %s %s decode failed: %v", method, path, err)
		return fmt.Errorf("user rpc client: decode response: %w", err)
	}
	return nil
}

func decodeByStrategy(body []byte, out any, strategy decodeStrategy) error {
	switch strategy {
	case decodeBareUser:
		return json.Unmarshal(body, out)
	case decodeUsersMap, decodeUsersList, decodeIdentitiesList:
		if len(body) == 0 || string(body) == "null" {
			return nil // leave out at its zero value; caller normalizes to empty map/slice
		}
		return json.Unmarshal(body, out)
	default:
		return fmt.Errorf("unknown decode strategy %d", strategy)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
