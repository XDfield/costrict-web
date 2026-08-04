// Cloud "create new" for a Git-backed capability: provision the repository,
// then register the index row.
//
// The one thing these tests exist to catch is a repository that cannot describe
// itself. Discovery classifies a root-level manifest by NAME — skill.md /
// agent.md / command.md / mcp.json — while the download naming
// (contentFilename) answers "<slug>.md" for subagent and command. A skeleton
// written under the download naming pushes cleanly and then produces no
// capability at all, silently. So every skeleton is fed back through the
// classifier the sync worker itself uses.

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// The sharpest form of the R3.5 decision: the name a skeleton is written under
// must be a name discovery classifies back as the SAME type, and it is not the
// download name. Both halves are asserted so neither function can be
// "simplified" into the other later.
func TestGitCapabilityManifestPath_MatchesDiscoveryAndDiffersFromDownloadName(t *testing.T) {
	for _, itemType := range []string{"skill", "subagent", "command", "mcp"} {
		manifestPath, ok := gitCapabilityManifestPath(itemType)
		if !ok {
			t.Fatalf("%s: no repository manifest name", itemType)
		}
		got, classified := services.ClassifyGitCapabilityManifestType(manifestPath)
		if !classified {
			t.Errorf("%s: discovery does not recognise %q at a repository root", itemType, manifestPath)
			continue
		}
		if got != itemType {
			t.Errorf("%s: discovery classifies %q as %q", itemType, manifestPath, got)
		}
	}

	// contentFilename answers the HTTP download's attachment name and returns
	// "<slug>.md" for subagent/command. A repository file of that name is
	// invisible to the classifier — which is exactly why the two must not be
	// merged.
	for _, itemType := range []string{"subagent", "command"} {
		downloadName := contentFilename(itemType, "some-slug")
		if services.IsGitCapabilityManifestPath(downloadName) {
			t.Errorf("%s: %q would now be discoverable — re-check whether the two namings can be unified",
				itemType, downloadName)
		}
	}
}

func newGitCreateRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ScanJobService = nil
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	db := database.GetDB()
	tagSvc := &services.TagService{DB: db}
	TagSvc = tagSvc
	itemHandler := NewItemHandler(db, &services.ParserService{}, nil, tagSvc)
	r.POST("/api/items/git", injectUser, itemHandler.CreateGitBackedItem)
	return r
}

func createGitItemReq(r *gin.Engine, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/items/git", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// Every type Cloud can create produces a repository whose manifest discovery
// classifies back as that same type. This is the direct guard against the
// <slug>.md trap: a skeleton under the download naming would push and index
// nothing.
func TestCreateGitBackedItem_SkeletonIsSelfDescribing(t *testing.T) {
	cases := []struct {
		itemType     string
		manifestPath string
	}{
		{"skill", "skill.md"},
		{"subagent", "agent.md"},
		{"command", "command.md"},
		{"mcp", "mcp.json"},
	}
	for _, tc := range cases {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			createGitCapabilitySyncJobTable(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)

			w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
				"itemType":    tc.itemType,
				"slug":        "my-" + tc.itemType,
				"name":        "My " + tc.itemType,
				"description": "does a thing",
				"category":    "utilities",
				"tags":        []string{"alpha"},
				"author":      "bob",
				"license":     "MIT",
			})
			if w.Code != http.StatusCreated {
				t.Fatalf("create: expected 201, got %d (%s)", w.Code, w.Body.String())
			}
			var resp gitForkResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.ContentBackend != "git" {
				t.Fatalf("created row must be git-backed, got %q", resp.ContentBackend)
			}
			if resp.SourceRepoPath != tc.manifestPath {
				t.Fatalf("manifest path: want %q, got %q", tc.manifestPath, resp.SourceRepoPath)
			}
			// The classifier the sync worker uses must recognise the file, and as
			// this type. contentFilename's "<slug>.md" would fail right here.
			if !services.IsGitCapabilityManifestPath(resp.SourceRepoPath) {
				t.Fatalf("discovery does not recognise %q as a capability manifest", resp.SourceRepoPath)
			}
			repoName := strings.TrimPrefix(resp.SourceRepoURL, fx.srv.URL+"/")
			raw := fx.gitea.fileOf(repoName, tc.manifestPath)
			if len(raw) == 0 {
				t.Fatalf("repository %s has no %s", repoName, tc.manifestPath)
			}
			parsed, err := (&services.ParserService{}).ParseGitDiscoveryFile(raw, tc.manifestPath, tc.itemType)
			if err != nil {
				t.Fatalf("discovery cannot parse the skeleton: %v", err)
			}
			if len(parsed) != 1 {
				t.Fatalf("skeleton must describe exactly one capability, got %d", len(parsed))
			}
			if got := strings.TrimSpace(parsed[0].Name); got == "" {
				t.Error("discovery parsed no capability name from the skeleton")
			}
			// Tags typed into the form must be tags the parser reads back —
			// V4 §5.2 nests them under `metadata:`, where nothing looks.
			if tc.itemType != "mcp" && len(parsed[0].Tags) != 1 {
				t.Errorf("skeleton tags did not project: %v", parsed[0].Tags)
			}
		})
	}
}

