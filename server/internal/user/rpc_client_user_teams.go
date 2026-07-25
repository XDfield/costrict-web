// Package user — RPC client for multica's workspace-list endpoint.
//
// ListUserTeams proxies GET /api/workspaces on multica, forwarding the
// caller's Casdoor JWT. multica's CasdoorAuth middleware resolves the
// JWT to a multica user and returns the workspaces they belong to.
// We project each workspace into the UserTeam shape expected by
// @server's kb/ensure handler (via handlers.TeamResolver).
//
// Mirrors GetTenantGitServer's HTTP/error-shape pattern.

package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// UserTeam is the local projection of a multica workspace.
// Server-side type decoupled from multica per ADR D1.
type UserTeam struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// Sentinel errors.
var (
	// ErrUserTeamsEmptyToken — caller passed an empty JWT. We surface as a
	// sentinel for the handler to map back.
	ErrUserTeamsEmptyToken = errors.New("user_teams: jwt token required")
	// ErrOrgTeamServiceUnavailable — multica returned 503 (service not
	// ready) OR the client isn't configured. Server's kb/ensure maps this
	// to its own 503 of the same code.
	ErrOrgTeamServiceUnavailable = errors.New("user_teams: org-team-service unavailable")
)

// ListUserTeams proxies GET /api/workspaces on multica using the caller's
// Casdoor JWT (Bearer). multica resolves the JWT to a user and returns
// the workspaces they belong to; we project each row to UserTeam.
//
// Returns:
//   - []UserTeam, nil on 200 (slice may be empty — legitimate "user has
//     no teams" state, not an error)
//   - nil, ErrUserTeamsEmptyToken on empty jwtToken (client-side guard)
//   - nil, ErrOrgTeamServiceUnavailable on multica 503 or 401/403/421/5xx
//     (treat any auth/transport failure as "service unavailable" so the
//     handler maps to a single 503 code)
//   - nil, fmt-wrapped error on decode failure
func (c *RPCClient) ListUserTeams(ctx context.Context, jwtToken string) ([]UserTeam, error) {
	// ListUserTeams doesn't use internalToken (multica's /api/workspaces
	// authenticates via the forwarded Casdoor JWT, not a shared internal
	// secret), so gate on baseURL only — not the shared Configured() which
	// also demands internalToken for the other (still-X-Internal-Token)
	// RPC methods on this client.
	if c == nil || c.baseURL == "" {
		// No multica wired (e.g. dev mode without RPC); surface same
		// sentinel as 503 so handlers map to a single error code.
		return nil, ErrOrgTeamServiceUnavailable
	}
	if jwtToken == "" {
		return nil, ErrUserTeamsEmptyToken
	}

	const path = "/api/workspaces"

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("user_teams: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("user_teams: rpc: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	bodyStr := string(body)

	switch resp.StatusCode {
	case http.StatusOK:
		// multica returns a top-level array of workspace objects.
		var rows []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("user_teams: decode 200 body: %w; body=%s", err, truncate(bodyStr, 256))
		}
		teams := make([]UserTeam, 0, len(rows))
		for _, r := range rows {
			teams = append(teams, UserTeam{
				TeamID:      r.ID,
				DisplayName: r.Name,
				// Role not exposed by multica's /api/workspaces; left blank.
			})
		}
		return teams, nil
	case http.StatusBadRequest:
		return nil, ErrUserTeamsEmptyToken
	case http.StatusServiceUnavailable:
		logger.Warn("user_teams: multica 503 (org-team-service unavailable) body=%s",
			truncate(bodyStr, 256))
		return nil, ErrOrgTeamServiceUnavailable
	case http.StatusUnauthorized, http.StatusForbidden:
		// JWT rejected by multica — surface as "service unavailable" so
		// kb/ensure fails closed with a single 503 error code rather
		// than leaking auth state to the end user.
		logger.Warn("user_teams: multica auth rejected status=%d body=%s",
			resp.StatusCode, truncate(bodyStr, 256))
		return nil, ErrOrgTeamServiceUnavailable
	default:
		logger.Warn("user_teams: multica unexpected status=%d body=%s",
			resp.StatusCode, truncate(bodyStr, 256))
		return nil, ErrOrgTeamServiceUnavailable
	}
}
