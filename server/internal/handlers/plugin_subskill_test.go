package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
)

// skillMD builds a minimal SKILL.md body with frontmatter.
func skillMD(name, body string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + name + " skill\n---\n# " + name + "\n" + body)
}

// uploadPlugin uploads a plugin archive carrying the given sub-skill SKILL.md files
// and returns the created plugin item id.
func uploadPlugin(t *testing.T, slug string, skills map[string][]byte) string {
	t.Helper()
	files := map[string][]byte{
		"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills."),
	}
	for path, content := range skills {
		files[path] = content
	}
	zipBytes := createTestZip(files)
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "plugin",
		"name":     "Demo Plugin",
		"slug":     slug,
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("plugin upload: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("expected plugin id in response")
	}
	return id
}

func TestUploadPlugin_PromotesSubSkills(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
	})

	var children []models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ?", pluginID).
		Order("source_path asc").Find(&children).Error; err != nil {
		t.Fatalf("load sub-skills: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("expected 2 promoted sub-skills, got %d", len(children))
	}

	bySlug := make(map[string]models.CapabilityItem)
	for _, ch := range children {
		if ch.ItemType != "skill" {
			t.Fatalf("expected sub-skill item_type=skill, got %q", ch.ItemType)
		}
		if ch.ParentPluginID == nil || *ch.ParentPluginID != pluginID {
			t.Fatalf("expected parent_plugin_id=%s, got %v", pluginID, ch.ParentPluginID)
		}
		bySlug[ch.Slug] = ch
	}

	alpha, ok := bySlug["demo-plugin-alpha"]
	if !ok {
		t.Fatalf("expected slug demo-plugin-alpha, got slugs %v", bySlug)
	}
	if alpha.SourcePath != "skills/alpha/SKILL.md" {
		t.Fatalf("expected alpha source_path skills/alpha/SKILL.md, got %q", alpha.SourcePath)
	}
	if string(skillMD("Alpha", "alpha body")) != alpha.Content {
		t.Fatalf("alpha content mismatch:\n got: %q", alpha.Content)
	}
	if _, ok := bySlug["demo-plugin-beta"]; !ok {
		t.Fatalf("expected slug demo-plugin-beta, got slugs %v", bySlug)
	}
}

func TestUploadPlugin_PromotesMCP(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	mcpJSON := []byte(`{"mcpServers":{"demo":{"command":"node","args":["server.js"]}}}`)
	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		".mcp.json":             mcpJSON,
	})

	var mcp models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ? AND source_path = ?", pluginID, "mcp", ".mcp.json#mcp-demo").First(&mcp).Error; err != nil {
		t.Fatalf("load promoted MCP: %v", err)
	}
	if mcp.Slug != "demo-plugin-mcp-demo" {
		t.Fatalf("expected MCP slug demo-plugin-mcp-demo, got %q", mcp.Slug)
	}
	var mcpContent map[string]any
	if err := json.Unmarshal([]byte(mcp.Content), &mcpContent); err != nil {
		t.Fatalf("decode MCP content: %v", err)
	}
	servers, _ := mcpContent["mcpServers"].(map[string]any)
	if len(servers) != 1 || servers["demo"] == nil {
		t.Fatalf("expected single demo MCP server content, got %#v", mcpContent)
	}
	var meta map[string]any
	if err := json.Unmarshal(mcp.Metadata, &meta); err != nil {
		t.Fatalf("decode MCP metadata: %v", err)
	}
	if meta["command"] != "node" {
		t.Fatalf("expected normalized MCP command=node, got %#v", meta["command"])
	}

	w := get(newItemRouter("u1"), "/api/items?type=mcp")
	if w.Code != http.StatusOK {
		t.Fatalf("list MCP: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var listed struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list MCP response: %v", err)
	}
	foundInList := false
	for _, it := range listed.Items {
		if it["id"] == mcp.ID {
			foundInList = true
			if it["parentPluginId"] != pluginID || it["parentPluginName"] != "Demo Plugin" || it["parentPluginSlug"] != "demo-plugin" {
				t.Fatalf("MCP list parent plugin fields mismatch: %+v", it)
			}
		}
	}
	if !foundInList {
		t.Fatalf("promoted MCP not found in /api/items?type=mcp: %+v", listed.Items)
	}

	w = get(newItemRouter("u1"), "/api/items/"+mcp.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("get MCP detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode MCP detail: %v", err)
	}
	if detail["parentPluginId"] != pluginID || detail["parentPluginName"] != "Demo Plugin" || detail["parentPluginSlug"] != "demo-plugin" {
		t.Fatalf("MCP detail parent plugin fields mismatch: %+v", detail)
	}
}

