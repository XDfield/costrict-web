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
// (the default dispatch for RULE.md / TEMPLATE.md / COMMAND.md / AGENT.md)
// succeeds. For an independent entry the item type then comes from
// InferItemType's <type-dir>/ heuristic on the bundle path.
func mdBodyFor(name string) string {
	return "---\nname: " + name + "\ndescription: a " + name + "\n---\n# " + name + "\nbody\n"
}

// allBundledChildTypes is the fixture-side mirror of pluginBundledChildTypes:
// entry type, its bundle type-dir, and the primary file upstream writes there.
// Keeping the whole set in one table is what makes the AC-FP4 assertion below
// cover every type the flat model must ignore, not just the two that used to
// have tests.
var allBundledChildTypes = []struct {
	entryType string
	typeDir   string
	primary   string
}{
	{"skill", "skills", "SKILL.md"},
	{"mcp", "mcp", ".mcp.json"},
	{"command", "commands", "COMMAND.md"},
	{"subagent", "subagents", "AGENT.md"},
	{"rule", "rules", "RULE.md"},
	{"template", "templates", "TEMPLATE.md"},
}

func TestAllBundledChildTypes_CoversPluginBundledChildTypes(t *testing.T) {
	if len(allBundledChildTypes) != len(pluginBundledChildTypes) {
		t.Fatalf("fixture table has %d types, pluginBundledChildTypes has %d — a new bundled child type is untested",
			len(allBundledChildTypes), len(pluginBundledChildTypes))
	}
	for _, tc := range allBundledChildTypes {
		if !pluginBundledChildTypes[tc.entryType] {
			t.Errorf("fixture type %q is not in pluginBundledChildTypes", tc.entryType)
		}
	}
}

// bundleWithEveryBundledChildType returns a bundle carrying one plugin plus one
// bundled entry of EVERY type in pluginBundledChildTypes, all declaring
// bundled_in against that plugin.
func bundleWithEveryBundledChildType(t *testing.T, pluginID string) string {
	t.Helper()
	entries := []catalogEntry{pluginEntry(pluginID)}
	bodies := map[string]string{pluginID: pluginBodyFor(pluginID)}
	for _, tc := range allBundledChildTypes {
		id := pluginID + "-" + tc.entryType
		entries = append(entries, bundledChildEntry(id, tc.entryType, pluginID))
		if tc.entryType == "mcp" {
			bodies[id] = mcpBodyFor("node")
		} else {
			bodies[id] = mdBodyFor(id)
		}
	}
	return writeMultiEntryBundle(t, entries, bodies)
}

// TestIngest_BundledChildTypes_AllTypesIgnored is AC-FP4 / PAC-12 across the
// full type set: skill, mcp, command, subagent, rule, template. A bundle with a
// plugin plus one bundled entry per type produces exactly ONE capability_items
// row (the plugin) and zero parent links.
func TestIngest_BundledChildTypes_AllTypesIgnored(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	dir := bundleWithEveryBundledChildType(t, "cospower-plugin")
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 || res.Incomplete != 0 {
		t.Fatalf("ingest failed=%d incomplete=%d errs=%v %v", res.Failed, res.Incomplete, res.Errors, res.IncompleteErrors)
	}

	if got := countItems(t, db); got != 1 {
		var rows []models.CapabilityItem
		db.Select("id", "item_type", "slug", "source_path").Find(&rows)
		t.Fatalf("capability_items = %d, want 1 (only the plugin); rows=%+v", got, rows)
	}
	if got := countParentLinkedItems(t, db); got != 0 {
		t.Fatalf("rows with parent_plugin_id = %d, want 0", got)
	}

	var plugin models.CapabilityItem
	if err := db.First(&plugin).Error; err != nil {
		t.Fatalf("load the surviving row: %v", err)
	}
	if plugin.ItemType != "plugin" {
		t.Fatalf("the surviving row is %q, want the plugin", plugin.ItemType)
	}

	wantIgnored := len(allBundledChildTypes)
	if res.BundledChildrenIgnored != wantIgnored {
		t.Errorf("bundledChildrenIgnored = %d, want %d (one per bundled child type)", res.BundledChildrenIgnored, wantIgnored)
	}
	if res.Skipped != wantIgnored {
		t.Errorf("skipped = %d, want %d (ignored children are also counted as skipped)", res.Skipped, wantIgnored)
	}
	if res.Added != 1 {
		t.Errorf("added = %d, want 1", res.Added)
	}
}

