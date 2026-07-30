// Package handlers — user-facing KB ensure handler.
//
// POST /api/kb/ensure is the user-side entry point (csc direct call) that
// resolves the caller's identity from the JWT and ensures the kb repo
// backing (code_repo_url, space_type) exists.
//
// Three modes:
//
//   - Discovery (space_type omitted): returns team + personal space overview,
//     no repo is created.
//   - Team mode (space_type="team"): resolves team membership, creates repo
//     under t-<team_short> org (existing behavior).
//   - User mode (space_type="user"): creates repo under the user's personal
//     Gitea namespace, fallback-provisioning the Gitea account if needed.
//
// Auth: user JWT (middleware.RequireAuth sets UserIDKey).

package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/costrict/costrict-web/server/internal/user"
	"github.com/costrict/costrict-web/server/internal/userspace"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// KBEnsureRequest is the POST /api/kb/ensure body.
//
// Modes:
//   - Discovery:  code_repo_url only, space_type omitted → returns space overview
//   - Team:       space_type="team", space_id=team_id (optional, auto-derived)
//   - User:       space_type="user", space_id=subject_id (optional, auto-derived from JWT)
type KBEnsureRequest struct {
	CodeRepoURL string `json:"code_repo_url"`
	SpaceType   string `json:"space_type,omitempty"` // "" | "team" | "user"
	SpaceID     string `json:"space_id,omitempty"`   // team_id or user_subject_id
}

// KBEnsureResponse carries the KB ensure result. In discovery mode
// (space_type omitted), only SpaceType, TeamSpace and PersonalSpace are
// populated — no repo is created.
type KBEnsureResponse struct {
	// --- Repo info (populated only when a repo was created) ---
	KbRepoPath       string              `json:"kb_repo_path,omitempty"`
	KbCloneURL       string              `json:"kb_clone_url,omitempty"`
	KbWebURL         string              `json:"kb_web_url,omitempty"`
	Created          *KBEnsureCreated    `json:"created,omitempty"`
	AlgorithmVersion string              `json:"algorithm_version,omitempty"`
	BotCredentials   *BotCredentialsView `json:"bot_credentials,omitempty"`

	// --- Space overview (always populated) ---
	SpaceType     string                  `json:"space_type"`              // "" (discovery) | "team" | "user"
	TeamSpace     *TeamSpaceInfo       `json:"team_space,omitempty"`     // user's default team
	PersonalSpace *userspace.UserSpaceInfo `json:"personal_space,omitempty"` // user's personal space
}

// TeamSpaceInfo carries the user's team namespace summary for discovery mode.
type TeamSpaceInfo struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	OrgName     string `json:"org_name"`
	Status      string `json:"status"`
	Role        string `json:"role,omitempty"`
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
// Production impl (TeamDirectoryResolver) forwards the caller's Casdoor
// JWT to the team-directory backend; tests inject a stub. The handler
// never falls back to Gitea org membership (KB_USER_ENSURE_API.md §2.3
// — avoid double-source-of-truth drift).
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

// userspaceService is the package-level holder, set via InitUserSpaceService.
var userspaceService *userspace.Service

// InitUserSpaceService wires the personal-space service. Called once from
// cmd/api/main.go during boot.
func InitUserSpaceService(svc *userspace.Service) {
	userspaceService = svc
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing subject in JWT", "error_code": "UNAUTHORIZED"})
		return
	}

	// ---- Discovery mode: space_type omitted, return space overview ----
	if req.SpaceType == "" {
		handleDiscovery(c, subjectID, req.CodeRepoURL)
		return
	}

	// ---- Team mode ----
	if req.SpaceType == "team" {
		handleTeamEnsure(c, subjectID, req)
		return
	}

	// ---- User mode ----
	if req.SpaceType == "user" {
		handleUserEnsure(c, subjectID, req)
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{
		"error":      "invalid space_type; must be team or user",
		"error_code": "INVALID_REQUEST",
	})
}

// handleDiscovery returns team + personal space overview without creating a repo.
func handleDiscovery(c *gin.Context, subjectID, codeRepoURL string) {
	tenantID := resolveTenantID(c)
	if tenantID == "" {
		tenantID = "default"
	}

	resp := KBEnsureResponse{
		SpaceType: "",
	}

	// Team space.
	teams, err := teamResolver.ResolveCurrentUserTeams(c, subjectID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "team membership lookup failed: " + err.Error(),
			"error_code": "ORG_TEAM_SERVICE_UNAVAILABLE",
		})
		return
	}
	if len(teams) > 0 {
		// Pick the first team as the "default" team.
		t := teams[0]
		ns, _ := lookupTeamNSForKB(c, t.TeamID)
		ts := &TeamSpaceInfo{
			TeamID:      t.TeamID,
			DisplayName: t.DisplayName,
			Role:        t.Role,
		}
		if ns != nil {
			ts.OrgName = ns.TeamNSOrg
			ts.Status = ns.Status
		}
		resp.TeamSpace = ts
	}

	// Personal space.
	if userspaceService != nil {
		us, err := userspaceService.GetUserSpace(c.Request.Context(), subjectID, tenantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if us == nil {
			us = &userspace.UserSpaceInfo{
				UserSubjectID: subjectID,
				Ready:         false,
			}
		}
		resp.PersonalSpace = us
	}

	c.JSON(http.StatusOK, resp)
}

