package services

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

func seedGitDiscoveryOwner(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	if err := db.Exec(`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, ?, ?)`,
		"tenant-1", gitCapabilityTestServerID, now, now).Error; err != nil {
		t.Fatalf("seed tenant Git server binding: %v", err)
	}
	if err := db.Exec(`INSERT INTO user_git_binding (
		user_subject_id, tenant_id, git_uid, git_username, provider_kind, sync_status, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, "user-alice", "tenant-1", 1001, "alice", "gitea", models.GitSyncStatusSynced, now, now).Error; err != nil {
		t.Fatalf("seed user Git binding: %v", err)
	}
}

func TestGitCapabilityDiscovery_RepairsOwnerProjectionAfterBindingAppears(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Ownership Recovery\n---\n\nBody"),
	})
	reader.tree = []gitsync.GitTreeEntry{{Path: "SKILL.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	if _, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-owner-system", "lease-owner-system")); err != nil {
		t.Fatalf("discover before user binding: %v", err)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load discovery binding: %v", err)
	}
	if binding.CreatedBy != gitCapabilityDiscoverySystemOwner {
		t.Fatalf("initial created_by = %q, want system", binding.CreatedBy)
	}

	seedGitDiscoveryOwner(t, db)
	if _, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-owner-user", "lease-owner-user")); err != nil {
		t.Fatalf("sync after user binding: %v", err)
	}

	var platformRepo models.Repository
	if err := db.First(&platformRepo, "id = ?", binding.RepositoryID).Error; err != nil {
		t.Fatalf("load repository projection: %v", err)
	}
	if platformRepo.OwnerID != "user-alice" {
		t.Fatalf("repository owner = %q, want user-alice", platformRepo.OwnerID)
	}
	var registry models.CapabilityRegistry
	if err := db.First(&registry, "id = ?", binding.RegistryID).Error; err != nil {
		t.Fatalf("load registry projection: %v", err)
	}
	if registry.OwnerID != "user-alice" {
		t.Fatalf("registry owner = %q, want user-alice", registry.OwnerID)
	}
	var members []models.RepoMember
	if err := db.Where("repo_id = ? AND role = ?", binding.RepositoryID, "owner").Find(&members).Error; err != nil {
		t.Fatalf("load owner membership: %v", err)
	}
	if len(members) != 1 || members[0].UserID != "user-alice" || members[0].Username != "alice" {
		t.Fatalf("owner memberships = %+v, want user-alice", members)
	}
	if err := db.First(&binding, "id = ?", binding.ID).Error; err != nil {
		t.Fatalf("reload discovery binding: %v", err)
	}
	if binding.CreatedBy != gitCapabilityDiscoverySystemOwner {
		t.Fatalf("created_by changed to %q; creation identity must remain immutable", binding.CreatedBy)
	}
}

func TestGitCapabilityDiscovery_CreatesCompoundRepositoryAndLocksTypes(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md":           []byte("---\nslug: repo-skill\nname: Repo Skill\ndescription: Root skill\nversion: 1.2.0\n---\n\nBody"),
		"commands/review.md": []byte("---\nname: Review Command\ndescription: Review code\n---\n\nRun review"),
	})
	reader.repo.Private = true
	reader.tree = []gitsync.GitTreeEntry{
		{Path: "README.md", Type: "blob"},
		{Path: "SKILL.md", Type: "blob"},
		{Path: "commands/review.md", Type: "blob"},
	}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "discover-compound", "lease-compound")

	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, "stale/name", "main", false, lease)
	if err != nil {
		t.Fatalf("SyncRepository discovery: %v", err)
	}
	if result.Created != 2 || result.Updated != 0 || result.CommitSHA != gitCapabilityTestSHA {
		t.Fatalf("unexpected result: %+v", result)
	}

	var binding models.GitCapabilityRepository
	if err := db.First(&binding, "git_server_id = ? AND git_repo_id = ?", gitCapabilityTestServerID, gitCapabilityTestRepoID).Error; err != nil {
		t.Fatalf("load discovery binding: %v", err)
	}
	if binding.RepoKind != "standalone" || binding.IdentificationStatus != models.GitCapabilityIdentificationClean || binding.Visibility != "private" {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	if binding.CreatedBy != "user-alice" {
		t.Fatalf("created_by = %q, want user-alice", binding.CreatedBy)
	}

	var items []models.CapabilityItem
	if err := db.Order("item_type ASC").Find(&items).Error; err != nil {
		t.Fatalf("load discovered items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("item count = %d, want 2", len(items))
	}
	types := []string{items[0].ItemType, items[1].ItemType}
	sort.Strings(types)
	if strings.Join(types, ",") != "command,skill" {
		t.Fatalf("types = %v", types)
	}
	for _, item := range items {
		if item.ContentBackend != "git" || item.SourceGitRepoID != gitCapabilityTestRepoID || item.GitSyncStatus != gitCapabilitySyncSynced {
			t.Fatalf("item was not fully Git-bound: %+v", item)
		}
	}

	// A later manifest attempts to change the existing skill type. Bound rows
	// must preserve their locked item types during set reconciliation.
	reader.tree = []gitsync.GitTreeEntry{
		{Path: "SKILL.md", Type: "blob"},
		{Path: "commands/review.md", Type: "blob"},
	}
	reader.files["SKILL.md"] = []byte("---\nname: Still Skill\ntype: plugin\n---\n\nChanged")
	secondLease := createGitCapabilityLease(t, db, "discover-compound-2", "lease-compound-2")
	second, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false, secondLease)
	if err != nil {
		t.Fatalf("SyncRepository locked refresh: %v", err)
	}
	if second.Created != 0 || second.Updated != 2 {
		t.Fatalf("unexpected second result: %+v", second)
	}
	items = nil
	if err := db.Order("item_type ASC").Find(&items).Error; err != nil {
		t.Fatalf("reload locked items: %v", err)
	}
	if len(items) != 2 || items[0].ItemType != "command" || items[1].ItemType != "skill" {
		t.Fatalf("locked types changed: %+v", items)
	}
}

func TestGitCapabilityDiscovery_SkipsExcludedUnboundOwner(t *testing.T) {
	t.Setenv("PLUGIN_GIT_MIRROR_OWNER", "mirror-owner")
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Duplicate\n---\nbody"),
	})
	reader.repo.FullName = "mirror-owner/plugin-one"
	reader.tree = []gitsync.GitTreeEntry{{Path: "SKILL.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-excluded", "lease-excluded"))
	if err != nil {
		t.Fatalf("excluded discovery: %v", err)
	}
	if result.Created != 0 || result.Updated != 0 || result.CommitSHA != gitCapabilityTestSHA {
		t.Fatalf("unexpected excluded result: %+v", result)
	}
	var itemCount int64
	if err := db.Model(&models.CapabilityItem{}).Count(&itemCount).Error; err != nil {
		t.Fatalf("count items: %v", err)
	}
	if itemCount != 0 {
		t.Fatalf("excluded owner created %d capability items", itemCount)
	}
}

func TestGitCapabilityDiscovery_CreatesPluginPack(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"plugins/alpha/.plugin.json": []byte(discoveryPluginJSON("Alpha", "alpha")),
		"plugins/beta/.plugin.json":  []byte(discoveryPluginJSON("Beta", "beta")),
	})
	reader.tree = []gitsync.GitTreeEntry{
		{Path: "plugins/alpha/.plugin.json", Type: "blob"},
		{Path: "plugins/alpha/skills/internal/SKILL.md", Type: "blob"},
		{Path: "plugins/beta/.plugin.json", Type: "blob"},
	}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-pack", "lease-pack"))
	if err != nil {
		t.Fatalf("discover pack: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("created = %d, want 2", result.Created)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load binding: %v", err)
	}
	if binding.RepoKind != "pack" || binding.IdentificationStatus != models.GitCapabilityIdentificationClean {
		t.Fatalf("unexpected pack binding: %+v", binding)
	}
	var items []models.CapabilityItem
	if err := db.Order("slug ASC").Find(&items).Error; err != nil {
		t.Fatalf("load pack items: %v", err)
	}
	if len(items) != 2 || items[0].Slug != "alpha" || items[1].Slug != "beta" {
		t.Fatalf("plugin internals must not be indexed: %+v", items)
	}
}

func TestGitCapabilityDiscovery_ExpandsMCPEntries(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"github":{"command":"gh"},"postgres":{"command":"psql"}}}`),
	})
	reader.tree = []gitsync.GitTreeEntry{{Path: ".mcp.json", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-mcp", "lease-mcp"))
	if err != nil {
		t.Fatalf("discover MCP: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("created = %d, want 2", result.Created)
	}
	var items []models.CapabilityItem
	if err := db.Order("source_git_entry_key ASC").Find(&items).Error; err != nil {
		t.Fatalf("load MCP items: %v", err)
	}
	if len(items) != 2 || items[0].SourceGitEntryKey != "github" || items[1].SourceGitEntryKey != "postgres" {
		t.Fatalf("unexpected MCP identities: %+v", items)
	}
}

func TestGitCapabilityDiscovery_RecordsUnknownRepository(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.tree = []gitsync.GitTreeEntry{{Path: "README.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-unknown", "lease-unknown"))
	if err != nil {
		t.Fatalf("discover unknown: %v", err)
	}
	if result.Created != 0 || result.Skipped == 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load unknown binding: %v", err)
	}
	if binding.IdentificationStatus != models.GitCapabilityIdentificationUnknown || !strings.Contains(binding.LastError, "no supported") {
		t.Fatalf("unexpected unknown binding: %+v", binding)
	}
	var count int64
	if err := db.Model(&models.CapabilityItem{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unknown repo item count=%d err=%v", count, err)
	}
}

func TestGitCapabilityDiscovery_UnknownRepositoryCanBeIdentifiedByLaterPush(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.tree = []gitsync.GitTreeEntry{{Path: "README.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	first, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-later-unknown", "lease-later-unknown"))
	if err != nil {
		t.Fatalf("record unknown repository: %v", err)
	}
	if first.Created != 0 {
		t.Fatalf("first discovery created = %d, want 0", first.Created)
	}

	var original models.GitCapabilityRepository
	if err := db.First(&original).Error; err != nil {
		t.Fatalf("load original binding: %v", err)
	}
	seedGitDiscoveryOwner(t, db)
	reader.tree = []gitsync.GitTreeEntry{{Path: "SKILL.md", Type: "blob"}}
	reader.files["SKILL.md"] = []byte("---\nname: Later Skill\ndescription: Added after the first push\n---\n\nBody")
	second, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-later-clean", "lease-later-clean"))
	if err != nil {
		t.Fatalf("identify repository on later push: %v", err)
	}
	if second.Created != 1 {
		t.Fatalf("second discovery created = %d, want 1", second.Created)
	}

	var current models.GitCapabilityRepository
	if err := db.First(&current).Error; err != nil {
		t.Fatalf("reload binding: %v", err)
	}
	if current.ID != original.ID || current.RepositoryID != original.RepositoryID || current.RegistryID != original.RegistryID {
		t.Fatalf("later discovery replaced stable binding: original=%+v current=%+v", original, current)
	}
	if current.IdentificationStatus != models.GitCapabilityIdentificationClean || current.LastError != "" {
		t.Fatalf("later discovery did not become clean: %+v", current)
	}
	var platformRepo models.Repository
	if err := db.First(&platformRepo, "id = ?", current.RepositoryID).Error; err != nil {
		t.Fatalf("load recovered repository owner: %v", err)
	}
	var ownerMembers int64
	if err := db.Model(&models.RepoMember{}).
		Where("repo_id = ? AND user_id = ? AND role = ?", current.RepositoryID, "user-alice", "owner").
		Count(&ownerMembers).Error; err != nil {
		t.Fatalf("count recovered owner membership: %v", err)
	}
	if platformRepo.OwnerID != "user-alice" || ownerMembers != 1 {
		t.Fatalf("later discovery did not recover owner projection: repo=%+v owner_members=%d", platformRepo, ownerMembers)
	}
}

func TestGitCapabilityDiscovery_V4OptionalHeuristics(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		content  string
		itemType string
		version  string
	}{
		{
			name: "agent yaml", path: ".agent/agent.yaml", itemType: "subagent",
			content: "name: Release Agent\ndescription: Coordinates releases\nversion: 2.0.0\ntags: [release]\n", version: "2.0.0",
		},
		{
			name: "package name", path: "package.json", itemType: "mcp",
			content: `{"name":"github-mcp","description":"GitHub MCP","version":"1.4.0"}`, version: "1.4.0",
		},
		{
			name: "pep 621 project", path: "pyproject.toml", itemType: "mcp",
			content: "[project]\nname = \"postgres-mcp\"\nversion = \"3.2.1\"\ndescription = \"Postgres MCP\"\n", version: "3.2.1",
		},
		{
			name: "mcp manifest", path: "manifest.json", itemType: "mcp",
			content: `{"name":"manifest-mcp","version":"4.0.0","mcp":{"transport":"stdio"}}`, version: "4.0.0",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			reader := newGitCapabilityReader(map[string][]byte{tc.path: []byte(tc.content)})
			reader.tree = []gitsync.GitTreeEntry{{Path: tc.path, Type: "blob"}}
			svc, cfg := newGitCapabilitySyncService(db, reader)
			result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
				createGitCapabilityLease(t, db, fmt.Sprintf("discover-heuristic-%d", i), fmt.Sprintf("lease-heuristic-%d", i)))
			if err != nil {
				t.Fatalf("discover %s: %v", tc.path, err)
			}
			if result.Created != 1 {
				t.Fatalf("created = %d, want 1", result.Created)
			}
			var item models.CapabilityItem
			if err := db.First(&item).Error; err != nil {
				t.Fatalf("load discovered item: %v", err)
			}
			if item.ItemType != tc.itemType || item.Version != tc.version {
				t.Fatalf("unexpected item: type=%q version=%q", item.ItemType, item.Version)
			}
		})
	}
}