// TestIngest_BundledChildTypes_ExistingRowsNotSwept: for EVERY bundled child
// type, a row a previous model already created stays active while its entry is
// still in the bundle. This is the per-type proof that the gate seeds
// seenSourcePaths before dropping the entry — the seeding is what stops one
// ingest from archiving every derived row in prod under a false reason.
func TestIngest_BundledChildTypes_ExistingRowsNotSwept(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	const pluginID = "legacy-cospower"
	ids := make([]string, 0, len(allBundledChildTypes))
	for _, tc := range allBundledChildTypes {
		entryID := pluginID + "-" + tc.entryType
		rowID := "legacy-row-" + tc.entryType
		ids = append(ids, rowID)
		seedCatalogRow(t, db, models.CapabilityItem{
			ID: rowID, Slug: entryID, ItemType: tc.entryType, Name: entryID,
			SourcePath:      tc.typeDir + "/" + entryID + "/" + tc.primary,
			CatalogEntryDir: tc.typeDir + "/" + entryID,
			SourceSHA:       "old-sha",
		})
	}

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: bundleWithEveryBundledChildType(t, pluginID)}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d, want 0: ingest archived rows whose entries are still upstream", res.Deleted)
	}

	for _, id := range ids {
		row := loadItemByID(t, db, id)
		if row.Status != "active" {
			t.Errorf("row %s (%s) was archived; a bundled entry must be inert, not a delete signal", id, row.ItemType)
		}
		if row.SourceSHA != "old-sha" {
			t.Errorf("row %s: source_sha = %q, want old-sha (an inert entry must not rewrite the row)", id, row.SourceSHA)
		}
	}
	if got := countItems(t, db); got != int64(len(ids)+1) {
		t.Fatalf("capability_items = %d, want %d (plugin + the untouched legacy rows)", got, len(ids)+1)
	}
}

// TestIngest_BundledChildTypes_Idempotent ensures repeated rounds of a legacy
// bundle never start creating child rows and keep reporting the ignored volume.
func TestIngest_BundledChildTypes_Idempotent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	dir := bundleWithEveryBundledChildType(t, "idem-cospower")
	for i := 0; i < 3; i++ {
		res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
		if err != nil {
			t.Fatalf("ingest pass %d: %v", i, err)
		}
		if res.BundledChildrenIgnored != len(allBundledChildTypes) {
			t.Fatalf("pass %d: bundledChildrenIgnored = %d, want %d", i, res.BundledChildrenIgnored, len(allBundledChildTypes))
		}
		if res.Deleted != 0 {
			t.Fatalf("pass %d: deleted = %d, want 0", i, res.Deleted)
		}
		if got := countItems(t, db); got != 1 {
			t.Fatalf("pass %d: capability_items = %d, want 1", i, got)
		}
	}
}

