package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// mcpFieldKeyRe constrains the shared key scheme exchanged with the frontend
// heuristic: "env:<NAME>", "args:<INDEX>" or "headers:<NAME>". The backend only
// applies values by key (mechanical substitution); placeholder *detection* lives
// solely in the frontend, so the key scheme is the only cross-layer contract.
var mcpFieldKeyRe = regexp.MustCompile(`^(env:.+|args:\d+|headers:.+)$`)

// mcpVarRefRe matches a ${NAME} / {{NAME}} span inside a template value.
var mcpVarRefRe = regexp.MustCompile(`\$\{[^}]*\}|\{\{[^}]*\}\}`)

// mcpFieldEntry is one user-filled value. Stored plaintext in
// MCPUserConfig.FieldValues; `secret` drives display masking only.
type mcpFieldEntry struct {
	V      string `json:"v"`
	Secret bool   `json:"secret"`
}

// MCPFieldStatus is the outward-facing view of one configured field. It is only
// ever returned to the owner of the config, so Value is populated for every field
// (secret included); `secret` is kept for the frontend heuristic round-trip.
type MCPFieldStatus struct {
	Key      string `json:"key"`
	HasValue bool   `json:"hasValue"`
	Secret   bool   `json:"secret"`
	Value    string `json:"value,omitempty"`
}

// MCPConfigStatus is attached to ItemResponse for an MCP item the current user
// has configured. It is only built for the owner of the config, so it carries
// each field's saved value (secret included) to pre-fill the inline editor.
type MCPConfigStatus struct {
	Fields []MCPFieldStatus `json:"fields"`
}

// loadMCPUserFields returns the current user's saved field map for an item, or
// nil when absent/unreadable. Never errors out the request path.
func loadMCPUserFields(db *gorm.DB, userID, itemID string) map[string]mcpFieldEntry {
	if userID == "" || itemID == "" {
		return nil
	}
	var cfg models.MCPUserConfig
	if err := db.Where("user_id = ? AND item_id = ?", userID, itemID).First(&cfg).Error; err != nil {
		return nil
	}
	if len(cfg.FieldValues) == 0 {
		return nil
	}
	fields := map[string]mcpFieldEntry{}
	if err := json.Unmarshal(cfg.FieldValues, &fields); err != nil {
		log.Printf("loadMCPUserFields: bad field_values json for user=%s item=%s: %v", userID, itemID, err)
		return nil
	}
	return fields
}

// resolveMCPContent overlays the user's filled values onto the raw .mcp.json
// content, only at placeholder positions that already exist in the template
// (env/headers keys present / args indices in range). Structure-preserving. On
// any parse failure it returns the original content (fail-safe → template served).
func resolveMCPContent(rawContent string, fields map[string]mcpFieldEntry) string {
	if len(fields) == 0 || strings.TrimSpace(rawContent) == "" {
		return rawContent
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(rawContent), &root); err != nil {
		return rawContent
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return rawContent
	}
	for _, sv := range servers {
		server, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		applyMCPFieldsToServer(server, fields)
	}
	out, err := json.Marshal(root)
	if err != nil {
		return rawContent
	}
	return string(out)
}

// fillHeaderTemplate resolves a header template with the user's value. Header
// values commonly mix literals with a placeholder ("Bearer ${KEY}"), so when the
// template contains ${}/{{}} spans the value substitutes IN PLACE of each span,
// preserving literal prefixes/suffixes; a template without spans (e.g. a bare
// "YOUR_API_KEY" placeholder) is replaced wholesale.
func fillHeaderTemplate(tmpl any, value string) any {
	s, ok := tmpl.(string)
	if !ok {
		return value
	}
	if mcpVarRefRe.MatchString(s) {
		return mcpVarRefRe.ReplaceAllLiteralString(s, value)
	}
	return value
}