// The markdown skeleton carries the V4 §5.2 identity fields, and read-through
// returns exactly the bytes the repository holds — the row keeps no copy.
func TestCreateGitBackedItem_SkeletonFrontmatterAndNoDBContent(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	createGitCapabilitySyncJobTable(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)

	w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
		"itemType": "skill", "slug": "vetter", "name": "Skill Vetter",
		"description": "checks skills", "category": "security", "version": "2.1.0",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	repoName := strings.TrimPrefix(resp.SourceRepoURL, fx.srv.URL+"/")
	if repoName != "10001/vetter" {
		t.Fatalf("repository must be <short_id>/<slug>, got %q", repoName)
	}
	body := string(fx.gitea.fileOf(repoName, "skill.md"))
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("skeleton must open with frontmatter, got %q", body)
	}
	for _, want := range []string{"slug: vetter", "type: skill", "name: Skill Vetter", "version: 2.1.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("frontmatter missing %q:\n%s", want, body)
		}
	}
	// The frontmatter the skeleton writes must be frontmatter the platform
	// reads. A field written where the parser does not look is a field the
	// repository declares and the platform ignores.
	parsed, err := (&services.ParserService{}).ParseSKILLMD([]byte(body), "skill.md")
	if err != nil {
		t.Fatalf("parser cannot read the skeleton: %v", err)
	}
	if parsed.Name != "Skill Vetter" || parsed.Description != "checks skills" ||
		parsed.Category != "security" || parsed.Version != "2.1.0" {
		t.Errorf("projected fields drifted: name=%q description=%q category=%q version=%q",
			parsed.Name, parsed.Description, parsed.Category, parsed.Version)
	}

	var stored models.CapabilityItem
	database.GetDB().First(&stored, "id = ?", resp.ID)
	if stored.Content != "" {
		t.Errorf("a git-backed row keeps no content copy, got %q", stored.Content)
	}
	// The hash is derived the way discovery derives it, from the repository
	// bytes — otherwise the row can never match in CheckItemConsistency.
	want := services.HashGitCapabilityContent("skill", "skill.md", body)
	if stored.ContentMD5 != want {
		t.Errorf("content hash %q != discovery-equivalent %q", stored.ContentMD5, want)
	}
	if stored.SourceGitServerID != "gs-1" || stored.SourceGitRepoID <= 0 || stored.GitSyncStatus != "pending" {
		t.Errorf("git identity: server=%q repo=%d sync=%q",
			stored.SourceGitServerID, stored.SourceGitRepoID, stored.GitSyncStatus)
	}
	// Creation pushes one commit, but the push webhook is opt-in per deployment;
	// without an explicit job the row would sit at 'pending' forever.
	var jobs []models.GitCapabilitySyncJob
	database.GetDB().Find(&jobs)
	if len(jobs) != 1 || jobs[0].DeliveryID != "fork:"+resp.ID {
		t.Errorf("want one queued sync job fork:%s, got %+v", resp.ID, jobs)
	}
}

