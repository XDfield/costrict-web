package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestCatalogIngest_NonASCIISkillKeepsStableCanonicalSlugOnReingest(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	entry := catalogEntry{
		ID:          "技能",
		Type:        "skill",
		Source:      "personal/skill",
		Description: "中文技能",
	}
	dir := writeSkillBundle(t, entry, "---\nname: 技能\n---\n# v1\n")

	first, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.Added != 1 || first.Failed != 0 {
		t.Fatalf("first ingest result: added=%d failed=%d errors=%v", first.Added, first.Failed, first.Errors)
	}

	var created models.CapabilityItem
	if err := db.Where("catalog_entry_dir = ?", "skills/技能").First(&created).Error; err != nil {
		t.Fatalf("load created item: %v", err)
	}
	wantSlug := "skill-" + strings.ReplaceAll(created.ID, "-", "")
	if created.Slug != wantSlug {
		t.Fatalf("created slug = %q, want %q", created.Slug, wantSlug)
	}

	skillPath := filepath.Join(dir, "catalog-download", "skills", entry.ID, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: 技能\n---\n# v2\n"), 0o644); err != nil {
		t.Fatalf("update skill fixture: %v", err)
	}
	second, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.Updated != 1 || second.Added != 0 || second.Failed != 0 {
		t.Fatalf("second ingest result: added=%d updated=%d failed=%d errors=%v", second.Added, second.Updated, second.Failed, second.Errors)
	}

	var items []models.CapabilityItem
	if err := db.Where("catalog_entry_dir = ?", "skills/技能").Find(&items).Error; err != nil {
		t.Fatalf("reload items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("re-ingest created %d rows, want 1", len(items))
	}
	if items[0].ID != created.ID || items[0].Slug != wantSlug {
		t.Fatalf("re-ingest changed identity: id=%q slug=%q, want id=%q slug=%q", items[0].ID, items[0].Slug, created.ID, wantSlug)
	}
}

func TestCatalogIngest_CollisionSuffixKeepsEntryIdentityOnReingest(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	occupant := models.CapabilityItem{
		ID:              "occupant",
		RegistryID:      PublicRegistryID,
		RepoID:          PublicRepoID,
		Slug:            "foo-bar",
		ItemType:        "skill",
		Name:            "Foo Bar",
		Content:         "occupant content",
		SourcePath:      "skills/foo-bar/SKILL.md",
		CatalogEntryDir: "skills/foo-bar",
		SourceSHA:       "old-occupant-sha",
		Status:          "active",
		CreatedBy:       "tester",
		UpdatedBy:       "tester",
	}
	migrated := models.CapabilityItem{
		ID:              "migrated",
		RegistryID:      PublicRegistryID,
		RepoID:          PublicRepoID,
		Slug:            "foo-bar-migrated-12345678abcd4321abcd1234567890ab",
		ItemType:        "skill",
		Name:            "Foo Dot Bar",
		Content:         "migrated content",
		SourcePath:      "skills/foo.bar/SKILL.md",
		CatalogEntryDir: "skills/foo.bar",
		SourceSHA:       "old-migrated-sha",
		Status:          "active",
		CreatedBy:       "tester",
		UpdatedBy:       "tester",
	}
	if err := db.Create(&occupant).Error; err != nil {
		t.Fatalf("seed occupant: %v", err)
	}
	if err := db.Create(&migrated).Error; err != nil {
		t.Fatalf("seed migrated row: %v", err)
	}

	entry := catalogEntry{
		ID:          "foo.bar",
		Type:        "skill",
		Source:      "catalog/foo.bar",
		Description: "updated migrated entry",
	}
	dir := writeSkillBundle(t, entry, "---\nname: Foo Dot Bar\n---\n# updated migrated content\n")
	result, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("re-ingest collision entry: %v", err)
	}
	if result.Updated != 1 || result.Added != 0 || result.Failed != 0 {
		t.Fatalf("re-ingest result: added=%d updated=%d failed=%d errors=%v", result.Added, result.Updated, result.Failed, result.Errors)
	}

	var gotMigrated models.CapabilityItem
	if err := db.First(&gotMigrated, "id = ?", migrated.ID).Error; err != nil {
		t.Fatalf("load migrated row: %v", err)
	}
	if gotMigrated.Slug != migrated.Slug || gotMigrated.CatalogEntryDir != migrated.CatalogEntryDir {
		t.Fatalf("migrated identity changed: slug=%q entryDir=%q", gotMigrated.Slug, gotMigrated.CatalogEntryDir)
	}
	if !strings.Contains(gotMigrated.Content, "updated migrated content") {
		t.Fatalf("migrated row was not updated: content=%q", gotMigrated.Content)
	}

	var gotOccupant models.CapabilityItem
	if err := db.First(&gotOccupant, "id = ?", occupant.ID).Error; err != nil {
		t.Fatalf("load occupant: %v", err)
	}
	if gotOccupant.Slug != occupant.Slug || gotOccupant.CatalogEntryDir != occupant.CatalogEntryDir ||
		gotOccupant.Content != occupant.Content {
		t.Fatalf("foreign occupant was polluted: %+v", gotOccupant)
	}
}

