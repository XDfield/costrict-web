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

// This file pins the FLAT capability model: an upstream catalog entry that
// declares `bundled_in` is a file living inside another entry's plugin package.
// The plugin runtime loads it after the plugin is installed; Cloud does not
// index it as a capability of its own.
//
// Concretely, catalog ingest must treat such an entry as INERT — not inserted,
// not updated, not revived from archived, not asset-synced, and (critically)
// not archived either, because retiring the rows a previous model created is
// the flatten migration's job, where it is row-level auditable and reversible.
// Ingest no longer writes capability_items.parent_plugin_id in either
// direction; that column's only remaining writer is the migration.

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

// countItems is the exact-count assertion AC-FP4 needs: "only the plugin row
// exists" is only provable by counting the whole table, not by looking up the
// rows we expected to be absent.
func countItems(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.CapabilityItem{}).Count(&n).Error; err != nil {
		t.Fatalf("count capability_items: %v", err)
	}
	return n
}

// countParentLinkedItems counts rows carrying a parent_plugin_id. Ingest must
// never produce one; a non-zero result means the retired second pass (or an
// equivalent) is back.
func countParentLinkedItems(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.CapabilityItem{}).
		Where("parent_plugin_id IS NOT NULL AND parent_plugin_id <> ''").Count(&n).Error; err != nil {
		t.Fatalf("count parent-linked capability_items: %v", err)
	}
	return n
}

