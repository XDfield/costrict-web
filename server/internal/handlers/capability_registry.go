package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ListRegistries godoc
// @Summary      List registries
// @Description  Get registries visible to the current user. Visibility is determined by the parent repository's visibility field: public repos' registries are visible to everyone, private repos' registries are visible to repo members, and registries without a repo are visible to their owner.
// @Tags         registries
// @Produce      json
// @Param        repoId  query     string  false  "Filter by repository ID"
// @Success      200    {object}  object{registries=[]models.CapabilityRegistry}
// @Router       /registries [get]
func ListRegistries(c *gin.Context) {
	db := database.GetDB()

	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)

	var registries []models.CapabilityRegistry

	if repoId := c.Query("repoId"); repoId != "" {
		db.Where("repo_id = ?", repoId).Find(&registries)
		c.JSON(http.StatusOK, gin.H{"registries": registries})
		return
	}

	// Registries belonging to public repos (including the virtual "public" repo)
	db.Where("repo_id IN (SELECT CAST(id AS TEXT) FROM repositories WHERE visibility = 'public') OR repo_id = 'public'").Find(&registries)

	if userID != "" {
		// Registries belonging to private repos the user is a member of
		var repoIDs []string
		db.Model(&models.RepoMember{}).Where("user_id = ?", userID).Pluck("repo_id", &repoIDs)
		if len(repoIDs) > 0 {
			var repoRegs []models.CapabilityRegistry
			db.Where("repo_id IN ? AND repo_id NOT IN (SELECT CAST(id AS TEXT) FROM repositories WHERE visibility = 'public')", repoIDs).Find(&repoRegs)
			registries = append(registries, repoRegs...)
		}
	}

	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

// CreateRegistry godoc
// @Summary      Create registry
// @Description  Create a new skill registry. Visibility is inherited from the parent repository.
// @Tags         registries
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,sourceType=string,externalUrl=string,externalBranch=string,syncEnabled=boolean,syncInterval=integer,repoId=string,ownerId=string}  true  "Registry data"
// @Success      201   {object}  models.CapabilityRegistry
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /registries [post]
func CreateRegistry(c *gin.Context) {
	var req struct {
		Name           string `json:"name" binding:"required"`
		Description    string `json:"description"`
		SourceType     string `json:"sourceType"`
		ExternalURL    string `json:"externalUrl"`
		ExternalBranch string `json:"externalBranch"`
		SyncEnabled    bool   `json:"syncEnabled"`
		SyncInterval   int    `json:"syncInterval"`
		RepoID         string `json:"repoId"`
		OwnerID        string `json:"ownerId"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	ownerID := c.GetString(middleware.UserIDKey)
	if ownerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	registry := models.CapabilityRegistry{
		ID:             uuid.New().String(),
		Name:           req.Name,
		Description:    req.Description,
		SourceType:     req.SourceType,
		ExternalURL:    req.ExternalURL,
		ExternalBranch: req.ExternalBranch,
		SyncEnabled:    req.SyncEnabled,
		SyncInterval:   req.SyncInterval,
		RepoID:         req.RepoID,
		OwnerID:        ownerID,
	}

	db := database.GetDB()
	if result := db.Create(&registry); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create registry"})
		return
	}

	c.JSON(http.StatusCreated, registry)
}

// GetRegistry godoc
// @Summary      Get registry
// @Description  Get registry by ID with its items
// @Tags         registries
// @Produce      json
// @Param        id   path      string  true  "Registry ID"
// @Success      200  {object}  models.CapabilityRegistry
// @Failure      404  {object}  object{error=string}
// @Router       /registries/{id} [get]
func GetRegistry(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var registry models.CapabilityRegistry
	result := db.Preload("Items").First(&registry, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}
	c.JSON(http.StatusOK, registry)
}

// UpdateRegistry godoc
// @Summary      Update registry
// @Description  Update registry by ID. Note: visibility is controlled at the repository level, not the registry level.
// @Tags         registries
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Registry ID"
// @Param        body  body      object{name=string,description=string,sourceType=string,externalUrl=string,externalBranch=string,syncEnabled=boolean,syncInterval=integer}  false  "Registry data"
// @Success      200   {object}  models.CapabilityRegistry
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /registries/{id} [put]
func UpdateRegistry(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name           string `json:"name"`
		Description    string `json:"description"`
		SourceType     string `json:"sourceType"`
		ExternalURL    string `json:"externalUrl"`
		ExternalBranch string `json:"externalBranch"`
		SyncEnabled    *bool  `json:"syncEnabled"`
		SyncInterval   int    `json:"syncInterval"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var registry models.CapabilityRegistry
	result := db.First(&registry, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}

	if req.Name != "" {
		registry.Name = req.Name
	}
	if req.Description != "" {
		registry.Description = req.Description
	}
	if req.SourceType != "" {
		registry.SourceType = req.SourceType
	}
	if req.ExternalURL != "" {
		registry.ExternalURL = req.ExternalURL
	}
	if req.ExternalBranch != "" {
		registry.ExternalBranch = req.ExternalBranch
	}
	if req.SyncEnabled != nil {
		registry.SyncEnabled = *req.SyncEnabled
	}
	if req.SyncInterval != 0 {
		registry.SyncInterval = req.SyncInterval
	}

	if result := db.Save(&registry); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update registry"})
		return
	}

	if SyncScheduler != nil && (req.SyncEnabled != nil || req.SyncInterval != 0) {
		_ = SyncScheduler.RegisterRegistry(&registry)
	}
	if SyncScheduler != nil && req.SyncEnabled != nil && !*req.SyncEnabled {
		if JobService != nil {
			_ = JobService.CancelByRegistry(registry.ID)
		}
	}

	c.JSON(http.StatusOK, registry)
}

