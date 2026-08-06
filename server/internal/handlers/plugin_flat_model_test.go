package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
)

// The flattened plugin contract (see flattenedPluginArchiveContract): a plugin
// archive is ONE capability. Every bundled file — skills/, commands/, agents/,
// rules/, templates/, .mcp.json, .claude-plugin/plugin.json — is stored as an
// asset of that single row. Nothing is promoted to a child capability, so no
// row created by upload, update, or fork carries a parent_plugin_id.

// flatPluginFiles is the canonical mixed-type plugin bundle used by these tests:
// one file of every previously-promoted kind, plus a two-server .mcp.json.
func flatPluginFiles() map[string][]byte {
	return map[string][]byte{
		"CLAUDE.md":                  []byte("# Flat Plugin\nOne capability, many bundled files."),
		".claude-plugin/plugin.json": []byte(`{"name":"Flat Plugin","description":"Everything in one capability","version":"2.1.0"}`),
		"skills/alpha/SKILL.md":      skillMD("Alpha", "alpha body"),
		"skills/alpha/ref.md":        []byte("# alpha reference"),
		"commands/build.md":          []byte("# build command"),
		"agents/reviewer.md":         []byte("# reviewer agent"),
		"rules/style.md":             []byte("# style rule"),
		"templates/pr.md":            []byte("# pr template"),
		".mcp.json":                  []byte(`{"mcpServers":{"one":{"command":"one"},"two":{"command":"two"}}}`),
	}
}

// flatPluginAssetPaths is every bundled path that must survive as an asset.
// CLAUDE.md is absent because it wins the plugin main-file pick and is stored
// as the item's own content instead.
func flatPluginAssetPaths() []string {
	return []string{
		".claude-plugin/plugin.json",
		".mcp.json",
		"agents/reviewer.md",
		"commands/build.md",
		"rules/style.md",
		"skills/alpha/SKILL.md",
		"skills/alpha/ref.md",
		"templates/pr.md",
	}
}

// uploadFlatPlugin uploads flatPluginFiles (with the given overrides applied)
// and returns the created plugin item id.
func uploadFlatPlugin(t *testing.T, slug string, overrides map[string][]byte) string {
	t.Helper()
	files := flatPluginFiles()
	for path, content := range overrides {
		files[path] = content
	}
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "plugin",
		"name":     "Flat Plugin",
		"slug":     slug,
	}, createTestZip(files))
	if w.Code != http.StatusCreated {
		t.Fatalf("plugin upload: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("expected plugin id in response, got %+v", created)
	}
	return id
}

// assetRelPaths returns the sorted rel_paths of an item's assets.
func assetRelPaths(t *testing.T, itemID string) []string {
	t.Helper()
	var assets []models.CapabilityAsset
	if err := database.DB.Where("item_id = ?", itemID).Order("rel_path asc").Find(&assets).Error; err != nil {
		t.Fatalf("load assets of %s: %v", itemID, err)
	}
	paths := make([]string, 0, len(assets))
	for _, a := range assets {
		paths = append(paths, a.RelPath)
	}
	sort.Strings(paths)
	return paths
}

// assetText returns the stored text of one asset, failing if it is missing or
// not stored inline.
func assetText(t *testing.T, itemID, relPath string) string {
	t.Helper()
	var asset models.CapabilityAsset
	if err := database.DB.Where("item_id = ? AND rel_path = ?", itemID, relPath).First(&asset).Error; err != nil {
		t.Fatalf("load asset %s of %s: %v", relPath, itemID, err)
	}
	if asset.TextContent == nil {
		t.Fatalf("asset %s has no inline text content: %+v", relPath, asset)
	}
	return *asset.TextContent
}

// countItems counts capability_items rows matching an optional where clause.
func countItems(t *testing.T, where string, args ...any) int64 {
	t.Helper()
	q := database.DB.Model(&models.CapabilityItem{})
	if where != "" {
		q = q.Where(where, args...)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		t.Fatalf("count items (%s): %v", where, err)
	}
	return n
}

// assertNoChildRows fails when any row carries a parent_plugin_id.
func assertNoChildRows(t *testing.T) {
	t.Helper()
	var children []models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id IS NOT NULL AND parent_plugin_id <> ''").
		Order("slug asc").Find(&children).Error; err != nil {
		t.Fatalf("load child rows: %v", err)
	}
	if len(children) == 0 {
		return
	}
	for _, ch := range children {
		t.Errorf("bundled file was promoted to a child row: slug=%q type=%q source_path=%q parent=%v",
			ch.Slug, ch.ItemType, ch.SourcePath, ch.ParentPluginID)
	}
	t.Fatalf("expected 0 rows with parent_plugin_id, got %d", len(children))
}