// seedCatalogRow inserts a db-backed public-registry row shaped the way a
// PRE-flatten catalog ingest wrote it: catalog-shaped source_path, synthetic
// catalog_entry_dir match key, source_type='direct', content_backend='db'.
// content_backend is set explicitly because it is load-bearing — Ingest only
// pre-loads db-backed public rows, so a row missing it would silently drop out
// of the soft-archive scope and make an "is it swept?" assertion vacuous.
func seedCatalogRow(t *testing.T, db *gorm.DB, item models.CapabilityItem) models.CapabilityItem {
	t.Helper()
	item.RegistryID = PublicRegistryID
	item.RepoID = PublicRepoID
	item.ContentBackend = models.ContentBackendDB
	if item.SourceType == "" {
		item.SourceType = "direct"
	}
	if item.Status == "" {
		item.Status = "active"
	}
	if item.CreatedBy == "" {
		item.CreatedBy = "system"
	}
	if item.UpdatedBy == "" {
		item.UpdatedBy = "system"
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("seed catalog row %s: %v", item.ID, err)
	}
	return loadItemByID(t, db, item.ID)
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

// TestIngest_BundledChildren_OnlyPluginRowCreated is AC-FP4 / PAC-12 for the
// skill + mcp shapes: a bundle carrying a plugin plus several entries that
// declare bundled_in against it produces exactly ONE row — the plugin. The
// assertion is a whole-table count, not a per-row lookup, so an extra row
// under any slug/path shape still fails it.
func TestIngest_BundledChildren_OnlyPluginRowCreated(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("acme-plugin"),
		subSkillEntry("acme-plugin-alpha", "acme-plugin"),
		subSkillEntry("acme-plugin-beta", "acme-plugin"),
		bundledMCPEntry("acme-plugin-demo", "acme-plugin"),
	}
	bodies := map[string]string{
		"acme-plugin":       pluginBodyFor("Acme Plugin"),
		"acme-plugin-alpha": skillBodyFor("Alpha Skill"),
		"acme-plugin-beta":  skillBodyFor("Beta Skill"),
		"acme-plugin-demo":  mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 || res.Incomplete != 0 {
		t.Fatalf("ingest failed=%d incomplete=%d errs=%v %v", res.Failed, res.Incomplete, res.Errors, res.IncompleteErrors)
	}

	if got := countItems(t, db); got != 1 {
		var rows []models.CapabilityItem
		db.Find(&rows)
		t.Fatalf("bundled children must not be indexed: capability_items=%d, want 1 (the plugin); rows=%+v", got, rows)
	}
	if got := countParentLinkedItems(t, db); got != 0 {
		t.Fatalf("ingest must never write parent_plugin_id; rows with a parent link = %d, want 0", got)
	}

	var plugin models.CapabilityItem
	if err := db.Where("item_type = ?", "plugin").First(&plugin).Error; err != nil {
		t.Fatalf("plugin row missing: %v", err)
	}
	if plugin.CatalogEntryDir != "plugins/acme-plugin" {
		t.Fatalf("plugin catalog_entry_dir = %q, want plugins/acme-plugin", plugin.CatalogEntryDir)
	}

	// Counters: every bundled_in entry is reported separately AND folded into
	// Skipped. On a first pass the plugin is Added, so Skipped is exactly the
	// ignored count.
	if res.Added != 1 {
		t.Errorf("added = %d, want 1 (the plugin)", res.Added)
	}
	if res.BundledChildrenIgnored != 3 {
		t.Errorf("bundledChildrenIgnored = %d, want 3", res.BundledChildrenIgnored)
	}
	if res.Skipped != 3 {
		t.Errorf("skipped = %d, want 3 (ignored children are also counted as skipped)", res.Skipped)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d, want 0", res.Deleted)
	}
}

// TestIngest_BundledChildren_OrderIndependent: the gate is per-entry, so the
// outcome must not depend on the children appearing before their plugin in
// index.json (the retired second pass was the only order-sensitive part).
func TestIngest_BundledChildren_OrderIndependent(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		subSkillEntry("ord-plugin-alpha", "ord-plugin"),
		bundledMCPEntry("ord-plugin-demo", "ord-plugin"),
		pluginEntry("ord-plugin"), // parent last on purpose
	}
	bodies := map[string]string{
		"ord-plugin":       pluginBodyFor("Ord Plugin"),
		"ord-plugin-alpha": skillBodyFor("Ord Alpha"),
		"ord-plugin-demo":  mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := countItems(t, db); got != 1 {
		t.Fatalf("children-before-parent ordering changed the outcome: capability_items=%d, want 1", got)
	}
	if res.BundledChildrenIgnored != 2 {
		t.Errorf("bundledChildrenIgnored = %d, want 2", res.BundledChildrenIgnored)
	}
}

// TestIngest_BundledChildren_Idempotent: re-ingesting the same bundle keeps the
// single plugin row and keeps reporting the same ignored volume (the counter
// must not decay into the generic no-change bucket on later rounds).
func TestIngest_BundledChildren_Idempotent(t *testing.T) {
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
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if got := countItems(t, db); got != 1 {
		t.Fatalf("re-ingest changed the row set: capability_items=%d, want 1", got)
	}
	if res.Added != 0 {
		t.Errorf("re-ingest added = %d, want 0", res.Added)
	}
	if res.BundledChildrenIgnored != 1 {
		t.Errorf("re-ingest bundledChildrenIgnored = %d, want 1", res.BundledChildrenIgnored)
	}
	if res.Deleted != 0 {
		t.Errorf("re-ingest deleted = %d, want 0", res.Deleted)
	}

	plugin := loadItemBySourcePath(t, db, "plugins/idem-plugin/.plugin.json")
	if plugin.Status != "active" {
		t.Errorf("plugin status = %q, want active", plugin.Status)
	}
}

// TestIngest_BundledChild_MigrationArchivedRowNotResurrected is AC-FP15, the
// regression that proves the cleanup STICKS. The flatten migration archives the
// derived child rows and owns parent_plugin_id. Upstream keeps shipping bundles
// that still carry those `bundled_in` entries, so if ingest reached the
// archived → active revival in applyMetadataDelta (or cleared the column), the
// very next scheduled run would undo the migration.
func TestIngest_BundledChild_MigrationArchivedRowNotResurrected(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("legacy-plugin"),
		subSkillEntry("legacy-plugin-alpha", "legacy-plugin"),
		bundledMCPEntry("legacy-plugin-demo", "legacy-plugin"),
	}
	bodies := map[string]string{
		"legacy-plugin":       pluginBodyFor("Legacy Plugin"),
		"legacy-plugin-alpha": skillBodyFor("Legacy Alpha"),
		"legacy-plugin-demo":  mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	// Round 1 creates the plugin row only.
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	var plugin models.CapabilityItem
	if err := db.Where("item_type = ?", "plugin").First(&plugin).Error; err != nil {
		t.Fatalf("load plugin: %v", err)
	}
	parentID := plugin.ID

	// Post-migration state: the derived children exist, archived, still carrying
	// the parent link the migration recorded, still matching bundle entries.
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "migrated-skill", Slug: "legacy-plugin-alpha", ItemType: "skill", Name: "Legacy Alpha",
		SourcePath: "skills/legacy-plugin-alpha/SKILL.md", CatalogEntryDir: "skills/legacy-plugin-alpha",
		SourceSHA: "migration-sha", Content: "# from the old model\n",
		ParentPluginID: &parentID, Status: "archived",
	})
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "migrated-mcp", Slug: "legacy-plugin-demo", ItemType: "mcp", Name: "Legacy Demo",
		SourcePath: "mcp/legacy-plugin-demo/.mcp.json", CatalogEntryDir: "mcp/legacy-plugin-demo",
		SourceSHA: "migration-sha", Content: "{}",
		ParentPluginID: &parentID, Status: "archived",
	})

	// Round 2: a full ingest of a bundle that STILL carries those entries.
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if res.Added != 0 {
		t.Errorf("legacy bundle added %d rows; archived children must not be re-created", res.Added)
	}

	for _, id := range []string{"migrated-skill", "migrated-mcp"} {
		after := loadItemByID(t, db, id)
		if after.Status != "archived" {
			t.Errorf("row %s: legacy bundle resurrected a migration-archived child (status=%q)", id, after.Status)
		}
		if after.ParentPluginID == nil || *after.ParentPluginID != parentID {
			t.Errorf("row %s: parent_plugin_id = %v, want %q unchanged (the migration owns that column)", id, after.ParentPluginID, parentID)
		}
		if after.SourceSHA != "migration-sha" {
			t.Errorf("row %s: source_sha rewritten to %q; the row must be untouched", id, after.SourceSHA)
		}
	}

	// And no duplicate was inserted alongside the archived rows.
	if got := countItems(t, db); got != 3 {
		t.Fatalf("capability_items = %d, want 3 (plugin + the 2 untouched archived rows)", got)
	}
}

