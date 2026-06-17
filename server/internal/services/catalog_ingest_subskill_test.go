package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// writeMultiEntryBundle materializes a catalog bundle with an arbitrary set of
// entries, laying each one's primary file out under
// catalog-download/<type-dir>/<id>/<file> exactly like the upstream bundle.
// `bodies` maps entry.ID → primary-file body so callers can vary file SHAs and
// supply valid frontmatter for skills.
func writeMultiEntryBundle(t *testing.T, entries []catalogEntry, bodies map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	typeCounts := map[string]int{}
	for _, e := range entries {
		typeCounts[e.Type]++
	}
	manifest := map[string]any{
		"schema_version": SupportedBundleSchemaVersion,
		"generated_at":   "2026-06-08T00:00:00Z",
		"entry_count":    len(entries),
		"index_sha256":   "test-sha",
		"type_counts":    typeCounts,
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ib, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), ib, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	for _, e := range entries {
		typeDir, fileName, ok := typeDirAndFile(e.Type)
		if !ok {
			t.Fatalf("unsupported entry type %q in test fixture", e.Type)
		}
		entryDir := filepath.Join(dir, "catalog-download", typeDir, e.ID)
		if err := os.MkdirAll(entryDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", entryDir, err)
		}
		body, ok := bodies[e.ID]
		if !ok {
			t.Fatalf("no body provided for entry %q", e.ID)
		}
		if err := os.WriteFile(filepath.Join(entryDir, fileName), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", fileName, err)
		}
	}
	return dir
}

func loadItemByID(t *testing.T, db *gorm.DB, id string) models.CapabilityItem {
	t.Helper()
	var item models.CapabilityItem
	if err := db.First(&item, "id = ?", id).Error; err != nil {
		t.Fatalf("load item %q: %v", id, err)
	}
	return item
}

func pluginEntry(id string) catalogEntry {
	return catalogEntry{
		ID:          id,
		Type:        "plugin",
		Source:      "first-party/" + id,
		Description: "a plugin",
		Category:    "tooling",
		FinalScore:  4.0,
	}
}

func subSkillEntry(id, bundledIn string) catalogEntry {
	return catalogEntry{
		ID:          id,
		Type:        "skill",
		Source:      "first-party/" + id,
		Description: "a bundled skill",
		Category:    "tooling",
		FinalScore:  4.0,
		BundledIn:   bundledIn,
	}
}

func bundledMCPEntry(id, bundledIn string) catalogEntry {
	return catalogEntry{
		ID:          id,
		Type:        "mcp",
		Source:      "first-party/" + id,
		Description: "a bundled mcp",
		Category:    "tooling",
		FinalScore:  4.0,
		BundledIn:   bundledIn,
	}
}

func skillBodyFor(name string) string {
	return "---\nname: " + name + "\ndescription: a skill\n---\n# " + name + "\nbody\n"
}

func mcpBodyFor(command string) string {
	return `{"mcpServers":{"demo":{"command":"` + command + `"}}}`
}

func pluginBodyFor(name string) string {
	// ParsePluginJSON requires an install block with plugin_name /
	// marketplace_name / marketplace_repo; the slug is derived from the entry
	// directory name (== entry.ID), so no collision with sub-skills.
	b, _ := json.Marshal(map[string]any{
		"name":        name,
		"description": "a plugin",
		"install": map[string]any{
			"plugin_name":      name,
			"marketplace_name": "first-party",
			"marketplace_repo": "first-party/market",
		},
	})
	return string(b)
}

