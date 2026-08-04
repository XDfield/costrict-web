package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	gitCapabilityTestServerID = "git-server-1"
	gitCapabilityTestRepoID   = int64(101)
	gitCapabilityTestSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type fakeGitCapabilityReader struct {
	repo      *gitsync.Repo
	branch    *gitsync.Branch
	tree      []gitsync.GitTreeEntry
	files     map[string][]byte
	readErrs  map[string]error
	repoErr   error
	branchErr error
	treeErr   error
	onRead    func(string)
	refs      []string
}

func (r *fakeGitCapabilityReader) ListTree(_ context.Context, _, _, _ string) ([]gitsync.GitTreeEntry, error) {
	if r.treeErr != nil {
		return nil, r.treeErr
	}
	if r.tree != nil {
		return r.tree, nil
	}
	entries := make([]gitsync.GitTreeEntry, 0, len(r.files))
	seen := make(map[string]struct{}, len(r.files)+len(r.readErrs))
	for filePath := range r.files {
		entries = append(entries, gitsync.GitTreeEntry{Path: filePath, Type: "blob"})
		seen[filePath] = struct{}{}
	}
	for filePath := range r.readErrs {
		if _, exists := seen[filePath]; !exists {
			entries = append(entries, gitsync.GitTreeEntry{Path: filePath, Type: "blob"})
		}
	}
	return entries, nil
}

func (r *fakeGitCapabilityReader) GetRepoByID(_ context.Context, repoID int64) (*gitsync.Repo, error) {
	if r.repoErr != nil {
		return nil, r.repoErr
	}
	if r.repo != nil && r.repo.ID != repoID {
		return nil, fmt.Errorf("unexpected repository ID %d", repoID)
	}
	return r.repo, nil
}

func (r *fakeGitCapabilityReader) GetBranch(_ context.Context, _, _, branch string) (*gitsync.Branch, error) {
	if r.branchErr != nil {
		return nil, r.branchErr
	}
	if r.branch == nil {
		return nil, nil
	}
	if branch != r.branch.Name {
		return nil, fmt.Errorf("unexpected branch %q", branch)
	}
	return r.branch, nil
}

func (r *fakeGitCapabilityReader) ReadFile(_ context.Context, _, _, ref, filePath string) ([]byte, error) {
	r.refs = append(r.refs, ref)
	if r.onRead != nil {
		r.onRead(filePath)
	}
	if err := r.readErrs[filePath]; err != nil {
		return nil, err
	}
	return r.files[filePath], nil
}

func setupGitCapabilitySyncDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql database: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, ddl := range []string{
		`CREATE TABLE repositories (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, display_name TEXT, description TEXT,
			visibility TEXT, repo_type TEXT, owner_id TEXT NOT NULL, created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE repo_members (
			id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, user_id TEXT NOT NULL, username TEXT,
			role TEXT, created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE capability_registries (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT, source_type TEXT NOT NULL,
			external_url TEXT, external_branch TEXT, sync_enabled INTEGER, sync_interval INTEGER,
			last_synced_at DATETIME, last_sync_sha TEXT, sync_status TEXT, sync_config TEXT,
			last_sync_log_id TEXT, repo_id TEXT, owner_id TEXT NOT NULL, created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, repo_id TEXT NOT NULL, slug TEXT NOT NULL,
			item_type TEXT NOT NULL, name TEXT NOT NULL, description TEXT, category TEXT, version TEXT,
			descriptions TEXT, content TEXT, content_md5 TEXT, current_revision INTEGER, metadata TEXT,
			health TEXT, evaluation TEXT, source_path TEXT, catalog_entry_dir TEXT, source_sha TEXT, source_type TEXT, source TEXT,
			forked_from_item_id TEXT, forked_from_owner_id TEXT, parent_plugin_id TEXT,
			source_repo_url TEXT, source_repo_ref TEXT, source_repo_path TEXT,
			content_backend TEXT NOT NULL, source_git_server_id TEXT, source_git_repo_id INTEGER, source_git_entry_key TEXT NOT NULL DEFAULT '',
			git_sha TEXT, git_last_synced_at DATETIME, git_sync_status TEXT,
			git_sync_error TEXT, status TEXT, security_status TEXT, created_by TEXT, updated_by TEXT,
			last_scan_id TEXT, preview_count INTEGER, install_count INTEGER, favorite_count INTEGER,
			is_built_in INTEGER, experience_score REAL, created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE capability_versions (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, revision INTEGER NOT NULL, name TEXT,
			description TEXT, descriptions TEXT, category TEXT, version TEXT, content TEXT NOT NULL,
			content_md5 TEXT, metadata TEXT, commit_msg TEXT, created_by TEXT NOT NULL, source_path TEXT, created_at DATETIME
		)`,
		`CREATE TABLE git_capability_repositories (
			id TEXT PRIMARY KEY, git_server_id TEXT NOT NULL, git_repo_id INTEGER NOT NULL,
			repository_id TEXT NOT NULL UNIQUE, registry_id TEXT NOT NULL UNIQUE, full_name TEXT NOT NULL,
			repo_kind TEXT NOT NULL, identification_status TEXT NOT NULL, visibility TEXT NOT NULL,
			git_remote_url TEXT NOT NULL, default_branch TEXT NOT NULL, last_synced_commit TEXT NOT NULL,
			last_synced_at DATETIME, last_error TEXT NOT NULL, created_by TEXT NOT NULL,
			created_at DATETIME, updated_at DATETIME, UNIQUE(git_server_id, git_repo_id)
		)`,
		`CREATE TABLE user_git_binding (
			user_subject_id TEXT NOT NULL, tenant_id TEXT NOT NULL, git_uid INTEGER, git_username TEXT NOT NULL,
			provider_kind TEXT, sync_status TEXT NOT NULL, last_synced_at DATETIME, last_error TEXT,
			created_at DATETIME, updated_at DATETIME, PRIMARY KEY(user_subject_id, tenant_id)
		)`,
		`CREATE TABLE tenant_git_server_binding (
			tenant_id TEXT PRIMARY KEY, git_server_id TEXT NOT NULL, bound_at DATETIME, updated_at DATETIME
		)`,
		`CREATE UNIQUE INDEX uq_capability_items_git_manifest
			ON capability_items (source_git_server_id, source_git_repo_id, source_repo_path, source_git_entry_key)
			WHERE content_backend = 'git' AND source_git_server_id <> '' AND source_git_repo_id > 0 AND source_repo_path <> ''`,
		`CREATE UNIQUE INDEX idx_item_repo_type_slug ON capability_items (repo_id, item_type, slug)`,
		`CREATE TABLE item_tag_dicts (
			id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, tag_class TEXT NOT NULL, created_by TEXT NOT NULL, created_at DATETIME
		)`,
		`CREATE TABLE item_tags (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, tag_id TEXT NOT NULL, created_at DATETIME, UNIQUE(item_id, tag_id)
		)`,
		`CREATE TABLE git_capability_sync_jobs (
			id TEXT PRIMARY KEY, git_server_id TEXT NOT NULL, delivery_id TEXT NOT NULL,
			repo_id INTEGER NOT NULL, repo_full_name TEXT NOT NULL, default_branch TEXT NOT NULL,
			ref TEXT NOT NULL, before_sha TEXT NOT NULL DEFAULT '', after_sha TEXT NOT NULL,
			status TEXT NOT NULL, retry_count INTEGER NOT NULL, max_attempts INTEGER NOT NULL,
			last_error TEXT, scheduled_at DATETIME NOT NULL, started_at DATETIME,
			lease_token TEXT NOT NULL DEFAULT '', finished_at DATETIME, created_at DATETIME,
			UNIQUE(git_server_id, delivery_id)
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create test table: %v", err)
		}
	}
	return db
}

func newGitCapabilitySyncService(db *gorm.DB, reader *fakeGitCapabilityReader) (*GitCapabilitySyncService, *gitserver.Config) {
	return &GitCapabilitySyncService{
			DB:     db,
			Parser: &ParserService{},
			NewReader: func(*gitserver.Config) GitCapabilityReader {
				return reader
			},
		}, &gitserver.Config{
			ServerID: gitCapabilityTestServerID,
			Endpoint: "https://gitea.internal.example",
			WebURL:   "https://git.example",
		}
}

func newGitCapabilityReader(files map[string][]byte) *fakeGitCapabilityReader {
	return &fakeGitCapabilityReader{
		repo: &gitsync.Repo{
			ID:            gitCapabilityTestRepoID,
			FullName:      "alice/capabilities",
			DefaultBranch: "main",
			Owner:         &gitsync.RepoOwner{ID: 1001, Login: "alice"},
		},
		branch: &gitsync.Branch{Name: "main", CommitSHA: gitCapabilityTestSHA},
		files:  files,
	}
}

func createGitCapabilityLease(t *testing.T, db *gorm.DB, id, token string) GitCapabilitySyncLease {
	t.Helper()
	job := models.GitCapabilitySyncJob{
		ID:            id,
		GitServerID:   gitCapabilityTestServerID,
		DeliveryID:    "delivery-" + id,
		RepoID:        gitCapabilityTestRepoID,
		RepoFullName:  "alice/capabilities",
		DefaultBranch: "main",
		Ref:           "refs/heads/main",
		AfterSHA:      gitCapabilityTestSHA,
		Status:        models.GitCapabilitySyncJobStatusRunning,
		MaxAttempts:   3,
		ScheduledAt:   time.Now(),
		StartedAt:     ptrTime(time.Now()),
		LeaseToken:    token,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatalf("create lease: %v", err)
	}
	return GitCapabilitySyncLease{JobID: id, Token: token}
}

func newGitCapabilityItem(id, repoID, slug, itemType, sourcePath string) models.CapabilityItem {
	return models.CapabilityItem{
		ID:                id,
		RegistryID:        "registry-1",
		RepoID:            repoID,
		Slug:              slug,
		ItemType:          itemType,
		Name:              "old name",
		Description:       "old description",
		Category:          "old-category",
		Version:           "1.0.0",
		Content:           "DB content must not be mutated by index sync",
		Metadata:          datatypes.JSON([]byte(`{"keep":"existing"}`)),
		SourceRepoURL:     "https://git.example/alice/capabilities",
		SourceRepoRef:     "main",
		SourceRepoPath:    sourcePath,
		ContentBackend:    "git",
		SourceGitServerID: gitCapabilityTestServerID,
		SourceGitRepoID:   gitCapabilityTestRepoID,
		SourceSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		GitSHA:            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		GitSyncStatus:     gitCapabilitySyncPending,
		Status:            "active",
		CreatedBy:         "user-1",
	}
}

func createGitCapabilityItem(t *testing.T, db *gorm.DB, item models.CapabilityItem) {
	t.Helper()
	if err := db.Exec(`INSERT INTO capability_items (
			id, registry_id, repo_id, slug, item_type, name, description, category, version,
			content, metadata, source_repo_url, source_repo_ref, source_repo_path, content_backend,
			 source_git_server_id, source_git_repo_id, source_git_entry_key, source_sha, git_sha, git_sync_status,
			 git_sync_error, status, created_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.RegistryID, item.RepoID, item.Slug, item.ItemType, item.Name, item.Description,
		item.Category, item.Version, item.Content, item.Metadata, item.SourceRepoURL, item.SourceRepoRef,
		item.SourceRepoPath, item.ContentBackend, item.SourceGitServerID, item.SourceGitRepoID, item.SourceGitEntryKey,
		item.SourceSHA, item.GitSHA, item.GitSyncStatus, item.GitSyncError, item.Status, item.CreatedBy,
		time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("create capability item: %v", err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func loadGitCapabilityItem(t *testing.T, db *gorm.DB, id string) models.CapabilityItem {
	t.Helper()
	var item models.CapabilityItem
	if err := db.First(&item, "id = ?", id).Error; err != nil {
		t.Fatalf("load item %s: %v", id, err)
	}
	return item
}

func itemMetadata(t *testing.T, item models.CapabilityItem) map[string]any {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal(item.Metadata, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	return metadata
}

func itemTagSlugs(t *testing.T, db *gorm.DB, itemID string) []string {
	t.Helper()
	tags, err := (&TagService{DB: db}).GetItemTags([]string{itemID})
	if err != nil {
		t.Fatalf("load tags: %v", err)
	}
	result := make([]string, 0, len(tags[itemID]))
	for _, tag := range tags[itemID] {
		result = append(result, tag.Slug)
	}
	return result
}

func seedItemTag(t *testing.T, db *gorm.DB, itemID, tagID, slug string) {
	t.Helper()
	if err := db.Create(&models.ItemTagDict{ID: tagID, Slug: slug, TagClass: TagClassCustom, CreatedBy: "user-1"}).Error; err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := db.Create(&models.ItemTag{ID: "link-" + tagID, ItemID: itemID, TagID: tagID}).Error; err != nil {
		t.Fatalf("link tag: %v", err)
	}
}

func TestGitCapabilitySyncService_OnlyUpdatesGitBackedRows(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	gitItem := newGitCapabilityItem("git-item", "repo-git", "skill", "skill", "SKILL.md")
	dbItem := newGitCapabilityItem("db-item", "repo-db", "db-skill", "skill", "DB.md")
	dbItem.ContentBackend = "db"
	dbItem.SourceGitServerID = ""
	dbItem.SourceGitRepoID = 0
	dbItem.Name = "db-backed name"
	dbItem.Metadata = datatypes.JSON([]byte(`{"db":"unchanged"}`))
	createGitCapabilityItem(t, db, gitItem)
	createGitCapabilityItem(t, db, dbItem)

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: New Git Skill\ndescription: New description\ncategory: tooling\nversion: 2.0.0\ntags: [git]\n---\nGit-backed content"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-git", "lease-git")
	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Updated != 1 || result.CommitSHA != gitCapabilityTestSHA {
		t.Fatalf("unexpected result: %+v", result)
	}

	updated := loadGitCapabilityItem(t, db, gitItem.ID)
	if updated.Name != "New Git Skill" || updated.GitSHA != gitCapabilityTestSHA || updated.GitSyncStatus != gitCapabilitySyncSynced {
		t.Errorf("Git-backed item was not indexed: %+v", updated)
	}
	unchanged := loadGitCapabilityItem(t, db, dbItem.ID)
	if unchanged.Name != "db-backed name" || string(unchanged.Metadata) != `{"db":"unchanged"}` || unchanged.GitSHA != dbItem.GitSHA {
		t.Errorf("DB-backed item changed: %+v", unchanged)
	}
}

func TestGitCapabilitySyncService_UsesCurrentDefaultBranchHEADForOutOfOrderJobs(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-current-head", "repo-current-head", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	currentHead := "cccccccccccccccccccccccccccccccccccccccc"
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Current HEAD\ndescription: latest\n---\nbody"),
	})
	reader.branch.CommitSHA = currentHead
	svc, cfg := newGitCapabilitySyncService(db, reader)
	older := createGitCapabilityLease(t, db, "job-older", "lease-older")
	newer := createGitCapabilityLease(t, db, "job-newer", "lease-newer")

	for _, lease := range []GitCapabilitySyncLease{newer, older} {
		if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "stale-default", false, lease); err != nil {
			t.Fatalf("sync %s: %v", lease.JobID, err)
		}
	}
	updated := loadGitCapabilityItem(t, db, item.ID)
	if updated.GitSHA != currentHead || updated.SourceSHA != currentHead || updated.SourceRepoRef != "main" {
		t.Errorf("out-of-order delivery wrote non-current state: sha=%q sourceSHA=%q ref=%q", updated.GitSHA, updated.SourceSHA, updated.SourceRepoRef)
	}
	if !reflect.DeepEqual(reader.refs, []string{currentHead, currentHead}) {
		t.Errorf("file reads = %v, want current default branch HEAD twice", reader.refs)
	}
}

