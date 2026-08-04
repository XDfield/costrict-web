package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/scheduler"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

var (
	JobService     *services.JobService
	ScanJobService *services.ScanJobService
	CategorySvc    *services.CategoryService
	TagSvc         *services.TagService
	SyncScheduler  interface {
		RegisterRegistry(registry *models.CapabilityRegistry) error
		UnregisterRegistry(registryID string)
	}
)

func getRegistryIDsForRepo(repoID string) ([]string, error) {
	db := database.GetDB()
	var ids []string
	db.Model(&models.CapabilityRegistry{}).
		Where("repo_id = ? AND source_type = 'external'", repoID).
		Pluck("id", &ids)
	if len(ids) > 0 {
		return ids, nil
	}
	db.Model(&models.CapabilityRegistry{}).
		Where("repo_id = ?", repoID).
		Pluck("id", &ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("no registry found for repo %s", repoID)
	}
	return ids, nil
}

func getRegistryIDForRepo(repoID string) (string, error) {
	db := database.GetDB()
	var reg models.CapabilityRegistry
	err := db.Where("repo_id = ?", repoID).
		Order("CASE source_type WHEN 'external' THEN 0 ELSE 1 END").
		First(&reg).Error
	if err != nil {
		return "", fmt.Errorf("no registry found for repo %s", repoID)
	}
	return reg.ID, nil
}

// registryBelongsToRepo 校验 registry 是否属于指定的 repo。
// 用于 IDOR 防护:防止用户通过 ?registryId= 越权操作其他 repo 的 registry。
// 相关报告:secreport 20260731124653436798 (CVSS 5.3 TriggerRepoSync IDOR)
func registryBelongsToRepo(registryID, repoID string) bool {
	if registryID == "" || repoID == "" {
		return false
	}
	var count int64
	database.GetDB().Model(&models.CapabilityRegistry{}).
		Where("id = ? AND repo_id = ?", registryID, repoID).
		Count(&count)
	return count > 0
}

// requireRegistryAccess 反查 registry 所属 repo 并执行授权检查,闭合
// /registries/:id/sync* 系列端点的越权访问。
// write=true 走 requireRepoAdmin(owner/admin),write=false 走 canReadRepo(any member)。
// registry 不存在返回 404;授权失败按 helper 自身语义返回 403。
// 相关报告:secreport 20260731124653436798 (CVSS 5.3 TriggerRepoSync IDOR)
func requireRegistryAccess(c *gin.Context, registryID string, write bool) (string, bool) {
	var registry models.CapabilityRegistry
	if err := database.GetDB().First(&registry, "id = ?", registryID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return "", false
	}
	if write {
		if !requireRepoAdmin(c, registry.RepoID) {
			return "", false
		}
	} else {
		if !canReadRepo(c, registry.RepoID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this registry"})
			return "", false
		}
	}
	return registry.RepoID, true
}

// respondSyncDisabled 返回 503 表示 sync 触发入口已被安全策略封禁。
//
// 封禁动因：GitService.Clone (services/git_service.go) 对 externalUrl 零 SSRF
// 防护，HTTP 触发的 sync 会向 <externalUrl>/info/refs?service=git-upload-pack
// 发 HTTP GET，构成 SSRF。封禁由 scheduler.SyncDisabled 单一开关控制 —— 翻
// false 即同时解封 HTTP 入口与 scheduler 后台周期触发。
// 见 secreport 20260731141243580377 (CVSS 5.3)。
func respondSyncDisabled(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sync is disabled by security policy"})
}