// TestIngest_PluginMCP_LinksParentPlugin verifies that MCP entries expanded out
// of a plugin follow the same parent_plugin_id linking path as bundled skills.
func TestIngest_PluginMCP_LinksParentPlugin(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("mcp-plugin"),
		bundledMCPEntry("mcp-plugin-demo", "mcp-plugin"),
	}
	bodies := map[string]string{
		"mcp-plugin":      pluginBodyFor("MCP Plugin"),
		"mcp-plugin-demo": mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var plugin models.CapabilityItem
	if err := db.Where("item_type = ? AND source_path LIKE ?", "plugin", "plugins/mcp-plugin/%").First(&plugin).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}
	var mcp models.CapabilityItem
	if err := db.Where("item_type = ? AND source_path LIKE ?", "mcp", "mcp/mcp-plugin-demo/%").First(&mcp).Error; err != nil {
		t.Fatalf("load bundled MCP: %v", err)
	}
	if mcp.ParentPluginID == nil || *mcp.ParentPluginID != plugin.ID {
		t.Fatalf("bundled MCP parent_plugin_id = %v; want %s", mcp.ParentPluginID, plugin.ID)
	}
	meta := decodeObj(t, mcp.Metadata)
	if meta["bundled_in"] != "mcp-plugin" {
		t.Fatalf("bundled MCP metadata.bundled_in = %v; want mcp-plugin", meta["bundled_in"])
	}
}

func TestIngest_PluginMCP_DoesNotMergeSameServerNameAcrossPlugins(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("first-plugin"),
		pluginEntry("second-plugin"),
		bundledMCPEntry("first-plugin-demo", "first-plugin"),
		bundledMCPEntry("second-plugin-demo", "second-plugin"),
	}
	bodies := map[string]string{
		"first-plugin":       pluginBodyFor("First Plugin"),
		"second-plugin":      pluginBodyFor("Second Plugin"),
		"first-plugin-demo":  mcpBodyFor("node"),
		"second-plugin-demo": mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var mcps []models.CapabilityItem
	if err := db.Where("item_type = ? AND source_path LIKE ?", "mcp", "mcp/%-plugin-demo/%").
		Order("source_path asc").Find(&mcps).Error; err != nil {
		t.Fatalf("load bundled MCPs: %v", err)
	}
	if len(mcps) != 2 {
		t.Fatalf("same server name in two plugins must create two MCP rows; got %d", len(mcps))
	}
	for _, mcp := range mcps {
		if mcp.ParentPluginID == nil {
			t.Fatalf("bundled MCP %s missing parent_plugin_id", mcp.SourcePath)
		}
	}
	if mcps[0].Slug == mcps[1].Slug {
		t.Fatalf("bundled MCP slugs must be entry-scoped, both got %q", mcps[0].Slug)
	}
}

// TestIngest_SubSkill_LinksParentPlugin covers the happy path: a bundle with one
// plugin + two sub-skills (both bundled_in the plugin) links both sub-skills'
// parent_plugin_id to the plugin's DB id, and mirrors bundled_in into metadata.
func TestIngest_SubSkill_LinksParentPlugin(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("acme-plugin"),
		subSkillEntry("acme-plugin-alpha", "acme-plugin"),
		subSkillEntry("acme-plugin-beta", "acme-plugin"),
	}
	bodies := map[string]string{
		"acme-plugin":       pluginBodyFor("Acme Plugin"),
		"acme-plugin-alpha": skillBodyFor("Alpha Skill"),
		"acme-plugin-beta":  skillBodyFor("Beta Skill"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var plugin models.CapabilityItem
	if err := db.Where("item_type = ? AND source_path LIKE ?", "plugin", "plugins/acme-plugin/%").First(&plugin).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}

	for _, skillID := range []string{"acme-plugin-alpha", "acme-plugin-beta"} {
		var skill models.CapabilityItem
		if err := db.Where("item_type = ? AND source_path LIKE ?", "skill", "skills/"+skillID+"/%").First(&skill).Error; err != nil {
			t.Fatalf("load sub-skill %s: %v", skillID, err)
		}
		if skill.ParentPluginID == nil || *skill.ParentPluginID != plugin.ID {
			t.Fatalf("sub-skill %s parent_plugin_id = %v; want %s", skillID, skill.ParentPluginID, plugin.ID)
		}
		meta := decodeObj(t, skill.Metadata)
		if meta["bundled_in"] != "acme-plugin" {
			t.Errorf("sub-skill %s metadata.bundled_in = %v; want acme-plugin", skillID, meta["bundled_in"])
		}
	}
}