func TestUploadPlugin_ManifestPromotesInlineMCPsAndSubSkills(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	manifest := []byte(`{
		"name":"ruflo-core",
		"description":"Foundation plugin",
		"version":"0.2.2",
		"mcpServers":{
			"claude-flow":{"command":"npx","args":["claude-flow@alpha","mcp","start"],"description":"Core Claude Flow MCP server"},
			"ruv-swarm":{"command":"npx","args":["ruv-swarm","mcp","start"],"description":"Enhanced swarm coordination"}
		}
	}`)
	zipBytes := createTestZip(map[string][]byte{
		".claude-plugin/plugin.json": manifest,
		"skills/core/SKILL.md":       skillMD("Core", "core body"),
	})
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "plugin",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("manifest plugin upload: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	pluginID, _ := created["id"].(string)
	if created["name"] != "ruflo-core" || created["slug"] != "ruflo-core" || created["description"] != "Foundation plugin" || created["version"] != "0.2.2" {
		t.Fatalf("manifest fields not reflected in plugin response: %+v", created)
	}

	var children []models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ?", pluginID).Order("item_type asc, slug asc").Find(&children).Error; err != nil {
		t.Fatalf("load plugin children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 children (1 skill + 2 mcp), got %d: %+v", len(children), children)
	}
	bySlug := map[string]models.CapabilityItem{}
	for _, child := range children {
		bySlug[child.Slug] = child
	}
	if bySlug["ruflo-core-core"].ItemType != "skill" {
		t.Fatalf("expected promoted skill ruflo-core-core, got %+v", bySlug["ruflo-core-core"])
	}
	flow := bySlug["ruflo-core-mcp-claude-flow"]
	if flow.ItemType != "mcp" || flow.SourcePath != ".claude-plugin/plugin.json#mcp-claude-flow" {
		t.Fatalf("expected claude-flow MCP from manifest, got %+v", flow)
	}
	if strings.Contains(flow.Content, "ruv-swarm") {
		t.Fatalf("expected per-server MCP content, got %s", flow.Content)
	}
	var flowMeta map[string]any
	if err := json.Unmarshal(flow.Metadata, &flowMeta); err != nil {
		t.Fatalf("decode flow metadata: %v", err)
	}
	if flowMeta["command"] != "npx" {
		t.Fatalf("expected normalized command npx, got %#v", flowMeta["command"])
	}
	if bySlug["ruflo-core-mcp-ruv-swarm"].ItemType != "mcp" {
		t.Fatalf("expected ruv-swarm MCP, got %+v", bySlug["ruflo-core-mcp-ruv-swarm"])
	}
}

