// Package handlers — user-facing KB ensure handler.
//
// POST /api/kb/ensure is the user-side entry point (csc direct call) that
// resolves the caller's team membership from the JWT and ensures the kb
// repo backing (code_repo_url, team_id) exists. Behavior mirrors the
// internal §接口 9 contract; differences are documented in
// docs/repo-management/KB_USER_ENSURE_API.md (v1.0):
//
//   - Auth: user JWT (middleware.RequireAuth sets UserIDKey)
//   - team_id is optional; when omitted, server resolves the caller's
//     team list and either auto-derives (single team) or returns 409
//     TEAM_DISAMBIGUATION_REQUIRED (multi-team) or 403 NO_TEAM_MEMBERSHIP
//     (zero teams).
//
// Provisioning is delegated to teamns.Service.EnsureKBRepo (mirror of
// EnsureWorkflowRepo minus drift check and instance branch).

package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// KBEnsureRequest is the POST /api/kb/ensure body. TeamID is optional per
// the user-facing contract — see KB_USER_ENSURE_API.md §1.1.
type KBEnsureRequest struct {
	CodeRepoURL string `json:"code_repo_url"`
	TeamID      string `json:"team_id,omitempty"`
}

// KBEnsureResponse mirrors §接口 9.3 with two extra fields for user-side
// auditability (TeamID + TeamResolution).
type KBEnsureResponse struct {
	KbRepoPath       string              `json:"kb_repo_path"`
	KbCloneURL       string              `json:"kb_clone_url"`
	KbWebURL         string              `json:"kb_web_url"`
	Created          KBEnsureCreated     `json:"created"`
	TeamNSExists     bool                `json:"team_ns_exists"`
	AlgorithmVersion string              `json:"algorithm_version"`
	TeamID           string              `json:"team_id"`
	TeamResolution   string              `json:"team_resolution"` // "implicit_single" | "explicit"
	BotCredentials   *BotCredentialsView `json:"bot_credentials"`
}

// KBEnsureCreated flags whether the kb repo was newly created in this call.
type KBEnsureCreated struct {
	KbRepo bool `json:"kb_repo"`
}

// TeamSummary is a single entry in the user's team list, returned in the
// 409 disambiguation response so the csc client can render a picker.
type TeamSummary struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// TeamResolver abstracts "list the teams the current user belongs to".
// Production impl delegates to cs-user RPC → org-team-service; tests
// inject a stub. The handler never falls back to Gitea org membership
// (KB_USER_ENSURE_API.md §2.3 — avoid double-source-of-truth drift).
type TeamResolver interface {
	// ResolveCurrentUserTeams returns the teams the JWT subject belongs
	// to, in the tenant resolved by middleware.RequireAuth. An empty slice
	// (not nil-as-error) is the legitimate "user belongs to no team"
	// state — handlers map that to 403 NO_TEAM_MEMBERSHIP.
	ResolveCurrentUserTeams(c *gin.Context, subjectID string) ([]TeamSummary, error)
}

// ErrOrgTeamServiceUnavailable — returned by TeamResolver when the upstream
// membership service is unreachable. Handler maps to 503.
var ErrOrgTeamServiceUnavailable = errors.New("handlers: org-team-service unavailable")

// teamResolver is the package-level holder, set via InitTeamResolver.
// Default nil → handler returns 503 ORG_TEAM_SERVICE_UNAVAILABLE so the
// endpoint fails closed until the cs-user RPC is wired.
var teamResolver TeamResolver

// InitTeamResolver wires the user-team resolver. Called once from
// cmd/api/main.go during boot.
func InitTeamResolver(r TeamResolver) {
	teamResolver = r
}