// TestIngest_BundledChild_ExistingActiveRowNotSwept guards the other half of
// "inert": the soft-archive sweep must leave an existing derived child alone
// while its entry is still in the bundle. The mechanism is the seenSourcePaths
// seeding the gate does before it drops the entry; without it a single ingest
// would archive every derived row in prod (~6.7k) under the reason "vanished
// upstream", which is both false and unattributable.
//
// Both stored path forms are covered: the synthetic "<type-dir>/<id>/<file>"
// (what legacy rows and all MCP rows hold) and the faithful repo-relative path
// (what rows written after the path-faithful change hold).
func TestIngest_BundledChild_ExistingActiveRowNotSwept(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	faithful := subSkillEntry("sweep-plugin-faithful", "sweep-plugin")
	faithful.SourcePath = "skills/requirement-analysis/SKILL.md"
	entries := []catalogEntry{
		pluginEntry("sweep-plugin"),
		subSkillEntry("sweep-plugin-alpha", "sweep-plugin"),
		faithful,
		bundledMCPEntry("sweep-plugin-demo", "sweep-plugin"),
	}
	bodies := map[string]string{
		"sweep-plugin":          pluginBodyFor("Sweep Plugin"),
		"sweep-plugin-alpha":    skillBodyFor("Sweep Alpha"),
		"sweep-plugin-faithful": skillBodyFor("Sweep Faithful"),
		"sweep-plugin-demo":     mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	// Rows as a pre-flatten ingest left them, all still active.
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "live-synthetic", Slug: "sweep-plugin-alpha", ItemType: "skill", Name: "Sweep Alpha",
		SourcePath: "skills/sweep-plugin-alpha/SKILL.md", CatalogEntryDir: "skills/sweep-plugin-alpha",
		SourceSHA: "old-sha",
	})
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "live-faithful", Slug: "sweep-plugin-faithful", ItemType: "skill", Name: "Sweep Faithful",
		// source_path is the faithful repo path; the match key stays synthetic.
		SourcePath: "skills/requirement-analysis/SKILL.md", CatalogEntryDir: "skills/sweep-plugin-faithful",
		SourceSHA: "old-sha",
	})
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "live-mcp", Slug: "sweep-plugin-demo", ItemType: "mcp", Name: "Sweep Demo",
		SourcePath: "mcp/sweep-plugin-demo/.mcp.json", CatalogEntryDir: "mcp/sweep-plugin-demo",
		SourceSHA: "old-sha",
	})

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d, want 0: ingest archived rows the flatten migration owns", res.Deleted)
	}

	for _, id := range []string{"live-synthetic", "live-faithful", "live-mcp"} {
		after := loadItemByID(t, db, id)
		if after.Status != "active" {
			t.Errorf("row %s: status = %q, want active (its entry is still in the bundle)", id, after.Status)
		}
		if after.SourceSHA != "old-sha" {
			t.Errorf("row %s: source_sha = %q, want old-sha (an inert entry must not update the row)", id, after.SourceSHA)
		}
	}
	// plugin + the 3 pre-existing rows, nothing added.
	if got := countItems(t, db); got != 4 {
		t.Fatalf("capability_items = %d, want 4 (plugin + 3 untouched rows)", got)
	}
}