func TestGitCapabilitySyncService_PreservesPluginInstallMetadata(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("plugin-item", "repo-plugin", "demo", "plugin", "plugins/demo/.plugin.json")
	item.Metadata = datatypes.JSON([]byte(`{"existing":{"keep":true},"install":{"old":"value"}}`))
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{
		"plugins/demo/.plugin.json": []byte(`{
  "name": "Demo Plugin",
  "version": "2.4.0",
  "description": "A plugin",
  "tags": ["plugin"],
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "demo",
    "marketplace_name": "costrict-plugins",
    "marketplace_repo": "costrict/demo",
    "marketplace_verified": true
  }
}`),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-plugin", "lease-plugin")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := loadGitCapabilityItem(t, db, item.ID)
	if updated.Version != "2.4.0" {
		t.Fatalf("plugin version = %q, want 2.4.0 from .plugin.json", updated.Version)
	}
	metadata := itemMetadata(t, updated)
	install, ok := metadata["install"].(map[string]any)
	if !ok || install["plugin_name"] != "demo" || install["marketplace_repo"] != "costrict/demo" || install["marketplace_verified"] != true {
		t.Errorf("plugin install metadata was lost: %#v", metadata["install"])
	}
	if existing, ok := metadata["existing"].(map[string]any); !ok || existing["keep"] != true {
		t.Errorf("unrelated existing metadata was lost: %#v", metadata)
	}
}

func TestGitCapabilitySyncService_AppliesExplicitEmptyDescriptionAndTags(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-explicit-empty", "repo-explicit-empty", "skill", "skill", "SKILL.md")
	item.Metadata = datatypes.JSON([]byte(`{"tags":["old"],"keep":"value"}`))
	createGitCapabilityItem(t, db, item)
	seedItemTag(t, db, item.ID, "tag-old", "old")
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Explicit Empty\ndescription: \"\"\ntags: []\n---\nThis fallback must not overwrite an explicit empty description."),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-explicit-empty", "lease-explicit-empty")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}
	updated := loadGitCapabilityItem(t, db, item.ID)
	if updated.Description != "" {
		t.Errorf("description = %q, want explicit empty value", updated.Description)
	}
	if tags := itemTagSlugs(t, db, item.ID); len(tags) != 0 {
		t.Errorf("tags = %v, want no tags", tags)
	}
	metadata := itemMetadata(t, updated)
	if tags, ok := metadata["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("metadata.tags = %#v, want explicit empty array", metadata["tags"])
	}
	if metadata["keep"] != "value" {
		t.Errorf("metadata keep field was lost: %#v", metadata)
	}
}

