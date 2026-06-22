package services

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
)

// bundledChildEntry builds a catalog entry of an arbitrary bundled-child type
// (rule/template/command/subagent), linked to its parent plugin via bundled_in.
func bundledChildEntry(id, itemType, bundledIn string) catalogEntry {
	return catalogEntry{
		ID:          id,
		Type:        itemType,
		Source:      "first-party/" + id,
		Description: "a bundled " + itemType,
		Category:    "tooling",
		FinalScore:  4.0,
		BundledIn:   bundledIn,
	}
}

// mdBodyFor builds a minimal markdown body with frontmatter so ParseSKILLMD
// (the default dispatch for RULE.md / TEMPLATE.md / COMMAND.md / SUBAGENT.md)
// succeeds. The entry.Type override during ingest sets the final item_type.
func mdBodyFor(name string) string {
	return "---\nname: " + name + "\ndescription: a " + name + "\n---\n# " + name + "\nbody\n"
}

// TestIngest_BundledChildTypes_LinkParentPlugin verifies the catalog path
// promotes and parent-links every bundled-child type the work tree needs:
// rule, template, command, subagent (alongside the existing skill/mcp).
func TestIngest_BundledChildTypes_LinkParentPlugin(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("cospower-plugin"),
		subSkillEntry("cospower-plugin-skill", "cospower-plugin"),
		bundledChildEntry("cospower-plugin-rule", "rule", "cospower-plugin"),
		bundledChildEntry("cospower-plugin-template", "template", "cospower-plugin"),
		bundledChildEntry("cospower-plugin-command", "command", "cospower-plugin"),
		bundledChildEntry("cospower-plugin-agent", "subagent", "cospower-plugin"),
	}
	bodies := map[string]string{
		"cospower-plugin":          pluginBodyFor("Cospower Plugin"),
		"cospower-plugin-skill":    skillBodyFor("Cospower Skill"),
		"cospower-plugin-rule":     mdBodyFor("Security Rule"),
		"cospower-plugin-template": mdBodyFor("System Design Template"),
		"cospower-plugin-command":  mdBodyFor("Run Tests"),
		"cospower-plugin-agent":    mdBodyFor("Reviewer"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var plugin models.CapabilityItem
	if err := db.Where("item_type = ? AND source_path LIKE ?", "plugin", "plugins/cospower-plugin/%").First(&plugin).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}

	// (entry id, type-dir, expected item_type) for each bundled child.
	cases := []struct {
		entryID  string
		typeDir  string
		wantType string
	}{
		{"cospower-plugin-skill", "skills", "skill"},
		{"cospower-plugin-rule", "rules", "rule"},
		{"cospower-plugin-template", "templates", "template"},
		{"cospower-plugin-command", "commands", "command"},
		{"cospower-plugin-agent", "subagents", "subagent"},
	}
	for _, tc := range cases {
		var child models.CapabilityItem
		if err := db.Where("source_path LIKE ?", tc.typeDir+"/"+tc.entryID+"/%").First(&child).Error; err != nil {
			t.Fatalf("load bundled child %s: %v", tc.entryID, err)
		}
		if child.ItemType != tc.wantType {
			t.Errorf("child %s item_type = %q, want %q", tc.entryID, child.ItemType, tc.wantType)
		}
		if child.ParentPluginID == nil || *child.ParentPluginID != plugin.ID {
			t.Errorf("child %s parent_plugin_id = %v, want %s", tc.entryID, child.ParentPluginID, plugin.ID)
		}
		meta := decodeObj(t, child.Metadata)
		if meta["bundled_in"] != "cospower-plugin" {
			t.Errorf("child %s metadata.bundled_in = %v, want cospower-plugin", tc.entryID, meta["bundled_in"])
		}
	}
}

