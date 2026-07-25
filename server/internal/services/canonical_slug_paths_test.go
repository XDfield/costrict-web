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