func TestGitCapabilitySyncService_ArchivesMissingManifestAndReactivatesSameRow(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-recover", "repo-recover", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-delete", "lease-delete")); err != nil {
		t.Fatalf("archive sync: %v", err)
	}
	archived := loadGitCapabilityItem(t, db, item.ID)
	if archived.Status != "archived" {
		t.Fatalf("status = %q, want archived", archived.Status)
	}

	reader.files["SKILL.md"] = []byte("---\nname: Restored\ndescription: restored\n---\nbody")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-restore", "lease-restore")); err != nil {
		t.Fatalf("restore sync: %v", err)
	}
	restored := loadGitCapabilityItem(t, db, item.ID)
	if restored.ID != item.ID || restored.Status != "active" || restored.Name != "Restored" {
		t.Errorf("manifest restoration did not reuse archived row: %+v", restored)
	}
}

func TestGitCapabilitySyncService_ReconcilesAddedManifestAndRestoresStableIdentity(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	existing := newGitCapabilityItem("item-existing-skill", "repo-reconcile", "capabilities", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, existing)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md":           []byte("---\nname: Existing Skill\n---\nbody"),
		"commands/review.md": []byte("---\nname: Review Command\ndescription: Review changes\nversion: 2.0.0\n---\nreview"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)

	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-add-manifest", "lease-add-manifest"))
	if err != nil {
		t.Fatalf("add manifest sync: %v", err)
	}
	if result.Created != 1 || result.Updated != 1 || result.Archived != 0 {
		t.Fatalf("add manifest result = %+v, want one create and one update", result)
	}
	var created models.CapabilityItem
	if err := db.Where("source_git_server_id = ? AND source_git_repo_id = ? AND source_repo_path = ? AND source_git_entry_key = ''",
		gitCapabilityTestServerID, gitCapabilityTestRepoID, "commands/review.md").First(&created).Error; err != nil {
		t.Fatalf("load created command: %v", err)
	}
	if created.ItemType != "command" || created.Name != "Review Command" || created.Version != "2.0.0" || created.Status != "active" {
		t.Fatalf("created command projection = %+v", created)
	}
	var versionCount int64
	if err := db.Model(&models.CapabilityVersion{}).Where("item_id = ?", created.ID).Count(&versionCount).Error; err != nil {
		t.Fatalf("count initial versions: %v", err)
	}
	if versionCount != 1 {
		t.Fatalf("initial version count = %d, want 1", versionCount)
	}

	delete(reader.files, "commands/review.md")
	result, err = svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-remove-manifest", "lease-remove-manifest"))
	if err != nil {
		t.Fatalf("remove manifest sync: %v", err)
	}
	if result.Created != 0 || result.Archived != 1 {
		t.Fatalf("remove manifest result = %+v, want one archive", result)
	}
	if archived := loadGitCapabilityItem(t, db, created.ID); archived.Status != "archived" {
		t.Fatalf("removed command status = %q, want archived", archived.Status)
	}

	reader.files["commands/review.md"] = []byte("---\nname: Restored Review\nversion: 2.1.0\n---\nreview")
	result, err = svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-restore-manifest", "lease-restore-manifest"))
	if err != nil {
		t.Fatalf("restore manifest sync: %v", err)
	}
	if result.Created != 0 || result.Updated != 2 || result.Archived != 0 {
		t.Fatalf("restore manifest result = %+v, want two updates and no create", result)
	}
	restored := loadGitCapabilityItem(t, db, created.ID)
	if restored.ID != created.ID || restored.Status != "active" || restored.Name != "Restored Review" || restored.Version != "2.1.0" {
		t.Fatalf("restored command did not retain stable identity: %+v", restored)
	}
}