func TestUploadPlugin_PromotesMultipleRootMCPs(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"one":{"command":"one"},"two":{"command":"two"}}}`),
	})

	var mcps []models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ?", pluginID, "mcp").Order("slug asc").Find(&mcps).Error; err != nil {
		t.Fatalf("load promoted MCPs: %v", err)
	}
	if len(mcps) != 2 {
		t.Fatalf("expected 2 promoted MCPs, got %d: %+v", len(mcps), mcps)
	}
	if mcps[0].Slug != "demo-plugin-mcp-one" || mcps[0].SourcePath != ".mcp.json#mcp-one" {
		t.Fatalf("unexpected first MCP: %+v", mcps[0])
	}
	if mcps[1].Slug != "demo-plugin-mcp-two" || mcps[1].SourcePath != ".mcp.json#mcp-two" {
		t.Fatalf("unexpected second MCP: %+v", mcps[1])
	}
}

func TestUploadPlugin_DedupesManifestAndRootMCP(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	manifest := []byte(`{
		"name":"dupe-plugin",
		"mcpServers":{"demo":{"command":"manifest"}}
	}`)
	zipBytes := createTestZip(map[string][]byte{
		".claude-plugin/plugin.json": manifest,
		".mcp.json":                  []byte(`{"mcpServers":{"demo":{"command":"root"}}}`),
	})
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "plugin",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("manifest/root MCP upload: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	pluginID, _ := created["id"].(string)

	var mcps []models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ?", pluginID, "mcp").Find(&mcps).Error; err != nil {
		t.Fatalf("load promoted MCPs: %v", err)
	}
	if len(mcps) != 1 {
		t.Fatalf("expected duplicate manifest/root MCP to promote once, got %d: %+v", len(mcps), mcps)
	}
	if mcps[0].SourcePath != ".mcp.json#mcp-demo" {
		t.Fatalf("expected root .mcp.json to win, got source_path %q", mcps[0].SourcePath)
	}
	var meta map[string]any
	if err := json.Unmarshal(mcps[0].Metadata, &meta); err != nil {
		t.Fatalf("decode MCP metadata: %v", err)
	}
	if meta["command"] != "root" {
		t.Fatalf("expected root MCP metadata to win, got %#v", meta)
	}
}

func TestUploadPlugin_ReuploadSingleRootMCPToMultipleKeepsExistingChild(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"demo":{"command":"node"}}}`),
	})
	var before models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ? AND slug = ?", pluginID, "mcp", "demo-plugin-mcp-demo").First(&before).Error; err != nil {
		t.Fatalf("load MCP before: %v", err)
	}

	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills."),
		".mcp.json": []byte(`{"mcpServers":{
			"demo":{"command":"node","args":["server.js"]},
			"other":{"command":"other"}
		}}`),
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload multiple MCP: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var after models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ? AND slug = ?", pluginID, "mcp", "demo-plugin-mcp-demo").First(&after).Error; err != nil {
		t.Fatalf("load MCP after: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("expected existing MCP child updated in place, id changed %s -> %s", before.ID, after.ID)
	}
	if after.SourcePath != ".mcp.json#mcp-demo" || after.Status != "active" {
		t.Fatalf("expected stable active MCP child, got %+v", after)
	}
	var total int64
	database.DB.Model(&models.CapabilityItem{}).Where("parent_plugin_id = ? AND item_type = ? AND slug LIKE ?", pluginID, "mcp", "demo-plugin-mcp-demo%").Count(&total)
	if total != 1 {
		t.Fatalf("expected no duplicate demo MCP child, got %d", total)
	}
}

