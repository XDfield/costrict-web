// Package teamdir is the HTTP client for the team-directory service —
// the authoritative source of "which teams/workspaces does a user
// belong to". Today the backing service is multica (temporary); the
// long-term plan is a dedicated org-team-service. This package is the
// seam: callers depend on teamdir, not on whichever backend happens
// to be wired.
//
// Auth model: the caller forwards the end user's Casdoor JWT via
// Authorization: Bearer. The backing service's own auth middleware
// resolves the JWT to its internal user id. No shared internal token.
package teamdir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config configures a Client. TimeoutSec defaults to 10 when unset.
type Config struct {
	BaseURL    string
	TimeoutSec int
}

// Client talks to the team-directory backend over HTTP.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a Client. A Client with an empty baseURL is treated
// as "not wired" — ListUserTeams returns ErrServiceUnavailable so
// upstream handlers fail closed with a single error code.
func NewClient(cfg Config) *Client {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Configured reports whether the client has a backend URL. Absent
// token is not a failure mode here — auth is per-request via JWT.
func (c *Client) Configured() bool {
	return c != nil && c.baseURL != ""
}

// TeamSummary is the local projection of a team row from the backend.
type TeamSummary struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	// Role is currently not surfaced by the backend; left blank.
	// KB_USER_ENSURE_API.md only requires team_id + display_name to
	// render the disambiguation picker.
	Role string `json:"role"`
}

// Sentinel errors.
var (
	// ErrEmptyToken — caller passed an empty JWT.
	ErrEmptyToken = errors.New("teamdir: jwt token required")
	// ErrServiceUnavailable — backend unreachable, returned 503,
	// rejected the JWT (401/403), or returned an unexpected status.
	// Mapped upstream to HTTP 503 ORG_TEAM_SERVICE_UNAVAILABLE so the
	// end user sees a single fail-closed code instead of auth-state leak.
	ErrServiceUnavailable = errors.New("teamdir: team-directory service unavailable")
)

// ListUserTeams returns the teams the JWT subject belongs to.
//
// Returns:
//   - []TeamSummary, nil on 200 (slice may be empty — legitimate "no team")
//   - nil, ErrEmptyToken on empty jwtToken
//   - nil, ErrServiceUnavailable on any auth/transport/unexpected-status failure
//   - nil, fmt-wrapped error on decode failure
func (c *Client) ListUserTeams(ctx context.Context, jwtToken string) ([]TeamSummary, error) {
	if !c.Configured() {
		return nil, ErrServiceUnavailable
	}
	if jwtToken == "" {
		return nil, ErrEmptyToken
	}

	const path = "/api/workspaces"

	reqCtx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("teamdir: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("teamdir: rpc: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		// Backend returns a top-level array of workspace objects; we
		// only consume id + name. Unknown fields are ignored.
		var rows []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("teamdir: decode 200 body: %w; body=%s", err, truncate(string(body), 256))
		}
		out := make([]TeamSummary, 0, len(rows))
		for _, r := range rows {
			out = append(out, TeamSummary{
				TeamID:      r.ID,
				DisplayName: r.Name,
			})
		}
		return out, nil
	case http.StatusBadRequest:
		return nil, ErrEmptyToken
	case http.StatusUnauthorized, http.StatusForbidden:
		// Fail closed — don't leak auth state.
		return nil, ErrServiceUnavailable
	case http.StatusServiceUnavailable:
		return nil, ErrServiceUnavailable
	default:
		return nil, ErrServiceUnavailable
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