func TestGitCapabilitySyncService_ReconcilesNewMCPManifestEntry(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	existing := newGitCapabilityItem("mcp-entry-a", "repo-mcp-reconcile", "mcp-a", "mcp", ".mcp.json")
	existing.SourceGitEntryKey = "a"
	createGitCapabilityItem(t, db, existing)
	reader := newGitCapabilityReader(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"a":{"name":"MCP A","command":"a"},"b":{"name":"MCP B","command":"b"}}}`),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)

	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-add-mcp-entry", "lease-add-mcp-entry"))
	if err != nil {
		t.Fatalf("add MCP entry sync: %v", err)
	}
	if result.Created != 1 || result.Updated != 1 {
		t.Fatalf("MCP reconciliation result = %+v, want one create and one update", result)
	}
	var added models.CapabilityItem
	if err := db.Where("source_git_server_id = ? AND source_git_repo_id = ? AND source_repo_path = ? AND source_git_entry_key = ?",
		gitCapabilityTestServerID, gitCapabilityTestRepoID, ".mcp.json", "b").First(&added).Error; err != nil {
		t.Fatalf("load added MCP entry: %v", err)
	}
	if added.ItemType != "mcp" || added.Name != "MCP B" || added.ID == existing.ID {
		t.Fatalf("added MCP entry projection = %+v", added)
	}
}

func TestGitCapabilitySyncService_ReadOrParseFailureDoesNotPartiallyProject(t *testing.T) {
	for _, test := range []struct {
		name    string
		readErr error
		badFile []byte
	}{
		{name: "read failure", readErr: errors.New("Gitea unavailable")},
		{name: "parse failure", badFile: []byte("---\nname: [\n---\ninvalid")},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			first := newGitCapabilityItem("item-first", "repo-first", "first", "skill", "first/SKILL.md")
			second := newGitCapabilityItem("item-second", "repo-second", "second", "skill", "second/SKILL.md")
			first.Metadata = datatypes.JSON([]byte(`{"before":"first"}`))
			second.Metadata = datatypes.JSON([]byte(`{"before":"second"}`))
			createGitCapabilityItem(t, db, first)
			createGitCapabilityItem(t, db, second)
			seedItemTag(t, db, first.ID, "tag-first", "first-tag")
			seedItemTag(t, db, second.ID, "tag-second", "second-tag")
			reader := newGitCapabilityReader(map[string][]byte{
				"first/SKILL.md":  []byte("---\nname: New First\ndescription: changed\ntags: [new-tag]\n---\nbody"),
				"second/SKILL.md": test.badFile,
			})
			if test.readErr != nil {
				reader.readErrs = map[string]error{"second/SKILL.md": test.readErr}
			}
			svc, cfg := newGitCapabilitySyncService(db, reader)
			_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-failure", "lease-failure"))
			if err == nil {
				t.Fatal("sync succeeded, want failure")
			}
			for _, before := range []models.CapabilityItem{first, second} {
				after := loadGitCapabilityItem(t, db, before.ID)
				if after.Name != before.Name || after.Description != before.Description || after.Content != before.Content || string(after.Metadata) != string(before.Metadata) || after.SourceSHA != before.SourceSHA {
					t.Errorf("%s was partially projected after %s: before=%+v after=%+v", before.ID, test.name, before, after)
				}
				if after.GitSyncStatus != gitCapabilitySyncError {
					t.Errorf("%s sync status = %q, want error", before.ID, after.GitSyncStatus)
				}
			}
			if tags := itemTagSlugs(t, db, first.ID); !reflect.DeepEqual(tags, []string{"first-tag"}) {
				t.Errorf("first tags changed after %s: %v", test.name, tags)
			}
			if tags := itemTagSlugs(t, db, second.ID); !reflect.DeepEqual(tags, []string{"second-tag"}) {
				t.Errorf("second tags changed after %s: %v", test.name, tags)
			}
		})
	}
}

func TestGitCapabilitySyncService_RejectsLostLeaseWithoutWritingIndex(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-lost-lease", "repo-lost-lease", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Should Not Persist\ndescription: stale worker\n---\nbody"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-lost-lease", "old-token")
	if err := db.Model(&models.GitCapabilitySyncJob{}).Where("id = ?", lease.JobID).Updates(map[string]any{
		"status":      models.GitCapabilitySyncJobStatusPending,
		"lease_token": "new-token",
	}).Error; err != nil {
		t.Fatalf("reclaim lease: %v", err)
	}
	_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease)
	if !errors.Is(err, ErrGitCapabilityLeaseLost) {
		t.Fatalf("sync error = %v, want lease lost", err)
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Name != item.Name || after.GitSHA != item.GitSHA || after.GitSyncStatus != item.GitSyncStatus || after.GitSyncError != item.GitSyncError {
		t.Errorf("lost lease wrote index state: before=%+v after=%+v", item, after)
	}
}

func TestGitCapabilitySyncService_LostLeaseDuringReadDoesNotMarkNewLeaseFailed(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-late-read", "repo-late-read", "skill", "skill", "SKILL.md")
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.readErrs = map[string]error{"SKILL.md": errors.New("Gitea timed out")}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-late-read", "old-token")
	reader.onRead = func(string) {
		if err := db.Model(&models.GitCapabilitySyncJob{}).Where("id = ?", lease.JobID).Updates(map[string]any{
			"lease_token": "new-token",
		}).Error; err != nil {
			t.Fatalf("reclaim lease during read: %v", err)
		}
	}

	_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease)
	if err == nil {
		t.Fatal("sync succeeded, want read failure")
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	if after.GitSyncStatus != gitCapabilitySyncSynced || after.GitSyncError != "" {
		t.Errorf("stale worker overwrote newer projection status: %+v", after)
	}
}

func TestGitCapabilitySyncService_PreservesBannedStatus(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string][]byte
	}{
		{
			name:  "manifest exists",
			files: map[string][]byte{"SKILL.md": []byte("---\nname: Updated banned skill\n---\nbody")},
		},
		{name: "manifest removed", files: map[string][]byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			item := newGitCapabilityItem("item-banned", "repo-banned", "skill", "skill", "SKILL.md")
			item.Status = "banned"
			createGitCapabilityItem(t, db, item)
			svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(test.files))
			if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-banned", "lease-banned")); err != nil {
				t.Fatalf("sync: %v", err)
			}
			after := loadGitCapabilityItem(t, db, item.ID)
			if after.Status != "banned" {
				t.Errorf("banned item status = %q, want banned", after.Status)
			}
		})
	}
}

func TestGitCapabilitySyncService_ParsesStandaloneGitClonePluginManifest(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("plugin-git-clone", "repo-git-clone", "clone-plugin", "plugin", ".plugin.json")
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{
		".plugin.json": []byte(`{
  "name": "Clone Plugin",
  "description": "installed from Git",
  "install": {
    "method": "git-clone",
    "source": "https://example.test/acme/clone-plugin.git",
    "subpath": "plugin"
  }
}`),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-git-clone", "lease-git-clone")); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	metadata := itemMetadata(t, after)
	install, ok := metadata["install"].(map[string]any)
	if !ok || install["method"] != "git-clone" || install["source"] != "https://example.test/acme/clone-plugin.git" || install["subpath"] != "plugin" {
		t.Fatalf("standalone git-clone metadata was not preserved: %#v", metadata["install"])
	}
}

func TestGitCapabilitySyncService_ArchivesMCPEntryWhenItsKeyIsRenamed(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("mcp-entry-a", "repo-mcp-a", "mcp-mcp-a", "mcp", ".mcp.json")
	item.SourceGitEntryKey = "mcp-a"
	item.Name = "Old MCP A"
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"mcp-b":{"name":"New MCP B","command":"serve"}}}`),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-mcp-rename", "lease-mcp-rename")); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != "archived" || after.Name != "Old MCP A" {
		t.Errorf("renamed MCP key must archive old identity without adopting B: %+v", after)
	}
}