// handleTeamEnsure runs the existing team-based KB repo provisioning.
func handleTeamEnsure(c *gin.Context, subjectID string, req KBEnsureRequest) {
	if teamResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "team membership resolver not configured",
			"error_code": "ORG_TEAM_SERVICE_UNAVAILABLE",
		})
		return
	}

	resolvedTeamID, resolution, teams, httpErr := resolveTeamForKB(c, subjectID, req.SpaceID, teamResolver)
	if httpErr != nil {
		c.JSON(httpErr.status, httpErr.body)
		return
	}

	// Verify / auto-provision team ns.
	ns, err := lookupTeamNSForKB(c, resolvedTeamID)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		displayName := pickTeamDisplayName(teams, resolvedTeamID)
		if displayName == "" {
			displayName = "team-" + shortID(resolvedTeamID)
		}
		provReq := teamns.CreateTeamRequest{
			TeamID:          resolvedTeamID,
			TeamDisplayName: displayName,
			Creator:         user.UserRef{UserID: subjectID},
		}
		if _, perr := teamnsService.CreateTeam(c.Request.Context(), provReq); perr != nil {
			status, body := mapKBEnsureError(perr)
			c.JSON(status, body)
			return
		}
		ns, err = lookupTeamNSForKB(c, resolvedTeamID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	provResult, err := teamnsService.EnsureKBRepo(c.Request.Context(), resolvedTeamID, req.CodeRepoURL)
	if err != nil {
		status, body := mapKBEnsureError(err)
		c.JSON(status, body)
		return
	}

	endpoint, err := resolveTenantGiteaBaseURL(c, ns.TenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	plaintext, err := teamnsService.DecryptBotToken(c.Request.Context(), resolvedTeamID)
	if err != nil {
		status, code, msg := mapBotCredentialsError(err)
		c.JSON(status, gin.H{"error": msg, "error_code": code})
		return
	}
	botMeta, _ := lookupBotMetaForKB(c, resolvedTeamID)
	giteaUserID := int64(0)
	giteaUsername := ""
	if botMeta != nil {
		giteaUserID = botMeta.GiteaUserID
		giteaUsername = botMeta.GiteaUsername
	}

	// Also collect personal space.
	tenantID := resolveTenantID(c)
	if tenantID == "" {
		tenantID = "default"
	}
	var personalSpace *userspace.UserSpaceInfo
	if userspaceService != nil {
		personalSpace, _ = userspaceService.GetUserSpace(c.Request.Context(), subjectID, tenantID)
	}

	c.JSON(http.StatusOK, KBEnsureResponse{
		KbRepoPath:       provResult.KbRepoPath,
		KbCloneURL:       endpoint + "/" + provResult.KbRepoPath + ".git",
		KbWebURL:         endpoint + "/" + provResult.KbRepoPath,
		Created:          &KBEnsureCreated{KbRepo: provResult.KbRepoCreated},
		AlgorithmVersion: "v2",
		BotCredentials: &BotCredentialsView{
			GiteaUsername:     giteaUsername,
			GiteaUserID:       giteaUserID,
			Token:             plaintext,
			CloneURLWithToken: composeCloneURLWithToken(endpoint, provResult.KbRepoPath, giteaUsername, plaintext),
		},
		SpaceType:     "team",
		PersonalSpace: personalSpace,
	})
	_ = resolution // consumed in resolveTeamForKB
	_ = teams
}

// handleUserEnsure runs personal-space KB repo provisioning.
func handleUserEnsure(c *gin.Context, subjectID string, req KBEnsureRequest) {
	if userspaceService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "personal space service not configured",
			"error_code": "PERSONAL_SPACE_UNAVAILABLE",
		})
		return
	}

	// Enforce: user can only create in their own personal space.
	if req.SpaceID != "" && req.SpaceID != subjectID {
		c.JSON(http.StatusForbidden, gin.H{
			"error":      "cannot create repo in another user's personal space",
			"error_code": "FORBIDDEN",
		})
		return
	}

	tenantID := resolveTenantID(c)
	if tenantID == "" {
		tenantID = "default"
	}

	provResult, err := userspaceService.EnsureUserRepo(c.Request.Context(), subjectID, tenantID, req.CodeRepoURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "error_code": "KB_REPO_PROVISIONING_FAILED"})
		return
	}

	endpoint, err := resolveTenantGiteaBaseURL(c, tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	plaintext, err := userspaceService.DecryptUserToken(c.Request.Context(), subjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      "failed to decrypt user token: " + err.Error(),
			"error_code": "BOT_TOKEN_DECRYPT_FAILED",
		})
		return
	}

	userMeta, _ := userspaceService.LookupUserMeta(c.Request.Context(), subjectID)
	giteaUserID := int64(0)
	giteaUsername := ""
	if userMeta != nil {
		giteaUserID = userMeta.GitUserID
		giteaUsername = userMeta.GitUsername
	}

	// Also collect team space.
	var teamSpace *TeamSpaceInfo
	if teamResolver != nil {
		teams, err := teamResolver.ResolveCurrentUserTeams(c, subjectID)
		if err == nil && len(teams) > 0 {
			t := teams[0]
			teamSpace = &TeamSpaceInfo{
				TeamID:      t.TeamID,
				DisplayName: t.DisplayName,
				Role:        t.Role,
			}
			if ns, _ := lookupTeamNSForKB(c, t.TeamID); ns != nil {
				teamSpace.OrgName = ns.TeamNSOrg
				teamSpace.Status = ns.Status
			}
		}
	}

	c.JSON(http.StatusOK, KBEnsureResponse{
		KbRepoPath:       provResult.KbRepoPath,
		KbCloneURL:       endpoint + "/" + provResult.KbRepoPath + ".git",
		KbWebURL:         endpoint + "/" + provResult.KbRepoPath,
		Created:          &KBEnsureCreated{KbRepo: provResult.KbRepoCreated},
		AlgorithmVersion: "v2",
		BotCredentials: &BotCredentialsView{
			GiteaUsername:     giteaUsername,
			GiteaUserID:       giteaUserID,
			Token:             plaintext,
			CloneURLWithToken: composeCloneURLWithToken(endpoint, provResult.KbRepoPath, giteaUsername, plaintext),
		},
		SpaceType: "user",
		TeamSpace: teamSpace,
	})
}

