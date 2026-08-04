package services

import (
	"context"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// seedGitBackedItem creates a row whose content truth is a git repository, in
// the same public registry / repo_id catalog rows live in. That co-location is
// the whole point: ForkItem places git forks under PublicRegistryID with
// repo_id='public', so nothing about the registry or repo distinguishes them
// from a catalog row — only content_backend does.
func seedGitBackedItem(t *testing.T, db *gorm.DB, item models.CapabilityItem) models.CapabilityItem {
	t.Helper()
	item.RegistryID = PublicRegistryID
	item.RepoID = PublicRepoID
	item.ContentBackend = "git"
	if item.SourceRepoURL == "" {
		item.SourceRepoURL = "https://git.example/u-alice/caps"
	}
	if item.SourceRepoRef == "" {
		item.SourceRepoRef = "main"
	}
	if item.SourceGitServerID == "" {
		item.SourceGitServerID = "gs-1"
	}
	if item.SourceGitRepoID == 0 {
		item.SourceGitRepoID = 42
	}
	if item.GitSyncStatus == "" {
		item.GitSyncStatus = "synced"
	}
	if item.Status == "" {
		item.Status = "active"
	}
	if item.CreatedBy == "" {
		item.CreatedBy = "u-alice"
	}
	if item.UpdatedBy == "" {
		item.UpdatedBy = "git-sync"
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("seed git-backed item %s: %v", item.ID, err)
	}
	return loadItemByID(t, db, item.ID)
}

// assertItemUnchanged compares every column catalog ingest would have written
// on the update path (updateItem's assignment face) plus updated_at, which GORM
// stamps on any Save/Updates — so it catches a write even if every value it
// wrote happened to be identical.
func assertItemUnchanged(t *testing.T, before, after models.CapabilityItem) {
	t.Helper()
	if after.Content != before.Content {
		t.Errorf("content was rewritten: %q -> %q", before.Content, after.Content)
	}
	if after.ItemType != before.ItemType {
		t.Errorf("item_type was rewritten: %q -> %q", before.ItemType, after.ItemType)
	}
	if after.Slug != before.Slug {
		t.Errorf("slug was rewritten: %q -> %q", before.Slug, after.Slug)
	}
	if after.SourcePath != before.SourcePath {
		t.Errorf("source_path was rewritten: %q -> %q", before.SourcePath, after.SourcePath)
	}
	if after.SourceSHA != before.SourceSHA {
		t.Errorf("source_sha was rewritten: %q -> %q", before.SourceSHA, after.SourceSHA)
	}
	if after.CatalogEntryDir != before.CatalogEntryDir {
		t.Errorf("catalog_entry_dir was rewritten: %q -> %q", before.CatalogEntryDir, after.CatalogEntryDir)
	}
	if after.Name != before.Name {
		t.Errorf("name was rewritten: %q -> %q", before.Name, after.Name)
	}
	if after.Status != before.Status {
		t.Errorf("status was rewritten: %q -> %q", before.Status, after.Status)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at moved (%s -> %s): the row was written even if the values look the same",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// TestIngest_GitBackedSlugCollision_RowNotHijacked is the targeted regression for
// the highest-risk path in the writer-isolation work (gate matrix W17-a/b/c).
//
// A git-backed row sits in the public registry under repo_id='public' with a
// slug a catalog entry also derives. It is NOT reachable through the entry's
// entryDir (its source_path is the single-segment repo path a git manifest
// yields), so applyChangedEntry finds nothing in localBySlug and falls back to
// the cross-entry globalBySlug index. Before the fix that fallback classified
// the row as an existing item and updateItem's whole-row s.DB.Save(existing)
// overwrote content / item_type / slug / source_path on a row whose truth is a
// repository — silently, once per ingest round.
func TestIngest_GitBackedSlugCollision_RowNotHijacked(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Slug matches what InferSlug derives from the bundle path
	// skills/shared-slug-skill/SKILL.md (the "skills" and "skill" segments drop).
	before := seedGitBackedItem(t, db, models.CapabilityItem{
		ID: "git-1", Slug: "shared-slug-skill", ItemType: "skill",
		Name: "owned by the repository", Content: "# from git\n",
		SourcePath: "skill.md", SourceRepoPath: "skill.md", SourceType: "git",
		SourceSHA: "gitsha-do-not-touch", GitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})

	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "shared-slug-skill", Type: "skill", Source: "catalog/x",
			Description: "catalog wants this slug", Category: "tooling"}},
		map[string]string{"shared-slug-skill": skillBodyFor("shared-slug-skill")})

	result, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	assertItemUnchanged(t, before, loadItemByID(t, db, "git-1"))

	// The catalog entry must still land, under a de-duplicated slug: insertItem's
	// unique-constraint retry owns that outcome. Asserted explicitly because
	// excluding the git row from the slug index is exactly what turns this case
	// from "silent hijack" into "INSERT that has to dodge the unique index".
	if result.Added != 1 {
		t.Errorf("catalog entry should still be inserted; Added=%d Failed=%d errors=%v",
			result.Added, result.Failed, result.Errors)
	}
	if result.Failed != 0 {
		t.Errorf("a git row must not make the entry fail; Failed=%d errors=%v", result.Failed, result.Errors)
	}
	var inserted models.CapabilityItem
	if err := db.Where("content_backend = ? AND item_type = ?", "db", "skill").First(&inserted).Error; err != nil {
		t.Fatalf("catalog entry did not produce a db-backed row: %v", err)
	}
	if inserted.Slug == "shared-slug-skill" {
		t.Errorf("db row took the git row's slug slot — the unique index should have forced a suffix")
	}
}