func TestGitCapabilitySyncService_MCPChildKeyCannotOverrideEntryIdentity(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("mcp-entry-stable", "repo-mcp-stable", "mcp-stable", "mcp", ".mcp.json")
	item.SourceGitEntryKey = "stable"
	item.Name = "Old name"
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"stable":{"key":"spoofed","name":"Updated stable MCP","command":"serve"}}}`),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-mcp-conflicting-key", "lease-mcp-conflicting-key"))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Updated != 1 || result.Archived != 0 {
		t.Fatalf("result = %+v, want one update and no archive", result)
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != "active" || after.Name != "Updated stable MCP" {
		t.Fatalf("entry with conflicting child key lost its outer identity: %+v", after)
	}
	if got := itemMetadata(t, after)["key"]; got != "stable" {
		t.Fatalf("metadata key = %#v, want stable", got)
	}
}

func TestGitCapabilitySyncService_SyncsDistinctMCPEntriesFromSameManifest(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	first := newGitCapabilityItem("mcp-entry-a", "repo-mcp-a", "mcp-mcp-a", "mcp", ".mcp.json")
	first.SourceGitEntryKey = "mcp-a"
	second := newGitCapabilityItem("mcp-entry-b", "repo-mcp-b", "mcp-mcp-b", "mcp", ".mcp.json")
	second.SourceGitEntryKey = "mcp-b"
	createGitCapabilityItem(t, db, first)
	createGitCapabilityItem(t, db, second)
	reader := newGitCapabilityReader(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"mcp-a":{"name":"MCP A","command":"serve-a"},"mcp-b":{"name":"MCP B","command":"serve-b"}}}`),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-mcp-many", "lease-mcp-many"))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Updated != 2 {
		t.Fatalf("updated = %d, want 2", result.Updated)
	}
	for _, expected := range []struct {
		id, name, key string
	}{
		{id: first.ID, name: "MCP A", key: "mcp-a"},
		{id: second.ID, name: "MCP B", key: "mcp-b"},
	} {
		item := loadGitCapabilityItem(t, db, expected.id)
		if item.Name != expected.name || itemMetadata(t, item)["key"] != expected.key {
			t.Errorf("entry %s projected wrong manifest child: %+v", expected.id, item)
		}
	}
}

