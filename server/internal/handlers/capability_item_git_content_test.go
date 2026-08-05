// Read-through tests for Git-backed capability content.
//
// The Git edge is a httptest server speaking the two endpoints the read path
// uses (repository-by-id and raw file); everything below it is real — real
// gorm/sqlite rows, real gitserver.DBResolver, real gitsync.Client. That makes
// the assertions about call counts meaningful: they count actual HTTP requests,
// not stub invocations.

package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/datatypes"
)

const (
	gitContentTestServerID = "gcs-1"
	gitContentTestRepoID   = int64(4242)
	gitContentTestRepoName = "alice/skills"
	// gitContentStaleDBValue is what the row's `content` column holds. Nothing
	// may ever serve it: it stands in for the snapshot older rows still carry.
	gitContentStaleDBValue = "STALE-DB-RESIDUE-MUST-NEVER-BE-SERVED"
)

// fakeContentGitea serves GET /repositories/{id} and GET /repos/{o}/{r}/raw/{path}.
type fakeContentGitea struct {
	mu sync.Mutex

	adminToken string
	fullName   string
	// files maps repo-relative path → bytes at HEAD.
	files map[string]string

	repoLookups int
	rawReads    []string
	// rawStatus, when non-zero, is returned instead of the file (404/403/500).
	rawStatus int
}

func newFakeContentGitea(token string) *fakeContentGitea {
	return &fakeContentGitea{adminToken: token, fullName: gitContentTestRepoName, files: map[string]string{}}
}

func (f *fakeContentGitea) setFile(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

func (f *fakeContentGitea) counts() (repoLookups, rawReads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoLookups, len(f.rawReads)
}

func (f *fakeContentGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "token "+f.adminToken {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")

	if strings.HasPrefix(path, "/repositories/") {
		f.mu.Lock()
		f.repoLookups++
		fullName := f.fullName
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": gitContentTestRepoID, "name": strings.Split(fullName, "/")[1],
			"full_name": fullName, "default_branch": "main", "private": false,
		})
		return
	}

	if idx := strings.Index(path, "/git/trees/"); strings.HasPrefix(path, "/repos/") && idx >= 0 {
		f.mu.Lock()
		entries := make([]map[string]any, 0, len(f.files))
		for filePath, content := range f.files {
			entries = append(entries, map[string]any{
				"path": filePath, "type": "blob", "size": len([]byte(content)),
			})
		}
		f.mu.Unlock()
		sort.Slice(entries, func(i, j int) bool {
			return entries[i]["path"].(string) < entries[j]["path"].(string)
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tree": entries, "truncated": false, "total_count": len(entries),
		})
		return
	}

	if idx := strings.Index(path, "/raw/"); strings.HasPrefix(path, "/repos/") && idx >= 0 {
		filePath := path[idx+len("/raw/"):]
		f.mu.Lock()
		f.rawReads = append(f.rawReads, filePath+"?ref="+r.URL.Query().Get("ref"))
		status := f.rawStatus
		content, ok := f.files[filePath]
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, `{"message":"forced"}`, status)
			return
		}
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(content))
		return
	}

	http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
}

// setupGitContentFixture wires a fake git server into the current test DB and
// returns it. The git_servers row is what the read-through resolves through, so
// nothing else needs injecting.
func setupGitContentFixture(t *testing.T) *fakeContentGitea {
	t.Helper()
	db := database.GetDB()
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS git_servers (
		server_id    TEXT PRIMARY KEY,
		kind         TEXT NOT NULL,
		endpoint     TEXT NOT NULL,
		display_name TEXT NOT NULL,
		config       TEXT NOT NULL DEFAULT '{}',
		is_template  INTEGER NOT NULL DEFAULT 0,
		enabled      INTEGER NOT NULL DEFAULT 1,
		created_at   DATETIME NOT NULL,
		updated_at   DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create git_servers: %v", err)
	}

	gitea := newFakeContentGitea("admin-token")
	srv := httptest.NewServer(gitea)
	t.Cleanup(srv.Close)

	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		 VALUES (?,'gitea',?,'fake','{"admin_token":"admin-token"}',0,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		gitContentTestServerID, srv.URL).Error; err != nil {
		t.Fatalf("seed git_servers: %v", err)
	}
	return gitea
}