func TestUploadPlugin_MigratesLegacySingleRootMCPSourcePath(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"demo":{"command":"node"}}}`),
	})
	var before models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ?", pluginID, "mcp").First(&before).Error; err != nil {
		t.Fatalf("load MCP before: %v", err)
	}
	if err := database.DB.Model(&models.CapabilityItem{}).Where("id = ?", before.ID).Update("source_path", ".mcp.json").Error; err != nil {
		t.Fatalf("force legacy source path: %v", err)
	}

	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills."),
		".mcp.json": []byte(`{"mcpServers":{
			"demo":{"command":"node"},
			"other":{"command":"other"}
		}}`),
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload legacy MCP: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", before.ID).Error; err != nil {
		t.Fatalf("load migrated MCP: %v", err)
	}
	if after.SourcePath != ".mcp.json#mcp-demo" || after.Status != "active" {
		t.Fatalf("expected legacy MCP migrated in place, got %+v", after)
	}
}

func TestUploadPlugin_ReuploadReconcilesSubSkills(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v1"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
	})

	// Capture alpha's id + revision before re-upload to assert update (not recreate).
	var alphaBefore models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").
		First(&alphaBefore).Error; err != nil {
		t.Fatalf("load alpha before: %v", err)
	}

	// Re-upload: drop beta, change alpha content.
	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md":             []byte("# Demo Plugin\nA plugin bundling skills."),
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v2 changed"),
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{
		"commitMsg": "update skills",
	}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// No duplicates: still exactly one alpha row (same id), updated content + bumped revision.
	var alphaRows []models.CapabilityItem
	database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").Find(&alphaRows)
	if len(alphaRows) != 1 {
		t.Fatalf("expected exactly 1 alpha row, got %d", len(alphaRows))
	}
	alphaAfter := alphaRows[0]
	if alphaAfter.ID != alphaBefore.ID {
		t.Fatalf("alpha should be updated in place, id changed %s -> %s", alphaBefore.ID, alphaAfter.ID)
	}
	if alphaAfter.Content != string(skillMD("Alpha", "alpha v2 changed")) {
		t.Fatalf("expected alpha content updated, got %q", alphaAfter.Content)
	}
	if alphaAfter.CurrentRevision != alphaBefore.CurrentRevision+1 {
		t.Fatalf("expected alpha revision %d, got %d", alphaBefore.CurrentRevision+1, alphaAfter.CurrentRevision)
	}
	if alphaAfter.Status != "active" {
		t.Fatalf("expected alpha active, got %q", alphaAfter.Status)
	}

	// Beta no longer in the archive -> archived (not deleted).
	var beta models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/beta/SKILL.md").
		First(&beta).Error; err != nil {
		t.Fatalf("expected beta row to still exist (archived): %v", err)
	}
	if beta.Status != "archived" {
		t.Fatalf("expected beta archived, got %q", beta.Status)
	}
}

func TestUploadPlugin_ReuploadArchivesRemovedMCP(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"demo":{"command":"node","args":["server.js"]}}}`),
	})
	var mcpBefore models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND item_type = ?", pluginID, "mcp").First(&mcpBefore).Error; err != nil {
		t.Fatalf("load MCP before: %v", err)
	}

	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills."),
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload without MCP: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var mcpAfter models.CapabilityItem
	if err := database.DB.First(&mcpAfter, "id = ?", mcpBefore.ID).Error; err != nil {
		t.Fatalf("load MCP after: %v", err)
	}
	if mcpAfter.Status != "archived" {
		t.Fatalf("expected removed MCP archived, got %q", mcpAfter.Status)
	}
}

func TestUploadPlugin_ReuploadIsIdempotent(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	skills := map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
	}
	pluginID := uploadPlugin(t, "demo-plugin", skills)

	var revBefore int
	database.DB.Model(&models.CapabilityItem{}).
		Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").
		Select("current_revision").Scan(&revBefore)

	// Re-upload the identical archive.
	files := map[string][]byte{"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills.")}
	for p, c := range skills {
		files[p] = c
	}
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{}, createTestZip(files))
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var children []models.CapabilityItem
	database.DB.Where("parent_plugin_id = ?", pluginID).Find(&children)
	if len(children) != 2 {
		t.Fatalf("expected 2 sub-skills after idempotent re-upload, got %d", len(children))
	}
	for _, ch := range children {
		if ch.Status != "active" {
			t.Fatalf("expected %s active, got %q", ch.Slug, ch.Status)
		}
	}
	var revAfter int
	database.DB.Model(&models.CapabilityItem{}).
		Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").
		Select("current_revision").Scan(&revAfter)
	if revAfter != revBefore {
		t.Fatalf("expected alpha revision unchanged on idempotent re-upload, got %d -> %d", revBefore, revAfter)
	}
}