// TestIngest_FaithfulSourcePath_AllTypes verifies that when an INDEPENDENT
// upstream entry carries a repo-relative source_path, the ingested row stores
// that exact path on source_path while file content is still read from the
// synthetic bundle layout and the match key (catalog_entry_dir) stays
// synthetic. Covered for every non-plugin type the bundle can carry.
func TestIngest_FaithfulSourcePath_AllTypes(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// (entry id, entry type, faithful repo path, expected synthetic entry dir).
	cases := []struct {
		entryID      string
		entryType    string
		faithful     string
		wantEntryDir string
	}{
		{"faithful-skill", "skill", "skills/requirement-analysis/SKILL.md", "skills/faithful-skill"},
		{"faithful-rule", "rule", "rules/dfx/安全.md", "rules/faithful-rule"},
		{"faithful-template", "template", "templates/system-design.md", "templates/faithful-template"},
		{"faithful-command", "command", "commands/run-tests.md", "commands/faithful-command"},
		{"faithful-agent", "subagent", "agents/reviewer.md", "subagents/faithful-agent"},
	}

	entries := make([]catalogEntry, 0, len(cases))
	bodies := map[string]string{}
	for _, tc := range cases {
		e := bundledChildEntry(tc.entryID, tc.entryType, "") // no bundled_in → independent
		e.SourcePath = tc.faithful
		entries = append(entries, e)
		bodies[tc.entryID] = mdBodyFor(tc.entryID)
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 || res.Incomplete != 0 {
		t.Fatalf("ingest failed=%d incomplete=%d errs=%v %v", res.Failed, res.Incomplete, res.Errors, res.IncompleteErrors)
	}
	if res.BundledChildrenIgnored != 0 {
		t.Fatalf("bundledChildrenIgnored = %d, want 0: none of these entries declares bundled_in", res.BundledChildrenIgnored)
	}

	for _, tc := range cases {
		// Locate the row by the synthetic match key (catalog_entry_dir), NOT by
		// source_path — source_path is now the faithful path.
		var row models.CapabilityItem
		if err := db.Where("catalog_entry_dir = ?", tc.wantEntryDir).First(&row).Error; err != nil {
			t.Fatalf("load %s by entry-dir %q: %v", tc.entryID, tc.wantEntryDir, err)
		}
		if row.SourcePath != tc.faithful {
			t.Errorf("%s source_path = %q, want faithful %q", tc.entryID, row.SourcePath, tc.faithful)
		}
		if row.ItemType != tc.entryType {
			t.Errorf("%s item_type = %q, want %q", tc.entryID, row.ItemType, tc.entryType)
		}
	}
}

// TestIngest_FaithfulSourcePath_Idempotent is the key regression guard for the
// decoupling: with a faithful source_path, re-ingest must still match the
// existing row via catalog_entry_dir (NOT via source_path) and not duplicate.
func TestIngest_FaithfulSourcePath_Idempotent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	rule := bundledChildEntry("idem-faithful-rule", "rule", "")
	rule.SourcePath = "rules/dfx/安全.md"
	dir := writeMultiEntryBundle(t,
		[]catalogEntry{rule},
		map[string]string{"idem-faithful-rule": mdBodyFor("Idem Rule")})

	for i := 0; i < 2; i++ {
		if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
			t.Fatalf("ingest pass %d: %v", i, err)
		}
	}

	var count int64
	db.Model(&models.CapabilityItem{}).Where("catalog_entry_dir = ?", "rules/idem-faithful-rule").Count(&count)
	if count != 1 {
		t.Fatalf("re-ingest with faithful source_path duplicated the rule; count=%d", count)
	}
	var rl models.CapabilityItem
	db.Where("catalog_entry_dir = ?", "rules/idem-faithful-rule").First(&rl)
	if rl.SourcePath != "rules/dfx/安全.md" {
		t.Fatalf("rule source_path = %q, want faithful rules/dfx/安全.md", rl.SourcePath)
	}
}

