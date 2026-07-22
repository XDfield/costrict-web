// Package handlers — team-namespace API v1.1 internal surface.
//
// The 7 handlers here mirror docs/repo-management/TEAM_NAMESPACE_API_REFERENCE.md
// 1:1. Each handler is thin: parse JSON → call teamns.Service → map the
// returned sentinel error to the doc's HTTP/error_code pair → serialize.
//
// Auth is handled by middleware.InternalAuth (X-Internal-Service-Token);
// tenant scope by middleware.ResolveTenantSlug → tenant.WithTenantID. The
// handlers don't re-check those concerns.

package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/gin-gonic/gin"
)

// teamnsService is the package-level holder, set via InitTeamNSService.
// nil means feature-disabled (handlers return 503).
var teamnsService *teamns.Service

// InitTeamNSService wires the production teamns.Service. Pass nil to
// disable the surface (e.g. when CS_BOT_TOKEN_KEY is unset at boot).
func InitTeamNSService(svc *teamns.Service) {
	teamnsService = svc
}

// teamnsDisabled short-circuits a handler when the service wasn't wired.
func teamnsDisabled(c *gin.Context) bool {
	if teamnsService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "team namespace feature is not configured",
		})
		return true
	}
	return false
}

// mapTeamnsError translates teamns sentinels into the (status, body) pair
// gin.JSON expects. The error_code field mirrors the doc's enum so upstream
// callers can branch on code, not message text.
func mapTeamnsError(err error) (int, gin.H) {
	switch {
	case errors.Is(err, teamns.ErrInvalidRequest):
		return http.StatusBadRequest, gin.H{"error": err.Error(), "error_code": "INVALID_REQUEST"}
	case errors.Is(err, teamns.ErrTeamNotFound):
		// The doc distinguishes TEAM_NOT_FOUND (team_id never created) from
		// TEAM_NS_NOT_INITIALIZED (members:sync before team ns exists). We
		// don't have separate sentinels; teamns.ErrTeamNotFound serves both.
		// Handler-side we keep the team_id path semantics; members:sync
		// uses the same code (caller can distinguish via context).
		return http.StatusNotFound, gin.H{"error": err.Error(), "error_code": "TEAM_NOT_FOUND"}
	case errors.Is(err, teamns.ErrTeamIDTaken):
		return http.StatusConflict, gin.H{"error": err.Error(), "error_code": "TEAM_ID_TAKEN"}
	case errors.Is(err, teamns.ErrTeamArchived):
		return http.StatusGone, gin.H{"error": err.Error(), "error_code": "TEAM_ARCHIVED"}
	case errors.Is(err, teamns.ErrMemberUnresolved):
		return http.StatusNotFound, gin.H{"error": err.Error(), "error_code": "MEMBER_USER_NOT_FOUND"}
	case errors.Is(err, teamns.ErrBotUsernameTaken):
		return http.StatusConflict, gin.H{"error": err.Error(), "error_code": "BOT_USERNAME_TAKEN"}
	case errors.Is(err, teamns.ErrTenantGitServerUnresolved):
		return http.StatusPreconditionFailed, gin.H{"error": err.Error(), "error_code": "TENANT_GIT_SERVER_UNRESOLVED"}
	default:
		return http.StatusInternalServerError, gin.H{"error": err.Error()}
	}
}

// CreateTeam godoc
// @Summary      Create a team (atomic ns + bot account + bot token)
// @Description  POST /api/internal/teams — atomically creates Gitea org t-<team_short>, provisions bot account, mints bot PAT, optionally applies seed members. Idempotent on team_id within the same tenant.
// @Tags         internal,teams
// @Accept       json
// @Produce      json
// @Security     InternalAuth
// @Param        body  body  teamns.CreateTeamRequest  true  "Create team request"
// @Success      200  {object}  teamns.CreateTeamResult
// @Success      201  {object}  teamns.CreateTeamResult
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      409  {object}  object{error=string,error_code=string}
// @Failure      412  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams [post]
func CreateTeam(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	var req teamns.CreateTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	result, err := teamnsService.CreateTeam(c.Request.Context(), req)
	if err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	// 201 on actual create; 200 on idempotent re-POST.
	status := http.StatusCreated
	if !result.Created.TeamNS && !result.Created.BotAccount && !result.Created.BotToken {
		status = http.StatusOK
	}
	c.JSON(status, result)
}