func TestCatalogIngest_FirstCanonicalCollisionCreatesDistinctRows(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)
	entries := []catalogEntry{
		{ID: "foo.bar", Type: "skill", Source: "catalog/foo.bar"},
		{ID: "foo-bar", Type: "skill", Source: "catalog/foo-bar"},
	}
	bodies := map[string]string{
		"foo.bar": "---\nname: Foo Dot Bar\n---\ndot v1\n",
		"foo-bar": "---\nname: Foo Dash Bar\n---\ndash v1\n",
	}

	first, err := svc.Ingest(
		context.Background(),
		IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)},
		IngestOptions{TriggerUser: "tester"},
	)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.Added != 2 || first.Updated != 0 || first.Failed != 0 {
		t.Fatalf("first ingest result: added=%d updated=%d failed=%d errors=%v", first.Added, first.Updated, first.Failed, first.Errors)
	}

	dot := loadItemBySourcePath(t, db, "skills/foo.bar/SKILL.md")
	dash := loadItemBySourcePath(t, db, "skills/foo-bar/SKILL.md")
	if dot.ID == dash.ID {
		t.Fatal("canonical collision collapsed two catalog entries into one row")
	}
	gotSlugs := map[string]bool{dot.Slug: true, dash.Slug: true}
	if !gotSlugs["foo-bar"] || !gotSlugs["foo-bar-2"] {
		t.Fatalf("collision slugs = %q and %q, want foo-bar and foo-bar-2", dot.Slug, dash.Slug)
	}
	if !strings.Contains(dot.Content, "dot v1") || !strings.Contains(dash.Content, "dash v1") {
		t.Fatalf("catalog contents crossed entries: dot=%q dash=%q", dot.Content, dash.Content)
	}

	bodies["foo.bar"] = "---\nname: Foo Dot Bar\n---\ndot v2\n"
	bodies["foo-bar"] = "---\nname: Foo Dash Bar\n---\ndash v2\n"
	second, err := svc.Ingest(
		context.Background(),
		IngestSource{Dir: writeMultiEntryBundle(t, entries, bodies)},
		IngestOptions{TriggerUser: "tester"},
	)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.Added != 0 || second.Updated != 2 || second.Failed != 0 {
		t.Fatalf("second ingest result: added=%d updated=%d failed=%d errors=%v", second.Added, second.Updated, second.Failed, second.Errors)
	}

	dotAfter := loadItemBySourcePath(t, db, "skills/foo.bar/SKILL.md")
	dashAfter := loadItemBySourcePath(t, db, "skills/foo-bar/SKILL.md")
	if dotAfter.ID != dot.ID || dotAfter.Slug != dot.Slug ||
		dashAfter.ID != dash.ID || dashAfter.Slug != dash.Slug {
		t.Fatalf("re-ingest changed collision identities: dot=%+v dash=%+v", dotAfter, dashAfter)
	}
	if !strings.Contains(dotAfter.Content, "dot v2") || !strings.Contains(dashAfter.Content, "dash v2") {
		t.Fatalf("re-ingest crossed contents: dot=%q dash=%q", dotAfter.Content, dashAfter.Content)
	}
}

