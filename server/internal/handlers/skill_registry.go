package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func ListRegistries(c *gin.Context) {
	db := database.GetDB()
	var registries []models.SkillRegistry
	query := db
	if orgId := c.Query("orgId"); orgId != "" {
		query = query.Where("org_id = ?", orgId)
	}
	result := query.Find(&registries)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch registries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

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

	registry := models.SkillRegistry{
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

func GetRegistry(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var registry models.SkillRegistry
	result := db.Preload("Items").First(&registry, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}
	c.JSON(http.StatusOK, registry)
}

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
	var registry models.SkillRegistry
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

	c.JSON(http.StatusOK, registry)
}

func DeleteRegistry(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.SkillRegistry{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete registry"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Registry deleted"})
}

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
	var registry models.SkillRegistry
	result := db.Where("owner_id = ? AND source_type = 'internal' AND org_id = ''", req.OwnerID).First(&registry)
	if result.Error == nil {
		c.JSON(http.StatusOK, registry)
		return
	}

	name := "personal"
	if req.Username != "" {
		name = req.Username + "-skills"
	}
	registry = models.SkillRegistry{
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

func ListMyRegistries(c *gin.Context) {
	ownerID := c.Query("ownerId")
	if ownerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ownerId is required"})
		return
	}
	db := database.GetDB()
	var registries []models.SkillRegistry
	db.Where("owner_id = ?", ownerID).Find(&registries)
	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

func ListMyItems(c *gin.Context) {
	ownerID := c.Query("ownerId")
	if ownerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ownerId is required"})
		return
	}
	db := database.GetDB()
	var registryIDs []string
	db.Model(&models.SkillRegistry{}).Where("owner_id = ?", ownerID).Pluck("id", &registryIDs)

	var items []models.SkillItem
	if len(registryIDs) > 0 {
		query := db.Where("registry_id IN ?", registryIDs)
		if itemType := c.Query("type"); itemType != "" {
			query = query.Where("item_type = ?", itemType)
		}
		query.Order("created_at DESC").Find(&items)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