// TestIngest_FaithfulSourcePath_AllTypes verifies that when an upstream entry
// carries a plugin-root-relative source_path, the ingested row stores that exact
// path on source_path (so the work tree mirrors the real repo) for every bundled
// child type, while file content is still read from the synthetic bundle layout.
func TestIngest_FaithfulSourcePath_AllTypes(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	withPath := func(e catalogEntry, sourcePath string) catalogEntry {
		e.SourcePath = sourcePath
		return e
	}

	entries := []catalogEntry{
		pluginEntry("faithful-plugin"),
		withPath(subSkillEntry("faithful-plugin-skill", "faithful-plugin"), "skills/requirement-analysis/SKILL.md"),
		withPath(bundledChildEntry("faithful-plugin-rule", "rule", "faithful-plugin"), "rules/dfx/安全.md"),
		withPath(bundledChildEntry("faithful-plugin-template", "template", "faithful-plugin"), "templates/system-design.md"),
		withPath(bundledChildEntry("faithful-plugin-command", "command", "faithful-plugin"), "commands/run-tests.md"),
		withPath(bundledChildEntry("faithful-plugin-agent", "subagent", "faithful-plugin"), "agents/reviewer.md"),
	}
	bodies := map[string]string{
		"faithful-plugin":          pluginBodyFor("Faithful Plugin"),
		"faithful-plugin-skill":    skillBodyFor("Faithful Skill"),
		"faithful-plugin-rule":     mdBodyFor("Security Rule"),
		"faithful-plugin-template": mdBodyFor("System Design"),
		"faithful-plugin-command":  mdBodyFor("Run Tests"),
		"faithful-plugin-agent":    mdBodyFor("Reviewer"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var plugin models.CapabilityItem
	if err := db.Where("item_type = ? AND catalog_entry_dir = ?", "plugin", "plugins/faithful-plugin").First(&plugin).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}

	// (entry id, expected faithful source_path, synthetic entry-dir match key).
	cases := []struct {
		entryID      string
		wantSource   string
		wantEntryDir string
	}{
		{"faithful-plugin-skill", "skills/requirement-analysis/SKILL.md", "skills/faithful-plugin-skill"},
		{"faithful-plugin-rule", "rules/dfx/安全.md", "rules/faithful-plugin-rule"},
		{"faithful-plugin-template", "templates/system-design.md", "templates/faithful-plugin-template"},
		{"faithful-plugin-command", "commands/run-tests.md", "commands/faithful-plugin-command"},
		{"faithful-plugin-agent", "agents/reviewer.md", "subagents/faithful-plugin-agent"},
	}
	for _, tc := range cases {
		// Locate the row by the synthetic match key (catalog_entry_dir), NOT by
		// source_path — source_path is now the faithful path.
		var child models.CapabilityItem
		if err := db.Where("catalog_entry_dir = ?", tc.wantEntryDir).First(&child).Error; err != nil {
			t.Fatalf("load child %s by entry-dir %q: %v", tc.entryID, tc.wantEntryDir, err)
		}
		if child.SourcePath != tc.wantSource {
			t.Errorf("child %s source_path = %q, want faithful %q", tc.entryID, child.SourcePath, tc.wantSource)
		}
		if child.ParentPluginID == nil || *child.ParentPluginID != plugin.ID {
			t.Errorf("child %s parent_plugin_id = %v, want %s (parent link must survive faithful path)", tc.entryID, child.ParentPluginID, plugin.ID)
		}
	}
}

// TestIngest_FaithfulSourcePath_Idempotent is the key regression guard for the
// decoupling: with faithful source_path, re-ingest must still match the existing
// row via catalog_entry_dir (NOT via source_path) and therefore NOT duplicate.
func TestIngest_FaithfulSourcePath_Idempotent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	rule := bundledChildEntry("idem-faithful-rule", "rule", "idem-faithful")
	rule.SourcePath = "rules/dfx/安全.md"
	entries := []catalogEntry{
		pluginEntry("idem-faithful"),
		rule,
	}
	bodies := map[string]string{
		"idem-faithful":      pluginBodyFor("Idem Faithful"),
		"idem-faithful-rule": mdBodyFor("Idem Rule"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	for i := 0; i < 2; i++ {
		if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
			t.Fatalf("ingest pass %d: %v", i, err)
		}
	}

	var count int64
	db.Model(&models.CapabilityItem{}).Where("catalog_entry_dir = ?", "rules/idem-faithful-rule").Count(&count)
	if count != 1 {
		t.Fatalf("re-ingest with faithful source_path duplicated the rule child; count=%d", count)
	}
	var rl models.CapabilityItem
	db.Where("catalog_entry_dir = ?", "rules/idem-faithful-rule").First(&rl)
	if rl.SourcePath != "rules/dfx/安全.md" {
		t.Fatalf("rule source_path = %q, want faithful rules/dfx/安全.md", rl.SourcePath)
	}
	if rl.ParentPluginID == nil {
		t.Fatalf("rule parent link lost across re-ingest")
	}
}

// TestIngest_FaithfulSourcePath_MetadataOnlyConverges is the regression guard
// for the P3-rollout bug: an EXISTING skill whose content sha is unchanged (so
// it routes through the metadata-only path) but whose upstream entry now carries
// a faithful source_path must have its DB source_path + catalog_entry_dir
// converged — not left at the stale synthetic value.
func TestIngest_FaithfulSourcePath_MetadataOnlyConverges(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	const skillBody = "---\nname: Aireq Evaluator\ndescription: an evaluator\n---\n# Aireq\nbody\n"
	faithful := "skills/aireq-evaluator/SKILL.md"

	// Pass 1: no faithful source_path on the entry → row gets the synthetic
	// "skills/<id>/SKILL.md" path + synthetic catalog_entry_dir (simulates a
	// pre-P3 row already in prod).
	legacy := subSkillEntry("cospowers-requirements-aireq-evaluator", "cospowers-requirements")
	entries1 := []catalogEntry{pluginEntry("cospowers-requirements"), legacy}
	bodies := map[string]string{
		"cospowers-requirements":                 pluginBodyFor("Cospowers Requirements"),
		"cospowers-requirements-aireq-evaluator": skillBody,
	}
	dir1 := writeMultiEntryBundle(t, entries1, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir1}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("pass1 ingest: %v", err)
	}

	syntheticPath := "skills/cospowers-requirements-aireq-evaluator/SKILL.md"
	var before models.CapabilityItem
	if err := db.Where("item_type = ? AND catalog_entry_dir = ?", "skill", "skills/cospowers-requirements-aireq-evaluator").First(&before).Error; err != nil {
		t.Fatalf("load skill after pass1: %v", err)
	}
	if before.SourcePath != syntheticPath {
		t.Fatalf("pass1 source_path = %q, want synthetic %q", before.SourcePath, syntheticPath)
	}

	// Pass 2: SAME body (same sha → metadata-only path) but the entry now ships
	// the faithful source_path. The metadata-only path must converge it.
	withFaithful := legacy
	withFaithful.SourcePath = faithful
	entries2 := []catalogEntry{pluginEntry("cospowers-requirements"), withFaithful}
	dir2 := writeMultiEntryBundle(t, entries2, bodies)
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("pass2 ingest: %v", err)
	}
	// The skill must have routed through metadata-only (content unchanged), not
	// the content-changed update path.
	if res.MetadataUpdated < 1 {
		t.Fatalf("expected the unchanged skill to route through metadata-only (metadataUpdated>=1), got %+v", res)
	}
	if res.Updated != 0 {
		t.Fatalf("content was unchanged; expected updated=0, got %d", res.Updated)
	}

	var after models.CapabilityItem
	if err := db.Where("id = ?", before.ID).First(&after).Error; err != nil {
		t.Fatalf("reload skill after pass2: %v", err)
	}
	if after.SourcePath != faithful {
		t.Errorf("metadata-only path did not converge source_path: got %q, want faithful %q", after.SourcePath, faithful)
	}
	// catalog_entry_dir stays the synthetic match key (decoupled from source_path).
	if after.CatalogEntryDir != "skills/cospowers-requirements-aireq-evaluator" {
		t.Errorf("catalog_entry_dir = %q, want synthetic skills/cospowers-requirements-aireq-evaluator", after.CatalogEntryDir)
	}
	// Content untouched (metadata-only path must not rewrite content).
	if after.Content != before.Content {
		t.Errorf("metadata-only path must not change content")
	}

	// Pass 3: idempotency — re-ingest the faithful bundle; nothing should change
	// now (source_path already faithful, sha unchanged → skipped).
	res3, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("pass3 ingest: %v", err)
	}
	if res3.MetadataUpdated != 0 {
		t.Errorf("re-ingest of already-faithful bundle should not metadata-update the skill, got metadataUpdated=%d", res3.MetadataUpdated)
	}
	var final models.CapabilityItem
	db.Where("id = ?", before.ID).First(&final)
	if final.SourcePath != faithful {
		t.Errorf("idempotency: source_path drifted to %q", final.SourcePath)
	}
}

