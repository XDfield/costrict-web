package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
)

// These tests used to pin bundled sub-skill/MCP PROMOTION: every skills/,
// commands/, agents/ or .mcp.json entry became its own capability_items row
// linked by parent_plugin_id. That model is retired (see
// flattenedPluginArchiveContract) — an uploaded plugin is ONE capability whose
// archive files are its assets. What remains here is the live behaviour of the
// same scenarios (one row, complete assets, stable identity across re-upload)
// plus the still-functional, deprecated reader filters
// ?parentPluginId= / ?excludeSubSkills=, which are exercised against rows
// seeded directly in the DB because nothing promotes children any more.

// skillMD builds a minimal SKILL.md body with frontmatter.
func skillMD(name, body string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + name + " skill\n---\n# " + name + "\n" + body)
}

// uploadPlugin uploads a plugin archive carrying the given bundled files and
// returns the created plugin item id.
func uploadPlugin(t *testing.T, slug string, bundled map[string][]byte) string {
	t.Helper()
	files := map[string][]byte{
		"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills."),
	}
	for path, content := range bundled {
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

// reuploadPlugin re-uploads a plugin archive over an existing item. The
// CLAUDE.md main file is added automatically, mirroring uploadPlugin.
func reuploadPlugin(t *testing.T, pluginID, commitMsg string, bundled map[string][]byte) {
	t.Helper()
	files := map[string][]byte{
		"CLAUDE.md": []byte("# Demo Plugin\nA plugin bundling skills."),
	}
	for path, content := range bundled {
		files[path] = content
	}
	fields := map[string]string{}
	if commitMsg != "" {
		fields["commitMsg"] = commitMsg
	}
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, fields, createTestZip(files))
	if w.Code != http.StatusOK {
		t.Fatalf("plugin re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// seedLegacyChild inserts a row that still carries parent_plugin_id, the way
// catalog ingest and pre-flattening uploads left them. Nothing creates these
// any more, but the reader filters and the delete cascade must keep handling
// them.
func seedLegacyChild(t *testing.T, id, parentID, slug, itemType string) models.CapabilityItem {
	t.Helper()
	parent := parentID
	child := models.CapabilityItem{
		ID:              id,
		RegistryID:      PublicRegistryID,
		RepoID:          "public",
		Slug:            slug,
		ItemType:        itemType,
		Name:            slug,
		Content:         "legacy " + slug + " content",
		SourceType:      "direct",
		CreatedBy:       "u1",
		ParentPluginID:  &parent,
		CurrentRevision: 1,
		Status:          "active",
	}
	if err := database.DB.Create(&child).Error; err != nil {
		t.Fatalf("seed legacy child %s: %v", id, err)
	}
	return child
}

// Two bundled skills used to become two extra rows. Now they stay files of the
// one plugin capability, with their bodies preserved verbatim.
func TestUploadPlugin_BundledSkillsCreateNoChildRows(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
	})

	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row, got %d", total)
	}
	assertNoChildRows(t)

	if got := assetText(t, pluginID, "skills/alpha/SKILL.md"); got != string(skillMD("Alpha", "alpha body")) {
		t.Fatalf("alpha SKILL.md not preserved verbatim, got %q", got)
	}
	if got := assetText(t, pluginID, "skills/beta/SKILL.md"); got != string(skillMD("Beta", "beta body")) {
		t.Fatalf("beta SKILL.md not preserved verbatim, got %q", got)
	}
}