// stopFakeGitea makes every later request fail at the transport layer, which is
// what "the git server is down" looks like from here.
func stopFakeGitea(t *testing.T) {
	t.Helper()
	if err := database.GetDB().
		Exec(`UPDATE git_servers SET endpoint = 'http://127.0.0.1:1' WHERE server_id = ?`, gitContentTestServerID).
		Error; err != nil {
		t.Fatalf("point git server at a dead address: %v", err)
	}
}

// seedGitContentItem creates the repo/registry/item trio. The item carries
// stale content in the DB so every assertion below can prove it is not served.
func seedGitContentItem(t *testing.T, id, itemType, manifestPath string) {
	t.Helper()
	createTestRepository(t, "repo-gc-"+id, "public")
	if err := database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gc-" + id, Name: "reg-gc-" + id, SourceType: "git",
		RepoID: "repo-gc-" + id, OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := database.DB.Create(&models.CapabilityItem{
		ID: id, RegistryID: "reg-gc-" + id, RepoID: "repo-gc-" + id, Slug: id,
		ItemType: itemType, Name: "Git " + id, Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), Content: gitContentStaleDBValue,
		CurrentRevision: 1, ContentBackend: models.ContentBackendGit,
		SourceRepoURL: "https://gitea.example.test/" + gitContentTestRepoName,
		SourceRepoRef: "main", SourceRepoPath: manifestPath,
		SourceGitServerID: gitContentTestServerID, SourceGitRepoID: gitContentTestRepoID,
		GitSHA: strings.Repeat("a", 40), GitSyncStatus: "synced",
	}).Error; err != nil {
		t.Fatalf("seed git-backed item: %v", err)
	}
}

// seedDBContentItem is the control group: an ordinary DB-backed row whose
// behaviour must not change at all.
func seedDBContentItem(t *testing.T, id string) {
	t.Helper()
	createTestRepository(t, "repo-gc-"+id, "public")
	if err := database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gc-" + id, Name: "reg-gc-" + id, SourceType: "internal",
		RepoID: "repo-gc-" + id, OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := database.DB.Create(&models.CapabilityItem{
		ID: id, RegistryID: "reg-gc-" + id, RepoID: "repo-gc-" + id, Slug: id,
		ItemType: "skill", Name: "DB " + id, Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), Content: "---\nname: db\n---\ndb body",
		CurrentRevision: 1,
	}).Error; err != nil {
		t.Fatalf("seed db-backed item: %v", err)
	}
}

const gitContentSkillFile = "---\nname: FIX Skill\ndescription: from git\ndisable-model-invocation: true\n---\n\n# Body\n"