// GetTeam godoc
// @Summary      Get a team by id
// @Description  GET /api/internal/teams/:team_id — returns team ns + bot metadata. Never returns bot plaintext token.
// @Tags         internal,teams
// @Produce      json
// @Security     InternalAuth
// @Param        team_id  path  string  true  "Team UUID"
// @Success      200  {object}  teamns.GetTeamResult
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      404  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams/{team_id} [get]
func GetTeam(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	teamID := c.Param("team_id")
	result, err := teamnsService.GetTeam(c.Request.Context(), teamID)
	if err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ListTeams godoc
// @Summary      List teams (single-tenant)
// @Description  GET /api/internal/teams — paginated, single-tenant only (tenant_id from ctx). tenant_id query parameter is explicitly rejected.
// @Tags         internal,teams
// @Produce      json
// @Security     InternalAuth
// @Param        page       query  int     false  "1-based page"
// @Param        page_size  query  int     false  "page size (max 200)"
// @Param        q          query  string  false  "fuzzy match on team_display_name"
// @Param        status     query  string  false  "active | archived"
// @Success      200  {object}  teamns.ListResult
// @Failure      400  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams [get]
func ListTeams(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	params := teamns.ListParams{
		Query:         c.Query("q"),
		Status:        c.Query("status"),
		TenantIDQuery: c.Query("tenant_id"),
	}
	if v := c.Query("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a positive integer", "error_code": "INVALID_REQUEST"})
			return
		}
		params.Page = n
	}
	if v := c.Query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be a positive integer", "error_code": "INVALID_REQUEST"})
			return
		}
		params.PageSize = n
	}
	result, err := teamnsService.ListTeams(c.Request.Context(), params)
	if err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	c.JSON(http.StatusOK, result)
}

// PatchTeam godoc
// @Summary      Patch team metadata
// @Description  PATCH /api/internal/teams/:team_id — mirrors display_name into team_ns; description mirrors into Gitea org description.
// @Tags         internal,teams
// @Accept       json
// @Security     InternalAuth
// @Param        team_id  path  string  true  "Team UUID"
// @Param        body     body  teamns.PatchTeamRequest  true  "Patch request"
// @Success      204
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      404  {object}  object{error=string,error_code=string}
// @Failure      410  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams/{team_id} [patch]
func PatchTeam(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	teamID := c.Param("team_id")
	var req teamns.PatchTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	req.TeamDisplayName = strings.TrimSpace(req.TeamDisplayName)
	req.Description = strings.TrimSpace(req.Description)
	if err := teamnsService.PatchTeam(c.Request.Context(), teamID, req); err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	c.Status(http.StatusNoContent)
}

// SyncTeamMembers godoc
// @Summary      Sync team members (delta or full_sync)
// @Description  POST /api/internal/teams/:team_id/members:sync — apply add/remove deltas OR full reconcile against Gitea org. Partial failures surface as members_unresolved; full-batch failure returns 404.
// @Tags         internal,teams
// @Accept       json
// @Produce      json
// @Security     InternalAuth
// @Param        team_id  path  string  true  "Team UUID"
// @Param        body     body  teamns.SyncMembersRequest  true  "Sync request"
// @Success      200  {object}  teamns.SyncMembersResult
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      404  {object}  object{error=string,error_code=string}
// @Failure      410  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams/{team_id}/members:sync [post]
func SyncTeamMembers(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	teamID := c.Param("team_id")
	var req teamns.SyncMembersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	result, err := teamnsService.SyncTeamMembers(c.Request.Context(), teamID, req)
	if err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	c.JSON(http.StatusOK, result)
}

// DissolveTeam godoc
// @Summary      Dissolve a team
// @Description  POST /api/internal/teams/:team_id/dissolve — archive org, purge members, revoke bot token. Idempotent.
// @Tags         internal,teams
// @Accept       json
// @Produce      json
// @Security     InternalAuth
// @Param        team_id  path  string  true  "Team UUID"
// @Param        body     body  teamns.DissolveTeamRequest  true  "Dissolve request"
// @Success      200  {object}  teamns.DissolveTeamResult
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      404  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams/{team_id}/dissolve [post]
func DissolveTeam(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	teamID := c.Param("team_id")
	var req teamns.DissolveTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required", "error_code": "INVALID_REQUEST"})
		return
	}
	result, err := teamnsService.DissolveTeam(c.Request.Context(), teamID, req)
	if err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	c.JSON(http.StatusOK, result)
}

// RotateBotToken godoc
// @Summary      Rotate bot token
// @Description  POST /api/internal/teams/:team_id/bot-token:rotate — mints a new PAT, revokes the previous one. Returns new plaintext token once.
// @Tags         internal,teams
// @Accept       json
// @Produce      json
// @Security     InternalAuth
// @Param        team_id  path  string  true  "Team UUID"
// @Param        body     body  teamns.RotateBotTokenRequest  true  "Rotate request"
// @Success      200  {object}  teamns.RotateBotTokenResult
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      404  {object}  object{error=string,error_code=string}
// @Failure      410  {object}  object{error=string,error_code=string}
// @Router       /api/internal/teams/{team_id}/bot-token:rotate [post]
func RotateBotToken(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	teamID := c.Param("team_id")
	var req teamns.RotateBotTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason is required", "error_code": "INVALID_REQUEST"})
		return
	}
	result, err := teamnsService.RotateBotToken(c.Request.Context(), teamID, req)
	if err != nil {
		status, body := mapTeamnsError(err)
		c.JSON(status, body)
		return
	}
	c.JSON(http.StatusOK, result)
}