// TestIngest_FaithfulSourcePath_MetadataOnlyConverges is the regression guard
// for the P3-rollout bug: an EXISTING row whose content sha is unchanged (so it
// routes through the metadata-only path) but whose upstream entry now carries a
// faithful source_path must have its DB source_path converged — not left at the
// stale synthetic value.
func TestIngest_FaithfulSourcePath_MetadataOnlyConverges(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	const skillBody = "---\nname: Aireq Evaluator\ndescription: an evaluator\n---\n# Aireq\nbody\n"
	// The entry id and the faithful path must differ, otherwise the synthetic
	// path and the faithful one collide and there is nothing to converge.
	const entryID = "cospowers-requirements-aireq-evaluator"
	faithful := "skills/aireq-evaluator/SKILL.md"

	// Pass 1: no faithful source_path on the entry → row gets the synthetic
	// "skills/<id>/SKILL.md" path + synthetic catalog_entry_dir (simulates a
	// pre-P3 row already in prod).
	legacy := subSkillEntry(entryID, "")
	bodies := map[string]string{entryID: skillBody}
	dir1 := writeMultiEntryBundle(t, []catalogEntry{legacy}, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir1}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("pass1 ingest: %v", err)
	}

	syntheticPath := "skills/" + entryID + "/SKILL.md"
	var before models.CapabilityItem
	if err := db.Where("item_type = ? AND catalog_entry_dir = ?", "skill", "skills/"+entryID).First(&before).Error; err != nil {
		t.Fatalf("load skill after pass1: %v", err)
	}
	if before.SourcePath != syntheticPath {
		t.Fatalf("pass1 source_path = %q, want synthetic %q", before.SourcePath, syntheticPath)
	}

	// Pass 2: SAME body (same sha → metadata-only path) but the entry now ships
	// the faithful source_path. The metadata-only path must converge it.
	withFaithful := legacy
	withFaithful.SourcePath = faithful
	dir2 := writeMultiEntryBundle(t, []catalogEntry{withFaithful}, bodies)
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("pass2 ingest: %v", err)
	}
	if res.MetadataUpdated != 1 {
		t.Fatalf("expected the unchanged skill to route through metadata-only (metadataUpdated=1), got %+v", res)
	}
	if res.Updated != 0 {
		t.Fatalf("content was unchanged; expected updated=0, got %d", res.Updated)
	}

	after := loadItemByID(t, db, before.ID)
	if after.SourcePath != faithful {
		t.Errorf("metadata-only path did not converge source_path: got %q, want faithful %q", after.SourcePath, faithful)
	}
	// catalog_entry_dir stays the synthetic match key (decoupled from source_path).
	if after.CatalogEntryDir != "skills/"+entryID {
		t.Errorf("catalog_entry_dir = %q, want synthetic skills/%s", after.CatalogEntryDir, entryID)
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
		t.Errorf("re-ingest of an already-faithful bundle should not metadata-update, got metadataUpdated=%d", res3.MetadataUpdated)
	}
	if final := loadItemByID(t, db, before.ID); final.SourcePath != faithful {
		t.Errorf("idempotency: source_path drifted to %q", final.SourcePath)
	}
}

// TestIngest_FaithfulSourcePath_MCPStaysSynthetic verifies MCP rows ignore any
// upstream source_path and keep the synthetic "mcp/<id>/.mcp.json" form (their
// identity is "<path>#<key>", never a real file path).
func TestIngest_FaithfulSourcePath_MCPStaysSynthetic(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	mcp := bundledMCPEntry("mcp-faithful-demo", "")
	mcp.SourcePath = ".mcp.json" // even if upstream sets one, MCP must ignore it
	dir := writeMultiEntryBundle(t,
		[]catalogEntry{mcp},
		map[string]string{"mcp-faithful-demo": mcpBodyFor("node")})

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var m models.CapabilityItem
	if err := db.Where("item_type = ? AND catalog_entry_dir = ?", "mcp", "mcp/mcp-faithful-demo").First(&m).Error; err != nil {
		t.Fatalf("load MCP row: %v", err)
	}
	if m.SourcePath != "mcp/mcp-faithful-demo/.mcp.json" {
		t.Errorf("MCP source_path = %q, want synthetic mcp/mcp-faithful-demo/.mcp.json", m.SourcePath)
	}
}

// TestTypeDirAndFile_CrossRepoContract pins the (type-dir, primary-file) pairs to
// the EXACT layout the upstream catalog pipeline writes into the bundle. The
// upstream is the single source of truth for where a file physically lands at
// catalog-download/<type-dir>/<id>/<file>; if Go disagrees, readBundleFile
// 404s and the entry silently fails to ingest. This is a cross-repo contract
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

	// The fixture table above must also agree with typeDirAndFile, or the
	// bundled-child tests would lay files out somewhere ingest never looks.
	for _, tc := range allBundledChildTypes {
		dir, file, ok := typeDirAndFile(tc.entryType)
		if !ok || dir != tc.typeDir || file != tc.primary {
			t.Errorf("fixture table for %q = (%q, %q); typeDirAndFile says (%q, %q, ok=%v)",
				tc.entryType, tc.typeDir, tc.primary, dir, file, ok)
		}
	}
}