// TestIngest_SubSkill_ParentAfterSkillInBatch covers the order-independent batch
// case: the sub-skills appear BEFORE the parent plugin in index.json. The second
// pass must still resolve the parent because it reads rows fresh after all writes.
func TestIngest_SubSkill_ParentAfterSkillInBatch(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		subSkillEntry("ord-plugin-alpha", "ord-plugin"),
		subSkillEntry("ord-plugin-beta", "ord-plugin"),
		pluginEntry("ord-plugin"), // parent last on purpose
	}
	bodies := map[string]string{
		"ord-plugin":       pluginBodyFor("Ord Plugin"),
		"ord-plugin-alpha": skillBodyFor("Ord Alpha"),
		"ord-plugin-beta":  skillBodyFor("Ord Beta"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var plugin models.CapabilityItem
	if err := db.Where("item_type = ? AND source_path LIKE ?", "plugin", "plugins/ord-plugin/%").First(&plugin).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}
	for _, skillID := range []string{"ord-plugin-alpha", "ord-plugin-beta"} {
		var skill models.CapabilityItem
		if err := db.Where("source_path LIKE ?", "skills/"+skillID+"/%").First(&skill).Error; err != nil {
			t.Fatalf("load sub-skill %s: %v", skillID, err)
		}
		if skill.ParentPluginID == nil || *skill.ParentPluginID != plugin.ID {
			t.Fatalf("out-of-order sub-skill %s parent_plugin_id = %v; want %s", skillID, skill.ParentPluginID, plugin.ID)
		}
	}
}

