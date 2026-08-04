package services

import (
	"context"
	"sort"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The three writers that rebuild an item's tags (user API, Git sync worker,
// system side) each DELETE + re-insert. Before item_tags carried a `source`
// column the last writer to run silently dropped the other two writers' tags --
// most visibly, tags a user set in Cloud vanished on the next Git push. These
// tests pin the partition: every writer touches only its own domain, and the
// unattributable `legacy` rows that predate the partition are never deleted.

func seedTagDomain(t *testing.T, db *gorm.DB, itemID, tagID, slug, source string) {
	t.Helper()
	var existing models.ItemTagDict
	if err := db.Where("id = ?", tagID).First(&existing).Error; err != nil {
		if err := db.Create(&models.ItemTagDict{
			ID: tagID, Slug: slug, TagClass: TagClassCustom, CreatedBy: "tester",
		}).Error; err != nil {
			t.Fatalf("create tag dict %s: %v", slug, err)
		}
	}
	if err := db.Create(&models.ItemTag{
		ID: source + "-" + tagID + "-" + itemID, ItemID: itemID, TagID: tagID, Source: source,
	}).Error; err != nil {
		t.Fatalf("link tag %s as %s: %v", slug, source, err)
	}
}

func sortedItemTagSlugs(t *testing.T, db *gorm.DB, itemID string) []string {
	t.Helper()
	slugs := itemTagSlugs(t, db, itemID)
	sort.Strings(slugs)
	return slugs
}

func tagSourcesFor(t *testing.T, db *gorm.DB, itemID string) []string {
	t.Helper()
	var rows []models.ItemTag
	if err := db.Where("item_id = ?", itemID).Order("source ASC").Find(&rows).Error; err != nil {
		t.Fatalf("load item tag rows: %v", err)
	}
	sources := make([]string, 0, len(rows))
	for _, row := range rows {
		sources = append(sources, row.Source)
	}
	return sources
}

func assertSlugs(t *testing.T, got, want []string, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: tags = %v, want %v", context, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: tags = %v, want %v", context, got, want)
		}
	}
}

// AC: 用户 API 打 tag → 触发一次 git sync → 用户 tag 仍在。
// A push rebuilds only the `git` domain, so the user's tags, the scanner's tags
// and the pre-partition legacy rows all survive.
func TestTagPartition_GitSyncRebuildsOnlyGitDomain(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-tag-domains", "repo-tag-domains", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	seedTagDomain(t, db, item.ID, "tag-user", "user-set", TagSourceUser)
	seedTagDomain(t, db, item.ID, "tag-system", "system-set", TagSourceSystem)
	seedTagDomain(t, db, item.ID, "tag-legacy", "legacy-set", TagSourceLegacy)
	seedTagDomain(t, db, item.ID, "tag-git-old", "git-old", TagSourceGit)

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Domains\ndescription: d\ntags:\n  - git-new\n---\nbody"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-tag-domains", "lease-tag-domains")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}

	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID),
		[]string{"git-new", "legacy-set", "system-set", "user-set"},
		"after git push")
}

// AC: git frontmatter tag 更新 → 用户 tag 与 system tag 均未被剥离。
// Two consecutive pushes that change the frontmatter tag list must not touch
// anything outside the `git` domain.
func TestTagPartition_GitFrontmatterUpdateKeepsForeignDomains(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-frontmatter", "repo-frontmatter", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	seedTagDomain(t, db, item.ID, "tag-user", "user-set", TagSourceUser)
	seedTagDomain(t, db, item.ID, "tag-system", "system-set", TagSourceSystem)
	seedTagDomain(t, db, item.ID, "tag-legacy", "legacy-set", TagSourceLegacy)

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Frontmatter\ndescription: d\ntags:\n  - alpha\n---\nbody v1"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-frontmatter-1", "lease-frontmatter-1")); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID),
		[]string{"alpha", "legacy-set", "system-set", "user-set"},
		"after first push")

	reader.files["SKILL.md"] = []byte("---\nname: Frontmatter\ndescription: d\ntags:\n  - beta\n---\nbody v2")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-frontmatter-2", "lease-frontmatter-2")); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID),
		[]string{"beta", "legacy-set", "system-set", "user-set"},
		"after frontmatter tag change")
}