// AC-FP1: one archive upload yields exactly one capability row, and every
// bundled file survives as an asset of that row.
func TestUploadPlugin_FlatModel_OneRowKeepsEveryBundledFile(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadFlatPlugin(t, "flat-plugin", nil)

	// Exactly one capability row exists in the whole table.
	if total := countItems(t, ""); total != 1 {
		var rows []models.CapabilityItem
		database.DB.Order("slug asc").Find(&rows)
		for _, r := range rows {
			t.Logf("row: id=%s slug=%s type=%s parent=%v source_path=%s", r.ID, r.Slug, r.ItemType, r.ParentPluginID, r.SourcePath)
		}
		t.Fatalf("expected exactly 1 capability row after plugin upload, got %d", total)
	}
	assertNoChildRows(t)

	var plugin models.CapabilityItem
	if err := database.DB.First(&plugin, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}
	if plugin.ItemType != "plugin" {
		t.Fatalf("expected the single row to be the plugin, got item_type=%q", plugin.ItemType)
	}
	if plugin.ParentPluginID != nil {
		t.Fatalf("plugin row must have NULL parent_plugin_id, got %v", plugin.ParentPluginID)
	}
	if plugin.SourcePath != "CLAUDE.md" || plugin.Content != string(flatPluginFiles()["CLAUDE.md"]) {
		t.Fatalf("plugin main content mismatch: source_path=%q content=%q", plugin.SourcePath, plugin.Content)
	}

	// Nothing is lost: every bundled file is an asset of the plugin row.
	got := assetRelPaths(t, pluginID)
	want := flatPluginAssetPaths()
	if len(got) != len(want) {
		t.Fatalf("asset rel_paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asset rel_paths = %v, want %v", got, want)
		}
	}

	// Spot-check the two paths that used to be promoted hardest: the bundled
	// SKILL.md and the MCP manifest.
	if body := assetText(t, pluginID, "skills/alpha/SKILL.md"); body != string(skillMD("Alpha", "alpha body")) {
		t.Fatalf("bundled SKILL.md content not preserved verbatim, got %q", body)
	}
	var mcp struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(assetText(t, pluginID, ".mcp.json")), &mcp); err != nil {
		t.Fatalf("decode .mcp.json asset: %v", err)
	}
	if len(mcp.MCPServers) != 2 || mcp.MCPServers["one"] == nil || mcp.MCPServers["two"] == nil {
		t.Fatalf("both declared MCP servers must survive in the asset, got %#v", mcp.MCPServers)
	}
}

// AC-FP3: re-uploading with one bundled file changed bumps the plugin row's
// revision/content hash and still creates no child rows.
func TestUploadPlugin_FlatModel_UpdateBumpsRevisionWithoutChildren(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadFlatPlugin(t, "flat-plugin", nil)
	var before models.CapabilityItem
	if err := database.DB.First(&before, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin before: %v", err)
	}

	// Only a BUNDLED file changes; CLAUDE.md (the plugin's own content) does not.
	changed := flatPluginFiles()
	changed["commands/build.md"] = []byte("# build command v2")
	w := putMultipart(newItemRouter("u1"), "/api/items/"+pluginID, map[string]string{
		"commitMsg": "update bundled command",
	}, createTestZip(changed))
	if w.Code != http.StatusOK {
		t.Fatalf("re-upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", pluginID).Error; err != nil {
		t.Fatalf("load plugin after: %v", err)
	}
	if after.CurrentRevision != before.CurrentRevision+1 {
		t.Fatalf("expected revision %d after bundled-file change, got %d", before.CurrentRevision+1, after.CurrentRevision)
	}
	if after.ContentMD5 == before.ContentMD5 {
		t.Fatalf("bundled-file change must move the archive content hash, still %q", after.ContentMD5)
	}
	if body := assetText(t, pluginID, "commands/build.md"); body != "# build command v2" {
		t.Fatalf("updated bundled file not stored, got %q", body)
	}

	// Still one row, still no children.
	if total := countItems(t, ""); total != 1 {
		t.Fatalf("expected exactly 1 capability row after re-upload, got %d", total)
	}
	assertNoChildRows(t)

	// The archive's file set is still complete after the rebuild.
	got := assetRelPaths(t, pluginID)
	want := flatPluginAssetPaths()
	if len(got) != len(want) {
		t.Fatalf("asset rel_paths after re-upload = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asset rel_paths after re-upload = %v, want %v", got, want)
		}
	}
}