// TriggerRepoSync godoc
// @Summary      Trigger repo sync
// @Description  Manually trigger a sync job for the repository's registry
// @Tags         sync
// @Produce      json
// @Param        id          path   string  true  "Repository ID"
// @Param        registryId  query  string  false  "Registry ID"
// @Param        dryRun      query  bool    false "Dry run mode"
// @Success      202  {object}  object{jobId=string,status=string}
// @Failure      404  {object}  object{error=string}
// @Failure      409  {object}  object{error=string}
// @Failure      503  {object}  object{error=string}  "sync disabled by security policy"
// @Router       /repositories/{id}/sync [post]
func TriggerRepoSync(c *gin.Context) {
	if scheduler.SyncDisabled {
		respondSyncDisabled(c)
		return
	}
	repoID := c.Param("id")
	if !requireRepoAdmin(c, repoID) {
		return
	}

	registryID := c.Query("registryId")
	if registryID != "" {
		if !registryBelongsToRepo(registryID, repoID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Registry does not belong to this repository"})
			return
		}
		triggerSync(c, registryID)
		return
	}
	ids, err := getRegistryIDsForRepo(repoID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this repository"})
		return
	}
	if len(ids) == 1 {
		triggerSync(c, ids[0])
		return
	}
	if JobService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sync service not available"})
		return
	}
	dryRun := c.Query("dryRun") == "true"
	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)
	var jobs []gin.H
	for _, id := range ids {
		job, err := JobService.Enqueue(id, "manual", userID, services.EnqueueOptions{Priority: 1, DryRun: dryRun})
		if err == nil && job != nil {
			jobs = append(jobs, gin.H{"jobId": job.ID, "registryId": id, "status": job.Status})
		}
	}
	c.JSON(http.StatusAccepted, gin.H{"jobs": jobs})
}

// CancelRepoSync godoc
// @Summary      Cancel repo sync
// @Tags         sync
// @Produce      json
// @Param        id          path   string  true   "Repository ID"
// @Param        registryId  query  string  false  "Registry ID (cancel specific registry)"
// @Success      200  {object}  object{message=string}
// @Router       /repositories/{id}/sync/cancel [post]
func CancelRepoSync(c *gin.Context) {
	repoID := c.Param("id")
	if !requireRepoAdmin(c, repoID) {
		return
	}
	registryID := c.Query("registryId")
	if registryID != "" {
		if !registryBelongsToRepo(registryID, repoID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Registry does not belong to this repository"})
			return
		}
		cancelSync(c, registryID)
		return
	}
	ids, err := getRegistryIDsForRepo(repoID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this repository"})
		return
	}
	for _, id := range ids {
		if JobService != nil {
			_ = JobService.CancelByRegistry(id)
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "Pending sync jobs cancelled"})
}

// GetRepoSyncStatus godoc
// @Summary      Get repo sync status
// @Tags         sync
// @Produce      json
// @Param        id          path   string  true   "Repository ID"
// @Param        registryId  query  string  false  "Registry ID (get specific registry status)"
// @Success      200  {object}  object{}
// @Router       /repositories/{id}/sync-status [get]
func GetRepoSyncStatus(c *gin.Context) {
	repoID := c.Param("id")
	if !canReadRepo(c, repoID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this repository"})
		return
	}
	registryID := c.Query("registryId")
	if registryID != "" {
		if !registryBelongsToRepo(registryID, repoID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Registry does not belong to this repository"})
			return
		}
		getSyncStatus(c, registryID)
		return
	}
	ids, err := getRegistryIDsForRepo(repoID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this repository"})
		return
	}
	if len(ids) == 1 {
		getSyncStatus(c, ids[0])
		return
	}
	db := database.GetDB()
	var registries []models.CapabilityRegistry
	db.Where("id IN ?", ids).Find(&registries)
	type regStatus struct {
		RegistryID   string     `json:"registryId"`
		Name         string     `json:"name"`
		ExternalURL  string     `json:"externalUrl"`
		SyncStatus   string     `json:"syncStatus"`
		LastSyncedAt *string    `json:"lastSyncedAt"`
		LastSyncSha  string     `json:"lastSyncSha"`
		PendingJobs  int64      `json:"pendingJobs"`
	}
	var statuses []regStatus
	for _, reg := range registries {
		var pending int64
		if JobService != nil {
			pending, _ = JobService.GetPendingCount(reg.ID)
		}
		var lastSyncedAt *string
		if reg.LastSyncedAt != nil {
			s := reg.LastSyncedAt.Format("2006-01-02T15:04:05Z07:00")
			lastSyncedAt = &s
		}
		statuses = append(statuses, regStatus{
			RegistryID:   reg.ID,
			Name:         reg.Name,
			ExternalURL:  reg.ExternalURL,
			SyncStatus:   reg.SyncStatus,
			LastSyncedAt: lastSyncedAt,
			LastSyncSha:  reg.LastSyncSHA,
			PendingJobs:  pending,
		})
	}
	c.JSON(http.StatusOK, gin.H{"registries": statuses})
}