// TestIngest_UnbundledEntries_IngestNormally is the no-regression control: an
// entry of a bundled-child TYPE that carries no bundled_in is an ordinary
// independent capability and must be created exactly as before, on both the
// insert and the metadata-only round.
func TestIngest_UnbundledEntries_IngestNormally(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		subSkillEntry("solo-skill", ""),
		bundledMCPEntry("solo-mcp", ""),
	}
	bodies := map[string]string{
		"solo-skill": skillBodyFor("Solo Skill"),
		"solo-mcp":   mcpBodyFor("node"),
	}
	dir := writeMultiEntryBundle(t, entries, bodies)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 || res.Incomplete != 0 {
		t.Fatalf("ingest failed=%d incomplete=%d errs=%v %v", res.Failed, res.Incomplete, res.Errors, res.IncompleteErrors)
	}
	if res.Added != 2 {
		t.Fatalf("added = %d, want 2 (independent entries still ingest)", res.Added)
	}
	if res.BundledChildrenIgnored != 0 {
		t.Fatalf("bundledChildrenIgnored = %d, want 0: no entry declared bundled_in", res.BundledChildrenIgnored)
	}

	skill := loadItemBySourcePath(t, db, "skills/solo-skill/SKILL.md")
	if skill.ItemType != "skill" || skill.Status != "active" {
		t.Errorf("independent skill = (%q, %q), want (skill, active)", skill.ItemType, skill.Status)
	}
	if skill.CatalogEntryDir != "skills/solo-skill" {
		t.Errorf("independent skill catalog_entry_dir = %q, want skills/solo-skill", skill.CatalogEntryDir)
	}
	mcp := loadItemBySourcePath(t, db, "mcp/solo-mcp/.mcp.json")
	if mcp.ItemType != "mcp" || mcp.Status != "active" {
		t.Errorf("independent mcp = (%q, %q), want (mcp, active)", mcp.ItemType, mcp.Status)
	}

	// Second round with an upstream metadata change routes through the
	// metadata-only path and still converges — the flatten must not have made
	// independent entries inert by accident.
	entries[0].Description = "a re-described independent skill"
	res2, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if res2.MetadataUpdated != 1 {
		t.Fatalf("metadataUpdated = %d, want 1", res2.MetadataUpdated)
	}
	if got := loadItemByID(t, db, skill.ID); got.Description != "a re-described independent skill" {
		t.Errorf("description = %q, want the upstream value", got.Description)
	}
	if got := countItems(t, db); got != 2 {
		t.Fatalf("capability_items = %d, want 2", got)
	}
}