// TestIngest_FaithfulSourcePath_MCPStaysSynthetic verifies MCP children ignore
// any upstream source_path and keep the synthetic "mcp/<id>/.mcp.json" form
// (their identity is "<path>#<key>", never a real file path).
func TestIngest_FaithfulSourcePath_MCPStaysSynthetic(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	mcp := bundledMCPEntry("mcp-faithful-demo", "mcp-faithful")
	mcp.SourcePath = ".mcp.json" // even if upstream sets one, MCP must ignore it
	entries := []catalogEntry{
		pluginEntry("mcp-faithful"),
		mcp,
	}
	bodies := map[string]string{
		"mcp-faithful":      pluginBodyFor("MCP Faithful"),
		"mcp-faithful-demo": mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var m models.CapabilityItem
	if err := db.Where("item_type = ? AND catalog_entry_dir = ?", "mcp", "mcp/mcp-faithful-demo").First(&m).Error; err != nil {
		t.Fatalf("load MCP child: %v", err)
	}
	if m.SourcePath != "mcp/mcp-faithful-demo/.mcp.json" {
		t.Errorf("MCP child source_path = %q, want synthetic mcp/mcp-faithful-demo/.mcp.json", m.SourcePath)
	}
}

// TestIngest_BundledChildTypes_Idempotent ensures re-ingesting the same bundle
// neither duplicates new-type children nor disturbs their parent link.
func TestIngest_BundledChildTypes_Idempotent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("idem-cospower"),
		bundledChildEntry("idem-cospower-rule", "rule", "idem-cospower"),
		bundledChildEntry("idem-cospower-template", "template", "idem-cospower"),
	}
	bodies := map[string]string{
		"idem-cospower":          pluginBodyFor("Idem Cospower"),
		"idem-cospower-rule":     mdBodyFor("Idem Rule"),
		"idem-cospower-template": mdBodyFor("Idem Template"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	for i := 0; i < 2; i++ {
		if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
			t.Fatalf("ingest pass %d: %v", i, err)
		}
	}

	for _, suffix := range []string{"rule", "template"} {
		var count int64
		db.Model(&models.CapabilityItem{}).
			Where("source_path LIKE ?", suffix+"s/idem-cospower-"+suffix+"/%").Count(&count)
		if count != 1 {
			t.Fatalf("re-ingest duplicated %s child; count=%d", suffix, count)
		}
	}
}