// DeleteRegistry godoc
// @Summary      Delete registry
// @Description  Delete registry by ID
// @Tags         registries
// @Produce      json
// @Param        id   path      string  true  "Registry ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /registries/{id} [delete]
func DeleteRegistry(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.CapabilityRegistry{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete registry"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Registry deleted"})
}

// TransferRegistry godoc
// @Summary      Transfer registry to another repository
// @Description  Transfer a registry's ownership to a different repository. Caller must be the registry owner_id or an admin/owner of the current repo, and at least a member of the target repo. Sync registries cannot be transferred while syncing.
// @Tags         registries
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Registry ID"
// @Param        body  body      object{targetRepoId=string}  true  "Target repository ID"
// @Success      200   {object}  models.CapabilityRegistry
// @Failure      400   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /registries/{id}/transfer [put]
func TransferRegistry(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		TargetRepoID string `json:"targetRepoId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	callerIDVal, _ := c.Get(middleware.UserIDKey)
	callerID, _ := callerIDVal.(string)
	if callerID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Authentication required"})
		return
	}

	db := database.GetDB()

	var registry models.CapabilityRegistry
	if db.First(&registry, "id = ?", id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}

	isOwner := registry.OwnerID == callerID
	isSourceRepoAdmin := registry.RepoID != "" && isRepoAdmin(getCallerRepoRole(c, registry.RepoID))
	if !isOwner && !isSourceRepoAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the registry owner or source repo admin can transfer this registry"})
		return
	}

	var targetMember models.RepoMember
	if db.Where("repo_id = ? AND user_id = ?", req.TargetRepoID, callerID).First(&targetMember).Error != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You must be a member of the target repository"})
		return
	}

	var targetRepo models.Repository
	if db.First(&targetRepo, "id = ?", req.TargetRepoID).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Target repository not found"})
		return
	}

	if registry.RepoID == req.TargetRepoID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Registry already belongs to the target repository"})
		return
	}

	if registry.SyncStatus == "syncing" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot transfer a registry while it is syncing"})
		return
	}

	// Use a transaction to atomically update both the registry and all its items.
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&registry).Updates(map[string]interface{}{
			"repo_id": req.TargetRepoID,
		}).Error; err != nil {
			return err
		}
		// Batch-update repo_id for all items under this registry.
		if err := tx.Model(&models.CapabilityItem{}).
			Where("registry_id = ?", registry.ID).
			Update("repo_id", req.TargetRepoID).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to transfer registry"})
		return
	}

	c.JSON(http.StatusOK, registry)
}

// ListMyRegistries godoc
// @Summary      List my registries
// @Description  Get all registries owned by a user
// @Tags         registries
// @Produce      json
// @Param        ownerId  query     string  true  "Owner user ID"
// @Success      200      {object}  object{registries=[]models.CapabilityRegistry}
// @Failure      400      {object}  object{error=string}
// @Router       /registries/my [get]
func ListMyRegistries(c *gin.Context) {
	ownerID := c.GetString(middleware.UserIDKey)
	if ownerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	db := database.GetDB()
	var registries []models.CapabilityRegistry
	db.Where("owner_id = ?", ownerID).Find(&registries)
	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

// MyItem represents a capability item with associated repo info
type MyItem struct {
	models.CapabilityItem
	RepoID         string `json:"repoId"`
	RepoName       string `json:"repoName"`
	RepoVisibility string `json:"repoVisibility"`
}

// ListMyItems godoc
// @Summary      List my items
// @Description  Get all skill items owned by the current authenticated user
// @Tags         items
// @Produce      json
// @Param        type      query     string  false  "Filter by item type"
// @Param        page      query     int     false  "Page number (default: 1)"
// @Param        pageSize  query     int     false  "Page size (default: 20, max: 100)"
// @Success      200      {object}  object{items=[]MyItem,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      401      {object}  object{error=string}
// @Router       /items/my [get]
func ListMyItems(c *gin.Context) {
	ownerID, exists := c.Get(middleware.UserIDKey)
	if !exists || ownerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
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

	db := database.GetDB()

	var registries []models.CapabilityRegistry
	db.Where("owner_id = ?", ownerID).Find(&registries)

	// Build registry ID list and registry lookup map
	registryIDs := make([]string, len(registries))
	registryMap := make(map[string]models.CapabilityRegistry, len(registries))
	for i, reg := range registries {
		registryIDs[i] = reg.ID
		registryMap[reg.ID] = reg
	}

	// Include items in owned registries OR items the user created (e.g. in the public registry).
	var query *gorm.DB
	if len(registryIDs) > 0 {
		query = db.Where("registry_id IN ? OR created_by = ?", registryIDs, ownerID)
	} else {
		query = db.Where("created_by = ?", ownerID)
	}
	if itemType := c.Query("type"); itemType != "" {
		query = query.Where("item_type = ?", itemType)
	}

	var total int64
	query.Model(&models.CapabilityItem{}).Count(&total)

	var items []models.CapabilityItem
	offset := (page - 1) * pageSize
	query.Order("created_at DESC").Limit(pageSize).Offset(offset).Find(&items)

	// Supplement registryMap with any registries not yet loaded (e.g. public registry items created by the user)
	for _, item := range items {
		if _, ok := registryMap[item.RegistryID]; !ok {
			var reg models.CapabilityRegistry
			if err := db.Where("id = ?", item.RegistryID).First(&reg).Error; err == nil {
				registryMap[reg.ID] = reg
			}
		}
	}

	// Collect unique repo IDs and fetch repo info (name + visibility)
	repoIDSet := make(map[string]bool)
	for _, reg := range registryMap {
		if reg.RepoID != "" {
			repoIDSet[reg.RepoID] = true
		}
	}
	repoNameMap := make(map[string]string)
	repoVisibilityMap := make(map[string]string)
	if len(repoIDSet) > 0 {
		repoIDs := make([]string, 0, len(repoIDSet))
		for id := range repoIDSet {
			if id != "public" {
				repoIDs = append(repoIDs, id)
			}
		}
		if len(repoIDs) > 0 {
			var repos []models.Repository
			db.Where("id IN ?", repoIDs).Find(&repos)
			for _, repo := range repos {
				repoNameMap[repo.ID] = repo.Name
				repoVisibilityMap[repo.ID] = repo.Visibility
			}
		}
	}
	// The virtual "public" repo has no row in repositories; fill it explicitly.
	if repoIDSet["public"] {
		repoVisibilityMap["public"] = "public"
	}

	// Build response with repo info
	result := make([]MyItem, len(items))
	for i, item := range items {
		reg := registryMap[item.RegistryID]
		result[i] = MyItem{
			CapabilityItem: item,
			RepoID:         reg.RepoID,
			RepoName:       repoNameMap[reg.RepoID],
			RepoVisibility: repoVisibilityMap[reg.RepoID],
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"items":    result,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"hasMore":  int64(offset+pageSize) < total,
	})
}