func TestListItems_ParentPluginIdAndExcludeSubSkills(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
	})

	// ?parentPluginId=<id> returns only this plugin's sub-skills.
	w := get(newItemRouter("u1"), "/api/items?parentPluginId="+pluginID)
	if w.Code != http.StatusOK {
		t.Fatalf("list by parentPluginId: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var byParent struct {
		Items []map[string]interface{} `json:"items"`
		Total int                      `json:"total"`
	}
	json.NewDecoder(w.Body).Decode(&byParent)
	if len(byParent.Items) != 2 {
		t.Fatalf("expected 2 sub-skills for parentPluginId, got %d", len(byParent.Items))
	}
	for _, it := range byParent.Items {
		if it["parentPluginId"] != pluginID {
			t.Fatalf("expected parentPluginId=%s, got %v", pluginID, it["parentPluginId"])
		}
		if it["parentPluginName"] != "Demo Plugin" {
			t.Fatalf("expected parentPluginName=Demo Plugin, got %v", it["parentPluginName"])
		}
		if it["parentPluginSlug"] != "demo-plugin" {
			t.Fatalf("expected parentPluginSlug=demo-plugin, got %v", it["parentPluginSlug"])
		}
	}

	// ?excludeSubSkills=true hides sub-skills from the main browse list.
	w = get(newItemRouter("u1"), "/api/items?excludeSubSkills=true")
	if w.Code != http.StatusOK {
		t.Fatalf("list excludeSubSkills: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var excluded struct {
		Items []map[string]interface{} `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&excluded)
	for _, it := range excluded.Items {
		if pp, ok := it["parentPluginId"]; ok && pp != nil && pp != "" {
			t.Fatalf("excludeSubSkills should hide sub-skills, found %v", it["id"])
		}
		if it["itemType"] == "skill" {
			t.Fatalf("excludeSubSkills returned a sub-skill: %v", it["id"])
		}
	}
}

func TestDeleteItem_ArchivesSubSkills(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
	})

	w := deleteReq(newItemRouter("u1"), "/api/items/"+pluginID)
	if w.Code != http.StatusOK {
		t.Fatalf("delete plugin: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Plugin gone; sub-skill retained but archived.
	var plugin models.CapabilityItem
	if err := database.DB.Where("id = ?", pluginID).First(&plugin).Error; err == nil {
		t.Fatalf("expected plugin deleted, but it still exists")
	}
	var child models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ?", pluginID).First(&child).Error; err != nil {
		t.Fatalf("expected sub-skill retained after plugin delete: %v", err)
	}
	if child.Status != "archived" {
		t.Fatalf("expected sub-skill archived after plugin delete, got %q", child.Status)
	}
}

func TestUploadPlugin_PromotesSubSkillAssets(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md":         skillMD("Alpha", "alpha body"),
		"skills/alpha/scripts/setup.sh": []byte("#!/bin/sh\necho alpha\n"),
		"skills/alpha/data.bin":         []byte{0, 1, 2, 3},
	})

	var child models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").First(&child).Error; err != nil {
		t.Fatalf("load sub-skill: %v", err)
	}
	var assets []models.CapabilityAsset
	if err := database.DB.Where("item_id = ?", child.ID).Order("rel_path asc").Find(&assets).Error; err != nil {
		t.Fatalf("load sub-skill assets: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 sub-skill assets, got %d", len(assets))
	}
	if assets[0].RelPath != "data.bin" || assets[0].StorageKey == "" {
		t.Fatalf("expected binary asset data.bin with storage key, got %+v", assets[0])
	}
	if assets[1].RelPath != "scripts/setup.sh" || assets[1].TextContent == nil || *assets[1].TextContent != "#!/bin/sh\necho alpha\n" {
		t.Fatalf("expected text asset scripts/setup.sh copied, got %+v", assets[1])
	}
}

func TestUploadPlugin_ReuploadKeepsUpdatedSubSkillBinaryAsset(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	backend := setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/alpha/data.bin": []byte{0, 1, 2, 3},
	})

	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md":             []byte("# Demo Plugin\nA plugin bundling skills."),
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/alpha/data.bin": []byte{0, 5, 6, 7},
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload binary asset: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var child models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").First(&child).Error; err != nil {
		t.Fatalf("load sub-skill: %v", err)
	}
	var asset models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", child.ID, "data.bin").First(&asset).Error; err != nil {
		t.Fatalf("load binary asset: %v", err)
	}
	reader, _, err := backend.Get(context.Background(), asset.StorageKey)
	if err != nil {
		t.Fatalf("read binary asset from storage: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read binary asset body: %v", err)
	}
	if string(got) != string([]byte{0, 5, 6, 7}) {
		t.Fatalf("expected updated binary asset bytes, got %v", got)
	}
}

func TestUploadPlugin_RecreateReusesArchivedSubSkillSlug(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	firstPluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
	})
	var firstChild models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", firstPluginID, "skills/alpha/SKILL.md").First(&firstChild).Error; err != nil {
		t.Fatalf("load first child: %v", err)
	}

	w := deleteReq(newItemRouter("u1"), "/api/items/"+firstPluginID)
	if w.Code != http.StatusOK {
		t.Fatalf("delete plugin: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	secondPluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body v2"),
	})

	var secondChild models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", secondPluginID, "skills/alpha/SKILL.md").First(&secondChild).Error; err != nil {
		t.Fatalf("load reused child: %v", err)
	}
	if secondChild.ID != firstChild.ID {
		t.Fatalf("expected archived child reused, got new id %s (old %s)", secondChild.ID, firstChild.ID)
	}
	if secondChild.Slug != "demo-plugin-alpha" {
		t.Fatalf("expected stable slug demo-plugin-alpha, got %q", secondChild.Slug)
	}
	if secondChild.Status != "active" {
		t.Fatalf("expected reused child active, got %q", secondChild.Status)
	}
}

func createScanJobTable(t *testing.T) {
	t.Helper()
	if err := database.DB.Exec(`CREATE TABLE IF NOT EXISTS scan_jobs (
		id TEXT PRIMARY KEY,
		item_id TEXT NOT NULL,
		item_revision INTEGER NOT NULL DEFAULT 0,
		trigger_type TEXT NOT NULL,
		trigger_user TEXT,
		priority INTEGER NOT NULL DEFAULT 5,
		status TEXT NOT NULL DEFAULT 'pending',
		retry_count INTEGER DEFAULT 0,
		max_attempts INTEGER DEFAULT 2,
		last_error TEXT,
		scheduled_at DATETIME NOT NULL,
		started_at DATETIME,
		finished_at DATETIME,
		scan_result_id TEXT,
		created_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create scan_jobs: %v", err)
	}
}

func waitForScanJobs(t *testing.T, itemID, triggerType string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int64
		if err := database.DB.Model(&models.ScanJob{}).Where("item_id = ? AND trigger_type = ?", itemID, triggerType).Count(&count).Error; err != nil {
			t.Fatalf("count scan jobs: %v", err)
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d %s scan_jobs for %s, timed out", want, triggerType, itemID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReconcilePluginSubSkills_EnqueuesScanJobs(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)
	createScanJobTable(t)
	ScanJobService = &services.ScanJobService{DB: database.DB}
	defer func() { ScanJobService = nil }()

	plugin := models.CapabilityItem{
		ID:         "plugin-scan",
		RegistryID: PublicRegistryID,
		RepoID:     "public",
		Slug:       "demo-plugin",
		ItemType:   "plugin",
		Name:       "Demo Plugin",
		Content:    "# Demo Plugin",
		CreatedBy:  "u1",
		Status:     "active",
	}
	if err := database.DB.Create(&plugin).Error; err != nil {
		t.Fatalf("create plugin: %v", err)
	}
	h := NewItemHandler(database.DB, &services.ParserService{}, nil, nil)
	children := reconcilePluginSubSkills(h, &plugin, []services.ArchiveAsset{
		{Path: "skills/alpha/SKILL.md", Content: skillMD("Alpha", "alpha v1"), Size: int64(len(skillMD("Alpha", "alpha v1"))), MimeType: "text/markdown"},
	}, "u1")
	if len(children) != 1 {
		t.Fatalf("expected 1 created child, got %d", len(children))
	}
	waitForScanJobs(t, children[0].ID, "create", 1)
	if err := database.DB.Model(&models.ScanJob{}).Where("item_id = ? AND trigger_type = ?", children[0].ID, "create").Update("status", "success").Error; err != nil {
		t.Fatalf("mark create scan job success: %v", err)
	}

	reconcilePluginSubSkills(h, &plugin, []services.ArchiveAsset{
		{Path: "skills/alpha/SKILL.md", Content: skillMD("Alpha", "alpha v2"), Size: int64(len(skillMD("Alpha", "alpha v2"))), MimeType: "text/markdown"},
	}, "u1")
	waitForScanJobs(t, children[0].ID, "update", 1)
}

// TestUploadPlugin_DeepPathMigrationKeepsChildIdentity: moving a sub-skill to
// a deeper directory (skills/alpha → skills/nested/alpha) keeps the same slug
// but changes the source_path. The reconcile must adopt the existing row as a
// path migration — previously it minted a "-2"-suffixed duplicate and archived
// the original, permanently drifting the slug.
func TestUploadPlugin_DeepPathMigrationKeepsChildIdentity(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v1"),
	})
	var before models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").
		First(&before).Error; err != nil {
		t.Fatalf("load alpha before: %v", err)
	}

	// Re-upload with the skill moved one level deeper (same dir name → same slug).
	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md":                    []byte("# Demo Plugin\nA plugin bundling skills."),
		"skills/nested/alpha/SKILL.md": skillMD("Alpha", "alpha v2 moved"),
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{
		"commitMsg": "move alpha deeper",
	}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var rows []models.CapabilityItem
	database.DB.Where("parent_plugin_id = ?", pluginID).Find(&rows)
	active := 0
	for _, r := range rows {
		if r.Status == "active" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("expected exactly 1 active child after migration, got %d (total %d)", active, len(rows))
	}
	var after models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/nested/alpha/SKILL.md").
		First(&after).Error; err != nil {
		t.Fatalf("migrated row missing: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("path migration must adopt the existing row, id changed %s -> %s", before.ID, after.ID)
	}
	if after.Slug != before.Slug {
		t.Fatalf("slug must stay stable across path migration: %q -> %q", before.Slug, after.Slug)
	}
}

// TestUploadPlugin_FailedAssetRebuildKeepsLiveObjects: storage keys are
// revision-scoped, so a re-upload's asset write can never overwrite or delete
// the previous revision's live objects mid-failure.
func TestUploadPlugin_RevisionScopedAssetKeys(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md":  skillMD("Alpha", "alpha v1"),
		"skills/alpha/model.bin": {0x00, 0x01, 0x02, 0x03},
	})
	var child models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ? AND source_path = ?", pluginID, "skills/alpha/SKILL.md").
		First(&child).Error; err != nil {
		t.Fatalf("load child: %v", err)
	}
	var assetV1 models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", child.ID, "model.bin").First(&assetV1).Error; err != nil {
		t.Fatalf("load v1 asset: %v", err)
	}

	// Re-upload with changed binary content → new revision, new storage key.
	updatedZip := createTestZip(map[string][]byte{
		"CLAUDE.md":              []byte("# Demo Plugin\nA plugin bundling skills."),
		"skills/alpha/SKILL.md":  skillMD("Alpha", "alpha v2"),
		"skills/alpha/model.bin": {0x00, 0x09, 0x08},
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{
		"commitMsg": "update binary",
	}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var assetV2 models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", child.ID, "model.bin").First(&assetV2).Error; err != nil {
		t.Fatalf("load v2 asset: %v", err)
	}
	if assetV2.StorageKey == assetV1.StorageKey {
		t.Fatalf("update must write a NEW revision-scoped key, got identical %q", assetV2.StorageKey)
	}
}