// waitForDownloadLog blocks until DownloadItem's asynchronous behaviour log has
// landed. Without it the goroutine outlives the test and dereferences a DB
// handle the cleanup already dropped — the same reason the existing download
// tests wait rather than sleep.
func waitForDownloadLog(t *testing.T, itemID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var logged int64
		database.DB.Model(&models.BehaviorLog{}).
			Where("item_id = ? AND action_type = ?", itemID, string(models.ActionInstall)).
			Count(&logged)
		if logged >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("download of %s was never logged", itemID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func decodeItemBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	return body
}

// AC: content comes from Git, not from the DB — and a push is visible on the
// next request without anything writing it back.
func TestGetItem_GitBackedContentIsReadThroughAndNotPersisted(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-detail", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-detail")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if body["content"] != gitContentSkillFile {
		t.Fatalf("content did not come from git: %q", body["content"])
	}
	if !strings.HasPrefix(body["content"].(string), "---") {
		t.Fatal("frontmatter was stripped; the device loader reads its fields out of it")
	}
	if body["gitSyncStatus"] != "synced" {
		t.Fatalf("gitSyncStatus missing from the response: %v", body["gitSyncStatus"])
	}

	// A push lands; the very next read reflects it.
	const updated = "---\nname: FIX Skill\ndescription: pushed\n---\n\n# New body\n"
	gitea.setFile("skill.md", updated)
	w = get(newItemRouter(""), "/api/items/gc-detail")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after push, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeItemBody(t, w.Body.Bytes())["content"]; got != updated {
		t.Fatalf("second read served a cached body: %q", got)
	}

	// Nothing was written back: the column still holds exactly what was seeded.
	var stored models.CapabilityItem
	if err := database.DB.First(&stored, "id = ?", "gc-detail").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Content != gitContentStaleDBValue {
		t.Fatalf("the read wrote content back to the DB: %q", stored.Content)
	}

	// Two requests, two round trips — no cache to make the second one free.
	if _, rawReads := gitea.counts(); rawReads != 2 {
		t.Fatalf("expected 2 raw reads (one per request), got %d", rawReads)
	}
}

// AC (hard red line): an unreachable git server produces an error. Not the
// stored column, not an empty body.
func TestGetItem_GitBackedContentFailsClosedWhenGiteaIsDown(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-down", "skill", "skill.md")
	stopFakeGitea(t)

	w := get(newItemRouter(""), "/api/items/gc-down")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when git is unreachable, got %d: %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	if strings.Contains(raw, gitContentStaleDBValue) {
		t.Fatal("the failure path fell back to the stored content column")
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if body["error_code"] != "GIT_CONTENT_UNREACHABLE" {
		t.Fatalf("error does not identify an upstream failure: %v", body)
	}
	if _, ok := body["content"]; ok {
		t.Fatal("the failure response carried a content field")
	}
}

// A manifest deleted from the repository is distinguishable from a git server
// that is merely down — the two need different operator responses.
func TestGetItem_GitBackedMissingManifestIsReportedDistinctly(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t) // no files registered → 404 from the raw endpoint
	seedGitContentItem(t, "gc-missing", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-missing")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	if code := decodeItemBody(t, w.Body.Bytes())["error_code"]; code != "GIT_CONTENT_MISSING" {
		t.Fatalf("expected GIT_CONTENT_MISSING, got %v", code)
	}
}

// A credential problem on the git server is its own verdict: retrying will not
// help, and an operator has to look at the admin token rather than at the
// network.
func TestGetItem_GitBackedRejectedReadIsReportedDistinctly(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	gitea.mu.Lock()
	gitea.rawStatus = http.StatusForbidden
	gitea.mu.Unlock()
	seedGitContentItem(t, "gc-forbidden", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-forbidden")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	if code := decodeItemBody(t, w.Body.Bytes())["error_code"]; code != "GIT_CONTENT_FORBIDDEN" {
		t.Fatalf("expected GIT_CONTENT_FORBIDDEN, got %v", code)
	}
	if strings.Contains(w.Body.String(), gitContentStaleDBValue) {
		t.Fatal("a rejected read fell back to the stored content column")
	}
}

// A row bound to a git server that no longer exists cannot be served either.
func TestGetItem_GitBackedUnknownServerIsReported(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedGitContentItem(t, "gc-noserver", "skill", "skill.md")
	if err := database.DB.Exec(`DELETE FROM git_servers WHERE server_id = ?`, gitContentTestServerID).Error; err != nil {
		t.Fatalf("drop git server: %v", err)
	}

	w := get(newItemRouter(""), "/api/items/gc-noserver")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), gitContentStaleDBValue) {
		t.Fatal("the failure path leaked the stored content column")
	}
}

