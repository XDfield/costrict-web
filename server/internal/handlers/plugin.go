package handlers

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
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
	c.Set("defaultItemType", "plugin")
	h.createItemFromArchive(c)
}

// ListBuiltinPlugins godoc
// @Summary      List built-in plugins
// @Description  Get all plugins marked as built-in (is_builtin = true).
// @Tags         plugins
// @Produce      json
// @Param        page      query  int  false  "Page number"
// @Param        pageSize  query  int  false  "Page size"
// @Success      200  {object}  object{items=[]builtinPluginItemResponse,total=int}
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

	respItems := make([]builtinPluginItemResponse, 0, len(items))
	for _, item := range items {
		respItems = append(respItems, toBuiltinPluginItemResponse(item))
	}

	c.JSON(http.StatusOK, gin.H{
		"items":    respItems,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"hasMore":  int64(offset+pageSize) < total,
	})
}

// builtinPluginItemResponse is a lightweight response for ListBuiltinPlugins
// that omits the large content/textContent fields.
type builtinPluginItemResponse struct {
	ID                string                       `json:"id"`
	RegistryID        string                       `json:"registryId"`
	RepoID            string                       `json:"repoId"`
	Slug              string                       `json:"slug"`
	ItemType          string                       `json:"itemType"`
	Name              string                       `json:"name"`
	Description       string                       `json:"description"`
	Descriptions      any                          `json:"descriptions"`
	Category          string                       `json:"category"`
	Version           string                       `json:"version"`
	ContentMD5        string                       `json:"contentMd5"`
	CurrentRevision   int                          `json:"currentRevision"`
	Metadata          any                          `json:"metadata"`
	Health            any                          `json:"health"`
	Evaluation        any                          `json:"evaluation"`
	SourcePath        string                       `json:"sourcePath"`
	SourceSHA         string                       `json:"sourceSha"`
	SourceType        string                       `json:"sourceType"`
	Source            string                       `json:"source"`
	ForkedFromItemID  *string                      `json:"forkedFromItemId,omitempty"`
	ForkedFromOwnerID *string                      `json:"forkedFromOwnerId,omitempty"`
	PreviewCount      int                          `json:"previewCount"`
	InstallCount      int                          `json:"installCount"`
	FavoriteCount     int                          `json:"favoriteCount"`
	Status            string                       `json:"status"`
	SecurityStatus    string                       `json:"securityStatus"`
	LastScanID        *string                      `json:"lastScanId,omitempty"`
	CreatedBy         string                       `json:"createdBy"`
	UpdatedBy         string                       `json:"updatedBy"`
	IsBuiltIn         bool                         `json:"isBuiltIn"`
	Registry          *models.CapabilityRegistry   `json:"registry,omitempty"`
	Assets            []builtinPluginAssetResponse `json:"assets,omitempty"`
	CreatedAt         time.Time                    `json:"createdAt"`
	UpdatedAt         time.Time                    `json:"updatedAt"`
	Tags              []models.ItemTagDict         `json:"tags,omitempty"`
	ShareURL          string                       `json:"shareUrl"`
}

