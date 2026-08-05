package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GitCapabilityResyncAPI queues an authenticated, administrator-triggered
// replay. The worker remains the sole owner of capability index writes.
type GitCapabilityResyncAPI struct{ DB *gorm.DB }

func NewGitCapabilityResyncAPI(db *gorm.DB) *GitCapabilityResyncAPI {
	return &GitCapabilityResyncAPI{DB: db}
}

// ResyncGitCapabilityRepository godoc
// @Summary Queue a Git capability repository resync
// @Tags git-capability-sync
// @Produce json
// @Security BearerAuth
// @Param git_server_id path string true "Stable Git server id the repository belongs to"
// @Param git_repo_id path integer true "Numeric Git repository id, unique only within that server"
// @Success 202 {object} object{status=string,job_id=string,duplicate=bool}
// @Failure 401 {object} object{error=string}
// @Failure 403 {object} object{error=string}
// @Failure 404 {object} object{error=string}
// @Failure 409 {object} object{error=string}
// @Failure 500 {object} object{error=string}
// @Router /admin/git-capability-repositories/{git_server_id}/{git_repo_id}/resync [post]
func (a *GitCapabilityResyncAPI) ResyncGitCapabilityRepository(c *gin.Context) {
	if a == nil || a.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "git capability resync is not configured"})
		return
	}
	// Both halves of the identity, because a Gitea repository id is unique only
	// within one server. Selecting on git_repo_id alone matches whichever server
	// happens to sort first, so on a multi-server deployment this endpoint would
	// queue a job against a DIFFERENT repository and still answer 202 "queued" —
	// the administrator is told the resync started while the intended repository
	// is never touched. The uniqueness constraint on the table is
	// (git_server_id, git_repo_id); this query now uses the same pair, which is
	// also the first half of V4's stable identity quadruple.
	serverID := strings.TrimSpace(c.Param("git_server_id"))
	if serverID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "git_server_id is required"})
		return
	}
	repoID, err := strconv.ParseInt(strings.TrimSpace(c.Param("git_repo_id")), 10, 64)
	if err != nil || repoID <= 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "git_repo_id must be a positive integer"})
		return
	}
	var repo models.GitCapabilityRepository
	if err := a.DB.WithContext(c.Request.Context()).
		Where("git_server_id = ? AND git_repo_id = ?", serverID, repoID).First(&repo).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Git-backed repository not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load Git-backed repository failed"})
		return
	}
	if strings.TrimSpace(repo.GitServerID) == "" || repo.GitRepoID <= 0 || strings.TrimSpace(repo.FullName) == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "repository has no valid Git binding"})
		return
	}
	now := time.Now().UTC()
	// The minute bucket deduplicates retries while pending/running, yet permits
	// a fresh manual replay after a completed job in a later bucket.
	delivery := "manual:" + strconv.FormatInt(repo.GitRepoID, 10) + ":" + strconv.FormatInt(now.Unix()/60, 10)
	job := models.GitCapabilitySyncJob{ID: uuid.NewString(), GitServerID: repo.GitServerID, DeliveryID: delivery,
		RepoID: repo.GitRepoID, RepoFullName: repo.FullName, DefaultBranch: repo.DefaultBranch,
		Ref: repo.DefaultBranch, AfterSHA: repo.LastSyncedCommit, Status: models.GitCapabilitySyncJobStatusPending,
		MaxAttempts: 3, ScheduledAt: now, CreatedAt: now}
	result := a.DB.WithContext(c.Request.Context()).Where("git_server_id = ? AND delivery_id = ?", repo.GitServerID, delivery).FirstOrCreate(&job)
	if result.Error != nil {
		// Concurrent retries can race on the delivery unique key. Once the
		// competing transaction commits, re-query its row and report the same
		// idempotent 202 response instead of surfacing a transient 500.
		if isUniqueConstraintError(result.Error) {
			var existing models.GitCapabilitySyncJob
			if queryErr := a.DB.WithContext(c.Request.Context()).Where("git_server_id = ? AND delivery_id = ?", repo.GitServerID, delivery).First(&existing).Error; queryErr == nil {
				c.JSON(http.StatusAccepted, gin.H{"status": "queued", "job_id": existing.ID, "duplicate": true})
				return
			}
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "queue Git capability resync failed"})
		return
	}
	duplicate := result.RowsAffected == 0
	c.JSON(http.StatusAccepted, gin.H{"status": "queued", "job_id": job.ID, "duplicate": duplicate})
}
