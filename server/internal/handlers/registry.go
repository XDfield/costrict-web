package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// resolveOrgID resolves an org name to the value stored in capability_registries.org_id.
// "public" is a reserved virtual org backed by the default public registry (org_id = "public").
// For all other names the Organization table is consulted.
// Returns ("", false) when the org does not exist.
func resolveOrgID(orgName string) (string, bool) {
	if orgName == "public" {
		return "public", true
	}
	db := database.GetDB()
	var org models.Organization
	if db.Select("id").Where("name = ?", orgName).First(&org).Error != nil {
		return "", false
	}
	return org.ID, true
}

// RegistryAccess godoc
// @Summary      Check registry access
// @Description  Probe whether a registry requires authentication. Returns {"public":false} for non-existent orgs to avoid leaking org existence.
// @Tags         registry
// @Produce      json
// @Param        org  path      string  true  "Organization name"
// @Success      200  {object}  object{public=boolean}
// @Router       /registry/{org}/access [get]
func RegistryAccess(c *gin.Context) {
	orgID, ok := resolveOrgID(c.Param("org"))
	if !ok {
		c.JSON(http.StatusOK, gin.H{"public": false})
		return
	}

	db := database.GetDB()
	var count int64
	db.Model(&models.CapabilityRegistry{}).
		Where("org_id = ? AND visibility = 'public'", orgID).
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
// @Description  Return the index.json for an org's registry, filtered by the caller's access rights. Requires Bearer token for non-public registries.
// @Tags         registry
// @Produce      json
// @Param        org  path      string  true  "Organization name"
// @Success      200  {object}  object{version=integer,items=[]object}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Router       /registry/{org}/index.json [get]
func RegistryIndex(c *gin.Context) {
	orgID, ok := resolveOrgID(c.Param("org"))
	if !ok {
		c.JSON(http.StatusOK, indexJSON{Version: 1, Items: []indexItem{}})
		return
	}

	db := database.GetDB()

	var publicCount int64
	db.Model(&models.CapabilityRegistry{}).
		Where("org_id = ? AND visibility = 'public'", orgID).
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
			Where("org_id = ? AND visibility = 'public'", orgID).
			Pluck("id", &registryIDs)
	}

	if userID != "" && orgID != "public" {
		var isMember int64
		db.Model(&models.OrgMember{}).
			Where("user_id = ? AND org_id = ?", userID, orgID).
			Count(&isMember)

		if !isPublic && isMember == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this registry"})
			return
		}

		if isMember > 0 {
			var orgRegIDs []string
			db.Model(&models.CapabilityRegistry{}).
				Where("org_id = ? AND visibility IN ('public','org')", orgID).
				Pluck("id", &orgRegIDs)
			registryIDs = mergeUnique(registryIDs, orgRegIDs)
		}
	}

	if len(registryIDs) == 0 {
		c.JSON(http.StatusOK, indexJSON{Version: 1, Items: []indexItem{}})
		return
	}

	var capabilityItems []models.CapabilityItem
	db.Where("registry_id IN ? AND status = 'active'", registryIDs).Find(&capabilityItems)

	items := make([]indexItem, 0, len(capabilityItems))
	for _, si := range capabilityItems {
		entry := indexItem{
			Slug:        si.Slug,
			Type:        si.ItemType,
			Name:        si.Name,
			Description: si.Description,
		}

		switch si.ItemType {
		case "skill":
			entry.Files = []string{"SKILL.md"}
		case "subagent":
			entry.Files = []string{"agent.md"}
		case "command":
			entry.Files = []string{si.Slug + ".md"}
		case "mcp":
			entry.MCP = buildMCPConfig(si)
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

	if reg.Visibility == "org" {
		db := database.GetDB()
		var count int64
		db.Model(&models.OrgMember{}).
			Where("org_id = ? AND user_id = ?", reg.OrgID, userID).
			Count(&count)
		return count > 0
	}

	return false
}

// DownloadRegistryFile godoc
// @Summary      Download registry item file by slug
// @Description  Download a specific file of an item identified by org/slug/filename. Respects visibility rules.
// @Tags         registry
// @Produce      text/plain
// @Param        org   path      string  true  "Organization name"
// @Param        slug  path      string  true  "Item slug"
// @Param        file  path      string  true  "Filename (e.g. SKILL.md, agent.md, command.md)"
// @Success      200   {string}  string  "File content"
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Router       /registry/{org}/{slug}/{file} [get]
func DownloadRegistryFile(c *gin.Context) {
	orgID, ok := resolveOrgID(c.Param("org"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	slug := c.Param("slug")
	db := database.GetDB()

	var registryIDs []string
	db.Model(&models.CapabilityRegistry{}).
		Where("org_id = ?", orgID).
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

	filename := c.Param("file")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(item.Content))
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