// AC: 用户 API 重建 → git tag 与 system tag 均未被剥离；legacy 行仍在。
func TestTagPartition_UserRebuildKeepsForeignDomains(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-user-rebuild", "repo-user-rebuild", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	seedTagDomain(t, db, item.ID, "tag-git", "git-set", TagSourceGit)
	seedTagDomain(t, db, item.ID, "tag-system", "system-set", TagSourceSystem)
	seedTagDomain(t, db, item.ID, "tag-legacy", "legacy-set", TagSourceLegacy)
	seedTagDomain(t, db, item.ID, "tag-user-old", "user-old", TagSourceUser)

	svc := &TagService{DB: db}
	if err := db.Create(&models.ItemTagDict{
		ID: "tag-user-new", Slug: "user-new", TagClass: TagClassCustom, CreatedBy: "tester",
	}).Error; err != nil {
		t.Fatalf("create tag dict: %v", err)
	}
	if err := svc.SetItemTags(item.ID, []string{"tag-user-new"}, TagSourceUser); err != nil {
		t.Fatalf("set user tags: %v", err)
	}

	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID),
		[]string{"git-set", "legacy-set", "system-set", "user-new"},
		"after user rebuild")
}

// AC: 系统侧重建 → git tag 与 user tag 均未被剥离；legacy 行仍在。
func TestTagPartition_SystemRebuildKeepsForeignDomains(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-system-rebuild", "repo-system-rebuild", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	seedTagDomain(t, db, item.ID, "tag-git", "git-set", TagSourceGit)
	seedTagDomain(t, db, item.ID, "tag-user", "user-set", TagSourceUser)
	seedTagDomain(t, db, item.ID, "tag-legacy", "legacy-set", TagSourceLegacy)
	seedTagDomain(t, db, item.ID, "tag-system-old", "system-old", TagSourceSystem)

	svc := &TagService{DB: db}
	if err := db.Create(&models.ItemTagDict{
		ID: "tag-system-new", Slug: "system-new", TagClass: TagClassSystem, CreatedBy: "system",
	}).Error; err != nil {
		t.Fatalf("create tag dict: %v", err)
	}
	if err := svc.SetItemTags(item.ID, []string{"tag-system-new"}, TagSourceSystem); err != nil {
		t.Fatalf("set system tags: %v", err)
	}

	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID),
		[]string{"git-set", "legacy-set", "system-new", "user-set"},
		"after system rebuild")
}

// The old (item_id, tag_id) unique key made two domains fight over the same
// tag: the loser's INSERT was swallowed as a duplicate, so the tag lived in one
// domain only and disappeared the moment that domain was rebuilt.
func TestTagPartition_SameTagSurvivesInSecondDomain(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-shared-tag", "repo-shared-tag", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	seedTagDomain(t, db, item.ID, "tag-shared", "shared", TagSourceGit)

	svc := &TagService{DB: db}
	if err := svc.SetItemTags(item.ID, []string{"tag-shared"}, TagSourceUser); err != nil {
		t.Fatalf("set user tags: %v", err)
	}
	if sources := tagSourcesFor(t, db, item.ID); len(sources) != 2 {
		t.Fatalf("expected the shared tag in both domains, got sources %v", sources)
	}
	// The partition is a write-side mechanism, so the read path collapses the
	// duplicate before it reaches the UI.
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID), []string{"shared"}, "union is de-duplicated")

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Shared\ndescription: d\ntags: []\n---\nbody"),
	})
	syncSvc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := syncSvc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-shared", "lease-shared")); err != nil {
		t.Fatalf("sync: %v", err)
	}
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID), []string{"shared"}, "after git clears its own copy")
}

// A caller inventing a fourth domain would create rows nothing ever rebuilds.
func TestTagPartition_RejectsUnknownSource(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-bad-source", "repo-bad-source", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	svc := &TagService{DB: db}
	for _, source := range []string{"", TagSourceLegacy, "worker"} {
		if err := svc.SetItemTags(item.ID, nil, source); err != ErrInvalidTagSource {
			t.Fatalf("SetItemTags(source=%q) error = %v, want ErrInvalidTagSource", source, err)
		}
	}
}

// Control group: a DB-backed item never touched by Git must behave exactly as
// it did before the partition -- the user API remains authoritative over the
// tags it set, and the scanner keeps adding builtin tags on top.
func TestTagPartition_DBBackedItemBehaviourUnchanged(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-db-backed", "repo-db-backed", "db-skill", "skill", "DB.md")
	item.ContentBackend = "db"
	item.SourceGitServerID = ""
	item.SourceGitRepoID = 0
	createGitCapabilityItem(t, db, item)

	for _, tag := range []models.ItemTagDict{
		{ID: "tag-a", Slug: "alpha", TagClass: TagClassCustom, CreatedBy: "tester"},
		{ID: "tag-b", Slug: "beta", TagClass: TagClassCustom, CreatedBy: "tester"},
		{ID: "tag-c", Slug: "gamma", TagClass: TagClassBuiltin, CreatedBy: "system"},
	} {
		if err := db.Create(&tag).Error; err != nil {
			t.Fatalf("create tag dict: %v", err)
		}
	}

	svc := &TagService{DB: db}
	if err := svc.SetItemTags(item.ID, []string{"tag-a", "tag-b"}, TagSourceUser); err != nil {
		t.Fatalf("set user tags: %v", err)
	}
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID), []string{"alpha", "beta"}, "initial user tags")

	// Removing a tag through the user API still removes it.
	if err := svc.SetItemTags(item.ID, []string{"tag-a"}, TagSourceUser); err != nil {
		t.Fatalf("shrink user tags: %v", err)
	}
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID), []string{"alpha"}, "after user drops beta")

	// The scanner adds its builtin tag without disturbing the user's.
	if err := svc.SetItemTags(item.ID, []string{"tag-c"}, TagSourceSystem); err != nil {
		t.Fatalf("set system tags: %v", err)
	}
	assertSlugs(t, sortedItemTagSlugs(t, db, item.ID), []string{"alpha", "gamma"}, "user + system union")
}

