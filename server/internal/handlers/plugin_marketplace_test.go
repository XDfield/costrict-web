package handlers

import (
	"encoding/json"
	"net/http"
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
	if item.SourceGitServerID == "" {
		item.SourceGitServerID = "marketplace-test"
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
