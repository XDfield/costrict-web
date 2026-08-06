package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

const (
	marketplaceSHA1 = "1111111111111111111111111111111111111111"
	marketplaceSHA2 = "2222222222222222222222222222222222222222"
)

var marketplaceTestRepoID atomic.Int64

type marketplaceTestResponse struct {
	Name  string `json:"name"`
	Owner struct {
		Name string `json:"name"`
	} `json:"owner"`
	Plugins []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Version     string   `json:"version"`
		Category    string   `json:"category"`
		Tags        []string `json:"tags"`
		Strict      bool     `json:"strict"`
		Source      struct {
			Source string `json:"source"`
			URL    string `json:"url"`
			Path   string `json:"path"`
			Ref    string `json:"ref"`
			SHA    string `json:"sha"`
		} `json:"source"`
	} `json:"plugins"`
}

func newPluginMarketplaceRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/marketplace/:repo/marketplace.json", func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		MarketplaceJSON(c)
	})
	return r
}

func seedMarketplaceRegistry(t *testing.T, repoID, repoName, visibility string) string {
	t.Helper()
	if repoID != "public" {
		if err := database.DB.Create(&models.Repository{
			ID: repoID, Name: repoName, OwnerID: "owner", Visibility: visibility,
		}).Error; err != nil {
			t.Fatalf("seed repository: %v", err)
		}
	}
	registryID := "registry-" + repoID
	if err := database.DB.Create(&models.CapabilityRegistry{
		ID: registryID, Name: repoName, SourceType: "internal", RepoID: repoID, OwnerID: "owner",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	return registryID
}

func seedGitMarketplacePlugin(t *testing.T, item models.CapabilityItem) {
	t.Helper()
	if item.Name == "" {
		item.Name = item.Slug
	}
	if item.ItemType == "" {
		item.ItemType = "plugin"
	}
	if item.Status == "" {
		item.Status = "active"
	}
	if item.Version == "" {
		item.Version = "1.0.0"
	}
	if item.CreatedBy == "" {
		item.CreatedBy = "owner"
	}
	if item.ContentBackend == "" {
		item.ContentBackend = "git"
	}
	if item.GitSyncStatus == "" {
		item.GitSyncStatus = "synced"
	}
	// The shared fake Gitea's server id, not an invented one: since F-20 a
	// publicly-served marketplace re-verifies each entry's repository against
	// its git server before handing out the coordinate, so a row bound to a
	// server that cannot answer is (correctly) a 503 for the whole request.
	// Tests that want that failure call stopFakeGitea explicitly.
	if item.SourceGitServerID == "" {
		item.SourceGitServerID = gitContentTestServerID
	}
	if item.SourceGitRepoID == 0 {
		item.SourceGitRepoID = marketplaceTestRepoID.Add(1)
	}
	if item.SourceRepoRef == "" {
		item.SourceRepoRef = "main"
	}
	if err := database.DB.Create(&item).Error; err != nil {
		t.Fatalf("seed marketplace plugin %s: %v", item.ID, err)
	}
}

func decodeMarketplaceResponse(t *testing.T, repo string) marketplaceTestResponse {
	t.Helper()
	w := get(newPluginMarketplaceRouter(""), "/api/marketplace/"+repo+"/marketplace.json")
	if w.Code != http.StatusOK {
		t.Fatalf("marketplace response: status=%d body=%s", w.Code, w.Body.String())
	}
	var body marketplaceTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode marketplace response: %v", err)
	}
	return body
}

func TestMarketplaceJSON_ProjectsStandaloneAndPackGitSources(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	registryID := seedMarketplaceRegistry(t, "public", "public", "public")

	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "standalone", RegistryID: registryID, RepoID: "public",
		Slug: "catalog-owner-repo-runtime-plugin", Name: "Runtime Plugin",
		Description: "standalone", Category: "development", Version: "1.2.3",
		Metadata: datatypes.JSON([]byte(`{
			"name":"runtime-plugin",
			"tags":["git","tools"],
			"install":{"plugin_name":"runtime-plugin","marketplace_name":"upstream","marketplace_repo":"owner/repo"}
		}`)),
		SourceRepoURL: "https://gitea.example.test/plugins/runtime-plugin.git",
		SourceRepoRef: "release", SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA1,
	})
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "pack", RegistryID: registryID, RepoID: "public",
		Slug: "catalog-pack-tool", Name: "Pack Tool", Version: "2.0.0",
		Metadata: datatypes.JSON([]byte(`{
			"install":{"plugin_name":"pack-tool","marketplace_name":"upstream","marketplace_repo":"owner/pack"}
		}`)),
		SourceRepoURL: "https://gitea.example.test/plugins/pack.git",
		SourceRepoRef: "main", SourceRepoPath: "plugins/pack-tool/.plugin.json", GitSHA: marketplaceSHA2,
	})

	body := decodeMarketplaceResponse(t, "public")
	if body.Name != "public" || body.Owner.Name == "" {
		t.Fatalf("invalid marketplace envelope: %+v", body)
	}
	if len(body.Plugins) != 2 {
		t.Fatalf("plugins=%d, want 2: %+v", len(body.Plugins), body.Plugins)
	}
	plugins := make(map[string]struct {
		source, url, path, ref, sha, version string
		strict                               bool
	})
	for _, plugin := range body.Plugins {
		plugins[plugin.Name] = struct {
			source, url, path, ref, sha, version string
			strict                               bool
		}{plugin.Source.Source, plugin.Source.URL, plugin.Source.Path, plugin.Source.Ref, plugin.Source.SHA, plugin.Version, plugin.Strict}
	}
	standalone := plugins["runtime-plugin"]
	if standalone.source != "url" || standalone.url != "https://gitea.example.test/plugins/runtime-plugin.git" ||
		standalone.path != "" || standalone.ref != "release" || standalone.sha != marketplaceSHA1 ||
		standalone.version != "1.2.3" || !standalone.strict {
		t.Fatalf("unexpected standalone projection: %+v", standalone)
	}
	pack := plugins["pack-tool"]
	if pack.source != "git-subdir" || pack.url != "https://gitea.example.test/plugins/pack.git" ||
		pack.path != "plugins/pack-tool" || pack.ref != "main" || pack.sha != marketplaceSHA2 || !pack.strict {
		t.Fatalf("unexpected pack projection: %+v", pack)
	}
}

