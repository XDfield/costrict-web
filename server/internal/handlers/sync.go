package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

var (
	JobService    *services.JobService
	SyncScheduler interface {
		RegisterRegistry(registry *models.CapabilityRegistry) error
		UnregisterRegistry(registryID string)
	}
)

func getRegistryIDForOrg(orgID string) (string, error) {
	db := database.GetDB()
	var registry models.CapabilityRegistry
	if err := db.Where("org_id = ? AND source_type = 'external'", orgID).First(&registry).Error; err != nil {
		if err2 := db.Where("org_id = ?", orgID).First(&registry).Error; err2 != nil {
			return "", err2
		}
	}
	return registry.ID, nil
}

// TriggerOrgSync godoc
// @Summary      Trigger org sync
// @Description  Manually trigger a sync job for the organization's registry
// @Tags         sync
// @Produce      json
// @Param        id      path   string  true  "Organization ID"
// @Param        dryRun  query  bool    false "Dry run mode"
// @Success      202  {object}  object{jobId=string,status=string}
// @Failure      404  {object}  object{error=string}
// @Failure      409  {object}  object{error=string}
// @Router       /organizations/{id}/sync [post]
func TriggerOrgSync(c *gin.Context) {
	orgID := c.Param("id")
	registryID, err := getRegistryIDForOrg(orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this organization"})
		return
	}
	triggerSync(c, registryID)
}

// CancelOrgSync godoc
// @Summary      Cancel org sync
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  object{message=string}
// @Router       /organizations/{id}/sync/cancel [post]
func CancelOrgSync(c *gin.Context) {
	orgID := c.Param("id")
	registryID, err := getRegistryIDForOrg(orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this organization"})
		return
	}
	cancelSync(c, registryID)
}

// GetOrgSyncStatus godoc
// @Summary      Get org sync status
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  object{}
// @Router       /organizations/{id}/sync-status [get]
func GetOrgSyncStatus(c *gin.Context) {
	orgID := c.Param("id")
	registryID, err := getRegistryIDForOrg(orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this organization"})
		return
	}
	getSyncStatus(c, registryID)
}

// ListOrgSyncLogs godoc
// @Summary      List org sync logs
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  object{logs=[]models.SyncLog,total=integer}
// @Router       /organizations/{id}/sync-logs [get]
func ListOrgSyncLogs(c *gin.Context) {
	orgID := c.Param("id")
	registryID, err := getRegistryIDForOrg(orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this organization"})
		return
	}
	listSyncLogs(c, registryID)
}

// ListOrgSyncJobs godoc
// @Summary      List org sync jobs
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  object{jobs=[]models.SyncJob,total=integer}
// @Router       /organizations/{id}/sync-jobs [get]
func ListOrgSyncJobs(c *gin.Context) {
	orgID := c.Param("id")
	registryID, err := getRegistryIDForOrg(orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this organization"})
		return
	}
	listSyncJobs(c, registryID)
}

// TriggerRegistrySync godoc
// @Summary      Trigger registry sync
// @Tags         sync
// @Produce      json
// @Param        id      path   string  true  "Registry ID"
// @Param        dryRun  query  bool    false "Dry run mode"
// @Success      202  {object}  object{jobId=string,status=string}
// @Failure      409  {object}  object{error=string}
// @Router       /registries/{id}/sync [post]
func TriggerRegistrySync(c *gin.Context) {
	triggerSync(c, c.Param("id"))
}

// CancelRegistrySync godoc
// @Summary      Cancel registry sync
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{message=string}
// @Router       /registries/{id}/sync/cancel [post]
func CancelRegistrySync(c *gin.Context) {
	cancelSync(c, c.Param("id"))
}

// GetRegistrySyncStatus godoc
// @Summary      Get registry sync status
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{}
// @Router       /registries/{id}/sync-status [get]
func GetRegistrySyncStatus(c *gin.Context) {
	getSyncStatus(c, c.Param("id"))
}

// ListRegistrySyncLogs godoc
// @Summary      List registry sync logs
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{logs=[]models.SyncLog,total=integer}
// @Router       /registries/{id}/sync-logs [get]
func ListRegistrySyncLogs(c *gin.Context) {
	listSyncLogs(c, c.Param("id"))
}

// ListRegistrySyncJobs godoc
// @Summary      List registry sync jobs
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{jobs=[]models.SyncJob,total=integer}
// @Router       /registries/{id}/sync-jobs [get]
func ListRegistrySyncJobs(c *gin.Context) {
	listSyncJobs(c, c.Param("id"))
}

// GetSyncLogDetail godoc
// @Summary      Get sync log detail
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "SyncLog ID"
// @Success      200  {object}  models.SyncLog
// @Failure      404  {object}  object{error=string}
// @Router       /sync-logs/{id} [get]
func GetSyncLogDetail(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var log models.SyncLog
	if err := db.First(&log, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sync log not found"})
		return
	}
	c.JSON(http.StatusOK, log)
}

