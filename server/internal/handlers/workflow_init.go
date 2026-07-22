// Package handlers — workflow/init handler.
//
// POST /api/internal/workflow/init is the single entry point that the
// workflow orchestrator hits to (a) ensure the team's wf-<def> type repo
// exists with main + inst-* branch protection, (b) ensure the per-instance
// branch exists, and (c) receive the bot plaintext token for git auth.
//
// This file's handler is thin — it parses the body, calls into teamns +
// workflow packages, and serializes the response. Path computation lives
// in internal/workflow/paths.go (deterministic pure function per
// WORKFLOW_REPO_PATH_ALGORITHM.md v2.0); bot token retrieval lives in
// teamns.Service.DecryptBotToken; Gitea-side provisioning (repo + branch
// protection + snapshot + instance branch) lives in
// teamns.Service.EnsureWorkflowRepo.

package handlers

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/teamns"
	"github.com/costrict/costrict-web/server/internal/workflow"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// WorkflowInitRequest is the POST /api/internal/workflow/init body.
type WorkflowInitRequest struct {
	WorkflowDefSlug    string `json:"workflow_def_slug"`
	InstanceID         string `json:"instance_id"`
	TeamID             string `json:"team_id"`
	DefinitionSnapshot string `json:"definition_snapshot"`
}

// WorkflowInitResponse is the response shape per doc §8.3.
type WorkflowInitResponse struct {
	WfRepoPath       string              `json:"wf_repo_path"`
	WfCloneURL       string              `json:"wf_clone_url"`
	WfWebURL         string              `json:"wf_web_url"`
	InstanceBranch   string              `json:"instance_branch"`
	Created          WorkflowInitCreated `json:"created"`
	TeamNSExists     bool                `json:"team_ns_exists"`
	AlgorithmVersion string              `json:"algorithm_version"`
	BotCredentials   *BotCredentialsView `json:"bot_credentials"`
}

// WorkflowInitCreated flags which sub-ops ran in this call.
type WorkflowInitCreated struct {
	TypeRepo       bool `json:"type_repo"`
	InstanceBranch bool `json:"instance_branch"`
}

// BotCredentialsView is the response shape for the bot creds subset
// workflow_init returns. CloneURLWithToken embeds the plaintext PAT —
// the handler MUST NOT log this value; it lives only in the JSON response.
type BotCredentialsView struct {
	GiteaUsername     string `json:"gitea_username"`
	GiteaUserID       int64  `json:"gitea_user_id"`
	Token             string `json:"token"`
	CloneURLWithToken string `json:"clone_url_with_token"`
}