// TestIngest_DroppedBundledIn_BecomesIndexableAgain: upstream re-classifying an
// entry as independent (bundled_in removed) makes it an ordinary entry again.
// The pre-existing archived row keeps its stale parent_plugin_id — ingest never
// writes that column, in either direction; clearing it belongs to the migration.
func TestIngest_DroppedBundledIn_BecomesIndexableAgain(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	staleParent := "some-old-plugin-id"
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "flip-row", Slug: "shared-skill", ItemType: "skill", Name: "Shared Skill",
		SourcePath: "skills/shared-skill/SKILL.md", CatalogEntryDir: "skills/shared-skill",
		SourceSHA: "old-sha", Content: "# old\n",
		ParentPluginID: &staleParent, Status: "archived",
	})

	// Round 1: the entry still declares bundled_in → inert, row untouched.
	bundled := []catalogEntry{
		pluginEntry("link-plugin"),
		subSkillEntry("shared-skill", "link-plugin"),
	}
	bodies := map[string]string{
		"link-plugin":  pluginBodyFor("Link Plugin"),
		"shared-skill": skillBodyFor("Shared Skill"),
	}
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, bundled, bodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("bundled ingest: %v", err)
	}
	if mid := loadItemByID(t, db, "flip-row"); mid.Status != "archived" {
		t.Fatalf("precondition: a bundled entry must not revive the row, got status=%q", mid.Status)
	}

	// Round 2: bundled_in dropped → the entry is indexed again, so the existing
	// row is matched by catalog_entry_dir and revived.
	independent := []catalogEntry{
		pluginEntry("link-plugin"),
		subSkillEntry("shared-skill", ""),
	}
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, independent, bodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("independent ingest: %v", err)
	}

	after := loadItemByID(t, db, "flip-row")
	if after.Status != "active" {
		t.Errorf("an entry without bundled_in must ingest normally; status=%q", after.Status)
	}
	if after.ParentPluginID == nil || *after.ParentPluginID != staleParent {
		t.Errorf("parent_plugin_id = %v, want %q: ingest must not write that column in either direction", after.ParentPluginID, staleParent)
	}
	if got := countItems(t, db); got != 2 {
		t.Fatalf("capability_items = %d, want 2 (plugin + the adopted row)", got)
	}
}

// TestIngest_ArchivedItemResurrectsWhenEntryReappears verifies the round-trip
// for INDEXED entries: an item archived because its entry vanished from one
// bundle comes back to active when a later bundle carries the entry again —
// including the content-UNCHANGED case (same SHA), which routes through the
// metadata-only path rather than updateItem.
func TestIngest_ArchivedItemResurrectsWhenEntryReappears(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("revive-plugin"),
		subSkillEntry("revive-skill", ""), // independent: indexed, so archivable
	}
	bodies := map[string]string{
		"revive-plugin": pluginBodyFor("Revive Plugin"),
		"revive-skill":  skillBodyFor("Revive Skill"),
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
	plugin := loadItemBySourcePath(t, db, "plugins/revive-plugin/.plugin.json")
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

	pluginAfter := loadItemBySourcePath(t, db, "plugins/revive-plugin/.plugin.json")
	skillAfter := loadItemBySourcePath(t, db, "skills/revive-skill/SKILL.md")
	if pluginAfter.Status != "active" {
		t.Fatalf("plugin must resurrect to active, got %q", pluginAfter.Status)
	}
	if skillAfter.Status != "active" {
		t.Fatalf("independent skill must resurrect to active, got %q", skillAfter.Status)
	}
}

// TestIngest_UploadedRows_ParentLinkUntouched: a zip-promoted sub-skill row
// (source_type='archive') sharing the exact entryDir shape of a catalog skill
// entry must survive an ingest with its parent_plugin_id intact. The catalog
// used to rewrite that column from bundled_in; now nothing in ingest may write
// it, and the uploaded row is the case where a leak would be user-visible.
func TestIngest_UploadedRows_ParentLinkUntouched(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Seed an uploaded plugin + its promoted child OUTSIDE the catalog flow.
	uploadParent := seedCatalogRow(t, db, models.CapabilityItem{
		ID: "upload-plugin-1", Slug: "my-upload-plugin", ItemType: "plugin", Name: "My Upload Plugin",
		SourcePath: ".plugin.json", SourceType: "archive", CreatedBy: "user-1", UpdatedBy: "user-1",
	})
	parentID := uploadParent.ID
	seedCatalogRow(t, db, models.CapabilityItem{
		ID: "upload-child-1", Slug: "my-upload-plugin-shared-name", ItemType: "skill", Name: "shared-name",
		// Byte-identical entryDir shape to the catalog entry below.
		SourcePath: "skills/shared-name/SKILL.md", SourceType: "archive",
		ParentPluginID: &parentID, CreatedBy: "user-1", UpdatedBy: "user-1",
	})

	// Catalog bundle ships an INDEPENDENT skill with the same entry id.
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
		t.Fatalf("catalog ingest must not touch an uploaded (source_type=archive) child's parent link; got parent=%v", after.ParentPluginID)
	}
	if after.Status != "active" {
		t.Fatalf("uploaded child must not be archived; status=%q", after.Status)
	}
}