// A bundled .mcp.json no longer mints an mcp capability; it stays an asset, and
// /api/items?type=mcp shows nothing for it.
func TestUploadPlugin_MCPManifestCreatesNoMCPRow(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	mcpJSON := []byte(`{"mcpServers":{"demo":{"command":"node","args":["server.js"]}}}`)
	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		".mcp.json":             mcpJSON,
	})

	if n := countItems(t, "item_type = ?", "mcp"); n != 0 {
		t.Fatalf("expected no mcp rows from a bundled .mcp.json, got %d", n)
	}
	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row, got %d", total)
	}
	assertNoChildRows(t)

	if got := assetText(t, pluginID, ".mcp.json"); got != string(mcpJSON) {
		t.Fatalf(".mcp.json asset not preserved verbatim, got %q", got)
	}

	w := get(newItemRouter("u1"), "/api/items?type=mcp")
	if w.Code != http.StatusOK {
		t.Fatalf("list MCP: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var listed struct {
		Items []map[string]interface{} `json:"items"`
		Total int                      `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list MCP response: %v", err)
	}
	if len(listed.Items) != 0 || listed.Total != 0 {
		t.Fatalf("expected no MCP items listed, got %d (total %d): %+v", len(listed.Items), listed.Total, listed.Items)
	}
}

// The plugin manifest still drives name/slug/description/version, and its
// inline mcpServers stay inside the item's own content instead of becoming rows.
func TestUploadPlugin_ManifestFieldsWithoutChildren(t *testing.T) {
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

	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row for a manifest plugin, got %d", total)
	}
	assertNoChildRows(t)

	// The manifest is the main file here, so it lands as the item's content;
	// both declared servers stay inside it.
	var plugin models.CapabilityItem
	if err := database.DB.First(&plugin, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}
	if plugin.SourcePath != ".claude-plugin/plugin.json" || plugin.Content != string(manifest) {
		t.Fatalf("manifest content not stored on the plugin row: source_path=%q", plugin.SourcePath)
	}
	// The bundled skill is still an asset of the same row.
	if got := assetText(t, pluginID, "skills/core/SKILL.md"); got != string(skillMD("Core", "core body")) {
		t.Fatalf("bundled skill asset missing/altered, got %q", got)
	}
}

// Re-uploading with a second MCP server added updates the one row's asset; it
// used to create/keep separate MCP child rows.
//
// The archive also changes a tracked file on purpose: HashArchiveContent drops
// every dot-prefixed path (shouldSkipAsset), so a .mcp.json-only edit does not
// move the item's content hash and updateItemFromArchive takes its
// "nothing changed" short-circuit, which no longer rebuilds anything now that
// the unconditional sub-skill reconcile is gone. That gap is reported
// separately; it is not the contract this test pins.
func TestUploadPlugin_ReuploadMCPExpansionKeepsSingleRow(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		".mcp.json":     []byte(`{"mcpServers":{"demo":{"command":"node"}}}`),
		"docs/usage.md": []byte("# usage v1"),
	})

	expanded := []byte(`{"mcpServers":{"demo":{"command":"node","args":["server.js"]},"other":{"command":"other"}}}`)
	reuploadPlugin(t, pluginID, "", map[string][]byte{
		".mcp.json":     expanded,
		"docs/usage.md": []byte("# usage v2"),
	})

	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row after MCP expansion, got %d", total)
	}
	assertNoChildRows(t)

	var mcp struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(assetText(t, pluginID, ".mcp.json")), &mcp); err != nil {
		t.Fatalf("decode .mcp.json asset: %v", err)
	}
	if len(mcp.MCPServers) != 2 || mcp.MCPServers["demo"] == nil || mcp.MCPServers["other"] == nil {
		t.Fatalf("expected both servers in the updated asset, got %#v", mcp.MCPServers)
	}
}

// Re-upload rebuilds the asset set: a changed file is updated, and files that
// left the archive leave the item. No row is created or archived for them.
func TestUploadPlugin_ReuploadRebuildsAssetsWithoutChildren(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v1"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
		".mcp.json":             []byte(`{"mcpServers":{"demo":{"command":"node"}}}`),
	})
	var before models.CapabilityItem
	if err := database.DB.First(&before, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin before: %v", err)
	}

	// Change alpha, drop beta and the MCP manifest.
	reuploadPlugin(t, pluginID, "update skills", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v2 changed"),
	})

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin after: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("plugin must be updated in place, id changed %s -> %s", before.ID, after.ID)
	}
	if after.CurrentRevision != before.CurrentRevision+1 {
		t.Fatalf("expected revision %d, got %d", before.CurrentRevision+1, after.CurrentRevision)
	}
	if after.Status != "active" {
		t.Fatalf("expected plugin active, got %q", after.Status)
	}

	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row after re-upload, got %d", total)
	}
	assertNoChildRows(t)

	got := assetRelPaths(t, pluginID)
	if len(got) != 1 || got[0] != "skills/alpha/SKILL.md" {
		t.Fatalf("asset set must match the new archive exactly, got %v", got)
	}
	if body := assetText(t, pluginID, "skills/alpha/SKILL.md"); body != string(skillMD("Alpha", "alpha v2 changed")) {
		t.Fatalf("expected alpha asset updated, got %q", body)
	}
}

// An identical re-upload changes nothing: same row, same revision, same assets.
func TestUploadPlugin_ReuploadIsIdempotent(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	bundled := map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
	}
	pluginID := uploadPlugin(t, "demo-plugin", bundled)

	var before models.CapabilityItem
	if err := database.DB.First(&before, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin before: %v", err)
	}
	pathsBefore := assetRelPaths(t, pluginID)

	reuploadPlugin(t, pluginID, "", bundled)

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin after: %v", err)
	}
	if after.CurrentRevision != before.CurrentRevision {
		t.Fatalf("expected revision unchanged on identical re-upload, got %d -> %d", before.CurrentRevision, after.CurrentRevision)
	}
	if after.ContentMD5 != before.ContentMD5 {
		t.Fatalf("expected content hash unchanged, got %q -> %q", before.ContentMD5, after.ContentMD5)
	}
	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row, got %d", total)
	}
	assertNoChildRows(t)

	pathsAfter := assetRelPaths(t, pluginID)
	if len(pathsAfter) != len(pathsBefore) {
		t.Fatalf("asset set changed on idempotent re-upload: %v -> %v", pathsBefore, pathsAfter)
	}
	for i := range pathsBefore {
		if pathsBefore[i] != pathsAfter[i] {
			t.Fatalf("asset set changed on idempotent re-upload: %v -> %v", pathsBefore, pathsAfter)
		}
	}
}

// ?parentPluginId= and ?excludeSubSkills= are deprecated but still functional
// readers over legacy parent-linked rows, and item responses still carry the
// parent plugin's display fields. Upload no longer produces such rows, so they
// are seeded directly.
func TestListItems_ParentPluginIdAndExcludeSubSkills(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
	})
	alpha := seedLegacyChild(t, "legacy-alpha", pluginID, "demo-plugin-alpha", "skill")
	beta := seedLegacyChild(t, "legacy-beta", pluginID, "demo-plugin-beta", "skill")

	// A parent-linked row belonging to a DIFFERENT plugin must not leak in.
	otherPluginID := uploadPlugin(t, "other-plugin", map[string][]byte{})
	seedLegacyChild(t, "legacy-other", otherPluginID, "other-plugin-child", "skill")

	// ?parentPluginId=<id> returns only that plugin's linked rows.
	w := get(newItemRouter("u1"), "/api/items?parentPluginId="+pluginID)
	if w.Code != http.StatusOK {
		t.Fatalf("list by parentPluginId: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var byParent struct {
		Items []map[string]interface{} `json:"items"`
		Total int                      `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&byParent); err != nil {
		t.Fatalf("decode parentPluginId response: %v", err)
	}
	if len(byParent.Items) != 2 {
		t.Fatalf("expected 2 linked rows for parentPluginId, got %d: %+v", len(byParent.Items), byParent.Items)
	}
	seen := map[string]bool{}
	for _, it := range byParent.Items {
		id, _ := it["id"].(string)
		seen[id] = true
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
	if !seen[alpha.ID] || !seen[beta.ID] {
		t.Fatalf("parentPluginId did not return both linked rows: %+v", byParent.Items)
	}

	// The same fields appear on item detail.
	w = get(newItemRouter("u1"), "/api/items/"+alpha.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("get linked item detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}
	if detail["parentPluginId"] != pluginID || detail["parentPluginName"] != "Demo Plugin" || detail["parentPluginSlug"] != "demo-plugin" {
		t.Fatalf("detail parent plugin fields mismatch: %+v", detail)
	}

	// ?excludeSubSkills=true hides every parent-linked row from the main list.
	w = get(newItemRouter("u1"), "/api/items?excludeSubSkills=true")
	if w.Code != http.StatusOK {
		t.Fatalf("list excludeSubSkills: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var excluded struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&excluded); err != nil {
		t.Fatalf("decode excludeSubSkills response: %v", err)
	}
	foundPlugin := false
	for _, it := range excluded.Items {
		if pp, ok := it["parentPluginId"]; ok && pp != nil && pp != "" {
			t.Fatalf("excludeSubSkills should hide parent-linked rows, found %v", it["id"])
		}
		if it["id"] == pluginID {
			foundPlugin = true
		}
	}
	if !foundPlugin {
		t.Fatalf("excludeSubSkills must still return the plugin itself: %+v", excluded.Items)
	}
}

// Deleting a plugin still hard-deletes rows that legacy data left linked to it,
// so no orphan survives the parent.
func TestDeleteItem_HardDeletesLegacyChildRows(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
	})
	seedLegacyChild(t, "legacy-alpha", pluginID, "demo-plugin-alpha", "skill")

	w := deleteReq(newItemRouter("u1"), "/api/items/"+pluginID)
	if w.Code != http.StatusOK {
		t.Fatalf("delete plugin: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if n := countItems(t, "id = ?", pluginID); n != 0 {
		t.Fatalf("expected plugin deleted, still found %d row(s)", n)
	}
	if n := countItems(t, "parent_plugin_id = ?", pluginID); n != 0 {
		t.Fatalf("expected linked rows hard-deleted with the plugin, got %d remaining", n)
	}
}

// Files nested under a bundled skill directory are assets of the plugin itself,
// keyed by their FULL archive path; binaries go to storage, text stays inline.
func TestUploadPlugin_KeepsNestedTextAndBinaryAssets(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md":         skillMD("Alpha", "alpha body"),
		"skills/alpha/scripts/setup.sh": []byte("#!/bin/sh\necho alpha\n"),
		"skills/alpha/data.bin":         {0, 1, 2, 3},
	})

	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row, got %d", total)
	}
	assertNoChildRows(t)

	got := assetRelPaths(t, pluginID)
	want := []string{"skills/alpha/SKILL.md", "skills/alpha/data.bin", "skills/alpha/scripts/setup.sh"}
	if len(got) != len(want) {
		t.Fatalf("asset rel_paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asset rel_paths = %v, want %v", got, want)
		}
	}

	if text := assetText(t, pluginID, "skills/alpha/scripts/setup.sh"); text != "#!/bin/sh\necho alpha\n" {
		t.Fatalf("nested text asset not copied, got %q", text)
	}
	var binary models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", pluginID, "skills/alpha/data.bin").First(&binary).Error; err != nil {
		t.Fatalf("load binary asset: %v", err)
	}
	if binary.StorageKey == "" || binary.TextContent != nil {
		t.Fatalf("expected binary asset stored by key, got %+v", binary)
	}
}