// The row lands in the public registry with repo_id='public' — the same place
// catalog rows live. Nothing about its placement distinguishes it from a
// catalog row; only content_backend does, which is why the ingest queries must
// filter on that value (locked down by the services-side ingest tests).
func TestCreateGitBackedItem_LandsInPublicRegistry(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)

	w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
		"itemType": "skill", "slug": "placed", "name": "Placed",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	var stored models.CapabilityItem
	database.GetDB().First(&stored, "id = ?", resp.ID)
	if stored.RegistryID != PublicRegistryID {
		t.Errorf("registry: want %q, got %q", PublicRegistryID, stored.RegistryID)
	}
	if stored.RepoID != "public" {
		t.Errorf("repo id: want public, got %q", stored.RepoID)
	}
	// A git-backed row anchors its version on the commit, so it grows no
	// capability_versions row — serving one would hand back a snapshot.
	var revisions int64
	database.GetDB().Model(&models.CapabilityVersion{}).Where("item_id = ?", resp.ID).Count(&revisions)
	if revisions != 0 {
		t.Errorf("a git-backed row must produce no revision rows, got %d", revisions)
	}
}

// Fail closed: with Gitea down nothing is created — not a git-backed row, and
// not a DB row standing in for it.
func TestCreateGitBackedItem_FailsClosedWhenGiteaDown(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	fx.srv.Close()

	w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
		"itemType": "skill", "slug": "doomed", "name": "Doomed",
	})
	if w.Code < 400 {
		t.Fatalf("expected a failure status, got %d (%s)", w.Code, w.Body.String())
	}
	var count int64
	database.GetDB().Model(&models.CapabilityItem{}).Where("slug = ?", "doomed").Count(&count)
	if count != 0 {
		t.Errorf("a failed creation must leave no row, found %d", count)
	}
}

// Without a provisioned Gitea identity there is nowhere to put the repository.
// Falling back to a DB row would look like success and produce a capability the
// user cannot edit anywhere.
func TestCreateGitBackedItem_RequiresReadyGitAccount(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setupGitForkFixture(t)

	w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
		"itemType": "skill", "slug": "no-account", "name": "No Account",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_ACCOUNT_NOT_READY") {
		t.Errorf("expected GIT_ACCOUNT_NOT_READY, got %s", w.Body.String())
	}
	var count int64
	database.GetDB().Model(&models.CapabilityItem{}).Where("slug = ?", "no-account").Count(&count)
	if count != 0 {
		t.Errorf("must create no row, found %d", count)
	}
}

// Types that discovery cannot re-index are refused outright rather than
// creating a repository nothing will ever read back.
func TestCreateGitBackedItem_RejectsUnsupportedType(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)

	for _, itemType := range []string{"rule", "template", "plugin"} {
		w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
			"itemType": itemType, "slug": "x-" + itemType, "name": "X",
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (%s)", itemType, w.Code, w.Body.String())
		}
	}
	if len(fx.gitea.createCalls) != 0 {
		t.Errorf("no repository may be created for an unsupported type: %d", len(fx.gitea.createCalls))
	}
}

// A slug that is not a legal repository name is refused before anything is
// created — the repository name and the slug are the same identity.
func TestCreateGitBackedItem_RejectsUnusableSlug(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)

	w := createGitItemReq(newGitCreateRouter("bob"), map[string]any{
		"itemType": "skill", "slug": "../escape", "name": "Escape",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_REPO_NAME_INVALID") {
		t.Errorf("expected GIT_REPO_NAME_INVALID, got %s", w.Body.String())
	}
	if len(fx.gitea.createCalls) != 0 {
		t.Errorf("nothing may be created for an invalid slug: %d", len(fx.gitea.createCalls))
	}
}
