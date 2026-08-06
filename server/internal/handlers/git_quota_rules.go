// Platform-admin CRUD over git_quota_rules (Gitea fork integration FI-4).
//
// These rows are the source of truth for the push-quota snapshot that
// GiteaConfigSyncWorker pushes into each Gitea server's memory. Editing here
// does NOT reach Gitea synchronously — the worker's next reconcile carries it,
// so a change is visible on the Git server within one tick.
//
// Two things this surface deliberately does not offer, because the fork gives
// no way to honour them:
//
//   - the GLOBAL default tier. It lives in Gitea's app.ini as
//     [costrict] QUOTA_DEFAULT_MAX_FILE_SIZE_MB / QUOTA_DEFAULT_REPO_QUOTA_MB
//     and cannot be pushed. Changing it is a Gitea config change plus a
//     restart. Looking for a "global" row here will not find one.
//   - per-capability-type differentiation. The fork's rule has no such field.
//
// Route surface (all under /api/admin, platform_admin only):
//
//	GET    /api/admin/git-quota-rules?git_server_id=…
//	PUT    /api/admin/git-quota-rules                     — create or replace one rule
//	DELETE /api/admin/git-quota-rules?git_server_id=&owner=&repo=

package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GitQuotaRuleAPI is the receiver for the quota-rule endpoints.
type GitQuotaRuleAPI struct {
	DB *gorm.DB
}

// NewGitQuotaRuleAPI binds the handler to the supplied pool.
func NewGitQuotaRuleAPI(db *gorm.DB) *GitQuotaRuleAPI {
	return &GitQuotaRuleAPI{DB: db}
}

// gitQuotaRuleUpsertRequest is the PUT body.
//
// The two limits are pointers so that "omitted" is distinguishable from
// "zero" — 0 is not a neutral value here, it is the fork's encoding of
// "unlimited", and silently defaulting an omitted field to it would remove a
// limit that the operator never asked to remove.
type gitQuotaRuleUpsertRequest struct {
	GitServerID   string `json:"git_server_id" binding:"required"`
	Owner         string `json:"owner" binding:"required"`
	Repo          string `json:"repo"`
	MaxFileSizeMB *int64 `json:"max_file_size_mb"`
	RepoQuotaMB   *int64 `json:"repo_quota_mb"`
}