// Forking a DB-backed plugin creates exactly one new row, with a NULL
// parent_plugin_id, carrying the source plugin's assets. Legacy child rows that
// still point at the source are NOT forked alongside it.
func TestForkPlugin_FlatModel_CreatesOneRowWithNoChildren(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	pluginID := uploadFlatPlugin(t, "flat-plugin", nil)

	// Archive-sourced items are refused by ForkItem's separate MVP gate
	// ("fork_archive_unsupported"), which would hide the behaviour under test.
	// Flip the row to a plain DB-backed source so the fork path actually runs.
	if err := database.DB.Model(&models.CapabilityItem{}).Where("id = ?", pluginID).
		Update("source_type", "direct").Error; err != nil {
		t.Fatalf("relax archive fork gate: %v", err)
	}

	// A legacy child row (e.g. left by catalog ingest) still pointing at this
	// plugin must not be dragged into the fork.
	legacyChild := models.CapabilityItem{
		ID:              "legacy-child-1",
		RegistryID:      PublicRegistryID,
		RepoID:          "public",
		Slug:            "flat-plugin-legacy-child",
		ItemType:        "skill",
		Name:            "Legacy Child",
		Content:         "legacy child content",
		SourcePath:      "skills/alpha/SKILL.md",
		SourceType:      "direct",
		CreatedBy:       "u1",
		ParentPluginID:  &pluginID,
		CurrentRevision: 1,
		Status:          "active",
	}
	if err := database.DB.Create(&legacyChild).Error; err != nil {
		t.Fatalf("seed legacy child: %v", err)
	}

	w := forkReq(newForkRouter("u2"), pluginID)
	if w.Code != http.StatusCreated {
		t.Fatalf("fork plugin: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var forked map[string]any
	if err := json.NewDecoder(w.Body).Decode(&forked); err != nil {
		t.Fatalf("decode fork response: %v", err)
	}
	forkID, _ := forked["id"].(string)
	if forkID == "" || forkID == pluginID {
		t.Fatalf("expected a new fork id, got %q", forkID)
	}

	// Exactly one row was created: plugin + seeded legacy child + the fork.
	if total := countItems(t, ""); total != 3 {
		var rows []models.CapabilityItem
		database.DB.Order("slug asc").Find(&rows)
		for _, r := range rows {
			t.Logf("row: id=%s slug=%s type=%s parent=%v forkedFrom=%v", r.ID, r.Slug, r.ItemType, r.ParentPluginID, r.ForkedFromItemID)
		}
		t.Fatalf("expected 3 rows (plugin + legacy child + 1 fork), got %d", total)
	}
	if forks := countItems(t, "created_by = ?", "u2"); forks != 1 {
		t.Fatalf("expected the forker to own exactly 1 new row, got %d", forks)
	}
	if childForks := countItems(t, "forked_from_item_id = ?", legacyChild.ID); childForks != 0 {
		t.Fatalf("legacy child must not be forked, got %d forks of it", childForks)
	}

	var fork models.CapabilityItem
	if err := database.DB.First(&fork, "id = ?", forkID).Error; err != nil {
		t.Fatalf("load fork: %v", err)
	}
	if fork.ParentPluginID != nil {
		t.Fatalf("fork must have NULL parent_plugin_id, got %v", fork.ParentPluginID)
	}
	if fork.ItemType != "plugin" || fork.ForkedFromItemID == nil || *fork.ForkedFromItemID != pluginID {
		t.Fatalf("unexpected fork row: %+v", fork)
	}

	// The fork carries the whole bundle, so nothing is lost by not forking children.
	got := assetRelPaths(t, forkID)
	want := flatPluginAssetPaths()
	if len(got) != len(want) {
		t.Fatalf("fork asset rel_paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fork asset rel_paths = %v, want %v", got, want)
		}
	}
	if body := assetText(t, forkID, "skills/alpha/SKILL.md"); body != string(skillMD("Alpha", "alpha body")) {
		t.Fatalf("fork did not carry the bundled SKILL.md content, got %q", body)
	}
}