// ListRepoSyncLogs godoc
// @Summary      List repo sync logs
// @Tags         sync
// @Produce      json
// @Param        id          path   string  true   "Repository ID"
// @Param        registryId  query  string  false  "Registry ID (filter by registry)"
// @Param        page        query  int     false  "Page number (default: 1)"
// @Param        pageSize    query  int     false  "Page size (default: 20)"
// @Success      200  {object}  object{logs=[]models.SyncLog,total=integer}
// @Router       /repositories/{id}/sync-logs [get]
func ListRepoSyncLogs(c *gin.Context) {
	repoID := c.Param("id")
	if !canReadRepo(c, repoID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this repository"})
		return
	}
	registryID := c.Query("registryId")
	if registryID != "" {
		if !registryBelongsToRepo(registryID, repoID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Registry does not belong to this repository"})
			return
		}
		listSyncLogs(c, registryID)
		return
	}
	ids, err := getRegistryIDsForRepo(repoID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this repository"})
		return
	}
	if len(ids) == 1 {
		listSyncLogs(c, ids[0])
		return
	}
	db := database.GetDB()
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 { page = 1 }
	if pageSize < 1 || pageSize > 100 { pageSize = 20 }
	var logs []models.SyncLog
	var total int64
	db.Model(&models.SyncLog{}).Where("registry_id IN ?", ids).Count(&total)
	db.Where("registry_id IN ?", ids).Order("created_at DESC").
		Offset((page-1)*pageSize).Limit(pageSize).Find(&logs)
	c.JSON(http.StatusOK, gin.H{"logs": logs, "total": total})
}

// ListRepoSyncJobs godoc
// @Summary      List repo sync jobs
// @Tags         sync
// @Produce      json
// @Param        id          path   string  true   "Repository ID"
// @Param        registryId  query  string  false  "Registry ID (filter by registry)"
// @Param        page        query  int     false  "Page number (default: 1)"
// @Param        pageSize    query  int     false  "Page size (default: 20)"
// @Success      200  {object}  object{jobs=[]models.SyncJob,total=integer}
// @Router       /repositories/{id}/sync-jobs [get]
func ListRepoSyncJobs(c *gin.Context) {
	repoID := c.Param("id")
	if !canReadRepo(c, repoID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this repository"})
		return
	}
	registryID := c.Query("registryId")
	if registryID != "" {
		if !registryBelongsToRepo(registryID, repoID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Registry does not belong to this repository"})
			return
		}
		listSyncJobs(c, registryID)
		return
	}
	ids, err := getRegistryIDsForRepo(repoID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No registry found for this repository"})
		return
	}
	if len(ids) == 1 {
		listSyncJobs(c, ids[0])
		return
	}
	if JobService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sync service not available"})
		return
	}
	db := database.GetDB()
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 { page = 1 }
	if pageSize < 1 || pageSize > 100 { pageSize = 20 }
	var jobs []models.SyncJob
	var total int64
	db.Model(&models.SyncJob{}).Where("registry_id IN ?", ids).Count(&total)
	db.Where("registry_id IN ?", ids).Order("created_at DESC").
		Offset((page-1)*pageSize).Limit(pageSize).Find(&jobs)
	c.JSON(http.StatusOK, gin.H{"jobs": jobs, "total": total})
}