// Storage keys are revision-scoped, so a re-upload writes new objects and can
// never overwrite the previous revision's live bytes.
func TestUploadPlugin_ReuploadWritesRevisionScopedBinaryAsset(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	backend := setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md":  skillMD("Alpha", "alpha v1"),
		"skills/alpha/model.bin": {0x00, 0x01, 0x02, 0x03},
	})
	var v1 models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", pluginID, "skills/alpha/model.bin").First(&v1).Error; err != nil {
		t.Fatalf("load v1 asset: %v", err)
	}

	reuploadPlugin(t, pluginID, "update binary", map[string][]byte{
		"skills/alpha/SKILL.md":  skillMD("Alpha", "alpha v2"),
		"skills/alpha/model.bin": {0x00, 0x09, 0x08},
	})

	var v2 models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", pluginID, "skills/alpha/model.bin").First(&v2).Error; err != nil {
		t.Fatalf("load v2 asset: %v", err)
	}
	if v2.StorageKey == v1.StorageKey {
		t.Fatalf("update must write a NEW revision-scoped key, got identical %q", v2.StorageKey)
	}
	if !backend.Has(v1.StorageKey) {
		t.Fatalf("previous revision's object %q was destroyed by the update", v1.StorageKey)
	}
	reader, _, err := backend.Get(context.Background(), v2.StorageKey)
	if err != nil {
		t.Fatalf("read updated binary asset: %v", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read updated binary body: %v", err)
	}
	if string(body) != string([]byte{0x00, 0x09, 0x08}) {
		t.Fatalf("expected updated binary bytes, got %v", body)
	}
}