// TestIngest_SubSkill_Idempotent ensures a re-ingest of the same bundle does not
// create duplicate rows and keeps parent_plugin_id stable.
func TestIngest_SubSkill_Idempotent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("idem-plugin"),
		subSkillEntry("idem-plugin-alpha", "idem-plugin"),
	}
	bodies := map[string]string{
		"idem-plugin":       pluginBodyFor("Idem Plugin"),
		"idem-plugin-alpha": skillBodyFor("Idem Alpha"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Re-ingest the SAME bundle (same SHAs).
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	var skillCount int64
	db.Model(&models.CapabilityItem{}).Where("source_path LIKE ?", "skills/idem-plugin-alpha/%").Count(&skillCount)
	if skillCount != 1 {
		t.Fatalf("re-ingest must not duplicate sub-skill; count=%d", skillCount)
	}
	var pluginCount int64
	db.Model(&models.CapabilityItem{}).Where("source_path LIKE ?", "plugins/idem-plugin/%").Count(&pluginCount)
	if pluginCount != 1 {
		t.Fatalf("re-ingest must not duplicate plugin; count=%d", pluginCount)
	}

	var plugin models.CapabilityItem
	db.Where("source_path LIKE ?", "plugins/idem-plugin/%").First(&plugin)
	var skill models.CapabilityItem
	db.Where("source_path LIKE ?", "skills/idem-plugin-alpha/%").First(&skill)
	if skill.ParentPluginID == nil || *skill.ParentPluginID != plugin.ID {
		t.Fatalf("parent link not stable across re-ingest: %v", skill.ParentPluginID)
	}
}

// TestIngest_SubSkill_OrphanArchivedWhenPluginRemoved verifies the cascade: when
// the parent plugin and its orphan sub-skills vanish from a later bundle, the
// existing "disappeared → soft-archive" mechanism archives them.
func TestIngest_SubSkill_OrphanArchivedWhenPluginRemoved(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("gone-plugin"),
		subSkillEntry("gone-plugin-alpha", "gone-plugin"),
	}
	bodies := map[string]string{
		"gone-plugin":       pluginBodyFor("Gone Plugin"),
		"gone-plugin-alpha": skillBodyFor("Gone Alpha"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Second bundle: plugin + sub-skill both removed (empty index, but keep one
	// unrelated entry so the bundle is non-trivial and seenSourcePaths is populated).
	keep := []catalogEntry{pluginEntry("survivor-plugin")}
	keepBodies := map[string]string{"survivor-plugin": pluginBodyFor("Survivor")}
	dir2 := writeMultiEntryBundle(t, keep, keepBodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	var skill models.CapabilityItem
	if err := db.Where("source_path LIKE ?", "skills/gone-plugin-alpha/%").First(&skill).Error; err != nil {
		t.Fatalf("load archived sub-skill: %v", err)
	}
	if skill.Status != "archived" {
		t.Fatalf("orphan sub-skill should be archived when parent removed; status=%q", skill.Status)
	}
	var plugin models.CapabilityItem
	db.Where("source_path LIKE ?", "plugins/gone-plugin/%").First(&plugin)
	if plugin.Status != "archived" {
		t.Fatalf("removed plugin should be archived; status=%q", plugin.Status)
	}
}

// TestIngest_SubSkill_UnlinkWhenBundledInDropped verifies that a skill which was
// previously a sub-skill, but is emitted WITHOUT bundled_in in a later bundle
// (upstream now matches it to an independent skill), has its parent_plugin_id
// cleared while the row itself survives.
func TestIngest_SubSkill_UnlinkWhenBundledInDropped(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("link-plugin"),
		subSkillEntry("shared-skill", "link-plugin"),
	}
	bodies := map[string]string{
		"link-plugin":  pluginBodyFor("Link Plugin"),
		"shared-skill": skillBodyFor("Shared Skill"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	var linked models.CapabilityItem
	db.Where("source_path LIKE ?", "skills/shared-skill/%").First(&linked)
	if linked.ParentPluginID == nil {
		t.Fatalf("setup: sub-skill should be linked after first ingest")
	}

	// Second bundle: same plugin + same skill, but the skill no longer carries
	// bundled_in (now treated as independent). Keep body identical so it routes
	// through the metadata-only / unchanged path — the link must still clear.
	entries2 := []catalogEntry{
		pluginEntry("link-plugin"),
		subSkillEntry("shared-skill", ""), // bundled_in dropped → independent
	}
	dir2 := writeMultiEntryBundle(t, entries2, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	after := loadItemByID(t, db, linked.ID)
	if after.ParentPluginID != nil && *after.ParentPluginID != "" {
		t.Fatalf("dropping bundled_in must clear parent_plugin_id; got %v", after.ParentPluginID)
	}
	meta := decodeObj(t, after.Metadata)
	if _, ok := meta["bundled_in"]; ok {
		t.Fatalf("dropping bundled_in must clear metadata.bundled_in; got %#v", meta["bundled_in"])
	}
	if after.Status == "archived" {
		t.Fatalf("un-linked independent skill must survive, not archive")
	}
}

// TestIngest_ArchivedItemResurrectsWhenEntryReappears verifies the round-trip:
// an item archived because its entry vanished from one bundle must come back
// to status=active when a later bundle carries the entry again — including the
// content-UNCHANGED case (same SHA), which routes through the metadata-only
// path rather than updateItem.
func TestIngest_ArchivedItemResurrectsWhenEntryReappears(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("revive-plugin"),
		subSkillEntry("revive-plugin-alpha", "revive-plugin"),
	}
	bodies := map[string]string{
		"revive-plugin":       pluginBodyFor("Revive Plugin"),
		"revive-plugin-alpha": skillBodyFor("Revive Alpha"),
	}
	full := writeMultiEntryBundle(t, entries, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: full}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Second bundle drops both entries → both rows soft-archive.
	empty := writeMultiEntryBundle(t, nil, nil)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: empty}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("archiving ingest: %v", err)
	}
	var plugin models.CapabilityItem
	db.Where("source_path LIKE ?", "plugins/revive-plugin/%").First(&plugin)
	if plugin.Status != "archived" {
		t.Fatalf("precondition: plugin should be archived, got %q", plugin.Status)
	}

	// Third bundle re-ships the SAME content (identical SHAs) → both rows
	// must resurrect via the metadata-only path.
	revived := writeMultiEntryBundle(t, entries, bodies)
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: revived}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("revive ingest: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("revive ingest failed=%d errs=%v", res.Failed, res.Errors)
	}

	var pluginAfter, skillAfter models.CapabilityItem
	db.Where("source_path LIKE ?", "plugins/revive-plugin/%").First(&pluginAfter)
	db.Where("source_path LIKE ?", "skills/revive-plugin-alpha/%").First(&skillAfter)
	if pluginAfter.Status != "active" {
		t.Fatalf("plugin must resurrect to active, got %q", pluginAfter.Status)
	}
	if skillAfter.Status != "active" {
		t.Fatalf("sub-skill must resurrect to active, got %q", skillAfter.Status)
	}
	if skillAfter.ParentPluginID == nil || *skillAfter.ParentPluginID != pluginAfter.ID {
		t.Fatalf("parent link must survive resurrection: %v", skillAfter.ParentPluginID)
	}
}

// TestIngest_SubSkill_EntryTypeAuthoritative guards against InferItemType's
// path-substring heuristics re-classifying bundled children: a sub-skill dir
// named "...-webhooks" matches "hooks/" (and "...-commands" matches
// "commands/"), which would type the row hook/command and orphan it from
// parent-link reconciliation. For plugin-bundled children the upstream entry
// type wins.
func TestIngest_SubSkill_EntryTypeAuthoritative(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("tplug"),
		subSkillEntry("tplug-webhooks", "tplug"),
		subSkillEntry("tplug-interactive-commands", "tplug"),
	}
	bodies := map[string]string{
		"tplug":                      pluginBodyFor("T Plugin"),
		"tplug-webhooks":             skillBodyFor("Webhooks Guide"),
		"tplug-interactive-commands": skillBodyFor("Tmux Commands Guide"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("ingest failed=%d errs=%v", res.Failed, res.Errors)
	}

	var plugin models.CapabilityItem
	db.Where("source_path LIKE ?", "plugins/tplug/%").First(&plugin)
	for _, entryDir := range []string{"tplug-webhooks", "tplug-interactive-commands"} {
		var child models.CapabilityItem
		if err := db.Where("source_path LIKE ?", "skills/"+entryDir+"/%").First(&child).Error; err != nil {
			t.Fatalf("child %s not found: %v", entryDir, err)
		}
		if child.ItemType != "skill" {
			t.Fatalf("child %s: entry type must win over path heuristics, got %q", entryDir, child.ItemType)
		}
		if child.ParentPluginID == nil || *child.ParentPluginID != plugin.ID {
			t.Fatalf("child %s: parent link missing", entryDir)
		}
	}
}

// TestIngest_Reconcile_DoesNotTouchUploadedRows: a zip-promoted sub-skill row
// (source_type='archive') sharing the exact entryDir shape of a catalog skill
// entry must be invisible to the catalog's parent-link reconcile — previously
// the single-valued entryDir map could pick the uploaded row and silently
// clear (or rewrite) its parent_plugin_id on every scheduled ingest.
func TestIngest_Reconcile_DoesNotTouchUploadedRows(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Seed an uploaded plugin + its promoted child OUTSIDE the catalog flow.
	uploadParent := models.CapabilityItem{
		ID: "upload-plugin-1", RegistryID: PublicRegistryID, RepoID: PublicRepoID,
		Slug: "my-upload-plugin", ItemType: "plugin", Name: "My Upload Plugin",
		SourcePath: ".plugin.json", SourceType: "archive", Status: "active",
		CreatedBy: "user-1", UpdatedBy: "user-1",
	}
	parentID := uploadParent.ID
	uploadChild := models.CapabilityItem{
		ID: "upload-child-1", RegistryID: PublicRegistryID, RepoID: PublicRepoID,
		Slug: "my-upload-plugin-shared-name", ItemType: "skill", Name: "shared-name",
		// Byte-identical entryDir shape to the catalog entry below.
		SourcePath: "skills/shared-name/SKILL.md", SourceType: "archive",
		ParentPluginID: &parentID, Status: "active",
		CreatedBy: "user-1", UpdatedBy: "user-1",
	}
	if err := db.Create(&uploadParent).Error; err != nil {
		t.Fatalf("seed upload parent: %v", err)
	}
	if err := db.Create(&uploadChild).Error; err != nil {
		t.Fatalf("seed upload child: %v", err)
	}

	// Catalog bundle ships an INDEPENDENT skill with the same entry id —
	// reconcile sees bundled_in="" for entryDir skills/shared-name.
	entries := []catalogEntry{{
		ID: "shared-name", Type: "skill", Source: "catalog/shared-name",
		Description: "an independent catalog skill", Category: "tooling",
	}}
	bodies := map[string]string{"shared-name": skillBodyFor("shared-name")}
	dir := writeMultiEntryBundle(t, entries, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	after := loadItemByID(t, db, "upload-child-1")
	if after.ParentPluginID == nil || *after.ParentPluginID != parentID {
		t.Fatalf("catalog reconcile must not unlink an uploaded (source_type=archive) child; got parent=%v", after.ParentPluginID)
	}
}

// TestIngest_Reconcile_SkipsArchivedParent: a child whose bundled_in points at
// a plugin that only exists as an ARCHIVED row must not be linked to it.
func TestIngest_Reconcile_SkipsArchivedParent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Round 1: plugin + child ingest and link.
	entries := []catalogEntry{
		pluginEntry("ghost-plugin"),
		subSkillEntry("ghost-plugin-alpha", "ghost-plugin"),
	}
	bodies := map[string]string{
		"ghost-plugin":       pluginBodyFor("Ghost Plugin"),
		"ghost-plugin-alpha": skillBodyFor("Ghost Alpha"),
	}
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Round 2: plugin vanishes, child stays (still declares bundled_in).
	childOnly := []catalogEntry{subSkillEntry("ghost-plugin-alpha", "ghost-plugin")}
	childBodies := map[string]string{"ghost-plugin-alpha": skillBodyFor("Ghost Alpha")}
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, childOnly, childBodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	var plugin models.CapabilityItem
	db.Where("source_path LIKE ?", "plugins/ghost-plugin/%").First(&plugin)
	if plugin.Status != "archived" {
		t.Fatalf("vanished plugin should be archived, got %q", plugin.Status)
	}
	var child models.CapabilityItem
	db.Where("source_path LIKE ?", "skills/ghost-plugin-alpha/%").First(&child)
	if child.Status != "active" {
		t.Fatalf("child still upstream must stay active, got %q", child.Status)
	}
	// Archived parent must not retain (or re-acquire) the link.
	if child.ParentPluginID != nil && *child.ParentPluginID == plugin.ID {
		t.Fatalf("active child must not stay linked to an archived parent")
	}

	// Round 3: same child-only bundle again — link must not silently return.
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, childOnly, childBodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	child = loadItemByID(t, db, child.ID)
	if child.ParentPluginID != nil && *child.ParentPluginID == plugin.ID {
		t.Fatalf("archived parent re-linked on later round")
	}
}

// TestIngest_BundledSkill_SlugConflictFallsBackToSuffix: a foreign row already
// holding the bundled child's (repo, type, slug) must not make the entry fail
// every round — insertItem retries with -2.. suffixes.
func TestIngest_BundledSkill_SlugConflictFallsBackToSuffix(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Foreign occupant of the slug (e.g. a user createDirect row).
	occupant := models.CapabilityItem{
		ID: "occupant-1", RegistryID: PublicRegistryID, RepoID: PublicRepoID,
		Slug: "conflict-plugin-tool", ItemType: "skill", Name: "occupant",
		SourcePath: "SKILL.md", SourceType: "direct", Status: "active",
		CreatedBy: "user-2", UpdatedBy: "user-2",
	}
	if err := db.Create(&occupant).Error; err != nil {
		t.Fatalf("seed occupant: %v", err)
	}

	entries := []catalogEntry{
		pluginEntry("conflict-plugin"),
		subSkillEntry("conflict-plugin-tool", "conflict-plugin"),
	}
	bodies := map[string]string{
		"conflict-plugin":      pluginBodyFor("Conflict Plugin"),
		"conflict-plugin-tool": skillBodyFor("Conflict Tool"),
	}
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("slug conflict must not fail the entry: failed=%d errs=%v", res.Failed, res.Errors)
	}

	var child models.CapabilityItem
	if err := db.Where("source_path LIKE ?", "skills/conflict-plugin-tool/%").First(&child).Error; err != nil {
		t.Fatalf("bundled child not inserted despite conflict: %v", err)
	}
	if child.Slug != "conflict-plugin-tool-2" {
		t.Fatalf("expected suffixed slug, got %q", child.Slug)
	}
	if child.ParentPluginID == nil {
		t.Fatalf("suffixed child must still link to parent")
	}
	// Occupant untouched.
	occ := loadItemByID(t, db, "occupant-1")
	if occ.Slug != "conflict-plugin-tool" || occ.ParentPluginID != nil {
		t.Fatalf("foreign occupant must be untouched: %+v", occ)
	}

	// Idempotency: rerun must update the suffixed row, not add a third.
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	var count int64
	db.Model(&models.CapabilityItem{}).Where("source_path LIKE ?", "skills/conflict-plugin-tool/%").Count(&count)
	if count != 1 {
		t.Fatalf("re-ingest duplicated the child: count=%d", count)
	}
}

// TestIngest_MCP_IndependentToBundledFlip_AdoptsRow: when an existing
// independent mcp entry gains bundled_in upstream, the slug rewrite must adopt
// (and migrate) the entry's own pre-flip row instead of inserting a duplicate
// that leaves the old row to churn versions/scans every round.
func TestIngest_MCP_IndependentToBundledFlip_AdoptsRow(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	independent := catalogEntry{
		ID: "flip-mcp", Type: "mcp", Source: "catalog/flip-mcp",
		Description: "an mcp", Category: "tooling",
	}
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t,
		[]catalogEntry{independent}, map[string]string{"flip-mcp": mcpBodyFor("demo-cmd")})},
		IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	var before models.CapabilityItem
	if err := db.Where("source_path LIKE ?", "mcp/flip-mcp/%").First(&before).Error; err != nil {
		t.Fatalf("independent row missing: %v", err)
	}

	// Flip: same entry id, now bundled (content changed so the full path runs).
	entries := []catalogEntry{
		pluginEntry("flip-plugin"),
		bundledMCPEntry("flip-mcp", "flip-plugin"),
	}
	bodies := map[string]string{
		"flip-plugin": pluginBodyFor("Flip Plugin"),
		"flip-mcp":    mcpBodyFor("demo-cmd-v2"),
	}
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("flip ingest: %v", err)
	}

	var rows []models.CapabilityItem
	db.Where("source_path LIKE ?", "mcp/flip-mcp/%").Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("flip must adopt the existing row, got %d rows", len(rows))
	}
	if rows[0].ID != before.ID {
		t.Fatalf("adopted row id changed: %s -> %s", before.ID, rows[0].ID)
	}
	if rows[0].ParentPluginID == nil {
		t.Fatalf("flipped row must link to the parent plugin")
	}
	if rows[0].Slug == before.Slug {
		t.Fatalf("flipped row slug must migrate to the scoped slug, still %q", rows[0].Slug)
	}
}