func TestGitCapabilityDiscovery_UnmatchedOptionalCandidateHasDiagnostic(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"package.json": []byte(`{"name":"ordinary-web-app"}`),
	})
	reader.tree = []gitsync.GitTreeEntry{{Path: "package.json", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-unmatched", "lease-unmatched"))
	if err != nil {
		t.Fatalf("discover unmatched package: %v", err)
	}
	if result.Created != 0 || result.Skipped == 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load unknown binding: %v", err)
	}
	if binding.IdentificationStatus != models.GitCapabilityIdentificationUnknown ||
		!strings.Contains(binding.LastError, "did not match") {
		t.Fatalf("unmatched candidate has no diagnostic: %+v", binding)
	}
}

func TestGitCapabilityDiscovery_RetriesTreeTransportFailure(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.treeErr = errors.New("tree unavailable")
	svc, cfg := newGitCapabilitySyncService(db, reader)
	_, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-tree-error", "lease-tree-error"))
	if err == nil || !strings.Contains(err.Error(), "tree unavailable") {
		t.Fatalf("expected retryable tree failure, got %v", err)
	}
	var count int64
	if err := db.Model(&models.GitCapabilityRepository{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("transport failure must not commit binding: count=%d err=%v", count, err)
	}
}

func discoveryPluginJSON(name, pluginName string) string {
	return `{"name":"` + name + `","description":"Discovered plugin","install":{"method":"plugin_marketplace","plugin_name":"` +
		pluginName + `","marketplace_name":"native","marketplace_repo":"owner/marketplace"}}`
}