// backfillBuiltinTags used to rewrite the whole tag set. It must now write only
// the system domain, while still consulting every domain to decide whether a
// suggested builtin tag is already present.
func TestTagPartition_BackfillBuiltinTagsScopedToSystemDomain(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE item_tag_dicts (
			id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, tag_class TEXT NOT NULL DEFAULT 'custom',
			created_by TEXT NOT NULL, created_at DATETIME
		)`,
		`CREATE TABLE item_tags (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, tag_id TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'legacy', created_at DATETIME,
			UNIQUE(item_id, tag_id, source)
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	for _, tag := range []models.ItemTagDict{
		{ID: "tag-planning", Slug: "planning", TagClass: TagClassBuiltin, CreatedBy: "system"},
		{ID: "tag-testing", Slug: "testing", TagClass: TagClassBuiltin, CreatedBy: "system"},
		{ID: "tag-user", Slug: "user-set", TagClass: TagClassCustom, CreatedBy: "tester"},
		{ID: "tag-git", Slug: "git-set", TagClass: TagClassCustom, CreatedBy: "tester"},
		{ID: "tag-legacy", Slug: "legacy-set", TagClass: TagClassCustom, CreatedBy: "tester"},
	} {
		if err := db.Create(&tag).Error; err != nil {
			t.Fatalf("create tag dict: %v", err)
		}
	}
	const itemID = "item-backfill"
	seedTagDomain(t, db, itemID, "tag-user", "user-set", TagSourceUser)
	seedTagDomain(t, db, itemID, "tag-git", "git-set", TagSourceGit)
	seedTagDomain(t, db, itemID, "tag-legacy", "legacy-set", TagSourceLegacy)
	seedTagDomain(t, db, itemID, "tag-planning", "planning", TagSourceSystem)

	scanSvc := &ScanService{DB: db, TagSvc: &TagService{DB: db}}
	if err := scanSvc.backfillBuiltinTags(itemID, []string{"testing"}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	assertSlugs(t, sortedItemTagSlugs(t, db, itemID),
		[]string{"git-set", "legacy-set", "planning", "testing", "user-set"},
		"after builtin backfill")

	systemIDs, err := scanSvc.TagSvc.GetItemTagIDsBySource(itemID, TagSourceSystem)
	if err != nil {
		t.Fatalf("load system domain: %v", err)
	}
	sort.Strings(systemIDs)
	assertSlugs(t, systemIDs, []string{"tag-planning", "tag-testing"},
		"backfill must not re-home foreign domains into `system`")
}

// A builtin tag the user already set is not duplicated into the system domain.
func TestTagPartition_BackfillSkipsBuiltinAlreadyOwnedByUser(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE item_tag_dicts (
			id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, tag_class TEXT NOT NULL DEFAULT 'custom',
			created_by TEXT NOT NULL, created_at DATETIME
		)`,
		`CREATE TABLE item_tags (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, tag_id TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'legacy', created_at DATETIME,
			UNIQUE(item_id, tag_id, source)
		)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	if err := db.Create(&models.ItemTagDict{
		ID: "tag-planning", Slug: "planning", TagClass: TagClassBuiltin, CreatedBy: "system",
	}).Error; err != nil {
		t.Fatalf("create tag dict: %v", err)
	}
	const itemID = "item-owned"
	seedTagDomain(t, db, itemID, "tag-planning", "planning", TagSourceUser)

	scanSvc := &ScanService{DB: db, TagSvc: &TagService{DB: db}}
	if err := scanSvc.backfillBuiltinTags(itemID, []string{"planning"}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if sources := tagSourcesFor(t, db, itemID); len(sources) != 1 || sources[0] != TagSourceUser {
		t.Fatalf("sources = %v, want the user's single row", sources)
	}
}