// TestTypeDirAndFile_CrossRepoContract pins the (type-dir, primary-file) pairs to
// the EXACT layout the upstream catalog pipeline writes into the bundle. The
// upstream is the single source of truth for where a file physically lands at
// catalog-download/<type-dir>/<id>/<file>; if Go disagrees, readBundleFile
// 404s and the child silently fails to ingest. This is a cross-repo contract
// that no fixture-driven ingest test can catch (the test fixtures lay files out
// with THIS same function, so a mismatch stays self-consistent and green).
//
// Source of truth (must match byte-for-byte):
//   - costrict-skills-repo/scripts/build_catalog_bundle.py  TYPE_DIR_AND_FILE
//   - costrict-skills-repo/scripts/download_catalog.py      _PRIMARY_FILE_BY_TYPE
func TestTypeDirAndFile_CrossRepoContract(t *testing.T) {
	want := map[string][2]string{
		"mcp":      {"mcp", ".mcp.json"},
		"skill":    {"skills", "SKILL.md"},
		"plugin":   {"plugins", ".plugin.json"},
		"prompt":   {"prompts", "PROMPT.md"},
		"rule":     {"rules", "RULE.md"},
		"command":  {"commands", "COMMAND.md"},
		"subagent": {"subagents", "AGENT.md"}, // upstream uses AGENT.md, NOT SUBAGENT.md
		"template": {"templates", "TEMPLATE.md"},
	}
	for itemType, exp := range want {
		dir, file, ok := typeDirAndFile(itemType)
		if !ok {
			t.Errorf("typeDirAndFile(%q) not ok; upstream emits this type", itemType)
			continue
		}
		if dir != exp[0] || file != exp[1] {
			t.Errorf("typeDirAndFile(%q) = (%q, %q), want (%q, %q) per upstream bundle layout",
				itemType, dir, file, exp[0], exp[1])
		}
	}
}