// WorkflowInit godoc
// @Summary      Initialize workflow type repo + instance branch
// @Description  POST /api/internal/workflow/init — get-or-create wf-<def> type repo + inst-<short> branch, return bot credentials for git auth.
// @Tags         internal,workflow
// @Accept       json
// @Produce      json
// @Security     InternalAuth
// @Param        body  body  WorkflowInitRequest  true  "Workflow init request"
// @Success      200  {object}  WorkflowInitResponse
// @Failure      400  {object}  object{error=string,error_code=string}
// @Failure      412  {object}  object{error=string,error_code=string}
// @Router       /api/internal/workflow/init [post]
func WorkflowInit(c *gin.Context) {
	if teamnsDisabled(c) {
		return
	}
	var req WorkflowInitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "error_code": "INVALID_REQUEST"})
		return
	}
	if err := validateWorkflowInitRequest(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "error_code": "INVALID_REQUEST"})
		return
	}

	// 1. Verify team ns exists.
	ns, err := lookupTeamNSForWorkflow(c, req.TeamID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusPreconditionFailed, gin.H{
				"error":      "team ns not initialized; call POST /api/internal/teams first",
				"error_code": "TEAM_NS_NOT_INITIALIZED",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 2. Compute deterministic paths.
	wfRepoPath, err := workflow.WfRepoPath(req.WorkflowDefSlug, req.TeamID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "error_code": "INVALID_REQUEST"})
		return
	}
	instanceBranch, err := workflow.WfBranchName(req.InstanceID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "error_code": "INVALID_REQUEST"})
		return
	}

	// 3. Decrypt bot token plaintext (per doc §8.3 token recurrence rule).
	plaintext, err := teamnsService.DecryptBotToken(c.Request.Context(), req.TeamID)
	if err != nil {
		if errors.Is(err, teamns.ErrTeamNotFound) {
			c.JSON(http.StatusPreconditionFailed, gin.H{
				"error":      "team has no bot credentials; team ns not fully initialized",
				"error_code": "TEAM_NS_NOT_INITIALIZED",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 4. Resolve tenant git server endpoint for URL composition.
	endpoint, err := resolveTenantGiteaBaseURL(c, ns.TenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	cloneURL := endpoint + "/" + wfRepoPath + ".git"
	webURL := endpoint + "/" + wfRepoPath

	// 5. Look up bot metadata for the response view.
	botMeta, _ := lookupBotMetaForWorkflow(c, req.TeamID)
	giteaUserID := int64(0)
	giteaUsername := ""
	if botMeta != nil {
		giteaUserID = botMeta.GiteaUserID
		giteaUsername = botMeta.GiteaUsername
	}

	// Provision the Gitea side: type repo + definition_snapshot +
	// branch protection (main + inst-*) + instance branch. Idempotent —
	// re-running for an already-provisioned repo is a no-op (Created flags
	// all false). Definition drift between caller's snapshot and main HEAD
	// is the one non-idempotent error: 409 DEFINITION_DRIFT.
	provResult, err := teamnsService.EnsureWorkflowRepo(c.Request.Context(),
		req.TeamID, req.WorkflowDefSlug, req.DefinitionSnapshot, req.InstanceID)
	if err != nil {
		// Map teamns sentinels to HTTP codes per doc §8.4.
		status, body := mapWorkflowInitError(err)
		c.JSON(status, body)
		return
	}

	c.JSON(http.StatusOK, WorkflowInitResponse{
		WfRepoPath:     wfRepoPath,
		WfCloneURL:     cloneURL,
		WfWebURL:       webURL,
		InstanceBranch: instanceBranch,
		Created: WorkflowInitCreated{
			TypeRepo:       provResult.TypeRepoCreated,
			InstanceBranch: provResult.InstanceBranchCreated,
		},
		TeamNSExists:     true,
		AlgorithmVersion: "v2",
		BotCredentials: &BotCredentialsView{
			GiteaUsername:     giteaUsername,
			GiteaUserID:       giteaUserID,
			Token:             plaintext,
			CloneURLWithToken: composeCloneURLWithToken(endpoint, wfRepoPath, giteaUsername, plaintext),
		},
	})
}

// validateWorkflowInitRequest enforces the body shape per doc §8.2.
func validateWorkflowInitRequest(req WorkflowInitRequest) error {
	if strings.TrimSpace(req.WorkflowDefSlug) == "" {
		return errors.New("workflow_def_slug is required")
	}
	if !uuidShape.MatchString(req.InstanceID) {
		return errors.New("instance_id must be a UUID")
	}
	if !uuidShape.MatchString(req.TeamID) {
		return errors.New("team_id must be a UUID")
	}
	return nil
}

// mapWorkflowInitError translates teamns provisioning errors to the HTTP
// status + error_code shape from doc §8.4. The drift case is the only
// 4xx-class outcome from a fully-validated request: 409 with code
// DEFINITION_DRIFT signals the caller that main HEAD has a snapshot that
// doesn't match their input, and they must reconcile (typically by
// re-reading the canonical snapshot from upstream) before retrying.
//
// Other failures (git-side 5xx, network) collapse to 502 — we deliberately
// don't expose the upstream error to avoid leaking backend topology.
func mapWorkflowInitError(err error) (int, gin.H) {
	switch {
	case errors.Is(err, teamns.ErrDefinitionDrift):
		return http.StatusConflict, gin.H{
			"error_code": "DEFINITION_DRIFT",
			"message":    err.Error(),
		}
	case errors.Is(err, teamns.ErrInvalidRequest):
		return http.StatusBadRequest, gin.H{
			"error_code": "INVALID_REQUEST",
			"message":    err.Error(),
		}
	case errors.Is(err, teamns.ErrTeamNotFound):
		return http.StatusNotFound, gin.H{
			"error_code": "TEAM_NOT_FOUND",
			"message":    err.Error(),
		}
	case errors.Is(err, teamns.ErrTenantGitServerUnresolved):
		return http.StatusServiceUnavailable, gin.H{
			"error_code": "TENANT_GIT_SERVER_UNRESOLVED",
			"message":    err.Error(),
		}
	default:
		// ErrWorkflowRepoProvisioning and any unmapped error — collapse
		// to 502 to avoid leaking backend topology.
		return http.StatusBadGateway, gin.H{
			"error_code": "WORKFLOW_REPO_PROVISIONING_FAILED",
			"message":    err.Error(),
		}
	}
}

// uuidShape mirrors workflow.uuidRe at the handler layer (the handler doesn't
// import workflow for the regex — it only imports the algorithm functions).
// Keep in sync with workflow.uuidRe and teamns.uuidRe.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// lookupTeamNSForWorkflow fetches the team_ns row. Returns gorm.ErrRecordNotFound
// when the row is missing — handler maps that to 412 TEAM_NS_NOT_INITIALIZED.
//
// The handler accesses the teamns.Service's db via a new exposed accessor
// (TeamNSLookup); we deliberately avoid leaking gorm.DB to keep the handler
// thin. If teamns.Service ever loses its DB handle, this becomes an
// interface call.
func lookupTeamNSForWorkflow(c *gin.Context, teamID string) (*models.TeamNamespace, error) {
	return teamnsService.LookupTeamNS(c.Request.Context(), teamID)
}

// lookupBotMetaForWorkflow fetches bot creds metadata (no plaintext) for
// the response view.
func lookupBotMetaForWorkflow(c *gin.Context, teamID string) (*models.TeamBotCredentials, error) {
	return teamnsService.LookupBotMeta(c.Request.Context(), teamID)
}

// resolveTenantGiteaBaseURL fetches the Gitea endpoint via the teamns
// service's gitsync resolver.
func resolveTenantGiteaBaseURL(c *gin.Context, tenantID string) (string, error) {
	return teamnsService.ResolveGiteaBaseURL(c.Request.Context(), tenantID)
}

// composeCloneURLWithToken builds https://<user>:<token>@<host>/<path>.git
// for the orchestrator to feed straight into `git clone`. Token is embedded
// ONLY in this transient response; never logged.
func composeCloneURLWithToken(endpoint, wfRepoPath, user, token string) string {
	if endpoint == "" || user == "" || token == "" {
		return ""
	}
	// Insert credentials after the scheme.
	idx := strings.Index(endpoint, "://")
	if idx < 0 {
		return ""
	}
	scheme := endpoint[:idx]
	rest := endpoint[idx+3:]
	return scheme + "://" + user + ":" + token + "@" + rest + "/" + wfRepoPath + ".git"
}
