package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// resolveRepoID resolves a repo name to the value stored in capability_registries.repo_id.
// "public" is a reserved virtual repo backed by the default public registry (repo_id = "public").
// For all other names the Repository table is consulted.
// Returns ("", false) when the repo does not exist.
func resolveRepoID(repoName string) (string, bool) {
	if repoName == "public" {
		return "public", true
	}
	db := database.GetDB()
	var repo models.Repository
	if db.Select("id").Where("name = ?", repoName).First(&repo).Error != nil {
		return "", false
	}
	return repo.ID, true
}

// RegistryAccess godoc
// @Summary      Check registry access
// @Description  Probe whether a registry requires authentication. Returns {"public":false} for non-existent repos to avoid leaking repo existence.
// @Tags         registry
// @Produce      json
// @Param        repo  path      string  true  "Repository name"
// @Success      200  {object}  object{public=boolean}
// @Router       /registry/{repo}/access [get]
func RegistryAccess(c *gin.Context) {
	repoID, ok := resolveRepoID(c.Param("repo"))
	if !ok {
		c.JSON(http.StatusOK, gin.H{"public": false})
		return
	}

	db := database.GetDB()
	var count int64
	db.Model(&models.CapabilityRegistry{}).
		Where("repo_id = ? AND visibility = 'public'", repoID).
		Count(&count)

	c.JSON(http.StatusOK, gin.H{"public": count > 0})
}

// indexItem is the wire format for a single entry in index.json
type indexItem struct {
	Slug        string          `json:"slug"`
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Files       []string        `json:"files,omitempty"`
	MCP         json.RawMessage `json:"mcp,omitempty"`
}

// indexJSON is the top-level structure returned by the index endpoint
type indexJSON struct {
	Version int         `json:"version"`
	Items   []indexItem `json:"items"`
}

// RegistryIndex godoc
// @Summary      Get registry index
// @Description  Return the index.json for a repo's registry, filtered by the caller's access rights. Requires Bearer token for non-public registries.
// @Tags         registry
// @Produce      json
// @Param        repo  path      string  true  "Repository name"
// @Success      200  {object}  object{version=integer,items=[]object}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Router       /registry/{repo}/index.json [get]
func RegistryIndex(c *gin.Context) {
	repoID, ok := resolveRepoID(c.Param("repo"))
	if !ok {
		c.JSON(http.StatusOK, indexJSON{Version: 1, Items: []indexItem{}})
		return
	}

	db := database.GetDB()

	var publicCount int64
	db.Model(&models.CapabilityRegistry{}).
		Where("repo_id = ? AND visibility = 'public'", repoID).
		Count(&publicCount)

	isPublic := publicCount > 0

	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)

	if !isPublic && userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	var registryIDs []string

	if isPublic {
		db.Model(&models.CapabilityRegistry{}).
			Where("repo_id = ? AND visibility = 'public'", repoID).
			Pluck("id", &registryIDs)
	}

	if userID != "" && repoID != "public" {
		var isMember int64
		db.Model(&models.RepoMember{}).
			Where("user_id = ? AND repo_id = ?", userID, repoID).
			Count(&isMember)

		if !isPublic && isMember == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this registry"})
			return
		}

		if isMember > 0 {
			var repoRegIDs []string
			db.Model(&models.CapabilityRegistry{}).
				Where("repo_id = ? AND visibility IN ('public','repo')", repoID).
				Pluck("id", &repoRegIDs)
			registryIDs = mergeUnique(registryIDs, repoRegIDs)
		}
	}

	if len(registryIDs) == 0 {
		c.JSON(http.StatusOK, indexJSON{Version: 1, Items: []indexItem{}})
		return
	}

	var capabilityItems []models.CapabilityItem
	db.Where("registry_id IN ? AND status = 'active'", registryIDs).Find(&capabilityItems)

	itemIDs := make([]string, 0, len(capabilityItems))
	for _, si := range capabilityItems {
		itemIDs = append(itemIDs, si.ID)
	}

	var allAssets []models.CapabilityAsset
	if len(itemIDs) > 0 {
		db.Where("item_id IN ?", itemIDs).Find(&allAssets)
	}

	assetsByItem := make(map[string][]string, len(itemIDs))
	for _, asset := range allAssets {
		assetsByItem[asset.ItemID] = append(assetsByItem[asset.ItemID], asset.RelPath)
	}

	items := make([]indexItem, 0, len(capabilityItems))
	for _, si := range capabilityItems {
		assetPaths := assetsByItem[si.ID]
		entry := indexItem{
			Slug:        si.Slug,
			Type:        si.ItemType,
			Name:        si.Name,
			Description: si.Description,
		}

		switch si.ItemType {
		case "skill":
			entry.Files = append([]string{"SKILL.md"}, assetPaths...)
		case "subagent":
			entry.Files = append([]string{"agent.md"}, assetPaths...)
		case "command":
			entry.Files = append([]string{si.Slug + ".md"}, assetPaths...)
		case "mcp":
			entry.MCP = buildMCPConfig(si)
			if len(assetPaths) > 0 {
				entry.Files = append([]string{".mcp.json"}, assetPaths...)
			}
		}

		items = append(items, entry)
	}

	c.JSON(http.StatusOK, indexJSON{Version: 1, Items: items})
}