func TestGitCapabilitySyncService_DefaultBranchDeletionArchivesOnlyAfterAuthoritativeAbsence(t *testing.T) {
	for _, test := range []struct {
		name          string
		defaultBranch string
		branch        *gitsync.Branch
		branchErr     error
		wantArchived  bool
	}{
		{name: "default branch is empty", defaultBranch: "", wantArchived: true},
		{name: "current default branch returns 404", defaultBranch: "main", branch: nil, wantArchived: true},
		{name: "current default branch has HEAD", defaultBranch: "main", branch: &gitsync.Branch{Name: "main", CommitSHA: gitCapabilityTestSHA}, wantArchived: false},
		{name: "branch lookup fails", defaultBranch: "main", branchErr: errors.New("Gitea unavailable"), wantArchived: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			item := newGitCapabilityItem("item-default-delete", "repo-default-delete", "skill", "skill", "SKILL.md")
			createGitCapabilityItem(t, db, item)
			legacy := newGitCapabilityItem("item-default-delete-legacy", "repo-default-delete-legacy", "legacy", "skill", "")
			createGitCapabilityItem(t, db, legacy)
			reader := newGitCapabilityReader(map[string][]byte{
				"SKILL.md": []byte("---\nname: Current default branch\n---\nbody"),
			})
			reader.repo.DefaultBranch = test.defaultBranch
			reader.branch = test.branch
			reader.branchErr = test.branchErr
			svc, cfg := newGitCapabilitySyncService(db, reader)
			_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "stale-owner/stale-name", "main", true, createGitCapabilityLease(t, db, "job-default-delete", "lease-default-delete"))
			if test.wantArchived {
				if err != nil {
					t.Fatalf("archive deleted default branch: %v", err)
				}
				for _, id := range []string{item.ID, legacy.ID} {
					if after := loadGitCapabilityItem(t, db, id); after.Status != "archived" {
						t.Errorf("item %s status = %q, want archived", id, after.Status)
					}
				}
				return
			}
			if test.branchErr != nil {
				if err == nil {
					t.Fatal("sync succeeded after branch lookup error")
				}
				if after := loadGitCapabilityItem(t, db, item.ID); after.Status != "active" {
					t.Errorf("branch lookup error must retry, status = %q", after.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("current default branch should sync instead of archive: %v", err)
			}
			if after := loadGitCapabilityItem(t, db, item.ID); after.Status != "active" || after.Name != "Current default branch" {
				t.Errorf("current HEAD should win over deletion delivery: %+v", after)
			}
		})
	}
}

func TestGitCapabilitySyncService_MissingDefaultBranchOnNonDeletionRetriesInsteadOfArchiving(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-missing-normal", "repo-missing-normal", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.branch = nil
	svc, cfg := newGitCapabilitySyncService(db, reader)
	_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, createGitCapabilityLease(t, db, "job-missing-normal", "lease-missing-normal"))
	if err == nil {
		t.Fatal("sync succeeded, want missing default-branch retry")
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != "active" || after.GitSyncStatus != gitCapabilitySyncError {
		t.Errorf("non-deletion missing branch must retain active index and mark retry error: %+v", after)
	}
}