// TestIngest_DBBackedSlugCollision_StillAdopted is the control for the test
// above: the cross-entry slug adoption must keep working for DB-backed rows.
// Zero regression for existing data is the premise of the whole gray rollout —
// if this test ever flips, the exclusion was written too wide.
func TestIngest_DBBackedSlugCollision_StillAdopted(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	legacy := models.CapabilityItem{
		ID: "db-1", RegistryID: PublicRegistryID, RepoID: PublicRepoID,
		Slug: "shared-slug-skill", ItemType: "skill", Name: "stale name",
		Content: "# stale\n", SourcePath: "skills/somewhere-else/SKILL.md",
		SourceType: "direct", SourceSHA: "stale-sha", Status: "active",
		CreatedBy: "system", UpdatedBy: "system",
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatalf("seed db row: %v", err)
	}

	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "shared-slug-skill", Type: "skill", Source: "catalog/x",
			Description: "catalog owns this", Category: "tooling"}},
		map[string]string{"shared-slug-skill": skillBodyFor("shared-slug-skill")})

	result, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("db-backed slug collision must still be adopted as an update; Updated=%d Added=%d errors=%v",
			result.Updated, result.Added, result.Errors)
	}

	after := loadItemByID(t, db, "db-1")
	if after.SourceSHA == "stale-sha" {
		t.Errorf("db-backed row was not updated — the exclusion is too wide")
	}
	if after.SourcePath != "skills/shared-slug-skill/SKILL.md" {
		t.Errorf("db-backed row should converge on the catalog path; got %q", after.SourcePath)
	}
}

