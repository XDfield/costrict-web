package handlers

import (
	"errors"
	"strings"

	"github.com/costrict/costrict-web/server/internal/teamdir"
	"github.com/gin-gonic/gin"
)

// TeamDirectoryResolver is the production TeamResolver. It forwards the
// caller's Casdoor JWT to the team-directory backend (today: multica;
// future: dedicated org-team-service) and projects the response into
// TeamSummary rows.
//
// A nil *teamdir.Client (or one whose Configured() is false) is treated
// as "service unavailable" — ResolveCurrentUserTeams returns
// ErrOrgTeamServiceUnavailable so KBEnsure maps to 503.
type TeamDirectoryResolver struct {
	Client *teamdir.Client
}

// Compile-time guarantee the resolver satisfies the TeamResolver
// interface declared in kb_ensure.go.
var _ TeamResolver = (*TeamDirectoryResolver)(nil)

// ResolveCurrentUserTeams implements TeamResolver.
//
// subjectID is kept for interface compatibility but is no longer the
// lookup key — the team-directory backend resolves the user from the
// forwarded Casdoor JWT via its own auth middleware.
//
// Empty slice (not nil) is preserved as the legitimate "user belongs to
// no team" state — KBEnsure maps that to 403 NO_TEAM_MEMBERSHIP.
func (r *TeamDirectoryResolver) ResolveCurrentUserTeams(c *gin.Context, subjectID string) ([]TeamSummary, error) {
	if r == nil || r.Client == nil {
		return nil, ErrOrgTeamServiceUnavailable
	}
	jwt := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	teams, err := r.Client.ListUserTeams(c.Request.Context(), jwt)
	if err != nil {
		if errors.Is(err, teamdir.ErrServiceUnavailable) {
			return nil, ErrOrgTeamServiceUnavailable
		}
		// Wrap but keep the teamdir sentinel identifiable via errors.Is.
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