// GetSyncJobDetail godoc
// @Summary      Get sync job detail
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "SyncJob ID"
// @Success      200  {object}  models.SyncJob
// @Failure      404  {object}  object{error=string}
// @Router       /sync-jobs/{id} [get]
func GetSyncJobDetail(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var job models.SyncJob
	if err := db.First(&job, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sync job not found"})
		return
	}
	c.JSON(http.StatusOK, job)
}

func triggerSync(c *gin.Context, registryID string) {
	if JobService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sync service not available"})
		return
	}

	dryRun := c.Query("dryRun") == "true"
	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)

	job, err := JobService.Enqueue(registryID, "manual", userID, services.EnqueueOptions{
		Priority: 1,
		DryRun:   dryRun,
	})
	if errors.Is(err, services.ErrJobAlreadyQueued) {
		c.JSON(http.StatusConflict, gin.H{"message": "已有同步任务在队列中，请稍后再试"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "status": job.Status})
}

func cancelSync(c *gin.Context, registryID string) {
	if JobService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sync service not available"})
		return
	}

	if err := JobService.CancelByRegistry(registryID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Pending sync jobs cancelled"})
}

func getSyncStatus(c *gin.Context, registryID string) {
	db := database.GetDB()
	var registry models.CapabilityRegistry
	if err := db.First(&registry, "id = ?", registryID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}

	var pendingCount int64
	if JobService != nil {
		pendingCount, _ = JobService.GetPendingCount(registryID)
	}

	var lastLog models.SyncLog
	db.Where("registry_id = ?", registryID).Order("created_at DESC").First(&lastLog)

	c.JSON(http.StatusOK, gin.H{
		"syncStatus":   registry.SyncStatus,
		"lastSyncedAt": registry.LastSyncedAt,
		"lastSyncSha":  registry.LastSyncSHA,
		"pendingJobs":  pendingCount,
		"lastLog":      lastLog,
	})
}

func listSyncLogs(c *gin.Context, registryID string) {
	db := database.GetDB()
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var logs []models.SyncLog
	var total int64
	db.Model(&models.SyncLog{}).Where("registry_id = ?", registryID).Count(&total)
	db.Where("registry_id = ?", registryID).
		Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&logs)

	c.JSON(http.StatusOK, gin.H{"logs": logs, "total": total})
}

func listSyncJobs(c *gin.Context, registryID string) {
	if JobService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sync service not available"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	jobs, total, err := JobService.ListJobs(registryID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs, "total": total})
}

// HandleGitHubWebhook godoc
// @Summary      Handle GitHub webhook
// @Description  Receive GitHub push events and enqueue sync jobs
// @Tags         sync
// @Accept       json
// @Produce      json
// @Success      202  {object}  object{jobId=string,status=string}
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Router       /webhooks/github [post]
func HandleGitHubWebhook(c *gin.Context) {
	if JobService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sync service not available"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	event := c.GetHeader("X-GitHub-Event")
	if event != "push" {
		c.JSON(http.StatusOK, gin.H{"message": "Event ignored"})
		return
	}

	var payload struct {
		Repository struct {
			HTMLURL  string `json:"html_url"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	repoURL := payload.Repository.HTMLURL
	if repoURL == "" {
		repoURL = payload.Repository.CloneURL
	}

	db := database.GetDB()
	var registries []models.CapabilityRegistry
	db.Where("external_url = ? AND sync_enabled = true", repoURL).Find(&registries)
	if len(registries) == 0 {
		db.Where("external_url = ? OR external_url = ?",
			repoURL,
			repoURL+".git",
		).Find(&registries)
	}

	if len(registries) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "No matching registry found"})
		return
	}

	var queued []string
	for _, reg := range registries {
		sig := c.GetHeader("X-Hub-Signature-256")
		if sig != "" {
			var cfgMap map[string]interface{}
			if len(reg.SyncConfig) > 0 {
				_ = json.Unmarshal(reg.SyncConfig, &cfgMap)
			}
			if secret, ok := cfgMap["webhookSecret"].(string); ok && secret != "" {
				if !verifyGitHubSignature(body, sig, secret) {
					continue
				}
			}
		}

		job, err := JobService.Enqueue(reg.ID, "webhook", "", services.EnqueueOptions{Priority: 1})
		if err == nil && job != nil {
			queued = append(queued, job.ID)
		}
	}

	c.JSON(http.StatusAccepted, gin.H{"queued": queued})
}

func verifyGitHubSignature(body []byte, signature, secret string) bool {
	const prefix = "sha256="
	if len(signature) < len(prefix) {
		return false
	}
	sig := signature[len(prefix):]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}