// AC: the download payload is the repository file byte for byte.
func TestDownloadItem_GitBackedServesRepositoryBytes(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-dl", "skill", "skill.md")

	w := get(newRouter(""), "/api/items/gc-dl/download")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got, want := sha256.Sum256(w.Body.Bytes()), sha256.Sum256([]byte(gitContentSkillFile)); got != want {
		t.Fatalf("download payload differs from the repository file:\n%q", w.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), "---") {
		t.Fatal("download stripped the frontmatter")
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "SKILL.md") {
		t.Fatalf("filename contract changed: %q", cd)
	}
	waitForDownloadLog(t, "gc-dl")
}

func TestDownloadItem_GitBackedFailsClosedWhenGiteaIsDown(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-dl-down", "skill", "skill.md")
	stopFakeGitea(t)

	w := get(newRouter(""), "/api/items/gc-dl-down/download")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), gitContentStaleDBValue) {
		t.Fatal("download fell back to the stored content column")
	}
}

// The registry route serves both the main file and assets through live Git
// reads, without persisting the asset in capability_assets.
func TestDownloadRegistryFile_GitBackedMainFileAndAssetReadThrough(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	gitea.setFile("assets/logo.png", "PNG")
	seedGitContentItem(t, "gc-reg", "skill", "skill.md")

	w := get(newRouter(""), "/api/registry/repo-gc-gc-reg/skill/gc-reg/SKILL.md")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for the main file, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != gitContentSkillFile {
		t.Fatalf("main file did not come from git: %q", w.Body.String())
	}

	w = get(newRouter(""), "/api/registry/repo-gc-gc-reg/skill/gc-reg/assets/logo.png")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a Git asset, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "PNG" {
		t.Fatalf("asset did not come from Git: %q", w.Body.String())
	}
}

func TestListItemAssets_GitBackedListsLiveTree(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	gitea.setFile("assets/logo.png", "PNG")
	seedGitContentItem(t, "gc-assets", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-assets/assets")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body ItemAssetsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode assets: %v", err)
	}
	if body.AssetsBackend != contentBackendGit || len(body.Assets) != 1 {
		t.Fatalf("unexpected assets response: %#v", body)
	}
	if got := body.Assets[0]; got.RelPath != "assets/logo.png" || got.FileSize != 3 {
		t.Fatalf("unexpected Git asset: %#v", got)
	}
}

// The ownership marker provisioning writes is not an asset of the capability.
//
// It is bookkeeping that says "CoStrict created this repository for this
// item"; listing it means the device installs .costrict/capability.json next to
// the skill. The fixture carries it because the earlier fixtures did not — a
// tree that never contains the file cannot show that the file leaks.
func TestListItemAssets_GitBackedExcludesOwnershipMarker(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	gitea.setFile(services.GitCapabilityOwnershipMarkerPath,
		`{"schema":"costrict-capability/v1","itemType":"skill","slug":"gc-marker","manifestPath":"skill.md"}`)
	gitea.setFile("assets/logo.png", "PNG")
	seedGitContentItem(t, "gc-marker", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-marker/assets")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body ItemAssetsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode assets: %v", err)
	}
	// Whole-manifest assertion: "assets/logo.png is present" would pass while
	// the marker sat right next to it.
	if len(body.Assets) != 1 || body.Assets[0].RelPath != "assets/logo.png" {
		got := make([]string, 0, len(body.Assets))
		for _, a := range body.Assets {
			got = append(got, a.RelPath)
		}
		t.Fatalf("asset manifest = %v, want exactly [assets/logo.png]", got)
	}

	// And it is not reachable by asking for it directly: the download route
	// resolves against the same manifest, so an excluded path is a 404 rather
	// than an undocumented way to fetch it.
	w = get(newRouter(""), "/api/registry/repo-gc-gc-marker/skill/gc-marker/"+services.GitCapabilityOwnershipMarkerPath)
	if w.Code == http.StatusOK {
		t.Fatalf("ownership marker is downloadable through the registry route: %s", w.Body.String())
	}
}