func TestSyncRegistry_NonASCIISkillUsesStableCanonicalSlug(t *testing.T) {
	db := newIngestTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE sync_logs (
			id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, trigger_type TEXT,
			trigger_user TEXT, status TEXT, commit_sha TEXT, previous_sha TEXT,
			total_items INTEGER DEFAULT 0, added_items INTEGER DEFAULT 0,
			updated_items INTEGER DEFAULT 0, deleted_items INTEGER DEFAULT 0,
			skipped_items INTEGER DEFAULT 0, failed_items INTEGER DEFAULT 0,
			error_message TEXT, duration_ms INTEGER DEFAULT 0, started_at DATETIME,
			finished_at DATETIME, created_at DATETIME
		)`,
		`CREATE TABLE capability_assets (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, rel_path TEXT NOT NULL,
			text_content TEXT, storage_backend TEXT, storage_key TEXT,
			mime_type TEXT, file_size INTEGER DEFAULT 0, content_sha TEXT,
			created_at DATETIME, updated_at DATETIME
		)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create sync table: %v", err)
		}
	}

	repoDir := t.TempDir()
	skillDir := filepath.Join(repoDir, "skills", "技能")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: 技能\n---\n# v1\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	commitAll := func(message string) {
		t.Helper()
		worktree, err := repo.Worktree()
		if err != nil {
			t.Fatalf("open worktree: %v", err)
		}
		if _, err := worktree.Add("."); err != nil {
			t.Fatalf("git add: %v", err)
		}
		if _, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{
			Name: "Test", Email: "test@example.com", When: time.Now(),
		}}); err != nil {
			t.Fatalf("git commit: %v", err)
		}
	}
	commitAll("initial")

	registry := models.CapabilityRegistry{
		ID:             "registry-sync-canonical",
		Name:           "sync canonical",
		SourceType:     "git",
		ExternalURL:    repoDir,
		ExternalBranch: "master",
		RepoID:         "repo-sync-canonical",
		OwnerID:        "tester",
	}
	if err := db.Create(&registry).Error; err != nil {
		t.Fatalf("create registry: %v", err)
	}
	svc := &SyncService{
		DB:     db,
		Git:    &GitService{TempBaseDir: t.TempDir()},
		Parser: &ParserService{},
	}

	first, err := svc.SyncRegistry(context.Background(), registry.ID, SyncOptions{TriggerType: "manual", TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if first.Added != 1 || first.Failed != 0 {
		t.Fatalf("first sync result: added=%d failed=%d errors=%v", first.Added, first.Failed, first.Errors)
	}

	var created models.CapabilityItem
	if err := db.Where("registry_id = ?", registry.ID).First(&created).Error; err != nil {
		t.Fatalf("load synced item: %v", err)
	}
	wantSlug := "skill-" + strings.ReplaceAll(created.ID, "-", "")
	if created.Slug != wantSlug {
		t.Fatalf("synced slug = %q, want %q", created.Slug, wantSlug)
	}

	if err := os.WriteFile(skillPath, []byte("---\nname: 技能\n---\n# v2\n"), 0o644); err != nil {
		t.Fatalf("update skill: %v", err)
	}
	commitAll("update")
	second, err := svc.SyncRegistry(context.Background(), registry.ID, SyncOptions{TriggerType: "manual", TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if second.Updated != 1 || second.Added != 0 || second.Failed != 0 {
		t.Fatalf("second sync result: added=%d updated=%d failed=%d errors=%v", second.Added, second.Updated, second.Failed, second.Errors)
	}

	var items []models.CapabilityItem
	if err := db.Where("registry_id = ?", registry.ID).Find(&items).Error; err != nil {
		t.Fatalf("reload synced items: %v", err)
	}
	if len(items) != 1 || items[0].ID != created.ID || items[0].Slug != wantSlug {
		t.Fatalf("second sync changed identity: %+v", items)
	}
}

func TestSyncRegistry_CollisionSuffixSurvivesPathMatchedUpdate(t *testing.T) {
	db := newIngestTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE sync_logs (
			id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, trigger_type TEXT,
			trigger_user TEXT, status TEXT, commit_sha TEXT, previous_sha TEXT,
			total_items INTEGER DEFAULT 0, added_items INTEGER DEFAULT 0,
			updated_items INTEGER DEFAULT 0, deleted_items INTEGER DEFAULT 0,
			skipped_items INTEGER DEFAULT 0, failed_items INTEGER DEFAULT 0,
			error_message TEXT, duration_ms INTEGER DEFAULT 0, started_at DATETIME,
			finished_at DATETIME, created_at DATETIME
		)`,
		`CREATE TABLE capability_assets (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, rel_path TEXT NOT NULL,
			text_content TEXT, storage_backend TEXT, storage_key TEXT,
			mime_type TEXT, file_size INTEGER DEFAULT 0, content_sha TEXT,
			created_at DATETIME, updated_at DATETIME
		)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create sync table: %v", err)
		}
	}

	repoDir := t.TempDir()
	skillDir := filepath.Join(repoDir, "skills", "foo.bar")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: Foo Dot Bar\n---\n# updated\n"),
		0o644,
	); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("open worktree: %v", err)
	}
	if _, err := worktree.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := worktree.Commit("initial", &git.CommitOptions{Author: &object.Signature{
		Name: "Test", Email: "test@example.com", When: time.Now(),
	}}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	registry := models.CapabilityRegistry{
		ID:             "registry-sync-collision",
		Name:           "sync collision",
		SourceType:     "git",
		ExternalURL:    repoDir,
		ExternalBranch: "master",
		RepoID:         "repo-sync-collision",
		OwnerID:        "tester",
	}
	if err := db.Create(&registry).Error; err != nil {
		t.Fatalf("create registry: %v", err)
	}
	occupant := models.CapabilityItem{
		ID: "occupant", RegistryID: "other-registry", RepoID: registry.RepoID,
		Slug: "foo-bar", ItemType: "skill", Name: "Foo Bar",
		SourcePath: "skills/foo-bar/SKILL.md", SourceSHA: "occupant-sha",
		Status: "active", CreatedBy: "tester", UpdatedBy: "tester",
	}
	migrated := models.CapabilityItem{
		ID: "migrated", RegistryID: registry.ID, RepoID: registry.RepoID,
		Slug: "foo-bar-migrated-12345678abcd4321abcd1234567890ab", ItemType: "skill", Name: "Foo Dot Bar",
		SourcePath: "skills/foo.bar/SKILL.md", SourceSHA: "old-migrated-sha",
		Status: "active", CreatedBy: "tester", UpdatedBy: "tester",
	}
	if err := db.Create(&occupant).Error; err != nil {
		t.Fatalf("seed occupant: %v", err)
	}
	if err := db.Create(&migrated).Error; err != nil {
		t.Fatalf("seed migrated row: %v", err)
	}

	svc := &SyncService{
		DB:     db,
		Git:    &GitService{TempBaseDir: t.TempDir()},
		Parser: &ParserService{},
	}
	result, err := svc.SyncRegistry(context.Background(), registry.ID, SyncOptions{TriggerType: "manual", TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("sync collision entry: %v", err)
	}
	if result.Updated != 1 || result.Failed != 0 {
		t.Fatalf("sync result: updated=%d failed=%d errors=%v", result.Updated, result.Failed, result.Errors)
	}

	var gotMigrated models.CapabilityItem
	if err := db.First(&gotMigrated, "id = ?", migrated.ID).Error; err != nil {
		t.Fatalf("load migrated row: %v", err)
	}
	if gotMigrated.Slug != migrated.Slug || !strings.Contains(gotMigrated.Content, "# updated") {
		t.Fatalf("migrated row changed incorrectly: %+v", gotMigrated)
	}
	var gotOccupant models.CapabilityItem
	if err := db.First(&gotOccupant, "id = ?", occupant.ID).Error; err != nil {
		t.Fatalf("load occupant: %v", err)
	}
	if gotOccupant.Slug != occupant.Slug || gotOccupant.Content != occupant.Content {
		t.Fatalf("occupant was polluted: %+v", gotOccupant)
	}
}

func TestSyncRegistry_FirstCanonicalCollisionCreatesDistinctRows(t *testing.T) {
	db := newIngestTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE sync_logs (
			id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, trigger_type TEXT,
			trigger_user TEXT, status TEXT, commit_sha TEXT, previous_sha TEXT,
			total_items INTEGER DEFAULT 0, added_items INTEGER DEFAULT 0,
			updated_items INTEGER DEFAULT 0, deleted_items INTEGER DEFAULT 0,
			skipped_items INTEGER DEFAULT 0, failed_items INTEGER DEFAULT 0,
			error_message TEXT, duration_ms INTEGER DEFAULT 0, started_at DATETIME,
			finished_at DATETIME, created_at DATETIME
		)`,
		`CREATE TABLE capability_assets (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, rel_path TEXT NOT NULL,
			text_content TEXT, storage_backend TEXT, storage_key TEXT,
			mime_type TEXT, file_size INTEGER DEFAULT 0, content_sha TEXT,
			created_at DATETIME, updated_at DATETIME
		)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create sync table: %v", err)
		}
	}

	repoDir := t.TempDir()
	writeSkill := func(dir, name, body string) {
		t.Helper()
		skillDir := filepath.Join(repoDir, "skills", dir)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatalf("mkdir skill %s: %v", dir, err)
		}
		content := "---\nname: " + name + "\n---\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write skill %s: %v", dir, err)
		}
	}
	writeSkill("foo.bar", "Foo Dot Bar", "dot v1")
	writeSkill("foo-bar", "Foo Dash Bar", "dash v1")

	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	commitAll := func(message string) {
		t.Helper()
		worktree, err := repo.Worktree()
		if err != nil {
			t.Fatalf("open worktree: %v", err)
		}
		if _, err := worktree.Add("."); err != nil {
			t.Fatalf("git add: %v", err)
		}
		if _, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{
			Name: "Test", Email: "test@example.com", When: time.Now(),
		}}); err != nil {
			t.Fatalf("git commit: %v", err)
		}
	}
	commitAll("initial")

	registry := models.CapabilityRegistry{
		ID:             "registry-sync-first-collision",
		Name:           "sync first collision",
		SourceType:     "git",
		ExternalURL:    repoDir,
		ExternalBranch: "master",
		RepoID:         "repo-sync-first-collision",
		OwnerID:        "tester",
	}
	if err := db.Create(&registry).Error; err != nil {
		t.Fatalf("create registry: %v", err)
	}
	svc := &SyncService{
		DB:     db,
		Git:    &GitService{TempBaseDir: t.TempDir()},
		Parser: &ParserService{},
	}

	first, err := svc.SyncRegistry(context.Background(), registry.ID, SyncOptions{TriggerType: "manual", TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if first.Added != 2 || first.Updated != 0 || first.Failed != 0 {
		t.Fatalf("first sync result: added=%d updated=%d failed=%d errors=%v", first.Added, first.Updated, first.Failed, first.Errors)
	}

	var dot, dash models.CapabilityItem
	if err := db.First(&dot, "registry_id = ? AND source_path = ?", registry.ID, "skills/foo.bar/SKILL.md").Error; err != nil {
		t.Fatalf("load dot skill: %v", err)
	}
	if err := db.First(&dash, "registry_id = ? AND source_path = ?", registry.ID, "skills/foo-bar/SKILL.md").Error; err != nil {
		t.Fatalf("load dash skill: %v", err)
	}
	gotSlugs := map[string]bool{dot.Slug: true, dash.Slug: true}
	if dot.ID == dash.ID || !gotSlugs["foo-bar"] || !gotSlugs["foo-bar-2"] {
		t.Fatalf("first sync collapsed collision: dot=%+v dash=%+v", dot, dash)
	}
	if !strings.Contains(dot.Content, "dot v1") || !strings.Contains(dash.Content, "dash v1") {
		t.Fatalf("first sync crossed contents: dot=%q dash=%q", dot.Content, dash.Content)
	}

	writeSkill("foo.bar", "Foo Dot Bar", "dot v2")
	writeSkill("foo-bar", "Foo Dash Bar", "dash v2")
	commitAll("update")
	second, err := svc.SyncRegistry(context.Background(), registry.ID, SyncOptions{TriggerType: "manual", TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if second.Added != 0 || second.Updated != 2 || second.Failed != 0 {
		t.Fatalf("second sync result: added=%d updated=%d failed=%d errors=%v", second.Added, second.Updated, second.Failed, second.Errors)
	}

	var dotAfter, dashAfter models.CapabilityItem
	if err := db.First(&dotAfter, "id = ?", dot.ID).Error; err != nil {
		t.Fatalf("reload dot skill: %v", err)
	}
	if err := db.First(&dashAfter, "id = ?", dash.ID).Error; err != nil {
		t.Fatalf("reload dash skill: %v", err)
	}
	if dotAfter.Slug != dot.Slug || dashAfter.Slug != dash.Slug {
		t.Fatalf("second sync changed collision slugs: dot=%q dash=%q", dotAfter.Slug, dashAfter.Slug)
	}
	if !strings.Contains(dotAfter.Content, "dot v2") || !strings.Contains(dashAfter.Content, "dash v2") {
		t.Fatalf("second sync crossed contents: dot=%q dash=%q", dotAfter.Content, dashAfter.Content)
	}
}