func seedTenantGitServer(t *testing.T, db *gorm.DB, tenantID string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, ?, ?)`,
		tenantID, gitCapabilityTestServerID, time.Now(), time.Now(),
	).Error; err != nil {
		t.Fatalf("seed tenant git server binding: %v", err)
	}
}

func seedUserGitBinding(t *testing.T, db *gorm.DB, subjectID, tenantID string, gitUID int64, login string) {
	t.Helper()
	seedTenantGitServer(t, db, tenantID)
	if err := db.Exec(
		`INSERT INTO user_git_binding (user_subject_id, tenant_id, git_uid, git_username, sync_status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		subjectID, tenantID, gitUID, login, models.GitSyncStatusSynced, time.Now(), time.Now(),
	).Error; err != nil {
		t.Fatalf("seed user git binding: %v", err)
	}
}

// Repositories bound before the discovery pipeline existed (the fork and
// webhook rollouts) have no repository-level record, so the owner projection
// is skipped for them entirely. Resolving the owner up front made those rows
// inherit a failure they can never consume: an ambiguous Git identity would
// abort the whole repository sync on every push, forever.
func TestGitCapabilitySyncService_PreBoundRepoSyncsDespiteAmbiguousOwner(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-prebound", "repo-prebound", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	// Two synced bindings claim the same Git identity, which makes owner
	// resolution ambiguous and therefore an error.
	seedUserGitBinding(t, db, "user-a", "tenant-a", 1001, "alice")
	seedUserGitBinding(t, db, "user-b", "tenant-b", 1001, "alice")

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Prebound Skill\ndescription: synced\n---\nbody"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-prebound", "lease-prebound")

	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease)
	if err != nil {
		t.Fatalf("sync of a pre-bound repository failed on an owner it never uses: %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	synced := loadGitCapabilityItem(t, db, item.ID)
	if synced.Name != "Prebound Skill" || synced.GitSyncStatus != gitCapabilitySyncSynced {
		t.Errorf("item was not indexed: %+v", synced)
	}
}

// Behaviour guard for the other side of the same change: a repository that does
// have a repository-level record must still get its owner projected.
func TestGitCapabilitySyncService_BoundRepositoryStillProjectsOwner(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-bound", "repo-bound", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	seedUserGitBinding(t, db, "user-alice", "tenant-a", 1001, "alice")

	now := time.Now()
	if err := db.Exec(
		`INSERT INTO repositories (id, name, display_name, visibility, repo_type, owner_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"repo-bound", "gitea-101", "alice/capabilities", "public", "sync", "system", now, now,
	).Error; err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO capability_registries (id, name, source_type, external_url, external_branch, sync_enabled, sync_interval, sync_status, owner_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"registry-1", "alice/capabilities", "git", "https://git.example/alice/capabilities", "main", true, 3600, "idle", "system", now, now,
	).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO git_capability_repositories (
			id, git_server_id, git_repo_id, repository_id, registry_id, full_name, repo_kind,
			identification_status, visibility, git_remote_url, default_branch, last_synced_commit,
			last_error, created_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"binding-1", gitCapabilityTestServerID, gitCapabilityTestRepoID, "repo-bound", "registry-1",
		"alice/capabilities", "standalone", models.GitCapabilityIdentificationClean, "public",
		"https://git.example/alice/capabilities", "main", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"", "system", now, now,
	).Error; err != nil {
		t.Fatalf("seed git capability repository: %v", err)
	}

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Bound Skill\ndescription: synced\n---\nbody"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-bound", "lease-bound")

	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var ownerID string
	if err := db.Table("repositories").Where("id = ?", "repo-bound").Select("owner_id").Scan(&ownerID).Error; err != nil {
		t.Fatalf("read repository owner: %v", err)
	}
	if ownerID != "user-alice" {
		t.Errorf("repository owner = %q, want user-alice (owner projection regressed)", ownerID)
	}

	// The Git pipeline owns this registry; the legacy clone scheduler must not
	// be able to adopt it, so every sync converges sync_enabled to false.
	var syncEnabled bool
	if err := db.Table("capability_registries").Where("id = ?", "registry-1").Select("sync_enabled").Scan(&syncEnabled).Error; err != nil {
		t.Fatalf("read sync_enabled: %v", err)
	}
	if syncEnabled {
		t.Error("Git-backed registry still advertises sync_enabled = true to the legacy scheduler")
	}
}

func TestGitCapabilityOwnerResolver_ResolvesAtMostOnce(t *testing.T) {
	calls := 0
	resolver := &gitCapabilityOwnerResolver{resolve: func() (string, error) {
		calls++
		return "user-a", nil
	}}
	for i := 0; i < 3; i++ {
		owner, err := resolver.OwnerID()
		if err != nil || owner != "user-a" {
			t.Fatalf("OwnerID() = %q, %v", owner, err)
		}
	}
	if calls != 1 {
		t.Errorf("resolver ran %d times, want 1", calls)
	}

	failCalls := 0
	failing := &gitCapabilityOwnerResolver{resolve: func() (string, error) {
		failCalls++
		return "", errors.New("ambiguous")
	}}
	for i := 0; i < 3; i++ {
		if _, err := failing.OwnerID(); err == nil {
			t.Fatal("expected the memoized error")
		}
	}
	if failCalls != 1 {
		t.Errorf("failing resolver ran %d times, want 1 (errors must be memoized too)", failCalls)
	}
}