// AC: the list endpoint blanks git-backed content and makes no Git calls.
func TestListAllItems_GitBackedContentIsBlankWithoutGitCalls(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-list", "skill", "skill.md")
	if err := database.DB.Create(&models.ItemFavorite{
		ID: "fav-gc-list", ItemID: "gc-list", UserID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}

	w := get(newItemRouter("u1"), "/api/items?favorited=true")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected the favorited item, got %d rows", len(payload.Items))
	}
	if content, ok := payload.Items[0]["content"]; ok && content != "" {
		t.Fatalf("list served content for a git-backed row: %v", content)
	}
	if repoLookups, rawReads := gitea.counts(); repoLookups != 0 || rawReads != 0 {
		t.Fatalf("list made %d repo lookups and %d raw reads; it must make none", repoLookups, rawReads)
	}
}

// Control group: DB-backed rows keep every byte of their previous behaviour on
// all three read paths, and never touch the git server.
func TestDBBackedReadPathsAreUnchanged(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	seedDBContentItem(t, "gc-dbctl")
	if err := database.DB.Create(&models.ItemFavorite{
		ID: "fav-gc-dbctl", ItemID: "gc-dbctl", UserID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
	const want = "---\nname: db\n---\ndb body"

	w := get(newItemRouter(""), "/api/items/gc-dbctl")
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeItemBody(t, w.Body.Bytes())["content"]; got != want {
		t.Fatalf("detail content changed: %q", got)
	}

	w = get(newRouter(""), "/api/items/gc-dbctl/download")
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("download changed: %d %q", w.Code, w.Body.String())
	}
	waitForDownloadLog(t, "gc-dbctl")

	w = get(newRouter(""), fmt.Sprintf("/api/registry/%s/skill/gc-dbctl/SKILL.md", "repo-gc-gc-dbctl"))
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("registry file changed: %d %q", w.Code, w.Body.String())
	}

	w = get(newItemRouter("u1"), "/api/items?favorited=true")
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0]["content"] != want {
		t.Fatalf("list content changed: %+v", payload.Items)
	}

	if repoLookups, rawReads := gitea.counts(); repoLookups != 0 || rawReads != 0 {
		t.Fatalf("db-backed reads contacted the git server: %d lookups, %d raw reads", repoLookups, rawReads)
	}
}

// A Git-backed row with no repository assets returns [] rather than null.
//
// The distinction is load-bearing on the device: the installer treats a missing
// `assets` key as "none" but throws on a non-array, aborting the whole install.
// Go marshals a nil slice to null, so the empty response only stays safe while
// ListItemAssets builds a non-nil slice (make(..., 0, n)) and the field carries
// no omitempty. This pins both, since Git-backed rows are what make the empty
// case the common one.
func TestListItemAssets_GitBackedReturnsEmptyArrayNotNull(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-assets", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-assets/assets")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"assets":[]`) {
		t.Fatalf("assets must serialize as an empty array, got: %s", w.Body.String())
	}
	var payload struct {
		Assets        []map[string]any `json:"assets"`
		AssetsBackend string           `json:"assetsBackend"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Assets == nil {
		t.Fatal("assets decoded as null; the device installer aborts on that")
	}
	if payload.AssetsBackend != "git" {
		t.Fatalf(`assetsBackend must mark where the files really are, got %q`, payload.AssetsBackend)
	}
	// Listing is live: resolve the stable repository identity and walk its tree,
	// but do not fetch file bodies until the installer asks for an asset.
	if repoLookups, rawReads := gitea.counts(); repoLookups != 1 || rawReads != 0 {
		t.Fatalf("the live manifest made %d repo lookups and %d raw reads", repoLookups, rawReads)
	}
}