// KBEnsure godoc
// @Summary      Ensure KB repo for current user (user-side)
// @Description  POST /api/kb/ensure — get-or-create kb repo for (code_repo_url, team); JWT auth, team auto-derived or explicit.
// @Tags         kb,user
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body  KBEnsureRequest  true  "KB ensure request"
// @Success      200  {object}  KBEnsureResponse
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      403  {object}  object{error=string,error_code=string,teams=[]TeamSummary,hint=string}
// @Failure      409  {object}  object{error=string,error_code=string,teams=[]TeamSummary,hint=string}
// @Failure      412  {object}  object{error=string,error_code=string}
// @Failure      503  {object}  object{error=string,error_code=string}
// @Router       /api/kb/ensure [post]
func KBEnsure(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	if teamResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "team membership resolver not configured",
			"error_code": "ORG_TEAM_SERVICE_UNAVAILABLE",
		})
		return
	}

	var req KBEnsureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	if err := validateKBEnsureRequest(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "error_code": "INVALID_REQUEST"})
		return
	}

	subjectID := c.GetString(middleware.UserIDKey)
	if subjectID == "" {
		// Should never happen post-RequireAuth, but fail closed.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing subject in JWT", "error_code": "UNAUTHORIZED"})
		return
	}

	// Resolve team_id per KB_USER_ENSURE_API.md §2.
	resolvedTeamID, resolution, teams, httpErr := resolveTeamForKB(c, subjectID, req.TeamID, teamResolver)
	if httpErr != nil {
		c.JSON(httpErr.status, httpErr.body)
		return
	}

	// 1. Verify team ns exists.
	ns, err := lookupTeamNSForKB(c, resolvedTeamID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusPreconditionFailed, gin.H{
				"error":      "team ns not initialized; ask your platform admin to provision the team first",
				"error_code": "TEAM_NS_NOT_INITIALIZED",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 2. Provision kb repo (delegates to teamns.EnsureKBRepo).
	provResult, err := teamnsService.EnsureKBRepo(c.Request.Context(), resolvedTeamID, req.CodeRepoURL)
	if err != nil {
		status, body := mapKBEnsureError(err)
		c.JSON(status, body)
		return
	}

	// 3. Compose URLs + bot creds (mirrors workflow_init step 4-5).
	endpoint, err := resolveTenantGiteaBaseURL(c, ns.TenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	plaintext, err := teamnsService.DecryptBotToken(c.Request.Context(), resolvedTeamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt bot token: " + err.Error()})
		return
	}
	botMeta, _ := lookupBotMetaForKB(c, resolvedTeamID)
	giteaUserID := int64(0)
	giteaUsername := ""
	if botMeta != nil {
		giteaUserID = botMeta.GiteaUserID
		giteaUsername = botMeta.GiteaUsername
	}

	c.JSON(http.StatusOK, KBEnsureResponse{
		KbRepoPath:       provResult.KbRepoPath,
		KbCloneURL:       endpoint + "/" + provResult.KbRepoPath + ".git",
		KbWebURL:         endpoint + "/" + provResult.KbRepoPath,
		Created:          KBEnsureCreated{KbRepo: provResult.KbRepoCreated},
		TeamNSExists:     true,
		AlgorithmVersion: "v2",
		TeamID:           resolvedTeamID,
		TeamResolution:   resolution,
		BotCredentials: &BotCredentialsView{
			GiteaUsername:     giteaUsername,
			GiteaUserID:       giteaUserID,
			Token:             plaintext,
			CloneURLWithToken: composeCloneURLWithToken(endpoint, provResult.KbRepoPath, giteaUsername, plaintext),
		},
	})
	_ = teams // already consumed inside resolveTeamForKB; retained for signature clarity
}

// httpErr is a small carrier for "handler should write this status+body".
type httpErr struct {
	status int
	body   gin.H
}

// resolveTeamForKB implements KB_USER_ENSURE_API.md §2 matrix:
//
//	team_id passed   → verify membership; OK or 403 TEAM_MEMBERSHIP_REQUIRED
//	team_id omitted  → 0/1/multi branches (403 / implicit / 409)
func resolveTeamForKB(c *gin.Context, subjectID, requestedTeamID string, resolver TeamResolver) (teamID, resolution string, teams []TeamSummary, herr *httpErr) {
	teams, err := resolver.ResolveCurrentUserTeams(c, subjectID)
	if err != nil {
		return "", "", nil, &httpErr{
			status: http.StatusServiceUnavailable,
			body:   gin.H{"error": "team membership lookup failed: " + err.Error(), "error_code": "ORG_TEAM_SERVICE_UNAVAILABLE"},
		}
	}

	if strings.TrimSpace(requestedTeamID) != "" {
		// Explicit path — verify membership.
		for _, t := range teams {
			if t.TeamID == requestedTeamID {
				return requestedTeamID, "explicit", teams, nil
			}
		}
		return "", "", teams, &httpErr{
			status: http.StatusForbidden,
			body: gin.H{
				"error":      "current user is not a member of the specified team",
				"error_code": "TEAM_MEMBERSHIP_REQUIRED",
				"team_id":    requestedTeamID,
				"hint":       "ask the team owner to add you, or pick a team you belong to",
			},
		}
	}

	// Implicit path — 0/1/multi.
	switch len(teams) {
	case 0:
		return "", "", nil, &httpErr{
			status: http.StatusForbidden,
			body: gin.H{
				"error":      "current user does not belong to any team; join a team before initializing kb",
				"error_code": "NO_TEAM_MEMBERSHIP",
				"hint":       "ask your platform admin to add you to a team, or check your org-team-service membership",
			},
		}
	case 1:
		return teams[0].TeamID, "implicit_single", teams, nil
	default:
		return "", "", teams, &httpErr{
			status: http.StatusConflict,
			body: gin.H{
				"error":      "current user belongs to multiple teams; specify team_id explicitly",
				"error_code": "TEAM_DISAMBIGUATION_REQUIRED",
				"teams":      teams,
				"hint":       "re-call POST /api/kb/ensure with team_id field set to one of the above",
			},
		}
	}
}

// validateKBEnsureRequest enforces body shape per KB_USER_ENSURE_API.md §1.1.
func validateKBEnsureRequest(req KBEnsureRequest) error {
	if strings.TrimSpace(req.CodeRepoURL) == "" {
		return errors.New("code_repo_url is required")
	}
	return nil
}

// lookupTeamNSForKB mirrors lookupTeamNSForWorkflow — thin wrapper around
// teamnsService.LookupTeamNS.
func lookupTeamNSForKB(c *gin.Context, teamID string) (*models.TeamNamespace, error) {
	return teamnsService.LookupTeamNS(c.Request.Context(), teamID)
}

// lookupBotMetaForKB mirrors lookupBotMetaForWorkflow.
func lookupBotMetaForKB(c *gin.Context, teamID string) (*models.TeamBotCredentials, error) {
	return teamnsService.LookupBotMeta(c.Request.Context(), teamID)
}

// mapKBEnsureError projects teamns provisioning sentinels to HTTP responses
// per KB_USER_ENSURE_API.md §5. Mirrors mapWorkflowInitError minus the
// 409 drift branch (kb has no drift).
func mapKBEnsureError(err error) (int, gin.H) {
	switch {
	case errors.Is(err, teamns.ErrInvalidRequest):
		return http.StatusBadRequest, gin.H{"error": err.Error(), "error_code": "INVALID_REQUEST"}
	case errors.Is(err, teamns.ErrTeamNotFound):
		return http.StatusPreconditionFailed, gin.H{
			"error":      "team ns not initialized; call POST /api/internal/teams first",
			"error_code": "TEAM_NS_NOT_INITIALIZED",
		}
	case errors.Is(err, teamns.ErrTenantGitServerUnresolved):
		return http.StatusServiceUnavailable, gin.H{
			"error":      "tenant git server unavailable",
			"error_code": "ORG_TEAM_SERVICE_UNAVAILABLE",
		}
	case errors.Is(err, teamns.ErrKBRepoProvisioning):
		return http.StatusBadGateway, gin.H{
			"error":      "kb repo provisioning failed: " + err.Error(),
			"error_code": "KB_REPO_PROVISIONING_FAILED",
		}
	default:
		return http.StatusInternalServerError, gin.H{"error": err.Error()}
	}
}
