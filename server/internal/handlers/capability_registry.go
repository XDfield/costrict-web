package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ListRegistries godoc
// @Summary      List registries
// @Description  Get registries visible to the current user (public + org + personal)
// @Tags         registries
// @Produce      json
// @Param        orgId  query     string  false  "Filter by organization ID"
// @Success      200    {object}  object{registries=[]models.SkillRegistry}
// @Router       /registries [get]
func ListRegistries(c *gin.Context) {
	db := database.GetDB()

	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)

	var registries []models.CapabilityRegistry

	if orgId := c.Query("orgId"); orgId != "" {
		db.Where("org_id = ?", orgId).Find(&registries)
		c.JSON(http.StatusOK, gin.H{"registries": registries})
		return
	}

	db.Where("visibility = 'public'").Find(&registries)

	if userID != "" {
		var orgIDs []string
		db.Model(&models.OrgMember{}).Where("user_id = ?", userID).Pluck("org_id", &orgIDs)
		if len(orgIDs) > 0 {
			var orgRegs []models.CapabilityRegistry
			db.Where("org_id IN ? AND visibility = 'org'", orgIDs).Find(&orgRegs)
			registries = append(registries, orgRegs...)
		}

		var personalRegs []models.CapabilityRegistry
		db.Where("owner_id = ? AND visibility = 'private'", userID).Find(&personalRegs)
		registries = append(registries, personalRegs...)
	}

	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

// CreateRegistry godoc
// @Summary      Create registry
// @Description  Create a new skill registry
// @Tags         registries
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,sourceType=string,externalUrl=string,visibility=string,orgId=string,ownerId=string}  true  "Registry data"
// @Success      201   {object}  models.SkillRegistry
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
		Visibility     string `json:"visibility"`
		OrgID          string `json:"orgId"`
		OwnerID        string `json:"ownerId" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
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
		Visibility:     req.Visibility,
		OrgID:          req.OrgID,
		OwnerID:        req.OwnerID,
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
// @Success      200  {object}  models.SkillRegistry
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
// @Description  Update registry by ID
// @Tags         registries
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Registry ID"
// @Param        body  body      object{name=string,description=string,visibility=string}  false  "Registry data"
// @Success      200   {object}  models.SkillRegistry
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
		Visibility     string `json:"visibility"`
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
	if req.Visibility != "" {
		registry.Visibility = req.Visibility
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

// EnsurePersonalRegistry godoc
// @Summary      Ensure personal registry
// @Description  Get or create the personal registry for a user
// @Tags         registries
// @Accept       json
// @Produce      json
// @Param        body  body      object{ownerId=string,username=string}  true  "Owner data"
// @Success      200   {object}  models.SkillRegistry
// @Success      201   {object}  models.SkillRegistry
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /registries/ensure-personal [post]
func EnsurePersonalRegistry(c *gin.Context) {
	var req struct {
		OwnerID  string `json:"ownerId" binding:"required"`
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var registry models.CapabilityRegistry
	result := db.Where("owner_id = ? AND source_type = 'internal' AND org_id = ''", req.OwnerID).Limit(1).Find(&registry)
	if result.Error == nil && registry.ID != "" {
		c.JSON(http.StatusOK, registry)
		return
	}

	name := "personal"
	if req.Username != "" {
		name = req.Username + "-skills"
	}
	registry = models.CapabilityRegistry{
		ID:          uuid.New().String(),
		Name:        name,
		Description: "Personal skill registry",
		SourceType:  "internal",
		Visibility:  "public",
		OwnerID:     req.OwnerID,
	}
	if result := db.Create(&registry); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create registry"})
		return
	}
	c.JSON(http.StatusCreated, registry)
}

// ListMyRegistries godoc
// @Summary      List my registries
// @Description  Get all registries owned by a user
// @Tags         registries
// @Produce      json
// @Param        ownerId  query     string  true  "Owner user ID"
// @Success      200      {object}  object{registries=[]models.SkillRegistry}
// @Failure      400      {object}  object{error=string}
// @Router       /registries/my [get]
func ListMyRegistries(c *gin.Context) {
	ownerID := c.Query("ownerId")
	if ownerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ownerId is required"})
		return
	}
	db := database.GetDB()
	var registries []models.CapabilityRegistry
	db.Where("owner_id = ?", ownerID).Find(&registries)
	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

// ListMyItems godoc
// @Summary      List my items
// @Description  Get all skill items owned by a user
// @Tags         items
// @Produce      json
// @Param        ownerId  query     string  true   "Owner user ID"
// @Param        type     query     string  false  "Filter by item type"
// @Success      200      {object}  object{items=[]models.SkillItem}
// @Failure      400      {object}  object{error=string}
// @Router       /items/my [get]
func ListMyItems(c *gin.Context) {
	ownerID := c.Query("ownerId")
	if ownerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ownerId is required"})
		return
	}
	db := database.GetDB()
	var registryIDs []string
	db.Model(&models.CapabilityRegistry{}).Where("owner_id = ?", ownerID).Pluck("id", &registryIDs)

	var items []models.CapabilityItem
	if len(registryIDs) > 0 {
		query := db.Where("registry_id IN ?", registryIDs)
		if itemType := c.Query("type"); itemType != "" {
			query = query.Where("item_type = ?", itemType)
		}
		query.Order("created_at DESC").Find(&items)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