// Moving a bundled skill deeper used to risk a duplicated, slug-drifted child
// row. With one capability per archive it is just an asset path change.
func TestUploadPlugin_DeepPathMoveKeepsSingleRow(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v1"),
	})
	var before models.CapabilityItem
	if err := database.DB.First(&before, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin before: %v", err)
	}

	reuploadPlugin(t, pluginID, "move alpha deeper", map[string][]byte{
		"skills/nested/alpha/SKILL.md": skillMD("Alpha", "alpha v2 moved"),
	})

	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row after the move, got %d", total)
	}
	assertNoChildRows(t)

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin after: %v", err)
	}
	if after.ID != before.ID || after.Slug != before.Slug {
		t.Fatalf("plugin identity must survive a bundled path move: %s/%s -> %s/%s", before.ID, before.Slug, after.ID, after.Slug)
	}

	got := assetRelPaths(t, pluginID)
	if len(got) != 1 || got[0] != "skills/nested/alpha/SKILL.md" {
		t.Fatalf("expected the moved path as the only asset, got %v", got)
	}
	if body := assetText(t, pluginID, "skills/nested/alpha/SKILL.md"); body != string(skillMD("Alpha", "alpha v2 moved")) {
		t.Fatalf("moved asset content mismatch, got %q", body)
	}
}

