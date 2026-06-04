package handlers

import (
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
	h.createItemFromArchive(c)
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
