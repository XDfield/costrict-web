// Team-sync handlers (Phase E3b.1 step D).
//
// Single thin handler at POST /api/admin/teams/:team_id/sync that proxies
// to gitsync.Service.SyncTeam. Route gated by middleware.RequirePlatformAdmin
// (manual admin trigger only — E4 webhook receiver replaces this with an
// event-driven path in a future slice).
//
// Error mapping (translates gitsync sentinels → HTTP):
//   - ErrTeamNotFound / ErrGiteaTeamNotFound → 404
//   - ErrGiteaUnauthorized                  → 502 (config error, not caller's fault)
//   - ErrGiteaUnreachable / ErrGiteaTimeout → 502 (transient upstream)
//   - empty team_id (gin :team_id missing)  → 400
//   - nil TeamSyncService                   → 503 (feature disabled)

package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/gin-gonic/gin"
)

// TeamSyncService is the consumer-facing surface of *gitsync.Service.
// Declared as an interface so tests can substitute a fake.
type TeamSyncService interface {
	SyncTeam(ctx context.Context, teamID string) (*gitsync.SyncResult, error)
}

// teamSyncService is the package-level holder for the production
// *gitsync.Service. Set via InitTeamSyncService; nil means feature
// disabled (handler returns 503).
var teamSyncService TeamSyncService

// InitTeamSyncService wires the production gitsync.Service. Pass nil to
// explicitly disable the feature (e.g. when GITEA_BASE_URL is unset).
func InitTeamSyncService(svc TeamSyncService) {
	teamSyncService = svc
}

// SyncTeam godoc
// @Summary      Sync Gitea team membership (manual trigger)
// @Description  Runs a full reconcile: compares expected team members (from configured provider) against current Gitea state, adds missing, removes extra. Per-member failures are collected into response.errors[] and do not abort the batch. Returns 200 even on partial success.
// @Tags         admin,teams
// @Produce      json
// @Security     BearerAuth
// @Param        team_id  path  string  true  "Logical team ID (must be present in TEAM_SYNC_MAPPINGS)"
// @Success      200  {object}  gitsync.SyncResult
// @Failure      400  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      502  {object}  object{error=string}
// @Failure      503  {object}  object{error=string}
// @Router       /admin/teams/{team_id}/sync [post]
func SyncTeam(c *gin.Context) {
	if teamSyncService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Team sync feature is not configured"})
		return
	}

	teamID := c.Param("team_id")
	if teamID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "team_id is required"})
		return
	}

	result, err := teamSyncService.SyncTeam(c.Request.Context(), teamID)
	if err != nil {
		status, body := mapTeamSyncError(err)
		c.JSON(status, body)
		return
	}

	c.JSON(http.StatusOK, result)
}

// mapTeamSyncError translates a gitsync sentinel into the (status, body)
// pair gin.JSON expects. Centralised so the handler stays declarative.
func mapTeamSyncError(err error) (int, gin.H) {
	switch {
	case errors.Is(err, gitsync.ErrTeamNotFound), errors.Is(err, gitsync.ErrGiteaTeamNotFound):
		return http.StatusNotFound, gin.H{"error": err.Error()}
	case errors.Is(err, gitsync.ErrGiteaUnauthorized):
		// Config error (wrong admin token) — surface as 502 rather than
		// 401 so the admin caller doesn't think their platform-admin JWT
		// is the problem.
		return http.StatusBadGateway, gin.H{"error": "Gitea admin token rejected: " + err.Error()}
	case errors.Is(err, gitsync.ErrGiteaUnreachable), errors.Is(err, gitsync.ErrGiteaTimeout):
		return http.StatusBadGateway, gin.H{"error": "Gitea unreachable: " + err.Error()}
	default:
		return http.StatusInternalServerError, gin.H{"error": err.Error()}
	}
}