// TestIngest_GitBackedRow_NotSoftArchived covers gate matrix W17-e. The
// soft-archive sweep spared "user-owned" rows by source_type ('archive'/'fork'),
// which does not cover a git-discovered row: Git discovery writes
// source_type='git'. Such rows do carry catalog-shaped 2-segment paths in
// practice (git-backed plugins store .claude-plugin/plugin.json), so they reach
// the sweep, and their path is by definition absent from any catalog bundle —
// meaning every single ingest round would archive them.
//
// The db-backed sibling in the same fixture is the control: it must still be
// archived, so the test fails both if git rows are swept and if the exclusion
// disabled the sweep.
func TestIngest_GitBackedRow_NotSoftArchived(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	before := seedGitBackedItem(t, db, models.CapabilityItem{
		ID: "git-2", Slug: "repo-owned-plugin", ItemType: "plugin",
		Name: "repo owned", Content: "",
		// Catalog-shaped 2-segment path → enters itemsByEntryDir, exactly like the
		// git-backed plugin forks in the live DB.
		SourcePath: "plugins/repo-owned-plugin/.claude-plugin/plugin.json",
		SourceType: "git", SourceSHA: "gitsha-do-not-touch",
	})

	orphan := models.CapabilityItem{
		ID: "db-2", RegistryID: PublicRegistryID, RepoID: PublicRepoID,
		Slug: "gone-upstream", ItemType: "skill", Name: "gone upstream",
		SourcePath: "skills/gone-upstream/SKILL.md", SourceType: "direct",
		Status: "active", CreatedBy: "system", UpdatedBy: "system",
	}
	if err := db.Create(&orphan).Error; err != nil {
		t.Fatalf("seed db orphan: %v", err)
	}

	// Bundle carries neither path.
	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "unrelated-skill", Type: "skill", Source: "catalog/x", Description: "x", Category: "tooling"}},
		map[string]string{"unrelated-skill": skillBodyFor("unrelated-skill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	assertItemUnchanged(t, before, loadItemByID(t, db, "git-2"))
	if got := loadItemByID(t, db, "db-2").Status; got != "archived" {
		t.Errorf("db-backed catalog orphan must still be archived; status=%q", got)
	}
}

// TestIngest_GitBackedRow_AssetsNotReconciled covers gate matrix W17-g. The
// asset pass re-reads capability_items from the DB rather than slicing the
// pre-loaded snapshot, so it does not inherit the pre-load filter and needs its
// own guard. syncAssetsForItem reconciles the row's assets to exactly what the
// bundle directory holds — for a git-backed row that means deleting a file tree
// the catalog never owned.
func TestIngest_GitBackedRow_AssetsNotReconciled(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Shares the catalog entry's match key, which is what puts it in range of the
	// asset query. Its own slug differs, so it is not the entry's update target.
	seedGitBackedItem(t, db, models.CapabilityItem{
		ID: "git-3", Slug: "repo-owned-skill", ItemType: "skill", Name: "repo owned",
		SourcePath: "skill.md", SourceRepoPath: "skill.md", SourceType: "git",
		CatalogEntryDir: "skills/asset-skill", SourceSHA: "gitsha-do-not-touch",
	})
	asset := models.CapabilityAsset{
		ID: "asset-1", ItemID: "git-3", RelPath: "reference.md",
		MimeType: "text/markdown", ContentSHA: "sha-from-repo",
	}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "asset-skill", Type: "skill", Source: "catalog/x", Description: "x", Category: "tooling"}},
		map[string]string{"asset-skill": skillBodyFor("asset-skill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var count int64
	if err := db.Model(&models.CapabilityAsset{}).Where("item_id = ?", "git-3").Count(&count).Error; err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 1 {
		t.Errorf("catalog ingest deleted assets of a git-backed row (%d left, want 1)", count)
	}
}

// TestIngest_GitBackedChild_NotParentLinked covers gate matrix W17-k. Like the
// asset pass, reconcileParentPluginLinks deliberately re-queries (so same-batch
// inserts are visible) and therefore does not inherit the pre-load filter. Its
// existing scope guard is source_type NOT IN ('archive','fork'), which a
// git-discovered row (source_type='git') walks straight through.
//
// The db-backed child in the same entryDir is the control: it must still be
// linked, so this fails both on a leaked write and on an over-wide exclusion.
func TestIngest_GitBackedChild_NotParentLinked(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	entries := []catalogEntry{
		pluginEntry("host-plugin"),
		subSkillEntry("bundled-child", "host-plugin"),
	}
	bodies := map[string]string{
		"host-plugin":   pluginBodyFor("host-plugin"),
		"bundled-child": skillBodyFor("bundled-child"),
	}

	// First pass creates the plugin and its db-backed child.
	bundle := writeMultiEntryBundle(t, entries, bodies)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// A git-backed row that shares the child's match key: its parent link is
	// decided by the repository manifest, never by a catalog bundle. Its
	// source_path is one the bundle ships so the soft-archive sweep leaves it
	// alone — this test must fail on the parent-link write specifically, not on
	// an archive that happens first and then hides the link path behind
	// reconcileParentPluginLinks' own `status <> 'archived'` filter.
	before := seedGitBackedItem(t, db, models.CapabilityItem{
		ID: "git-4", Slug: "repo-owned-child", ItemType: "skill", Name: "repo owned child",
		SourcePath: "skills/bundled-child/SKILL.md", SourceRepoPath: "skill.md", SourceType: "git",
		CatalogEntryDir: "skills/bundled-child", SourceSHA: "gitsha-do-not-touch",
	})

	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	after := loadItemByID(t, db, "git-4")
	if after.ParentPluginID != nil {
		t.Errorf("catalog reconcile linked a git-backed row to plugin %q", *after.ParentPluginID)
	}
	assertItemUnchanged(t, before, after)

	var child models.CapabilityItem
	if err := db.Where("content_backend = ? AND item_type = ? AND catalog_entry_dir = ?",
		"db", "skill", "skills/bundled-child").First(&child).Error; err != nil {
		t.Fatalf("load db-backed child: %v", err)
	}
	if child.ParentPluginID == nil {
		t.Errorf("db-backed child must still be linked to its parent plugin")
	}
}

// TestIngest_GitBackedRow_SurvivesRepeatedRounds pins the property the whole
// exclusion exists for: ingest is idempotent for git-backed rows. A guard that
// only holds on the first pass (e.g. one that relies on a row not yet having a
// catalog_entry_dir) would pass the single-round tests above and still corrupt
// data in production, where ingest runs on a schedule.
func TestIngest_GitBackedRow_SurvivesRepeatedRounds(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	before := seedGitBackedItem(t, db, models.CapabilityItem{
		ID: "git-5", Slug: "steady-skill", ItemType: "skill", Name: "repo owned",
		Content: "# from git\n", SourcePath: "skill.md", SourceRepoPath: "skill.md",
		SourceType: "git", CatalogEntryDir: "skills/steady-skill", SourceSHA: "gitsha-do-not-touch",
	})

	for round := 1; round <= 3; round++ {
		// Vary the body each round so the entry always takes the content-changed
		// path rather than the metadata-only one.
		bundle := writeMultiEntryBundle(t,
			[]catalogEntry{{ID: "steady-skill", Type: "skill", Source: "catalog/x",
				Description: "round", Category: "tooling"}},
			map[string]string{"steady-skill": skillBodyFor("steady-skill") + "\nround\n"})
		if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
			t.Fatalf("ingest round %d: %v", round, err)
		}
		// updated_at has second-ish resolution on some backends; make any write
		// unmistakable rather than relying on timer granularity.
		time.Sleep(time.Millisecond)
	}

	assertItemUnchanged(t, before, loadItemByID(t, db, "git-5"))
}