// TestIngest_SlugConflict_FallsBackToSuffix keeps the insertItem unique-
// constraint retry covered now that bundled children (its original caller) no
// longer insert. The live case is a row that holds the (repo, type, slug) index
// slot but is invisible to the pre-loaded slug index: a git-backed row is
// filtered out by content_backend, so no adoption fallback can see it and the
// INSERT collides for real.
func TestIngest_SlugConflict_FallsBackToSuffix(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	occupantBefore := seedGitBackedItem(t, db, models.CapabilityItem{
		ID: "occupant-git-1", Slug: "conflict-tool", ItemType: "skill", Name: "repo owned",
		SourcePath: "skill.md", SourceRepoPath: "skill.md", SourceType: "git",
		SourceSHA: "gitsha-do-not-touch",
	})

	entries := []catalogEntry{{
		ID: "conflict-tool", Type: "skill", Source: "catalog/conflict-tool",
		Description: "an independent catalog skill", Category: "tooling",
	}}
	bodies := map[string]string{"conflict-tool": skillBodyFor("Conflict Tool")}
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("slug conflict must not fail the entry: failed=%d errs=%v", res.Failed, res.Errors)
	}

	var inserted models.CapabilityItem
	if err := db.Where("catalog_entry_dir = ?", "skills/conflict-tool").First(&inserted).Error; err != nil {
		t.Fatalf("catalog row not inserted despite conflict: %v", err)
	}
	if inserted.Slug != "conflict-tool-2" {
		t.Fatalf("expected suffixed slug, got %q", inserted.Slug)
	}
	assertItemUnchanged(t, occupantBefore, loadItemByID(t, db, "occupant-git-1"))

	// Idempotency: rerun must match the suffixed row by entryDir, not add a third.
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	var count int64
	db.Model(&models.CapabilityItem{}).Where("catalog_entry_dir = ?", "skills/conflict-tool").Count(&count)
	if count != 1 {
		t.Fatalf("re-ingest duplicated the row: count=%d", count)
	}
}

// TestIngest_MCP_IndependentToBundledFlip_LeavesRowUntouched: when an existing
// independent mcp entry GAINS bundled_in upstream, the entry becomes package
// content. Ingest must neither rewrite the pre-flip row (the old flip-adoption
// path is unreachable now) nor archive it — the seenSourcePaths seeding keeps
// the sweep off it, and retiring it is the migration's decision.
func TestIngest_MCP_IndependentToBundledFlip_LeavesRowUntouched(t *testing.T) {
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
	if err := db.Where("catalog_entry_dir = ?", "mcp/flip-mcp").First(&before).Error; err != nil {
		t.Fatalf("independent row missing: %v", err)
	}

	// Flip: same entry id, now bundled, and with changed content so the old
	// code would definitely have taken the full update path.
	entries := []catalogEntry{
		pluginEntry("flip-plugin"),
		bundledMCPEntry("flip-mcp", "flip-plugin"),
	}
	bodies := map[string]string{
		"flip-plugin": pluginBodyFor("Flip Plugin"),
		"flip-mcp":    mcpBodyFor("demo-cmd-v2"),
	}
	res, err := svc.Ingest(context.Background(), IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("flip ingest: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("flip archived %d rows; a newly-bundled entry must leave its old row alone", res.Deleted)
	}
	if res.BundledChildrenIgnored != 1 {
		t.Errorf("bundledChildrenIgnored = %d, want 1", res.BundledChildrenIgnored)
	}

	var rows []models.CapabilityItem
	db.Where("catalog_entry_dir = ?", "mcp/flip-mcp").Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("flip must not duplicate the row, got %d rows", len(rows))
	}
	assertItemUnchanged(t, before, rows[0])
	if rows[0].Status != "active" {
		t.Errorf("pre-flip row status = %q, want active", rows[0].Status)
	}
}
