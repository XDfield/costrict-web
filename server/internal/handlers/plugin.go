package handlers

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
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

	baseURL := origin(c)
	respItems := make([]builtinPluginItemResponse, 0, len(items))
	for _, item := range items {
		respItems = append(respItems, toBuiltinPluginItemResponse(item, fmt.Sprintf("%s/m/store/%s", baseURL, item.ID)))
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
	Content           string                       `json:"content"`
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

func toBuiltinPluginItemResponse(item models.CapabilityItem, shareURL string) builtinPluginItemResponse {
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
		Content:           item.Content,
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
		ShareURL:          shareURL,
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

	// Git-backed plugins keep no assets in the DB — zipping them would yield a
	// one-file archive that looks like a successful download but isn't the
	// plugin. Fail loudly and hand back the repo coordinate instead. Serving
	// the real bundle from Gitea's archive endpoint belongs to the follow-up
	// git-backing task.
	if isGitBacked(&item) {
		c.JSON(http.StatusConflict, gin.H{
			"error":      "This plugin is stored in git; download it from its repository instead",
			"error_code": "GIT_BACKED_ITEM",
			"repoUrl":    item.SourceRepoURL,
			"repoRef":    item.SourceRepoRef,
			"archiveUrl": gitArchiveURL(item.SourceRepoURL, item.SourceRepoRef),
		})
		return
	}

	var assets []models.CapabilityAsset
	if err := db.Where("item_id = ?", item.ID).Find(&assets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for _, asset := range assets {
		if asset.TextContent != nil || asset.StorageKey == "" {
			continue
		}
		if err := storage.ValidateRecordedBackend(asset.StorageBackend, StorageBackend); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Stored file is unavailable with the configured storage backend"})
			return
		}
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

var marketplaceGitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	cscMarketplaceName   = "costrict-plugins"
	cscMarketplaceRepoID = "public"
)

type marketplaceManifest struct {
	Name    string                   `json:"name"`
	Owner   marketplaceOwner         `json:"owner"`
	Plugins []marketplacePluginEntry `json:"plugins"`
}

type marketplaceOwner struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

type marketplacePluginEntry struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Version     string                  `json:"version,omitempty"`
	Category    string                  `json:"category,omitempty"`
	Source      marketplacePluginSource `json:"source"`
	Strict      bool                    `json:"strict"`
	Tags        []string                `json:"tags,omitempty"`
}

type marketplacePluginSource struct {
	Source string `json:"source"`
	URL    string `json:"url"`
	Path   string `json:"path,omitempty"`
	Ref    string `json:"ref,omitempty"`
	SHA    string `json:"sha"`
}

// MarketplaceJSON returns a csc-compatible marketplace.json for a given repo.
// @Summary      Get marketplace.json
// @Description  Return installable Git-backed plugins as a standard csc marketplace manifest. Plugin sources are pinned to the last successfully synced commit.
// @Tags         marketplace
// @Produce      json
// @Param        repo  path  string  true  "Marketplace identity: costrict-plugins, public, or a repository name"
// @Success      200   {object}  marketplaceManifest
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/{repo}/marketplace.json [get]
func MarketplaceJSON(c *gin.Context) {
	marketplaceName := c.Param("repo")
	repoName := marketplaceName
	if marketplaceName == cscMarketplaceName {
		// csc's cloud reconciler always installs plugin@costrict-plugins. Keep
		// that protocol identity stable while the current aggregate is backed by
		// the public registry. Private repositories retain their scoped routes.
		repoName = cscMarketplaceRepoID
	}
	repoID, ok := resolveRepoID(repoName)
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
	if err := db.Model(&models.CapabilityRegistry{}).Where("repo_id = ?", repoID).Pluck("id", &registryIDs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var items []models.CapabilityItem
	if err := db.Where("registry_id IN ? AND item_type = ? AND status = 'active'", registryIDs, "plugin").
		Order("created_at DESC").
		Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	plugins := make([]marketplacePluginEntry, 0, len(items))
	seenNames := make(map[string]struct{}, len(items))
	for _, item := range items {
		entry, ok := projectMarketplacePlugin(item)
		if !ok {
			continue
		}
		if _, duplicate := seenNames[entry.Name]; duplicate {
			continue
		}
		seenNames[entry.Name] = struct{}{}
		plugins = append(plugins, entry)
	}

	c.JSON(http.StatusOK, marketplaceManifest{
		Name:    marketplaceName,
		Owner:   marketplaceOwner{Name: "costrict", Email: "support@costrict.com"},
		Plugins: plugins,
	})
}

func projectMarketplacePlugin(item models.CapabilityItem) (marketplacePluginEntry, bool) {
	if !isGitBacked(&item) || item.GitSyncStatus != "synced" ||
		strings.TrimSpace(item.SourceGitServerID) == "" || item.SourceGitRepoID <= 0 {
		return marketplacePluginEntry{}, false
	}
	repoURL := strings.TrimSpace(item.SourceRepoURL)
	sha := strings.ToLower(strings.TrimSpace(item.GitSHA))
	root, ok := pluginRootFromManifestPath(item.SourceRepoPath)
	if repoURL == "" || !marketplaceGitSHA.MatchString(sha) || !ok {
		return marketplacePluginEntry{}, false
	}

	name, tags := marketplacePluginMetadata(item)
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		return marketplacePluginEntry{}, false
	}
	description := item.Description
	if description == "" {
		description = item.Slug
	}
	source := marketplacePluginSource{
		Source: "url",
		URL:    repoURL,
		Ref:    strings.TrimSpace(item.SourceRepoRef),
		SHA:    sha,
	}
	if root != "." {
		source.Source = "git-subdir"
		source.Path = root
	}
	return marketplacePluginEntry{
		Name:        name,
		Description: description,
		Version:     item.Version,
		Category:    item.Category,
		Source:      source,
		Strict:      true,
		Tags:        tags,
	}, true
}

func marketplacePluginMetadata(item models.CapabilityItem) (string, []string) {
	meta := make(map[string]any)
	if len(item.Metadata) > 0 {
		_ = json.Unmarshal(item.Metadata, &meta)
	}
	name := ""
	if install, ok := meta["install"].(map[string]any); ok {
		name, _ = install["plugin_name"].(string)
	}
	if strings.TrimSpace(name) == "" {
		name, _ = meta["name"].(string)
	}
	if strings.TrimSpace(name) == "" {
		name = item.Slug
	}

	rawTags, _ := meta["tags"].([]any)
	tags := make([]string, 0, len(rawTags))
	for _, raw := range rawTags {
		if tag, ok := raw.(string); ok && strings.TrimSpace(tag) != "" {
			tags = append(tags, tag)
		}
	}
	return strings.TrimSpace(name), tags
}

// pluginRootFromManifestPath converts the indexed metadata file path into the
// plugin root expected by csc's git-subdir source. A standard plugin.json lives
// below .claude-plugin/, while the catalog .plugin.json lives at the root.
func pluginRootFromManifestPath(manifestPath string) (string, bool) {
	raw := strings.TrimSpace(strings.ReplaceAll(manifestPath, "\\", "/"))
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", false
	}
	clean := pathpkg.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}

	var root string
	switch strings.ToLower(pathpkg.Base(clean)) {
	case ".plugin.json":
		root = pathpkg.Dir(clean)
	case "plugin.json":
		parent := pathpkg.Dir(clean)
		if pathpkg.Base(parent) == ".claude-plugin" {
			root = pathpkg.Dir(parent)
		} else {
			root = parent
		}
	default:
		return "", false
	}
	if root == "" {
		root = "."
	}
	if root == ".." || strings.HasPrefix(root, "../") {
		return "", false
	}
	return root, true
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