func TestMarketplaceJSON_CostrictPluginsAliasesPublicRegistry(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	registryID := seedMarketplaceRegistry(t, "public", "public", "public")
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "cloud-plugin", RegistryID: registryID, RepoID: "public",
		Slug: "cloud-plugin", Version: "1.0.0",
		Metadata:       datatypes.JSON([]byte(`{"install":{"plugin_name":"cloud-plugin"}}`)),
		SourceRepoURL:  "https://gitea.example.test/plugins/cloud-plugin.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA1,
	})
	privateRegistryID := seedMarketplaceRegistry(t, "private-repo", "private-repo", "private")
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "private-plugin", RegistryID: privateRegistryID, RepoID: "private-repo",
		Slug: "private-plugin", Metadata: datatypes.JSON([]byte(`{"name":"private-plugin"}`)),
		SourceRepoURL:  "https://gitea.example.test/private/private-plugin.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA2,
	})

	alias := decodeMarketplaceResponse(t, cscMarketplaceName)
	if alias.Name != cscMarketplaceName {
		t.Fatalf("marketplace name=%q, want %q", alias.Name, cscMarketplaceName)
	}
	if len(alias.Plugins) != 1 || alias.Plugins[0].Name != "cloud-plugin" {
		t.Fatalf("alias should project the public registry: %+v", alias.Plugins)
	}

	public := decodeMarketplaceResponse(t, "public")
	if public.Name != "public" || len(public.Plugins) != 1 || public.Plugins[0].Name != alias.Plugins[0].Name {
		t.Fatalf("public route changed while adding alias: public=%+v alias=%+v", public, alias)
	}
}

func TestMarketplaceJSON_OnlyPublishesInstallableGitRows(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	registryID := seedMarketplaceRegistry(t, "public", "public", "public")
	metadata := datatypes.JSON([]byte(`{"name":"valid-plugin"}`))
	base := models.CapabilityItem{
		RegistryID: registryID, RepoID: "public", Metadata: metadata,
		SourceRepoURL:  "https://gitea.example.test/plugins/valid.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA1,
	}

	valid := base
	valid.ID, valid.Slug = "valid", "valid-plugin"
	seedGitMarketplacePlugin(t, valid)

	dbBacked := base
	dbBacked.ID, dbBacked.Slug, dbBacked.ContentBackend = "db", "db-plugin", "db"
	seedGitMarketplacePlugin(t, dbBacked)

	pending := base
	pending.ID, pending.Slug, pending.GitSyncStatus = "pending", "pending-plugin", "pending"
	seedGitMarketplacePlugin(t, pending)

	badSHA := base
	badSHA.ID, badSHA.Slug, badSHA.GitSHA = "bad-sha", "bad-sha-plugin", "abc"
	seedGitMarketplacePlugin(t, badSHA)

	badPath := base
	badPath.ID, badPath.Slug, badPath.SourceRepoPath = "bad-path", "bad-path-plugin", "../.plugin.json"
	seedGitMarketplacePlugin(t, badPath)

	missingURL := base
	missingURL.ID, missingURL.Slug, missingURL.SourceRepoURL = "missing-url", "missing-url-plugin", ""
	seedGitMarketplacePlugin(t, missingURL)

	unbound := base
	unbound.ID, unbound.Slug = "unbound", "unbound-plugin"
	seedGitMarketplacePlugin(t, unbound)
	// Fixture stands in for the Git sync writer, so it carries that writer's
	// explicit opt-out from the Git-owned field guard.
	if err := database.DB.Set(models.GitSyncBypassSetting, true).
		Model(&models.CapabilityItem{}).Where("id = ?", unbound.ID).Updates(map[string]any{
		"source_git_server_id": "", "source_git_repo_id": 0,
	}).Error; err != nil {
		t.Fatalf("clear stable Git identity: %v", err)
	}

	archived := base
	archived.ID, archived.Slug, archived.Status = "archived", "archived-plugin", "archived"
	seedGitMarketplacePlugin(t, archived)

	body := decodeMarketplaceResponse(t, "public")
	if len(body.Plugins) != 1 || body.Plugins[0].Name != "valid-plugin" {
		t.Fatalf("only the synced Git row should be published: %+v", body.Plugins)
	}
}