// TriggerRegistrySync godoc
// @Summary      Trigger registry sync
// @Tags         sync
// @Produce      json
// @Param        id      path   string  true  "Registry ID"
// @Param        dryRun  query  bool    false "Dry run mode"
// @Success      202  {object}  object{jobId=string,status=string}
// @Failure      409  {object}  object{error=string}
// @Failure      503  {object}  object{error=string}  "sync disabled by security policy"
// @Router       /registries/{id}/sync [post]
func TriggerRegistrySync(c *gin.Context) {
	if scheduler.SyncDisabled {
		respondSyncDisabled(c)
		return
	}
	registryID := c.Param("id")
	if _, ok := requireRegistryAccess(c, registryID, true); !ok {
		return
	}
	triggerSync(c, registryID)
}

// CancelRegistrySync godoc
// @Summary      Cancel registry sync
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{message=string}
// @Router       /registries/{id}/sync/cancel [post]
func CancelRegistrySync(c *gin.Context) {
	registryID := c.Param("id")
	if _, ok := requireRegistryAccess(c, registryID, true); !ok {
		return
	}
	cancelSync(c, registryID)
}

// GetRegistrySyncStatus godoc
// @Summary      Get registry sync status
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{}
// @Router       /registries/{id}/sync-status [get]
func GetRegistrySyncStatus(c *gin.Context) {
	registryID := c.Param("id")
	if _, ok := requireRegistryAccess(c, registryID, false); !ok {
		return
	}
	getSyncStatus(c, registryID)
}

// ListRegistrySyncLogs godoc
// @Summary      List registry sync logs
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{logs=[]models.SyncLog,total=integer}
// @Router       /registries/{id}/sync-logs [get]
func ListRegistrySyncLogs(c *gin.Context) {
	registryID := c.Param("id")
	if _, ok := requireRegistryAccess(c, registryID, false); !ok {
		return
	}
	listSyncLogs(c, registryID)
}

// ListRegistrySyncJobs godoc
// @Summary      List registry sync jobs
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "Registry ID"
// @Success      200  {object}  object{jobs=[]models.SyncJob,total=integer}
// @Router       /registries/{id}/sync-jobs [get]
func ListRegistrySyncJobs(c *gin.Context) {
	registryID := c.Param("id")
	if _, ok := requireRegistryAccess(c, registryID, false); !ok {
		return
	}
	listSyncJobs(c, registryID)
}

// GetSyncLogDetail godoc
// @Summary      Get sync log detail
// @Tags         sync
// @Produce      json
// @Param        id  path  string  true  "SyncLog ID"
// @Success      200  {object}  models.SyncLog
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /sync-logs/{id} [get]
// GetSyncLogDetail 通过 syncLog.RegistryID 反查所属 registry→repo 并执行
// canReadRepo 校验,闭合跨仓库日志直读越权。
// 相关报告:secreport 20260731130222018451 (CVSS 2.3 SyncLog IDOR)
func GetSyncLogDetail(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var log models.SyncLog
	if err := db.First(&log, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sync log not found"})
		return
	}
	if _, ok := requireRegistryAccess(c, log.RegistryID, false); !ok {
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
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /sync-jobs/{id} [get]
// GetSyncJobDetail 通过 syncJob.RegistryID 反查所属 registry→repo 并执行
// canReadRepo 校验,与 GetSyncLogDetail 同根因闭合。
// 相关报告:secreport 20260731130222018451 (CVSS 2.3 SyncLog IDOR)
func GetSyncJobDetail(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var job models.SyncJob
	if err := db.First(&job, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sync job not found"})
		return
	}
	if _, ok := requireRegistryAccess(c, job.RegistryID, false); !ok {
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