// applyMCPFieldsToServer fills env/args/headers placeholders in a single server
// object. Only positions present in the template are touched (no spurious
// creation, no multi-server cross-contamination).
func applyMCPFieldsToServer(server map[string]any, fields map[string]mcpFieldEntry) {
	for key, entry := range fields {
		switch {
		case strings.HasPrefix(key, "env:"):
			name := strings.TrimPrefix(key, "env:")
			env, ok := server["env"].(map[string]any)
			if !ok {
				continue
			}
			if _, exists := env[name]; !exists {
				continue
			}
			env[name] = entry.V
		case strings.HasPrefix(key, "args:"):
			idx, err := strconv.Atoi(strings.TrimPrefix(key, "args:"))
			if err != nil {
				continue
			}
			args, ok := server["args"].([]any)
			if !ok || idx < 0 || idx >= len(args) {
				continue
			}
			args[idx] = entry.V
		case strings.HasPrefix(key, "headers:"):
			name := strings.TrimPrefix(key, "headers:")
			headers, ok := server["headers"].(map[string]any)
			if !ok {
				continue
			}
			tmpl, exists := headers[name]
			if !exists {
				continue
			}
			headers[name] = fillHeaderTemplate(tmpl, entry.V)
		}
	}
}

// buildMCPConfigStatus returns the status view for the current user's own config.
// Value is returned for ALL fields (secret included): per-user resolution only ever
// runs for the owner, and anonymous/other users receive no mcpConfig at all, so a
// user only ever sees back their own values. The `secret` flag is retained for the
// heuristic round-trip; it no longer hides the value. Returns nil when nothing is set.
func buildMCPConfigStatus(fields map[string]mcpFieldEntry) *MCPConfigStatus {
	if len(fields) == 0 {
		return nil
	}
	out := make([]MCPFieldStatus, 0, len(fields))
	for key, entry := range fields {
		fs := MCPFieldStatus{
			Key:      key,
			HasValue: entry.V != "",
			Secret:   entry.Secret,
			Value:    entry.V,
		}
		out = append(out, fs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return &MCPConfigStatus{Fields: out}
}

type upsertMCPConfigRequest struct {
	Fields map[string]mcpFieldEntry `json:"fields"`
}

// UpsertMCPUserConfig godoc
// @Summary      Upsert per-user MCP config values
// @Description  Save the current user's filled placeholder values for an MCP item (merge; empty v clears a key). Returns the saved status (owner-only).
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string                  true  "Item ID"
// @Param        body  body      upsertMCPConfigRequest  true  "Field values"
// @Success      200   {object}  object{mcpConfig=MCPConfigStatus}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Router       /items/{id}/mcp-config [put]
func UpsertMCPUserConfig(c *gin.Context) {
	itemID := c.Param("id")
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req upsertMCPConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Fields == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	db := database.GetDB()
	var item models.CapabilityItem
	if err := db.Select("id", "item_type").First(&item, "id = ?", itemID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}
	if item.ItemType != "mcp" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "item is not an MCP resource"})
		return
	}

	// Merge into existing: provided keys overwrite, empty v clears, untouched
	// keys persist (so editing a PATH doesn't require re-entering the token).
	merged := loadMCPUserFields(db, userID, itemID)
	if merged == nil {
		merged = map[string]mcpFieldEntry{}
	}
	for key, entry := range req.Fields {
		if !mcpFieldKeyRe.MatchString(key) {
			log.Printf("UpsertMCPUserConfig: ignoring invalid key %q for item %s", key, itemID)
			continue
		}
		if entry.V == "" {
			delete(merged, key)
			continue
		}
		merged[key] = entry
	}

	raw, err := json.Marshal(merged)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode config"})
		return
	}

	var cfg models.MCPUserConfig
	findErr := db.Where("user_id = ? AND item_id = ?", userID, itemID).First(&cfg).Error
	switch {
	case errors.Is(findErr, gorm.ErrRecordNotFound):
		cfg = models.MCPUserConfig{
			ID:          uuid.NewString(), // explicit (sqlite test DB has no gen_random_uuid default)
			UserID:      userID,
			ItemID:      itemID,
			FieldValues: datatypes.JSON(raw),
		}
		if err := db.Create(&cfg).Error; err != nil {
			log.Printf("UpsertMCPUserConfig: create failed user=%s item=%s: %v", userID, itemID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
			return
		}
	case findErr != nil:
		log.Printf("UpsertMCPUserConfig: lookup failed user=%s item=%s: %v", userID, itemID, findErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
		return
	default:
		if err := db.Model(&cfg).Update("field_values", datatypes.JSON(raw)).Error; err != nil {
			log.Printf("UpsertMCPUserConfig: update failed user=%s item=%s: %v", userID, itemID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"mcpConfig": buildMCPConfigStatus(merged)})
}