func TestMarketplaceJSON_ReflectsLatestSyncedVersionAndSHA(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	registryID := seedMarketplaceRegistry(t, "public", "public", "public")
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "updating", RegistryID: registryID, RepoID: "public",
		Slug: "updating-plugin", Version: "1.0.0",
		Metadata:       datatypes.JSON([]byte(`{"name":"updating-plugin"}`)),
		SourceRepoURL:  "https://gitea.example.test/plugins/updating.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA1,
	})

	before := decodeMarketplaceResponse(t, "public")
	if len(before.Plugins) != 1 || before.Plugins[0].Version != "1.0.0" || before.Plugins[0].Source.SHA != marketplaceSHA1 {
		t.Fatalf("unexpected initial projection: %+v", before.Plugins)
	}
	// Same as above: this simulates a completed Git sync, not a Cloud edit.
	if err := database.DB.Set(models.GitSyncBypassSetting, true).
		Model(&models.CapabilityItem{}).Where("id = ?", "updating").Updates(map[string]any{
		"version": "1.1.0", "git_sha": marketplaceSHA2,
	}).Error; err != nil {
		t.Fatalf("update synced projection: %v", err)
	}
	after := decodeMarketplaceResponse(t, "public")
	if len(after.Plugins) != 1 || after.Plugins[0].Version != "1.1.0" || after.Plugins[0].Source.SHA != marketplaceSHA2 {
		t.Fatalf("unexpected updated projection: %+v", after.Plugins)
	}
}

