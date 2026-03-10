package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

func ListItems(c *gin.Context) {
	registryId := c.Param("registryId")
	db := database.GetDB()
	var items []models.SkillItem
	query := db.Where("registry_id = ?", registryId)
	if itemType := c.Query("type"); itemType != "" {
		query = query.Where("item_type = ?", itemType)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("name LIKE ? OR description LIKE ?", "%"+search+"%", "%"+search+"%")
	}
	result := query.Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch items"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func CreateItem(c *gin.Context) {
	registryId := c.Param("registryId")
	var req struct {
		Slug        string `json:"slug" binding:"required"`
		ItemType    string `json:"itemType" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Version     string `json:"version"`
		Content     string `json:"content"`
		SourcePath  string `json:"sourcePath"`
		Visibility  string `json:"visibility"`
		CreatedBy   string `json:"createdBy" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	item := models.SkillItem{
		ID:          uuid.New().String(),
		RegistryID:  registryId,
		Slug:        req.Slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     req.Version,
		Content:     req.Content,
		Metadata:    datatypes.JSON([]byte("{}")),
		SourcePath:  req.SourcePath,
		Visibility:  req.Visibility,
		CreatedBy:   req.CreatedBy,
	}

	db := database.GetDB()
	if result := db.Create(&item); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create item"})
		return
	}

	version := models.SkillVersion{
		ID:        uuid.New().String(),
		ItemID:    item.ID,
		Version:   1,
		Content:   item.Content,
		Metadata:  datatypes.JSON([]byte("{}")),
		CommitMsg: "Initial version",
		CreatedBy: item.CreatedBy,
	}
	db.Create(&version)

	c.JSON(http.StatusCreated, item)
}

func GetItem(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var item models.SkillItem
	result := db.Preload("Registry").Preload("Versions").Preload("Artifacts").First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}
	c.JSON(http.StatusOK, item)
}

func UpdateItem(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Version     string `json:"version"`
		Content     string `json:"content"`
		Visibility  string `json:"visibility"`
		Status      string `json:"status"`
		UpdatedBy   string `json:"updatedBy"`
		CommitMsg   string `json:"commitMsg"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var item models.SkillItem
	result := db.First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	if req.Name != "" {
		item.Name = req.Name
	}
	if req.Description != "" {
		item.Description = req.Description
	}
	if req.Category != "" {
		item.Category = req.Category
	}
	if req.Version != "" {
		item.Version = req.Version
	}
	if req.Content != "" {
		item.Content = req.Content
	}
	if req.Visibility != "" {
		item.Visibility = req.Visibility
	}
	if req.Status != "" {
		item.Status = req.Status
	}
	if req.UpdatedBy != "" {
		item.UpdatedBy = req.UpdatedBy
	}

	if result := db.Save(&item); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item"})
		return
	}

	if req.Content != "" {
		var maxVersion int
		db.Model(&models.SkillVersion{}).Where("item_id = ?", id).Select("COALESCE(MAX(version), 0)").Scan(&maxVersion)
		sv := models.SkillVersion{
			ID:        uuid.New().String(),
			ItemID:    item.ID,
			Version:   maxVersion + 1,
			Content:   item.Content,
			Metadata:  datatypes.JSON([]byte("{}")),
			CommitMsg: req.CommitMsg,
			CreatedBy: item.UpdatedBy,
		}
		db.Create(&sv)
	}

	c.JSON(http.StatusOK, item)
}

func DeleteItem(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.SkillItem{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete item"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Item deleted"})
}

func ListItemVersions(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var versions []models.SkillVersion
	result := db.Where("item_id = ?", id).Find(&versions)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch versions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"versions": versions})
}

func GetItemVersion(c *gin.Context) {
	id := c.Param("id")
	versionStr := c.Param("version")
	versionNum, err := strconv.Atoi(versionStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version number"})
		return
	}
	db := database.GetDB()
	var version models.SkillVersion
	result := db.Where("item_id = ? AND version = ?", id, versionNum).First(&version)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}
	c.JSON(http.StatusOK, version)
}