// Deleting a plugin frees its slug: re-uploading the same slug yields one fresh
// row, with no leftover occupying the slug.
func TestUploadPlugin_RecreateAfterDeleteKeepsSlugAndSingleRow(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	firstID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body"),
	})
	w := deleteReq(newItemRouter("u1"), "/api/items/"+firstID)
	if w.Code != http.StatusOK {
		t.Fatalf("delete plugin: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	secondID := uploadPlugin(t, "demo-plugin", map[string][]byte{
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha body v2"),
	})
	if secondID == firstID {
		t.Fatalf("expected a fresh plugin id after delete, but reused %s", firstID)
	}
	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row after recreate, got %d", total)
	}
	assertNoChildRows(t)

	var second models.CapabilityItem
	if err := database.DB.First(&second, "id = ?", secondID).Error; err != nil {
		t.Fatalf("load recreated plugin: %v", err)
	}
	if second.Slug != "demo-plugin" || second.Status != "active" {
		t.Fatalf("expected active plugin on the stable slug, got %+v", second)
	}
	if body := assetText(t, secondID, "skills/alpha/SKILL.md"); body != string(skillMD("Alpha", "alpha body v2")) {
		t.Fatalf("recreated plugin carries stale asset content: %q", body)
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

// A bundle of six previously-promoted files used to enqueue a security scan per
// promoted child. One capability means exactly one scan job per change, and it
// belongs to the plugin row.
func TestUploadPlugin_EnqueuesScanJobsForPluginRowOnly(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)
	createScanJobTable(t)

	// newItemRouter clears ScanJobService, so enable scanning after building it.
	router := newItemRouter("u1")
	ScanJobService = &services.ScanJobService{DB: database.DB}
	defer func() { ScanJobService = nil }()

	files := map[string][]byte{
		"CLAUDE.md":             []byte("# Demo Plugin\nA plugin bundling skills."),
		"skills/alpha/SKILL.md": skillMD("Alpha", "alpha v1"),
		"skills/beta/SKILL.md":  skillMD("Beta", "beta body"),
		"commands/build.md":     []byte("# build"),
		"agents/reviewer.md":    []byte("# reviewer"),
		"rules/style.md":        []byte("# style"),
		".mcp.json":             []byte(`{"mcpServers":{"demo":{"command":"node"}}}`),
	}
	w := postMultipart(router, "/api/items", map[string]string{
		"itemType": "plugin",
		"name":     "Demo Plugin",
		"slug":     "demo-plugin",
	}, createTestZip(files))
	if w.Code != http.StatusCreated {
		t.Fatalf("plugin upload: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	pluginID, _ := created["id"].(string)
	if pluginID == "" {
		t.Fatal("expected plugin id in response")
	}

	waitForScanJobs(t, pluginID, "create", 1)
	var totalJobs int64
	if err := database.DB.Model(&models.ScanJob{}).Count(&totalJobs).Error; err != nil {
		t.Fatalf("count all scan jobs: %v", err)
	}
	if totalJobs != 1 {
		t.Fatalf("expected exactly 1 scan job for the whole bundle, got %d", totalJobs)
	}

	// Drain the pending job so the update enqueue is not short-circuited.
	if err := database.DB.Model(&models.ScanJob{}).Where("item_id = ? AND trigger_type = ?", pluginID, "create").
		Update("status", "success").Error; err != nil {
		t.Fatalf("mark create scan job success: %v", err)
	}

	updateRouter := newItemRouter("u1")
	ScanJobService = &services.ScanJobService{DB: database.DB}
	files["skills/alpha/SKILL.md"] = skillMD("Alpha", "alpha v2")
	w = putMultipart(updateRouter, "/api/items/"+pluginID, map[string]string{}, createTestZip(files))
	if w.Code != http.StatusOK {
		t.Fatalf("plugin re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	waitForScanJobs(t, pluginID, "update", 1)
	if err := database.DB.Model(&models.ScanJob{}).Count(&totalJobs).Error; err != nil {
		t.Fatalf("count all scan jobs after update: %v", err)
	}
	if totalJobs != 2 {
		t.Fatalf("expected exactly 2 scan jobs (create + update) for the plugin, got %d", totalJobs)
	}
	var otherItemJobs int64
	if err := database.DB.Model(&models.ScanJob{}).Where("item_id <> ?", pluginID).Count(&otherItemJobs).Error; err != nil {
		t.Fatalf("count foreign scan jobs: %v", err)
	}
	if otherItemJobs != 0 {
		t.Fatalf("bundled files must not get their own scan jobs, got %d", otherItemJobs)
	}
}