// Control group: a DB-backed row's manifest keeps its exact previous shape —
// the same payload, and no assetsBackend field, since its absence is what every
// response meant before Git backing existed.
func TestListItemAssets_DBBackedResponseIsUnchanged(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	seedDBContentItem(t, "gc-dbassets")
	text := "hello asset"
	if err := database.DB.Create(&models.CapabilityAsset{
		ID: "asset-gc-dbassets", ItemID: "gc-dbassets", RelPath: "docs/notes.md",
		TextContent: &text, MimeType: "text/markdown", FileSize: int64(len(text)),
		ContentSHA: strings.Repeat("b", 64),
	}).Error; err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	w := get(newItemRouter(""), "/api/items/gc-dbassets/assets")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	want := `{"assets":[{"relPath":"docs/notes.md","textContent":"hello asset","mimeType":"text/markdown","fileSize":11,"contentSha":"` +
		strings.Repeat("b", 64) + `"}]}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("db-backed manifest changed:\n got: %s\nwant: %s", got, want)
	}
	if repoLookups, rawReads := gitea.counts(); repoLookups != 0 || rawReads != 0 {
		t.Fatalf("a db-backed manifest contacted the git server: %d lookups, %d raw reads", repoLookups, rawReads)
	}
}

// The repository is located by its numeric id, so a rename on the git server
// must not break the read — the URL stored on the row is stale by then.
func TestGetItem_GitBackedFollowsRepositoryRename(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-renamed", "skill", "skill.md")

	gitea.mu.Lock()
	gitea.fullName = "bob/renamed-skills"
	gitea.mu.Unlock()

	w := get(newItemRouter(""), "/api/items/gc-renamed")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after rename, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeItemBody(t, w.Body.Bytes())["content"]; got != gitContentSkillFile {
		t.Fatalf("content after rename: %q", got)
	}
	gitea.mu.Lock()
	defer gitea.mu.Unlock()
	if len(gitea.rawReads) != 1 || !strings.HasSuffix(gitea.rawReads[0], "?ref=main") {
		t.Fatalf("unexpected raw reads: %v", gitea.rawReads)
	}
}

// A Git-backed row left over from before discovery stopped creating revision
// rows still has one, and its content is the snapshot taken at discovery time.
// Serving that snapshot would hand out exactly the stale copy read-through
// exists to eliminate — on the same item that answers with live content
// everywhere else.
func TestItemVersions_GitBackedNeverServeStoredSnapshots(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-versions", "skill", "skill.md")

	// A pre-existing revision row, exactly as older discovery would have left it.
	stale := "---\nname: stale\n---\n\nthe snapshot taken when this row was discovered\n"
	if err := database.DB.Create(&models.CapabilityVersion{
		ID: "gc-versions-r1", ItemID: "gc-versions", Revision: 1,
		Name: "FIX", Descriptions: datatypes.JSON([]byte(`{}`)),
		Version: "1.0.0", Content: stale, Metadata: datatypes.JSON([]byte(`{}`)),
		CreatedBy: "u1",
	}).Error; err != nil {
		t.Fatalf("seed stale version: %v", err)
	}

	w := get(newItemRouter(""), "/api/items/gc-versions/versions")
	if w.Code != http.StatusOK {
		t.Fatalf("list versions: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "snapshot taken when") {
		t.Fatalf("the stale snapshot leaked through the version list: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"versionBackend":"git"`) {
		t.Fatalf("the response must say where versioning lives: %s", w.Body.String())
	}

	w = get(newItemRouter(""), "/api/items/gc-versions/versions/1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get version: expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "snapshot taken when") {
		t.Fatalf("the stale snapshot leaked through the single-version route: %s", w.Body.String())
	}

	// Control: the item itself still answers with the live repository content.
	w = get(newItemRouter(""), "/api/items/gc-versions")
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d", w.Code)
	}
	if body := decodeItemBody(t, w.Body.Bytes()); body["content"] != gitContentSkillFile {
		t.Fatalf("detail content did not come from git: %q", body["content"])
	}
}