// gitQuotaRuleResponse is what callers read back.
type gitQuotaRuleResponse struct {
	GitServerID   string `json:"git_server_id"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	MaxFileSizeMB int64  `json:"max_file_size_mb"`
	RepoQuotaMB   int64  `json:"repo_quota_mb"`
	UpdatedBy     string `json:"updated_by"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func toGitQuotaRuleResponse(rule *models.GitQuotaRule) gitQuotaRuleResponse {
	return gitQuotaRuleResponse{
		GitServerID:   rule.GitServerID,
		Owner:         rule.Owner,
		Repo:          rule.Repo,
		MaxFileSizeMB: rule.MaxFileSizeMB,
		RepoQuotaMB:   rule.RepoQuotaMB,
		UpdatedBy:     rule.UpdatedBy,
		CreatedAt:     rule.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:     rule.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ListGitQuotaRules godoc
// @Summary  List push-quota rules
// @Tags     admin
// @Produce  json
// @Param    git_server_id  query  string  false  "restrict to one Git server"
// @Success  200  {object}  object{rules=[]handlers.gitQuotaRuleResponse,total=int}
// @Router   /api/admin/git-quota-rules [get]
func (a *GitQuotaRuleAPI) ListGitQuotaRules(c *gin.Context) {
	if a == nil || a.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota rule store unavailable"})
		return
	}
	query := a.DB.WithContext(c.Request.Context()).Model(&models.GitQuotaRule{})
	if serverID := strings.TrimSpace(c.Query("git_server_id")); serverID != "" {
		query = query.Where("git_server_id = ?", serverID)
	}
	var rules []models.GitQuotaRule
	if err := query.Order("git_server_id ASC, owner ASC, repo ASC").Find(&rules).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list quota rules"})
		return
	}
	out := make([]gitQuotaRuleResponse, 0, len(rules))
	for i := range rules {
		out = append(out, toGitQuotaRuleResponse(&rules[i]))
	}
	c.JSON(http.StatusOK, gin.H{"rules": out, "total": len(out)})
}

// UpsertGitQuotaRule godoc
// @Summary  Create or replace one push-quota rule
// @Tags     admin
// @Accept   json
// @Produce  json
// @Param    body  body  handlers.gitQuotaRuleUpsertRequest  true  "quota rule"
// @Success  200  {object}  handlers.gitQuotaRuleResponse
// @Failure  400  {object}  object{error=string}
// @Failure  404  {object}  object{error=string}
// @Router   /api/admin/git-quota-rules [put]
func (a *GitQuotaRuleAPI) UpsertGitQuotaRule(c *gin.Context) {
	if a == nil || a.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota rule store unavailable"})
		return
	}
	var req gitQuotaRuleUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "git_server_id and owner are required"})
		return
	}
	serverID := strings.TrimSpace(req.GitServerID)
	owner := strings.TrimSpace(req.Owner)
	// Repo is NOT trimmed to empty-means-something-else: "" is the fork's
	// owner-level sentinel, and any other value must match Gitea's stored
	// Repository.Name exactly, including case.
	repo := strings.TrimSpace(req.Repo)
	if owner == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "owner is required"})
		return
	}
	if req.MaxFileSizeMB == nil || req.RepoQuotaMB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_file_size_mb and repo_quota_mb are required (0 means unlimited)"})
		return
	}
	if *req.MaxFileSizeMB < 0 || *req.RepoQuotaMB < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota limits must not be negative"})
		return
	}

	ctx := c.Request.Context()
	var server models.GitServer
	if err := a.DB.WithContext(ctx).Select("server_id, kind").
		First(&server, "server_id = ?", serverID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "git_server not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load git_server"})
		return
	}
	if server.Kind != models.GitServerKindGitea {
		// Push quotas are a CoStrict Gitea fork feature; storing a rule for a
		// server that can never receive it would read as configured protection
		// that does not exist.
		c.JSON(http.StatusBadRequest, gin.H{"error": "push quotas are only supported on gitea servers"})
		return
	}

	rule := models.GitQuotaRule{
		GitServerID:   serverID,
		Owner:         owner,
		Repo:          repo,
		MaxFileSizeMB: *req.MaxFileSizeMB,
		RepoQuotaMB:   *req.RepoQuotaMB,
		UpdatedBy:     c.GetString(middleware.UserIDKey),
	}
	if err := a.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "git_server_id"}, {Name: "owner"}, {Name: "repo"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"max_file_size_mb", "repo_quota_mb", "updated_by", "updated_at",
		}),
	}).Create(&rule).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save quota rule"})
		return
	}

	// Re-read so the response carries the stored timestamps rather than the
	// in-memory ones, which differ on an update.
	var stored models.GitQuotaRule
	if err := a.DB.WithContext(ctx).
		Where("git_server_id = ? AND owner = ? AND repo = ?", serverID, owner, repo).
		First(&stored).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read back quota rule"})
		return
	}
	c.JSON(http.StatusOK, toGitQuotaRuleResponse(&stored))
}

// DeleteGitQuotaRule godoc
// @Summary  Delete one push-quota rule
// @Tags     admin
// @Produce  json
// @Param    git_server_id  query  string  true   "Git server id"
// @Param    owner          query  string  true   "owner"
// @Param    repo           query  string  false  "repository name; omit for the owner-level rule"
// @Success  204  {string}  string  "No Content"
// @Failure  400  {object}  object{error=string}
// @Failure  404  {object}  object{error=string}
// @Router   /api/admin/git-quota-rules [delete]
func (a *GitQuotaRuleAPI) DeleteGitQuotaRule(c *gin.Context) {
	if a == nil || a.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota rule store unavailable"})
		return
	}
	serverID := strings.TrimSpace(c.Query("git_server_id"))
	owner := strings.TrimSpace(c.Query("owner"))
	repo := strings.TrimSpace(c.Query("repo"))
	if serverID == "" || owner == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "git_server_id and owner are required"})
		return
	}
	result := a.DB.WithContext(c.Request.Context()).
		Where("git_server_id = ? AND owner = ? AND repo = ?", serverID, owner, repo).
		Delete(&models.GitQuotaRule{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete quota rule"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota rule not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
