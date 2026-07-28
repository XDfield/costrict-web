// Package handlers — user teams endpoint (kb/ensure backing).
//
// GET /api/internal/users/:subject_id/teams is the RPC entry point that
// @server's kb/ensure handler calls to resolve the caller's team list.
//
// Current state: returns 503 ORG_TEAM_SERVICE_UNAVAILABLE until the
// org-team-service integration in cs-user/internal/userteams lands.
// Contract is fixed so @server can wire its TeamResolver now and
// activate the feature by swapping the service impl later.

package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/userteams"
	"github.com/gin-gonic/gin"
)

// UserTeamsAPI wraps the userteams.Service for HTTP. Mirrors the
// UsersAPI pattern (handler struct + unavailable fallback for tests).
type UserTeamsAPI struct {
	Svc UserTeamsService
}

// UserTeamsService is the narrow interface the handler needs. Real impl
// is *userteams.Service; tests inject stubs.
type UserTeamsService interface {
	ListUserTeams(ctx context.Context, subjectID string) ([]userteams.TeamSummary, error)
}

// ListUserTeams godoc
//
//	@Summary		List teams for a user (kb/ensure backing)
//	@Description	Returns the teams the user belongs to, in the tenant resolved by X-Tenant-Id. Currently returns 503 ORG_TEAM_SERVICE_UNAVAILABLE until org-team-service integration lands.
//	@Tags			users,teams
//	@Produce		json
//	@Security		InternalToken
//	@Param			subject_id	path		string	true	"User subject_id"
//	@Success		200			{object}	object{teams=[]userteams.TeamSummary}
//	@Failure		400			{object}	object{error=string}
//	@Failure		503			{object}	object{error=string,error_code=string}
//	@Router			/api/internal/users/{subject_id}/teams [get]
func (a *UserTeamsAPI) ListUserTeams(c *gin.Context) {
	subjectID := c.Param("subject_id")
	if subjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		return
	}
	teams, err := a.Svc.ListUserTeams(c.Request.Context(), subjectID)
	if err != nil {
		switch {
		case errors.Is(err, userteams.ErrEmptySubjectID):
			c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		case errors.Is(err, userteams.ErrOrgTeamServiceNotIntegrated):
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "team membership lookup unavailable: org-team-service integration not yet wired",
				"error_code": "ORG_TEAM_SERVICE_UNAVAILABLE",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"teams": teams})
}