// builtinPluginAssetResponse is a lightweight asset response that omits TextContent.
type builtinPluginAssetResponse struct {
	ID             string    `json:"id"`
	ItemID         string    `json:"itemId"`
	RelPath        string    `json:"relPath"`
	StorageBackend string    `json:"storageBackend"`
	StorageKey     string    `json:"storageKey,omitempty"`
	MimeType       string    `json:"mimeType"`
	FileSize       int64     `json:"fileSize"`
	ContentSHA     string    `json:"contentSha"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

func toBuiltinPluginItemResponse(item models.CapabilityItem) builtinPluginItemResponse {
	assets := make([]builtinPluginAssetResponse, 0, len(item.Assets))
	for _, a := range item.Assets {
		assets = append(assets, builtinPluginAssetResponse{
			ID:             a.ID,
			ItemID:         a.ItemID,
			RelPath:        a.RelPath,
			StorageBackend: a.StorageBackend,
			StorageKey:     a.StorageKey,
			MimeType:       a.MimeType,
			FileSize:       a.FileSize,
			ContentSHA:     a.ContentSHA,
			CreatedAt:      a.CreatedAt,
			UpdatedAt:      a.UpdatedAt,
		})
	}
	return builtinPluginItemResponse{
		ID:                item.ID,
		RegistryID:        item.RegistryID,
		RepoID:            item.RepoID,
		Slug:              item.Slug,
		ItemType:          item.ItemType,
		Name:              item.Name,
		Description:       item.Description,
		Descriptions:      item.Descriptions,
		Category:          item.Category,
		Version:           item.Version,
		ContentMD5:        item.ContentMD5,
		CurrentRevision:   item.CurrentRevision,
		Metadata:          item.Metadata,
		Health:            item.Health,
		Evaluation:        item.Evaluation,
		SourcePath:        item.SourcePath,
		SourceSHA:         item.SourceSHA,
		SourceType:        item.SourceType,
		Source:            item.Source,
		ForkedFromItemID:  item.ForkedFromItemID,
		ForkedFromOwnerID: item.ForkedFromOwnerID,
		PreviewCount:      item.PreviewCount,
		InstallCount:      item.InstallCount,
		FavoriteCount:     item.FavoriteCount,
		Status:            item.Status,
		SecurityStatus:    item.SecurityStatus,
		LastScanID:        item.LastScanID,
		CreatedBy:         item.CreatedBy,
		UpdatedBy:         item.UpdatedBy,
		IsBuiltIn:         item.IsBuiltIn,
		Registry:          item.Registry,
		Assets:            assets,
		CreatedAt:         item.CreatedAt,
		UpdatedAt:         item.UpdatedAt,
		Tags:              item.Tags,
		ShareURL:          fmt.Sprintf("/m/store/%s", item.ID),
	}
}

// DownloadPluginZip streams a plugin and all its assets as a zip archive.
// @Summary      Download plugin zip
// @Description  Download a plugin (and its assets) packaged as a zip file.
// @Tags         plugins
// @Produce      application/zip
// @Param        slug  path  string  true  "Plugin slug"
// @Success      200   {file}  binary
// @Failure      404   {object}  object{error=string}
// @Router       /plugins/{slug}/download [get]
func DownloadPluginZip(c *gin.Context) {
	db := database.GetDB()
	slug := c.Param("slug")

	var item models.CapabilityItem
	if err := db.Where("slug = ? AND item_type = ? AND status = 'active'", slug, "plugin").First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Plugin not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Visibility check: public repos allow anonymous; private repos need membership.
	visibility := getRepoVisibility(item.RepoID)
	if visibility != "public" {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		if !callerIsPlatformAdmin(c, db) {
			var count int64
			db.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ?", item.RepoID, userID).Count(&count)
			if count == 0 {
				c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this plugin"})
				return
			}
		}
	}

	var assets []models.CapabilityAsset
	if err := db.Where("item_id = ?", item.ID).Find(&assets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", slug))

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	now := time.Now()
	// Deduplication guard: if an asset path matches SourcePath, prefer the asset.
	sourcePathCovered := false
	for _, asset := range assets {
		if asset.RelPath == item.SourcePath {
			sourcePathCovered = true
		}
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     asset.RelPath,
			Method:   zip.Deflate,
			Modified: now,
		})
		if err != nil {
			continue
		}
		if asset.TextContent != nil {
			_, _ = w.Write([]byte(*asset.TextContent))
		} else if asset.StorageKey != "" && StorageBackend != nil {
			reader, _, err := StorageBackend.Get(c.Request.Context(), asset.StorageKey)
			if err != nil {
				continue
			}
			_, _ = io.Copy(w, reader)
			_ = reader.Close()
		}
	}

	// If the main content (e.g. CLAUDE.md) wasn't stored as an asset, write it explicitly.
	if !sourcePathCovered && item.Content != "" && item.SourcePath != "" {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     item.SourcePath,
			Method:   zip.Deflate,
			Modified: now,
		})
		if err == nil {
			_, _ = w.Write([]byte(item.Content))
		}
	}
}

// MarketplaceJSON returns a csc-compatible marketplace.json for a given repo.
// @Summary      Get marketplace.json
// @Description  Return a csc-compatible marketplace manifest containing all plugins in the repo.
// @Tags         marketplace
// @Produce      json
// @Param        repo  path  string  true  "Repository name"
// @Success      200   {object}  object
// @Router       /marketplace/{repo}/marketplace.json [get]
func MarketplaceJSON(c *gin.Context) {
	repoID, ok := resolveRepoID(c.Param("repo"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return
	}

	db := database.GetDB()
	visibility := getRepoVisibility(repoID)
	isPublic := visibility == "public"

	userID := c.GetString(middleware.UserIDKey)
	if !isPublic && userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if !isPublic {
		if !callerIsPlatformAdmin(c, db) {
			var count int64
			db.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ?", repoID, userID).Count(&count)
			if count == 0 {
				c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this marketplace"})
				return
			}
		}
	}

	var registryIDs []string
	db.Model(&models.CapabilityRegistry{}).Where("repo_id = ?", repoID).Pluck("id", &registryIDs)

	var items []models.CapabilityItem
	db.Where("registry_id IN ? AND item_type = ? AND status = 'active'", registryIDs, "plugin").
		Order("created_at DESC").
		Find(&items)

	plugins := make([]gin.H, 0, len(items))
	for _, item := range items {
		var desc string
		if item.Description != "" {
			desc = item.Description
		} else {
			desc = item.Slug
		}
		entry := gin.H{
			"name":        item.Slug,
			"description": desc,
			"version":     item.Version,
			"category":    item.Category,
			"source": gin.H{
				"source": "zip",
				"url":    fmt.Sprintf("%s/api/plugins/%s/download", origin(c), item.Slug),
			},
			"strict": true,
		}
		if item.Metadata != nil {
			var meta map[string]any
			_ = json.Unmarshal(item.Metadata, &meta)
			if tags, ok := meta["tags"].([]any); ok && len(tags) > 0 {
				strTags := make([]string, 0, len(tags))
				for _, t := range tags {
					if s, ok := t.(string); ok {
						strTags = append(strTags, s)
					}
				}
				entry["tags"] = strTags
			}
		}
		plugins = append(plugins, entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"name":    c.Param("repo"),
		"owner":   gin.H{"name": "costrict", "email": "support@costrict.com"},
		"plugins": plugins,
	})
}

// origin returns the base URL for constructing absolute download URLs.
func origin(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Request.Host)
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