// TestIngest_ProvisionedGitRow_NotRewritten covers the row shape the Git write
// paths in package handlers now produce for the four standalone types: a fork
// or a Cloud creation landing in the PUBLIC registry with repo_id='public',
// carrying NO content at all (read-through serves it from the repository) and a
// single-segment manifest path.
//
// It is a deliberate duplicate of the coverage above, aimed at the dependency
// rather than the mechanism: the git write paths are only correct because these
// queries exclude content_backend='git'. Sharing a registry with the catalog is
// what makes that exclusion load-bearing — if it ever narrows, an ingest round
// would overwrite name/slug/content on rows a user created, and the empty
// content column means it would overwrite them with catalog text rather than
// merely refreshing them.
func TestIngest_ProvisionedGitRow_NotRewritten(t *testing.T) {
	for _, tc := range []struct {
		itemType     string
		manifestPath string
		sourceType   string
	}{
		{"skill", "skill.md", "fork"},
		{"subagent", "agent.md", "fork"},
		{"command", "command.md", "direct"},
		{"mcp", "mcp.json", "direct"},
	} {
		t.Run(tc.itemType, func(t *testing.T) {
			db := newIngestTestDB(t)
			svc := newIngestService(db)

			before := seedGitBackedItem(t, db, models.CapabilityItem{
				ID: "prov-" + tc.itemType, Slug: "provisioned-skill", ItemType: tc.itemType,
				Name: "user owned", Content: "", SourcePath: tc.manifestPath,
				SourceRepoPath: tc.manifestPath, SourceType: tc.sourceType,
				SourceRepoURL: "https://git.example/10001/provisioned-skill",
				GitSyncStatus: "pending",
			})

			// A catalog entry deriving the same slug: the collision path that used
			// to adopt the row across entries.
			bundle := writeMultiEntryBundle(t,
				[]catalogEntry{{ID: "provisioned-skill", Type: "skill", Source: "catalog/x",
					Description: "catalog wants this slug", Category: "tooling"}},
				map[string]string{"provisioned-skill": skillBodyFor("provisioned-skill")})
			if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
				t.Fatalf("ingest: %v", err)
			}

			after := loadItemByID(t, db, "prov-"+tc.itemType)
			assertItemUnchanged(t, before, after)
			if after.ContentBackend != "git" {
				t.Errorf("content_backend was rewritten: %q", after.ContentBackend)
			}
			if after.SourceRepoURL != before.SourceRepoURL {
				t.Errorf("source_repo_url was rewritten: %q -> %q", before.SourceRepoURL, after.SourceRepoURL)
			}
			if after.GitSHA != before.GitSHA {
				t.Errorf("git_sha was rewritten: %q -> %q", before.GitSHA, after.GitSHA)
			}
		})
	}
}
