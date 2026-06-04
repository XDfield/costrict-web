package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

// UploadPlugin handles plugin archive uploads.
// It delegates to the existing createItemFromArchive logic with itemType fixed to "plugin".
// @Summary      Upload a plugin archive
// @Description  Upload a plugin zip file to create or overwrite a plugin item.
// @Tags         plugins
// @Accept       mpfd
// @Produce      json
// @Param        repo_id  formData  string  true  "Target repository ID"
// @Param        file     formData  file    true  "Plugin zip archive"
// @Success      201      {object}  ItemResponse
// @Success      200      {object}  ItemResponse
// @Failure      400      {object}  object{error=string}
// @Failure      403      {object}  object{error=string}
// @Failure      409      {object}  object{error=string}
// @Router       /plugins/upload [post]
func (h *ItemHandler) UploadPlugin(c *gin.Context) {
	c.Request.PostForm.Set("itemType", "plugin")
	if c.PostForm("is_builtin") == "true" {
		c.Request.PostForm.Set("is_builtin", "true")
	}
	h.createItemFromArchive(c)
}

// ListBuiltinPlugins godoc
// @Summary      List built-in plugins
// @Description  Get all plugins marked as built-in (is_builtin = true).
// @Tags         plugins
// @Produce      json
// @Param        page      query  int  false  "Page number"
// @Param        pageSize  query  int  false  "Page size"
// @Success      200  {object}  object{items=[]models.CapabilityItem,total=int}
// @Router       /plugins/builtin [get]
func ListBuiltinPlugins(c *gin.Context) {
	db := database.GetDB()
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	var total int64
	db.Model(&models.CapabilityItem{}).Where("item_type = ? AND is_built_in = ?", "plugin", true).Count(&total)

	var items []models.CapabilityItem
	offset := (page - 1) * pageSize
	db.Preload("Registry").Preload("Assets").Where("item_type = ? AND is_built_in = ?", "plugin", true).
		Order("created_at DESC").
		Limit(pageSize).Offset(offset).
		Find(&items)

	c.JSON(http.StatusOK, gin.H{
		"items":    items,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"hasMore":  int64(offset+pageSize) < total,
	})
}

// canUploadToRepo checks if a user can upload items to a repository.
func canUploadToRepo(c *gin.Context, repoID string) bool {
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		return false
	}
	if callerIsPlatformAdmin(c, database.GetDB()) {
		return true
	}
	var count int64
	database.GetDB().Model(&models.RepoMember{}).
		Where("repo_id = ? AND user_id = ?", repoID, userID).
		Count(&count)
	return count > 0
}
