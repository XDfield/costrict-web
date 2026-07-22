// Package user — RPC client for cs-user's user-teams endpoint.
//
// ListUserTeams proxies GET /api/internal/users/:subject_id/teams. Used by
// @server's kb/ensure handler (via the handlers.TeamResolver interface) to
// resolve the caller's team list per KB_USER_ENSURE_API.md §2.
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
	"net/url"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// UserTeam is the local projection of cs-user's userteams.TeamSummary.
// Server-side type decoupled from cs-user per ADR D1.
type UserTeam struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// Sentinel errors.
var (
	// ErrUserTeamsEmptySubject — caller passed an empty subject_id. cs-user
	// returns 400; we surface as a sentinel for the handler to map back.
	ErrUserTeamsEmptySubject = errors.New("user_teams: subject_id required")
	// ErrOrgTeamServiceUnavailable — cs-user returned 503
	// ORG_TEAM_SERVICE_UNAVAILABLE (org-team-service integration not yet
	// wired). Server's kb/ensure maps this to its own 503 of the same code.
	ErrOrgTeamServiceUnavailable = errors.New("user_teams: org-team-service unavailable")
)

// ListUserTeams proxies GET /api/internal/users/:subject_id/teams.
//
// Returns:
//   - []UserTeam, nil on 200 (slice may be empty — legitimate "user has
//     no teams" state, not an error)
//   - nil, ErrUserTeamsEmptySubject on empty subjectID (client-side guard)
//   - nil, ErrOrgTeamServiceUnavailable on cs-user 503
//   - nil, fmt-wrapped error on transport / decode / unexpected status
func (c *RPCClient) ListUserTeams(ctx context.Context, subjectID string) ([]UserTeam, error) {
	if !c.Configured() {
		// No cs-user wired (e.g. dev mode without RPC); surface same
		// sentinel as 503 so handlers map to a single error code.
		return nil, ErrOrgTeamServiceUnavailable
	}
	if subjectID == "" {
		return nil, ErrUserTeamsEmptySubject
	}

	path := "/api/internal/users/" + url.PathEscape(subjectID) + "/teams"

	ctx, cancel := context.WithTimeout(ctx, c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("user_teams: build request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.internalToken)
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
		var payload struct {
			Teams []UserTeam `json:"teams"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("user_teams: decode 200 body: %w; body=%s", err, truncate(bodyStr, 256))
		}
		return payload.Teams, nil
	case http.StatusBadRequest:
		return nil, ErrUserTeamsEmptySubject
	case http.StatusServiceUnavailable:
		logger.Warn("user_teams: cs-user 503 (org-team-service unavailable) subject=%s body=%s",
			subjectID, truncate(bodyStr, 256))
		return nil, ErrOrgTeamServiceUnavailable
	default:
		return nil, fmt.Errorf("user_teams: unexpected status=%d body=%s", resp.StatusCode, truncate(bodyStr, 256))
	}
}