// resolveTenantID extracts the tenant_id from the gin context (set by
// TenantContext middleware). Falls back to "default" when unset.
func resolveTenantID(c *gin.Context) string {
	tid := c.GetString("tenant_id")
	if tid == "" {
		tid = "default"
	}
	return tid
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

// pickTeamDisplayName returns the display name the directory backend
// carried for teamID. Returns "" when unknown — KBEnsure then falls back
// to a placeholder derived from teamID (display name is non-core cached
// metadata; the directory backend stays authoritative).
func pickTeamDisplayName(teams []TeamSummary, teamID string) string {
	for _, t := range teams {
		if t.TeamID == teamID {
			return t.DisplayName
		}
	}
	return ""
}

// shortID returns the first 8 chars of a UUID-ish id for use as a
// display-name fallback. Returns the input unchanged if shorter.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// mapBotCredentialsError classifies the three real failure modes of
// DecryptBotToken so the response carries actionable context. None are
// auto-recoverable within the request — they signal prior-state damage
// (interrupted provisioning, key drift, or DB issues) that needs an
// operator.
func mapBotCredentialsError(err error) (status int, code string, msg string) {
	switch {
	case errors.Is(err, teamns.ErrTeamNotFound):
		// team_ns row exists but team_bot_credentials row is missing —
		// CreateTeam was interrupted mid-way. Re-running CreateTeam
		// (or POST /api/internal/teams) re-issues the bot.
		return http.StatusPreconditionFailed, "BOT_CREDENTIALS_MISSING",
			"team_ns is bound but bot credentials are missing — provision was interrupted; ask your platform admin to re-provision the team bot"
	case errors.Is(err, teamns.ErrBotTokenDecrypt):
		// AES-GCM Open failed — almost always CS_BOT_TOKEN_KEY drift
		// (key rotated without re-encrypting existing creds) or DB
		// row corruption.
		return http.StatusInternalServerError, "BOT_TOKEN_DECRYPT_FAILED",
			"failed to decrypt bot token — likely CS_BOT_TOKEN_KEY mismatch or corrupted credential row; ask your platform admin to verify the key and re-provision if needed"
	case errors.Is(err, teamns.ErrBotCredsLookup):
		// DB-level or transport failure on the credentials lookup.
		// Surface verbatim for ops.
		return http.StatusInternalServerError, "BOT_CREDENTIALS_LOOKUP_FAILED",
			"failed to load bot credentials: " + err.Error()
	default:
		// Unexpected / unwrapped error — surface verbatim rather than
		// misclassifying under a typed branch.
		return http.StatusInternalServerError, "BOT_CREDENTIALS_LOOKUP_FAILED",
			"failed to load bot credentials: " + err.Error()
	}
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