// buildMCPConfig constructs the mcp config object from a CapabilityItem's metadata.
// For hosting_type=command it returns {"type":"local","command":[...]}
// For hosting_type=remote  it returns {"type":"remote","url":"..."}
// Falls back to raw metadata if the structure is unrecognised.
func buildMCPConfig(si models.CapabilityItem) json.RawMessage {
	if len(si.Metadata) == 0 {
		return nil
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(si.Metadata, &meta); err != nil {
		return nil
	}

	hostingType, _ := meta["hosting_type"].(string)

	switch hostingType {
	case "command":
		cmd, _ := meta["command"].(string)
		argsRaw, _ := meta["args"].([]interface{})
		args := make([]string, 0, len(argsRaw)+1)
		if cmd != "" {
			args = append(args, cmd)
		}
		for _, a := range argsRaw {
			if s, ok := a.(string); ok {
				args = append(args, s)
			}
		}
		out, _ := json.Marshal(map[string]interface{}{
			"type":    "local",
			"command": args,
		})
		return out

	case "remote":
		url, _ := meta["url"].(string)
		serverType, _ := meta["server_type"].(string)
		if serverType == "" {
			serverType = "http"
		}
		out, _ := json.Marshal(map[string]interface{}{
			"type": serverType,
			"url":  url,
		})
		return out
	}

	return json.RawMessage(si.Metadata)
}

// DownloadItem godoc
// @Summary      Download item content
// @Description  Download the Markdown content of a capability item (skill/subagent/command) as a file. Respects visibility rules.
// @Tags         items
// @Produce      text/plain
// @Param        id  path      string  true  "Item ID"
// @Success      200 {string}  string  "Markdown content"
// @Failure      403 {object}  object{error=string}
// @Failure      404 {object}  object{error=string}
// @Router       /items/{id}/download [get]
func DownloadItem(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()

	var item models.CapabilityItem
	if result := db.Preload("Registry").First(&item, "id = ?", id); result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)

	if !canAccessItem(&item, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this item"})
		return
	}

	go db.Model(&models.CapabilityItem{}).Where("id = ?", id).
		UpdateColumn("install_count", gorm.Expr("install_count + 1"))

	filename := contentFilename(item.ItemType, item.Slug)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(item.Content))
}

func contentFilename(itemType, slug string) string {
	switch itemType {
	case "skill":
		return "SKILL.md"
	case "subagent":
		return slug + ".md"
	case "command":
		return slug + ".md"
	default:
		return slug + ".md"
	}
}

func canAccessItem(item *models.CapabilityItem, userID string) bool {
	reg := item.Registry
	if reg == nil {
		return false
	}

	if reg.Visibility == "public" {
		return true
	}

	if userID == "" {
		return false
	}

	if reg.Visibility == "private" {
		return reg.OwnerID == userID
	}

	if reg.Visibility == "repo" {
		db := database.GetDB()
		var count int64
		db.Model(&models.RepoMember{}).
			Where("repo_id = ? AND user_id = ?", reg.RepoID, userID).
			Count(&count)
		return count > 0
	}

	return false
}

// DownloadRegistryFile godoc
// @Summary      Download registry item file by slug
// @Description  Download a specific file of an item identified by repo/slug/filename. Respects visibility rules.
// @Tags         registry
// @Produce      text/plain
// @Param        repo  path      string  true  "Repository name"
// @Param        slug  path      string  true  "Item slug"
// @Param        file  path      string  true  "Filename (e.g. SKILL.md, agent.md, command.md)"
// @Success      200   {string}  string  "File content"
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Router       /registry/{repo}/{slug}/{file} [get]
func DownloadRegistryFile(c *gin.Context) {
	repoID, ok := resolveRepoID(c.Param("repo"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	slug := c.Param("slug")
	db := database.GetDB()

	var registryIDs []string
	db.Model(&models.CapabilityRegistry{}).
		Where("repo_id = ?", repoID).
		Pluck("id", &registryIDs)

	if len(registryIDs) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	var item models.CapabilityItem
	result := db.Preload("Registry").
		Where("registry_id IN ? AND slug = ?", registryIDs, slug).
		First(&item)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	userIDVal, _ := c.Get(middleware.UserIDKey)
	userID, _ := userIDVal.(string)

	if !canAccessItem(&item, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this item"})
		return
	}

	requestedFile := strings.TrimPrefix(c.Param("file"), "/")
	if strings.Contains(requestedFile, "..") {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}

	mainFilename := contentFilename(item.ItemType, item.Slug)
	if requestedFile == "" || requestedFile == mainFilename {
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", mainFilename))
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(item.Content))
		return
	}

	var asset models.CapabilityAsset
	if err := db.Where("item_id = ? AND rel_path = ?", item.ID, requestedFile).First(&asset).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", requestedFile))
	if asset.TextContent != nil {
		c.Data(http.StatusOK, asset.MimeType, []byte(*asset.TextContent))
		return
	}
	if asset.StorageKey == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}
	if StorageBackend == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve file"})
		return
	}

	reader, size, err := StorageBackend.Get(c.Request.Context(), asset.StorageKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve file"})
		return
	}
	defer reader.Close()

	c.Header("Content-Type", asset.MimeType)
	if size > 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		_ = c.Error(err)
	}
}

func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	result := make([]string, 0, len(a)+len(b))
	for _, v := range a {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}