func TestPluginRootFromManifestPath(t *testing.T) {
	tests := []struct {
		manifest string
		wantRoot string
		wantOK   bool
	}{
		{manifest: ".plugin.json", wantRoot: ".", wantOK: true},
		{manifest: ".claude-plugin/plugin.json", wantRoot: ".", wantOK: true},
		{manifest: "plugins/demo/.plugin.json", wantRoot: "plugins/demo", wantOK: true},
		{manifest: "plugins/demo/.claude-plugin/plugin.json", wantRoot: "plugins/demo", wantOK: true},
		{manifest: "plugin.json", wantRoot: ".", wantOK: true},
		{manifest: "../.plugin.json", wantOK: false},
		{manifest: "plugins/demo/README.md", wantOK: false},
		{manifest: "", wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.manifest, func(t *testing.T) {
			gotRoot, gotOK := pluginRootFromManifestPath(test.manifest)
			if gotRoot != test.wantRoot || gotOK != test.wantOK {
				t.Fatalf("pluginRootFromManifestPath(%q)=(%q,%v), want (%q,%v)", test.manifest, gotRoot, gotOK, test.wantRoot, test.wantOK)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// F-20: the marketplace manifest is a detail-scoped read of every entry's
// repository coordinate, and must be gated like one
// -----------------------------------------------------------------------------

// AC-LH16 for the marketplace. A repository that goes private on Gitea emits no
// webhook, so the local visibility column keeps saying "public"; before F-20
// this endpoint kept publishing the repository URL, ref and pinned SHA to
// anonymous callers on the strength of that stale column. Now the entry
// disappears from the publicly-served manifest — while the item's own creator,
// whose permission never came from public visibility, keeps seeing it.
func TestMarketplaceJSON_RepositoryTurnedPrivateStopsLeakingCoordinates(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	registryID := seedMarketplaceRegistry(t, "public", "public", "public")
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "leaky", RegistryID: registryID, RepoID: "public",
		Slug: "leaky-plugin", Metadata: datatypes.JSON([]byte(`{"name":"leaky-plugin"}`)),
		SourceRepoURL:  "https://gitea.example.test/private-now/leaky.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA1,
	})

	before := decodeMarketplaceResponse(t, "public")
	if len(before.Plugins) != 1 || before.Plugins[0].Source.URL != "https://gitea.example.test/private-now/leaky.git" {
		t.Fatalf("precondition: the public repository must be published: %+v", before.Plugins)
	}

	// The repository goes private on Gitea. Nothing tells us; the local column
	// still says public.
	gitea.goPrivate()
	resetGitVisibilityCache()

	w := get(newPluginMarketplaceRouter(""), "/api/marketplace/public/marketplace.json")
	if w.Code != http.StatusOK {
		t.Fatalf("the manifest itself stays servable: %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, leak := range []string{"private-now/leaky.git", marketplaceSHA1} {
		if strings.Contains(body, leak) {
			t.Fatalf("the manifest leaked %q after the repository went private: %s", leak, body)
		}
	}
	var manifest marketplaceTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Plugins) != 0 {
		t.Fatalf("a repository verified not-public was still published: %+v", manifest.Plugins)
	}

	// The creator's permission does not come from the repository being public,
	// so going private cannot revoke it: they still see their own entry.
	w = get(newPluginMarketplaceRouter("owner"), "/api/marketplace/public/marketplace.json")
	if w.Code != http.StatusOK {
		t.Fatalf("owner request: %d (%s)", w.Code, w.Body.String())
	}
	var ownerView marketplaceTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &ownerView); err != nil {
		t.Fatalf("decode owner manifest: %v", err)
	}
	if len(ownerView.Plugins) != 1 || ownerView.Plugins[0].Name != "leaky-plugin" {
		t.Fatalf("the item's creator lost their own entry: %+v", ownerView.Plugins)
	}
}

// Fail closed, not open: an unanswerable visibility question is not permission
// to hand out the coordinate — and the refusal itself must not carry it either.
func TestMarketplaceJSON_UnverifiableVisibilityFailsClosed(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	registryID := seedMarketplaceRegistry(t, "public", "public", "public")
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "unverified", RegistryID: registryID, RepoID: "public",
		Slug: "unverified-plugin", Metadata: datatypes.JSON([]byte(`{"name":"unverified-plugin"}`)),
		SourceRepoURL:  "https://gitea.example.test/unverified/plugin.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA1,
	})
	stopFakeGitea(t)

	w := get(newPluginMarketplaceRouter(""), "/api/marketplace/public/marketplace.json")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the git server cannot answer, got %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "GIT_VISIBILITY_UNVERIFIED") {
		t.Fatalf("refusal does not identify the unverified visibility: %s", body)
	}
	for _, leak := range []string{"unverified/plugin.git", marketplaceSHA1, "unverified-plugin"} {
		if strings.Contains(body, leak) {
			t.Fatalf("an unverified refusal handed out %q: %s", leak, body)
		}
	}
}

// The other half of the guard's scope rule: a member of a private marketplace
// is authorized by membership, which the repository going private on Gitea
// cannot revoke — so the gate owes the git server no question for them. This
// pins that no probe happens, not merely that the request succeeds.
func TestMarketplaceJSON_PrivateMarketplaceMembersAreNotProbed(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	registryID := seedMarketplaceRegistry(t, "team-repo", "team-repo", "private")
	seedGitMarketplacePlugin(t, models.CapabilityItem{
		ID: "team-plugin", RegistryID: registryID, RepoID: "team-repo",
		Slug: "team-plugin", Metadata: datatypes.JSON([]byte(`{"name":"team-plugin"}`)),
		SourceRepoURL:  "https://gitea.example.test/team/team-plugin.git",
		SourceRepoPath: ".claude-plugin/plugin.json", GitSHA: marketplaceSHA2,
	})
	if err := database.GetDB().Exec(`INSERT INTO repo_members (id, repo_id, user_id, role, created_at)
		VALUES ('mkt-m1', 'team-repo', 'member-1', 'member', CURRENT_TIMESTAMP)`).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}

	w := get(newPluginMarketplaceRouter("member-1"), "/api/marketplace/team-repo/marketplace.json")
	if w.Code != http.StatusOK {
		t.Fatalf("member request: %d (%s)", w.Code, w.Body.String())
	}
	var manifest marketplaceTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Plugins) != 1 || manifest.Plugins[0].Name != "team-plugin" {
		t.Fatalf("member does not see the private marketplace: %+v", manifest.Plugins)
	}
	if repoLookups, rawReads := gitea.counts(); repoLookups != 0 || rawReads != 0 {
		t.Fatalf("a membership-authorized read contacted the git server: %d lookups, %d raw reads", repoLookups, rawReads)
	}
}
