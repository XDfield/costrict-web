// Package handlers — production TeamResolver backed by cs-user RPC.
//
// CSUserTeamResolver adapts user.RPCClient.ListUserTeams to the TeamResolver
// interface used by the user-side KB ensure handler. Maps user.UserTeam →
// handlers.TeamSummary and propagates user.ErrOrgTeamServiceUnavailable so
// KBEnsure can render its 503 ORG_TEAM_SERVICE_UNAVAILABLE branch.
//
// Per KB_USER_ENSURE_API.md §2.3 — never falls back to Gitea org membership;
// the org-team-service is the sole source of truth for team lists.

package handlers

import (
	"errors"
	"strings"

	"github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// CSUserTeamResolver is the production TeamResolver. A nil *user.RPCClient
// (or one whose Configured() is false) is treated as "service unavailable"
// — ResolveCurrentUserTeams returns ErrOrgTeamServiceUnavailable so the
// handler maps to 503.
type CSUserTeamResolver struct {
	Client *user.RPCClient
}

// ResolveCurrentUserTeams implements TeamResolver.
//
// subjectID is kept for interface compatibility but is no longer the
// primary key — multica resolves the user from the forwarded Casdoor JWT
// (its CasdoorAuth middleware handles subject → multica_user mapping).
//
// Empty slice (not nil) is preserved as the legitimate "user belongs to no
// team" state — KBEnsure maps that to 403 NO_TEAM_MEMBERSHIP.
func (r *CSUserTeamResolver) ResolveCurrentUserTeams(c *gin.Context, subjectID string) ([]TeamSummary, error) {
	if r == nil || r.Client == nil {
		return nil, ErrOrgTeamServiceUnavailable
	}
	jwt := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	teams, err := r.Client.ListUserTeams(c.Request.Context(), jwt)
	if err != nil {
		if errors.Is(err, user.ErrOrgTeamServiceUnavailable) {
			return nil, ErrOrgTeamServiceUnavailable
		}
		// Wrap but keep the cs-user sentinel identifiable via errors.Is.
		return nil, err
	}
	out := make([]TeamSummary, 0, len(teams))
	for _, t := range teams {
		out = append(out, TeamSummary{
			TeamID:      t.TeamID,
			DisplayName: t.DisplayName,
			Role:        t.Role,
		})
	}
	return out, nil
}

// Compile-time interface check.
var _ TeamResolver = (*CSUserTeamResolver)(nil)
