// Tests for the git-backed plugin fork path (fork → Gitea user namespace).
//
// The Gitea edge is faked with httptest (same shape as
// gitsync/user_provision_e2e_test.go); everything below it is real: real
// gorm/sqlite writes, real gitserver.DBResolver, real gitsync.Client, real
// AES-GCM credential encryption.

package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------- fake Gitea

// fakeForkGitea serves the three endpoints the git fork path touches.
type fakeForkGitea struct {
	mu sync.Mutex

	adminToken string
	// repos holds "owner/name" → default branch.
	repos map[string]string
	// manifests holds "owner/name" → the plugin name its manifest declares,
	// backing the contents endpoint used to verify a guessed mirror.
	manifests map[string]string
	// unreadableManifests makes a repository behave like an empty shell: its
	// contents endpoint returns 404 for every supported manifest path.
	unreadableManifests map[string]bool
	// manifestPaths pins "owner/name" → the single repo-relative path its
	// manifest is served at; every other path 404s the way a real repo would.
	// Unset means "served at whichever path is asked for", which is what the
	// tests that only care about the name verdict want.
	manifestPaths map[string]string
	// forkParents holds "owner/name" → parent "owner/name", mirroring Gitea's
	// `parent` payload. ForkRepo's conflict recovery verifies lineage through
	// it, so a repo listed here reads as "a fork of that source".
	forkParents map[string]string

	forkCalls  []forkCall
	tokenCalls int

	// forkStatus overrides the fork response status (0 = 202 happy path).
	forkStatus int
	// forkCreatesRepo=false emulates "conflict, repo already there".
	forkCreatesRepo bool
	// forkResponseRepoID is Gitea's numeric repository identity returned from
	// the fork API. It must be present before the handler can persist a
	// git-backed capability row.
	forkResponseRepoID int64
}

type forkCall struct {
	srcOwner, srcRepo string
	auth              string
	body              string
}

func newFakeForkGitea(adminToken string) *fakeForkGitea {
	return &fakeForkGitea{
		adminToken:          adminToken,
		repos:               map[string]string{},
		forkParents:         map[string]string{},
		manifests:           map[string]string{},
		manifestPaths:       map[string]string{},
		unreadableManifests: map[string]bool{},
		forkCreatesRepo:     true,
		forkResponseRepoID:  501,
	}
}

func (f *fakeForkGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")

	// POST /users/{name}/tokens — PAT minting (Basic auth, not token auth).
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/users/") && strings.HasSuffix(path, "/tokens") {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "gitadmin" || pass != "gitadminpw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.tokenCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 55, "name": "user-pat", "sha1": "minted-pat"})
		return
	}

	// POST /repos/{owner}/{repo}/forks
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/forks") {
		parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(path, "/forks"), "/repos/"), "/")
		if len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.forkCalls = append(f.forkCalls, forkCall{
			srcOwner: parts[0], srcRepo: parts[1],
			auth: r.Header.Get("Authorization"), body: string(raw),
		})
		status := f.forkStatus
		branch := f.repos[parts[0]+"/"+parts[1]]
		if f.forkCreatesRepo {
			f.repos["10001/"+parts[1]] = branch
		}
		f.mu.Unlock()

		if status != 0 {
			http.Error(w, `{"message":"The repository with the same name already exists."}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": f.forkResponseRepoID, "name": parts[1], "full_name": "10001/" + parts[1],
			"default_branch": branch, "private": false,
		})
		return
	}

	// GET /repos/{owner}/{repo}/contents/{path} — plugin manifest lookup used to
	// confirm a guessed mirror coordinate really holds the plugin we want.
	if r.Method == http.MethodGet && strings.Contains(path, "/contents/") {
		repoAndPath := strings.TrimPrefix(path, "/repos/")
		idx := strings.Index(repoAndPath, "/contents/")
		repoName := repoAndPath[:idx]
		filePath := repoAndPath[idx+len("/contents/"):]
		f.mu.Lock()
		plugin := f.manifests[repoName]
		wantPath := f.manifestPaths[repoName]
		_, repoExists := f.repos[repoName]
		unreadable := f.unreadableManifests[repoName]
		f.mu.Unlock()
		if !repoExists || unreadable || (wantPath != "" && wantPath != filePath) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		if plugin == "" {
			// Most test sources use plugin_name="p". The explicit map is only
			// needed when a test exercises a mismatch or a different manifest.
			plugin = "p"
		}
		body, _ := json.Marshal(map[string]string{"name": plugin})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(body),
		})
		return
	}

	// GET /repos/{owner}/{repo}
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/") {
		name := strings.TrimPrefix(path, "/repos/")
		f.mu.Lock()
		branch, ok := f.repos[name]
		parent := f.forkParents[name]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		parts := strings.Split(name, "/")
		payload := map[string]any{
			"id": 77, "name": parts[1], "full_name": name, "default_branch": branch,
		}
		if parent != "" {
			payload["fork"] = true
			payload["parent"] = map[string]any{"full_name": parent}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(payload)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeForkGitea) forkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forkCalls)
}

// ------------------------------------------------------------------- fixture

// gitForkFixture wires the whole git-backed fork stack onto the current test
// DB. Callers tweak the returned fake before issuing the request.
type gitForkFixture struct {
	gitea *fakeForkGitea
	srv   *httptest.Server
	db    *gorm.DB
}

func setupGitForkFixture(t *testing.T) *gitForkFixture {
	t.Helper()
	db := database.GetDB()

	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS user_git_binding (
			user_subject_id TEXT NOT NULL,
			tenant_id       TEXT NOT NULL DEFAULT 'default',
			git_uid         INTEGER,
			git_username    TEXT NOT NULL,
			provider_kind   TEXT NOT NULL DEFAULT 'gitea',
			sync_status     TEXT NOT NULL DEFAULT 'pending',
			last_synced_at  DATETIME,
			last_error      TEXT,
			created_at      DATETIME NOT NULL,
			updated_at      DATETIME NOT NULL,
			PRIMARY KEY (user_subject_id, tenant_id)
		)`,
		`CREATE TABLE IF NOT EXISTS user_credentials (
			user_subject_id TEXT PRIMARY KEY,
			tenant_id       TEXT NOT NULL,
			git_server_id   TEXT NOT NULL,
			git_username    TEXT NOT NULL,
			git_user_id     INTEGER NOT NULL,
			git_token_id    INTEGER NOT NULL,
			token_encrypted TEXT NOT NULL,
			token_sha256    TEXT NOT NULL,
			created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			rotated_at      DATETIME,
			revoked_at      DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS git_servers (
			server_id    TEXT PRIMARY KEY,
			kind         TEXT NOT NULL,
			endpoint     TEXT NOT NULL,
			display_name TEXT NOT NULL,
			config       TEXT NOT NULL DEFAULT '{}',
			is_template  INTEGER NOT NULL DEFAULT 0,
			enabled      INTEGER NOT NULL DEFAULT 1,
			created_at   DATETIME NOT NULL,
			updated_at   DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tenant_git_server_binding (
			tenant_id     TEXT PRIMARY KEY,
			git_server_id TEXT NOT NULL,
			bound_at      DATETIME NOT NULL,
			updated_at    DATETIME NOT NULL
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}

	gitea := newFakeForkGitea("admin-token")
	srv := httptest.NewServer(gitea)
	t.Cleanup(srv.Close)

	cfg := fmt.Sprintf(`{"admin_token":"admin-token","admin_user":"gitadmin","admin_password":"gitadminpw"}`)
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		 VALUES ('gs-1','gitea',?,'fake',?,0,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		srv.URL, cfg).Error; err != nil {
		t.Fatalf("seed git_servers: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at)
		 VALUES ('default','gs-1',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`).Error; err != nil {
		t.Fatalf("seed tenant binding: %v", err)
	}

	InitUserSpaceService(db, mustAESHandler(t), gitserver.NewDBResolver(db), nil)
	t.Cleanup(func() { InitUserSpaceService(nil, nil, nil, nil) })

	return &gitForkFixture{gitea: gitea, srv: srv, db: db}
}

// seedUserGitAccount gives the user a synced binding plus (optionally) a PAT.
func seedUserGitAccount(t *testing.T, db *gorm.DB, subjectID, gitUsername string, withPAT bool) {
	t.Helper()
	uid := int64(4242)
	if err := db.Create(&models.UserGitBinding{
		UserSubjectID: subjectID,
		TenantID:      "default",
		GitUID:        &uid,
		GitUsername:   gitUsername,
		ProviderKind:  "gitea",
		SyncStatus:    models.GitSyncStatusSynced,
	}).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	if !withPAT {
		return
	}
	enc, err := mustAESHandler(t).Seal([]byte("user-pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := db.Create(&models.UserCredentials{
		UserSubjectID: subjectID, TenantID: "default", GitServerID: "gs-1",
		GitUsername: gitUsername, GitUserID: uid, GitTokenID: 9,
		TokenEncrypted: enc, TokenSHA256: strings.Repeat("a", 64),
	}).Error; err != nil {
		t.Fatalf("seed creds: %v", err)
	}
}

// seedPluginSource creates an active public plugin item carrying the
// marketplace install metadata.
func seedPluginSource(t *testing.T, id, slug, marketplaceRepo string) {
	t.Helper()
	meta := datatypes.JSON([]byte(`{}`))
	if marketplaceRepo != "" {
		meta = datatypes.JSON([]byte(fmt.Sprintf(
			`{"install":{"method":"plugin_marketplace","plugin_name":"p","marketplace_name":"m","marketplace_repo":%q}}`,
			marketplaceRepo)))
	}
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: id, RegistryID: PublicRegistryID, RepoID: "public", Slug: slug,
		ItemType: "plugin", Name: "Plugin " + slug, Description: "d",
		Descriptions: datatypes.JSON([]byte(`{}`)), Category: "utilities",
		Version: "1.0.0", Content: "# plugin summary", Metadata: meta,
		SourcePath: ".plugin.json", SourceType: "direct", CreatedBy: "alice",
		CurrentRevision: 1, Status: "active",
	}).Error; err != nil {
		t.Fatalf("seed plugin: %v", err)
	}
}

type gitForkResp struct {
	forkTestResp
	ContentBackend string `json:"contentBackend"`
	SourceRepoURL  string `json:"sourceRepoUrl"`
	SourceRepoRef  string `json:"sourceRepoRef"`
	SourceRepoPath string `json:"sourceRepoPath"`
}

// ---------------------------------------------------------------- happy path

func TestForkItem_Git_HappyPath(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-1", "cospowers-requirements", "costrict-plugins-repo/cospowers-requirements")
	fx.gitea.repos["costrict-plugins-repo/cospowers-requirements"] = "main"

	// A bundled sub-skill: must NOT be copied for a git-backed fork.
	database.GetDB().Create(&models.CapabilityItem{
		ID: "child-1", RegistryID: PublicRegistryID, RepoID: "public", Slug: "child-skill",
		ItemType: "skill", Name: "Child", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "child body", Metadata: datatypes.JSON([]byte(`{}`)),
		SourceType: "direct", CreatedBy: "alice", CurrentRevision: 1, Status: "active",
		ParentPluginID: strPtr("plug-1"),
	})
	// And an asset on the source: must NOT be copied either.
	text := "asset body"
	database.GetDB().Create(&models.CapabilityAsset{
		ID: "asset-1", ItemID: "plug-1", RelPath: "README.md", TextContent: &text,
	})

	w := forkReq(newForkRouter("bob"), "plug-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.ContentBackend != "git" {
		t.Errorf("contentBackend: want git, got %q", resp.ContentBackend)
	}
	wantURL := fx.srv.URL + "/10001/cospowers-requirements"
	if resp.SourceRepoURL != wantURL {
		t.Errorf("sourceRepoUrl: want %q, got %q", wantURL, resp.SourceRepoURL)
	}
	if resp.SourceRepoRef != "main" {
		t.Errorf("sourceRepoRef: want main, got %q", resp.SourceRepoRef)
	}

	// The fork was signed with the USER's PAT, not the admin token — Gitea
	// forks to whoever owns the token.
	if fx.gitea.forkCount() != 1 {
		t.Fatalf("fork calls: want 1, got %d", fx.gitea.forkCount())
	}
	call := fx.gitea.forkCalls[0]
	if call.auth != "token user-pat" {
		t.Errorf("fork auth: want the user PAT, got %q", call.auth)
	}
	if call.srcOwner != "costrict-plugins-repo" || call.srcRepo != "cospowers-requirements" {
		t.Errorf("fork source: got %s/%s", call.srcOwner, call.srcRepo)
	}
	if strings.Contains(call.body, "organization") {
		t.Errorf("fork body must never target an organization: %s", call.body)
	}

	// Metadata only: no assets, no child items copied.
	var assetCount int64
	database.GetDB().Model(&models.CapabilityAsset{}).Where("item_id = ?", resp.ID).Count(&assetCount)
	if assetCount != 0 {
		t.Errorf("git-backed fork must not copy assets, got %d", assetCount)
	}
	var childForks int64
	database.GetDB().Model(&models.CapabilityItem{}).
		Where("forked_from_item_id = ?", "child-1").Count(&childForks)
	if childForks != 0 {
		t.Errorf("git-backed fork must not copy sub-items, got %d", childForks)
	}

	// The persisted row carries the coordinate.
	var stored models.CapabilityItem
	database.GetDB().First(&stored, "id = ?", resp.ID)
	if stored.ContentBackend != "git" || stored.SourceRepoURL != wantURL || stored.SourceRepoRef != "main" {
		t.Errorf("stored row: backend=%q url=%q ref=%q", stored.ContentBackend, stored.SourceRepoURL, stored.SourceRepoRef)
	}
	if stored.SourceGitServerID != "gs-1" || stored.SourceGitRepoID != 501 || stored.GitSyncStatus != "pending" {
		t.Errorf("stored Git identity: server=%q repo=%d sync=%q", stored.SourceGitServerID, stored.SourceGitRepoID, stored.GitSyncStatus)
	}
	if stored.ForkedFromItemID == nil || *stored.ForkedFromItemID != "plug-1" {
		t.Errorf("fork provenance missing: %+v", stored.ForkedFromItemID)
	}
}

func TestForkItem_Git_RejectsForkWithoutStableRepositoryID(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-no-repo-id", "no-repo-id", "costrict-plugins-repo/no-repo-id")
	fx.gitea.repos["costrict-plugins-repo/no-repo-id"] = "main"
	fx.gitea.forkResponseRepoID = 0

	w := forkReq(newForkRouter("bob"), "plug-no-repo-id")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("fork without repository identity: expected 502, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_FORK_REPO_ID_INVALID") {
		t.Errorf("expected invalid repository identity error, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "plug-no-repo-id", "bob")
}

// The Gitea coordinate of a mirrored plugin is <mirror owner>/<slug>; the
// upstream marketplace_repo (a GitHub org for most catalog plugins) is only
// used when the mirror has no repo of that name.
func TestForkItem_Git_PrefersMirrorSlugOverUpstreamRepo(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-2", "github-trending-pentest", "0xSteph/pentest-ai-agents")
	// Only the mirror exists; the upstream coordinate does not.
	fx.gitea.repos["costrict-plugins-repo/github-trending-pentest"] = "master"

	w := forkReq(newForkRouter("bob"), "plug-2")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SourceRepoRef != "master" {
		t.Errorf("ref should follow the source repo's default branch, got %q", resp.SourceRepoRef)
	}
	if fx.gitea.forkCalls[0].srcOwner != "costrict-plugins-repo" {
		t.Errorf("fork source owner: want the mirror, got %q", fx.gitea.forkCalls[0].srcOwner)
	}
}

// The mirror coordinate is a guess from the naming convention, so a repo that
// merely shares the slug is not proof: if its manifest names a DIFFERENT
// plugin, forking it would hand the user somebody else's content. The guess
// must be rejected and the next candidate tried.
func TestForkItem_Git_SkipsMirrorHoldingADifferentPlugin(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-imp", "common-name", "upstream-org/real-mirror")
	// A same-slug repo exists in the mirror namespace, but it holds another plugin.
	fx.gitea.repos["costrict-plugins-repo/common-name"] = "main"
	fx.gitea.manifests["costrict-plugins-repo/common-name"] = "some-other-plugin"
	// The upstream coordinate is the genuine one (seedPluginSource uses plugin_name "p").
	fx.gitea.repos["upstream-org/real-mirror"] = "main"
	fx.gitea.manifests["upstream-org/real-mirror"] = "p"

	w := forkReq(newForkRouter("bob"), "plug-imp")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	if len(fx.gitea.forkCalls) == 0 {
		t.Fatal("no fork was attempted")
	}
	if got := fx.gitea.forkCalls[0].srcOwner + "/" + fx.gitea.forkCalls[0].srcRepo; got != "upstream-org/real-mirror" {
		t.Errorf("must skip the impostor and fork the real mirror, forked %q instead", got)
	}
}

// A per-item mirror is the catalog's authoritative coordinate. When that repo
// exists but is an empty shell, falling through to marketplace_repo would hide
// corrupted mirror state and fork a different source. Only a readable manifest
// for a different plugin is treated as a harmless same-name collision.
func TestForkItem_Git_EmptyPerItemMirrorRejectsBeforeMarketplaceFallback(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-empty-mirror", "empty-mirror", "upstream-org/valid-plugin")

	fx.gitea.repos["costrict-plugins-repo/empty-mirror"] = "main"
	fx.gitea.unreadableManifests["costrict-plugins-repo/empty-mirror"] = true
	// This would be a valid lower-priority candidate if the mirror were merely
	// a same-name collision. It must not be considered for an empty mirror.
	fx.gitea.repos["upstream-org/valid-plugin"] = "main"
	fx.gitea.manifests["upstream-org/valid-plugin"] = "p"

	w := forkReq(newForkRouter("bob"), "plug-empty-mirror")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_SOURCE_MANIFEST_INVALID") {
		t.Errorf("expected manifest error, got %s", w.Body.String())
	}
	if fx.gitea.forkCount() != 0 {
		t.Errorf("must not fork the marketplace fallback after an empty mirror: %d", fx.gitea.forkCount())
	}
	assertNoForkPersisted(t, "plug-empty-mirror", "bob")
}

// --------------------------------------------------------- main file probe

// The "edit in Gitea" hand-off links straight at the plugin manifest, and Gitea
// 404s on a path that isn't there — so the path must be the one the repo really
// serves. The catalog's source_path (.plugin.json) is no substitute: mirrored
// repos overwhelmingly use .claude-plugin/plugin.json.
func TestForkItem_Git_PersistsProbedManifestPath(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-path", "layout-nested", "costrict-plugins-repo/layout-nested")
	fx.gitea.repos["costrict-plugins-repo/layout-nested"] = "main"
	fx.gitea.manifests["costrict-plugins-repo/layout-nested"] = "p"
	fx.gitea.manifestPaths["costrict-plugins-repo/layout-nested"] = ".claude-plugin/plugin.json"

	w := forkReq(newForkRouter("bob"), "plug-path")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SourceRepoPath != ".claude-plugin/plugin.json" {
		t.Errorf("sourceRepoPath: want the probed path, got %q", resp.SourceRepoPath)
	}
	var stored models.CapabilityItem
	database.GetDB().First(&stored, "id = ?", resp.ID)
	if stored.SourceRepoPath != ".claude-plugin/plugin.json" {
		t.Errorf("stored source_repo_path: got %q", stored.SourceRepoPath)
	}
}

// A repo using the flat layout is probed down the candidate list, so the path
// recorded is the second one, not the first that was tried.
func TestForkItem_Git_ProbeFallsThroughToFlatLayout(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-flat", "layout-flat", "costrict-plugins-repo/layout-flat")
	fx.gitea.repos["costrict-plugins-repo/layout-flat"] = "main"
	fx.gitea.manifests["costrict-plugins-repo/layout-flat"] = "p"
	fx.gitea.manifestPaths["costrict-plugins-repo/layout-flat"] = "plugin.json"

	w := forkReq(newForkRouter("bob"), "plug-flat")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SourceRepoPath != "plugin.json" {
		t.Errorf("sourceRepoPath: want plugin.json, got %q", resp.SourceRepoPath)
	}
}

// An existing repository with no readable manifest is not a mirror of a
// usable plugin. It must fail closed instead of creating a git-backed dead
// shell or silently falling through to a DB fork.
func TestForkItem_Git_EmptySourceRepoIsRejectedBeforeFork(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-nopath", "layout-unknown", "costrict-plugins-repo/layout-unknown")
	fx.gitea.repos["costrict-plugins-repo/layout-unknown"] = "main"
	fx.gitea.unreadableManifests["costrict-plugins-repo/layout-unknown"] = true

	w := forkReq(newForkRouter("bob"), "plug-nopath")
	if w.Code != http.StatusConflict {
		t.Fatalf("empty source repo: expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_SOURCE_MANIFEST_INVALID") {
		t.Errorf("expected manifest error, got %s", w.Body.String())
	}
	if fx.gitea.forkCount() != 0 {
		t.Errorf("must reject before Gitea fork, got %d calls", fx.gitea.forkCount())
	}
	assertNoForkPersisted(t, "plug-nopath", "bob")
}

// Forking a fork probes the item's own (trusted) repo too — a trusted
// coordinate skips the *verdict*, not the path lookup, otherwise every fork of
// a fork would lose the edit link.
func TestForkItem_Git_ProbesTrustedCoordinateForPath(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "carol", "10001", true)
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: "fork-src3", RegistryID: PublicRegistryID, RepoID: "public", Slug: "plug-fork-2345",
		ItemType: "plugin", Name: "Bob's fork", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "# summary", Metadata: datatypes.JSON([]byte(`{}`)), SourcePath: ".plugin.json",
		SourceType: "fork", CreatedBy: "bob", CurrentRevision: 1, Status: "active",
		ContentBackend: "git", SourceRepoURL: fx.srv.URL + "/10002/plug", SourceRepoRef: "main",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.gitea.repos["10002/plug"] = "main"
	// Git-backed sources must retain the authoritative manifest identity too;
	// a persisted coordinate alone is not enough to accept its content.
	if err := database.GetDB().Model(&models.CapabilityItem{}).
		Where("id = ?", "fork-src3").
		Update("metadata", datatypes.JSON([]byte(`{"install":{"plugin_name":"p"}}`))).Error; err != nil {
		t.Fatalf("set metadata: %v", err)
	}
	fx.gitea.manifests["10002/plug"] = "p"
	fx.gitea.manifestPaths["10002/plug"] = ".claude-plugin/plugin.json"

	w := forkReq(newForkRouter("carol"), "fork-src3")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SourceRepoPath != ".claude-plugin/plugin.json" {
		t.Errorf("a trusted coordinate must still yield its manifest path, got %q", resp.SourceRepoPath)
	}
}

// A source_repo_url is a routing hint, not permanent proof of content. If a
// user later replaces that repository's manifest, a fork must stop instead of
// accepting the stale trusted coordinate or finding a different guessed repo.
func TestForkItem_Git_TrustedCoordinateWithWrongManifestIsRejected(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "carol", "10001", true)
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: "fork-src-invalid", RegistryID: PublicRegistryID, RepoID: "public", Slug: "plug-fork-invalid",
		ItemType: "plugin", Name: "Bob's fork", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "# summary", Metadata: datatypes.JSON([]byte(`{"install":{"plugin_name":"expected"}}`)), SourcePath: ".plugin.json",
		SourceType: "fork", CreatedBy: "bob", CurrentRevision: 1, Status: "active",
		ContentBackend: "git", SourceRepoURL: fx.srv.URL + "/10002/plug", SourceRepoRef: "main",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.gitea.repos["10002/plug"] = "main"
	fx.gitea.manifests["10002/plug"] = "another-plugin"

	w := forkReq(newForkRouter("carol"), "fork-src-invalid")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_SOURCE_MANIFEST_INVALID") {
		t.Errorf("expected manifest error, got %s", w.Body.String())
	}
	if fx.gitea.forkCount() != 0 {
		t.Errorf("must not fork a trusted coordinate with stale content: %d", fx.gitea.forkCount())
	}
	assertNoForkPersisted(t, "fork-src-invalid", "carol")
}

// ------------------------------------------------------------- idempotency

func TestForkItem_Git_ForkConflictReusesExistingRepo(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-3", "already-forked", "costrict-plugins-repo/already-forked")
	fx.gitea.repos["costrict-plugins-repo/already-forked"] = "main"
	// The user already has the fork from an earlier attempt whose DB write failed.
	fx.gitea.repos["10001/already-forked"] = "main"
	fx.gitea.forkParents["10001/already-forked"] = "costrict-plugins-repo/already-forked"
	fx.gitea.forkStatus = http.StatusConflict
	fx.gitea.forkCreatesRepo = false

	w := forkReq(newForkRouter("bob"), "plug-3")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.SourceRepoURL != fx.srv.URL+"/10001/already-forked" {
		t.Errorf("should reuse the existing repo, got %q", resp.SourceRepoURL)
	}
	if fx.gitea.forkCount() != 1 {
		t.Errorf("fork attempts: want 1, got %d", fx.gitea.forkCount())
	}
}

// A fork lands under the SOURCE's bare name, so an unrelated repo of the
// caller's carrying that same name (bare names like "mcp-server" recur across
// upstreams) also triggers Gitea's 409. Accepting it would point this item at
// somebody else's repository and persist that as the content truth, so the
// request must fail instead of silently "reusing" the clashing repo.
func TestForkItem_Git_ConflictWithUnrelatedRepoIsRejected(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-clash", "shared-name", "costrict-plugins-repo/shared-name")
	fx.gitea.repos["costrict-plugins-repo/shared-name"] = "main"
	// Same bare name in the user's namespace, but a fork of a DIFFERENT source.
	fx.gitea.repos["10001/shared-name"] = "main"
	fx.gitea.forkParents["10001/shared-name"] = "someone-else/shared-name"
	fx.gitea.forkStatus = http.StatusConflict
	fx.gitea.forkCreatesRepo = false

	w := forkReq(newForkRouter("bob"), "plug-clash")
	if w.Code == http.StatusCreated {
		t.Fatalf("fork must not adopt an unrelated same-name repo, got 201 (%s)", w.Body.String())
	}
	var count int64
	fx.db.Model(&models.CapabilityItem{}).
		Where("forked_from_item_id = ?", "plug-clash").Count(&count)
	if count != 0 {
		t.Errorf("no item may be written when lineage check fails, got %d", count)
	}
}

// Re-forking the same source returns the existing item and never touches Gitea
// a second time.
func TestForkItem_Git_DuplicateForkReturnsExistingItem(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-4", "dup-plugin", "costrict-plugins-repo/dup-plugin")
	fx.gitea.repos["costrict-plugins-repo/dup-plugin"] = "main"

	r := newForkRouter("bob")
	first := forkReq(r, "plug-4")
	if first.Code != http.StatusCreated {
		t.Fatalf("first fork: %d (%s)", first.Code, first.Body.String())
	}
	var firstResp gitForkResp
	_ = json.Unmarshal(first.Body.Bytes(), &firstResp)

	second := forkReq(r, "plug-4")
	if second.Code != http.StatusOK {
		t.Fatalf("second fork: expected 200, got %d (%s)", second.Code, second.Body.String())
	}
	var secondResp gitForkResp
	_ = json.Unmarshal(second.Body.Bytes(), &secondResp)
	if secondResp.ID != firstResp.ID {
		t.Errorf("re-fork should return the existing item %q, got %q", firstResp.ID, secondResp.ID)
	}
	if fx.gitea.forkCount() != 1 {
		t.Errorf("re-fork must not hit Gitea again: %d calls", fx.gitea.forkCount())
	}
	var itemCount int64
	database.GetDB().Model(&models.CapabilityItem{}).
		Where("forked_from_item_id = ? AND created_by = ?", "plug-4", "bob").Count(&itemCount)
	if itemCount != 1 {
		t.Errorf("expected exactly 1 fork item, got %d", itemCount)
	}
}

// ----------------------------------------------------------- failure modes

// A user whose Gitea account isn't provisioned gets an explicit 409 — never a
// silent DB copy that looks like success but produces no repo.
func TestForkItem_Git_BindingNotReadyFailsLoudly(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedPluginSource(t, "plug-5", "no-binding", "costrict-plugins-repo/no-binding")
	fx.gitea.repos["costrict-plugins-repo/no-binding"] = "main"

	// Case 1: no binding row at all.
	w := forkReq(newForkRouter("bob"), "plug-5")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_ACCOUNT_NOT_READY") {
		t.Errorf("expected GIT_ACCOUNT_NOT_READY, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "plug-5", "bob")

	// Case 2: binding exists but is still pending.
	if err := fx.db.Create(&models.UserGitBinding{
		UserSubjectID: "carol", TenantID: "default", GitUsername: "10002",
		ProviderKind: "gitea", SyncStatus: "pending",
	}).Error; err != nil {
		t.Fatalf("seed pending binding: %v", err)
	}
	w2 := forkReq(newForkRouter("carol"), "plug-5")
	if w2.Code != http.StatusConflict {
		t.Fatalf("pending binding: expected 409, got %d (%s)", w2.Code, w2.Body.String())
	}
	assertNoForkPersisted(t, "plug-5", "carol")
	if fx.gitea.forkCount() != 0 {
		t.Errorf("must not call Gitea without a ready account: %d calls", fx.gitea.forkCount())
	}
}

// A fork request must not use subject_id as a substitute for cs-user's
// ShortID. Even with a provisioner wired, account creation is event/reconciler
// driven; otherwise an old subject[:8] guess can permanently reserve another
// user's Gitea namespace.
func TestForkItem_Git_MissingBindingDoesNotProvisionWithGuessedShortID(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedPluginSource(t, "plug-no-guess", "no-guess", "costrict-plugins-repo/no-guess")
	fx.gitea.repos["costrict-plugins-repo/no-guess"] = "main"

	// If ForkItem invoked this provisioner, it would at least insert a pending
	// binding before its fake Gitea CreateUser call failed. A valid fork path
	// must never invoke it without an authoritative ShortID.
	provisioner := gitsync.NewUserProvisionService(fx.db, gitserver.NewDBResolver(fx.db), nil, nil)
	InitUserSpaceService(fx.db, mustAESHandler(t), gitserver.NewDBResolver(fx.db), provisioner)

	w := forkReq(newForkRouter("usr_550e8400-e29b-41d4-a716-446655440000"), "plug-no-guess")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_ACCOUNT_NOT_READY") {
		t.Errorf("expected account-not-ready response, got %s", w.Body.String())
	}
	var count int64
	if err := fx.db.Model(&models.UserGitBinding{}).
		Where("user_subject_id = ?", "usr_550e8400-e29b-41d4-a716-446655440000").
		Count(&count).Error; err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if count != 0 {
		t.Errorf("fork must not create a guessed binding, found %d", count)
	}
	if fx.gitea.forkCount() != 0 {
		t.Errorf("must not call Gitea fork without a ready binding: %d", fx.gitea.forkCount())
	}
}

// A provisioned account without credentials gets a PAT minted on the fly.
func TestForkItem_Git_MissingPATIsMinted(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", false) // binding only, no PAT
	seedPluginSource(t, "plug-6", "needs-pat", "costrict-plugins-repo/needs-pat")
	fx.gitea.repos["costrict-plugins-repo/needs-pat"] = "main"

	w := forkReq(newForkRouter("bob"), "plug-6")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	if fx.gitea.tokenCalls != 1 {
		t.Errorf("expected exactly one PAT mint, got %d", fx.gitea.tokenCalls)
	}
	var creds models.UserCredentials
	if err := fx.db.First(&creds, "user_subject_id = ?", "bob").Error; err != nil {
		t.Fatalf("credentials should have been persisted: %v", err)
	}
	if fx.gitea.forkCalls[0].auth != "token minted-pat" {
		t.Errorf("fork should use the freshly minted PAT, got %q", fx.gitea.forkCalls[0].auth)
	}
}

// Gitea down → the request fails and nothing is written to the DB.
func TestForkItem_Git_UnreachableLeavesNoItem(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-7", "gitea-down", "costrict-plugins-repo/gitea-down")
	fx.gitea.repos["costrict-plugins-repo/gitea-down"] = "main"
	fx.srv.Close() // everything now fails at the transport layer

	w := forkReq(newForkRouter("bob"), "plug-7")
	if w.Code < 400 {
		t.Fatalf("expected a failure status, got %d (%s)", w.Code, w.Body.String())
	}
	assertNoForkPersisted(t, "plug-7", "bob")
}

// The fork is refused when the source repo lookup fails for a reason other
// than "not found" — failing closed beats silently producing a DB copy.
func TestForkItem_Git_SourceLookupErrorFailsClosed(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-8", "boom", "costrict-plugins-repo/boom")

	// Swap the git server endpoint for one that 500s on every lookup.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	if err := fx.db.Exec(`UPDATE git_servers SET endpoint = ? WHERE server_id = 'gs-1'`, broken.URL).Error; err != nil {
		t.Fatalf("update endpoint: %v", err)
	}

	w := forkReq(newForkRouter("bob"), "plug-8")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", w.Code, w.Body.String())
	}
	assertNoForkPersisted(t, "plug-8", "bob")
}

// ------------------------------------------------------------- regressions

// Non-plugin items keep the legacy DB fork verbatim, even with the git stack
// fully wired.
func TestForkItem_Git_NonPluginKeepsDBFork(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedForkSourceItem("skill-1", "my-skill", "alice", "direct", "public")
	text := "asset body"
	database.GetDB().Create(&models.CapabilityAsset{
		ID: "asset-s1", ItemID: "skill-1", RelPath: "SKILL.md", TextContent: &text,
	})

	w := forkReq(newForkRouter("bob"), "skill-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// The column default must apply when the git path doesn't run — an empty
	// content_backend would mean the insert wrote a literal '' instead.
	if resp.ContentBackend != "db" || resp.SourceRepoURL != "" {
		t.Errorf("skill fork must stay db-backed: backend=%q url=%q", resp.ContentBackend, resp.SourceRepoURL)
	}
	if resp.Content != "original content" {
		t.Errorf("content must still be copied, got %q", resp.Content)
	}
	var assetCount int64
	database.GetDB().Model(&models.CapabilityAsset{}).Where("item_id = ?", resp.ID).Count(&assetCount)
	if assetCount != 1 {
		t.Errorf("db fork must copy assets, got %d", assetCount)
	}
	if fx.gitea.forkCount() != 0 {
		t.Errorf("non-plugin fork must not touch Gitea: %d calls", fx.gitea.forkCount())
	}
}

// A plugin without marketplace metadata, and one whose mirror doesn't exist,
// both stay on the DB path (children and assets copied as before).
func TestForkItem_Git_PluginWithoutGiteaSourceKeepsDBFork(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-9", "no-metadata", "")      // no install block
	seedPluginSource(t, "plug-10", "not-mirrored", "u/r") // metadata but no repo on Gitea
	database.GetDB().Create(&models.CapabilityItem{
		ID: "child-9", RegistryID: PublicRegistryID, RepoID: "public", Slug: "child-of-9",
		ItemType: "skill", Name: "Child9", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "child body", Metadata: datatypes.JSON([]byte(`{}`)),
		SourceType: "direct", CreatedBy: "alice", CurrentRevision: 1, Status: "active",
		ParentPluginID: strPtr("plug-9"),
	})

	for _, id := range []string{"plug-9", "plug-10"} {
		w := forkReq(newForkRouter("bob"), id)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: expected 201, got %d (%s)", id, w.Code, w.Body.String())
		}
		var resp gitForkResp
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.ContentBackend == "git" {
			t.Errorf("%s should stay db-backed, got %q", id, resp.ContentBackend)
		}
	}
	// Legacy behavior intact: sub-items still forked for the DB path.
	var childForks int64
	database.GetDB().Model(&models.CapabilityItem{}).
		Where("forked_from_item_id = ?", "child-9").Count(&childForks)
	if childForks != 1 {
		t.Errorf("db fork must copy sub-items, got %d", childForks)
	}
	if fx.gitea.forkCount() != 0 {
		t.Errorf("no fork call expected: %d", fx.gitea.forkCount())
	}
}

// Without the personal-space wiring the feature is simply off: plugins fork
// through the legacy DB path exactly as before.
func TestForkItem_Git_FeatureUnwiredKeepsDBFork(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	seedPluginSource(t, "plug-11", "unwired", "costrict-plugins-repo/unwired")

	w := forkReq(newForkRouter("bob"), "plug-11")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ContentBackend == "git" {
		t.Errorf("unwired deployment must keep the db fork, got %q", resp.ContentBackend)
	}
}

// Forking an already git-backed item forks ITS repo — a DB copy would produce
// an item with no content at all, since git-backed rows carry no assets.
func TestForkItem_Git_ForkOfForkUsesSourceRepo(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "carol", "10001", true)
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: "fork-src", RegistryID: PublicRegistryID, RepoID: "public", Slug: "plug-fork-abcd",
		ItemType: "plugin", Name: "Bob's fork", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "# summary", Metadata: datatypes.JSON([]byte(`{"install":{"plugin_name":"p"}}`)), SourcePath: ".plugin.json",
		SourceType: "fork", CreatedBy: "bob", CurrentRevision: 1, Status: "active",
		ContentBackend: "git", SourceRepoURL: fx.srv.URL + "/10002/plug", SourceRepoRef: "main",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.gitea.repos["10002/plug"] = "main"

	w := forkReq(newForkRouter("carol"), "fork-src")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ContentBackend != "git" {
		t.Errorf("fork of a git-backed item must stay git-backed, got %q", resp.ContentBackend)
	}
	if fx.gitea.forkCalls[0].srcOwner != "10002" || fx.gitea.forkCalls[0].srcRepo != "plug" {
		t.Errorf("should fork the source item's own repo, got %s/%s",
			fx.gitea.forkCalls[0].srcOwner, fx.gitea.forkCalls[0].srcRepo)
	}
}

// ...and when git backing is unavailable, that fork is refused rather than
// silently degraded into an empty DB item.
func TestForkItem_Git_ForkOfForkRefusesWhenGitUnavailable(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: "fork-src2", RegistryID: PublicRegistryID, RepoID: "public", Slug: "plug-fork-ef01",
		ItemType: "plugin", Name: "Bob's fork", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "# summary", Metadata: datatypes.JSON([]byte(`{}`)), SourcePath: ".plugin.json",
		SourceType: "fork", CreatedBy: "bob", CurrentRevision: 1, Status: "active",
		ContentBackend: "git", SourceRepoURL: "https://git.example/10002/plug", SourceRepoRef: "main",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := forkReq(newForkRouter("carol"), "fork-src2")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_BACKING_UNAVAILABLE") {
		t.Errorf("expected GIT_BACKING_UNAVAILABLE, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "fork-src2", "carol")
}

// A git-backed plugin must not be served as a zip of its (absent) DB assets.
func TestDownloadPluginZip_GitBackedRefuses(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: "git-plug", RegistryID: PublicRegistryID, RepoID: "public", Slug: "git-plug",
		ItemType: "plugin", Name: "Git Plugin", Descriptions: datatypes.JSON([]byte(`{}`)),
		Content: "# summary", Metadata: datatypes.JSON([]byte(`{}`)), SourcePath: ".plugin.json",
		SourceType: "fork", CreatedBy: "bob", CurrentRevision: 1, Status: "active",
		ContentBackend: "git", SourceRepoURL: "https://git.example/10001/git-plug", SourceRepoRef: "main",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/plugins/:slug/download", DownloadPluginZip)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/plugins/git-plug/download", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "GIT_BACKED_ITEM") || !strings.Contains(body, "archive/main.zip") {
		t.Errorf("response should point at the repo archive: %s", body)
	}
}

// ---------------------------------------------------------------- helpers

func assertNoForkPersisted(t *testing.T, srcID, userID string) {
	t.Helper()
	var count int64
	database.GetDB().Model(&models.CapabilityItem{}).
		Where("forked_from_item_id = ? AND created_by = ?", srcID, userID).Count(&count)
	if count != 0 {
		t.Errorf("a failed git fork must leave no item behind, found %d", count)
	}
}

func strPtr(s string) *string { return &s }

// createGitCapabilitySyncJobTable mirrors the production DDL for the sync-job
// queue. setupTestDB only builds the capability tables, and the fork path is
// the one non-webhook producer of jobs, so the table has to exist here for the
// enqueue to be observable rather than swallowed as a best-effort failure.
func createGitCapabilitySyncJobTable(t *testing.T) {
	t.Helper()
	ddl := `CREATE TABLE IF NOT EXISTS git_capability_sync_jobs (
		id TEXT PRIMARY KEY,
		git_server_id TEXT NOT NULL,
		delivery_id TEXT NOT NULL,
		repo_id INTEGER NOT NULL,
		repo_full_name TEXT NOT NULL,
		default_branch TEXT NOT NULL,
		ref TEXT NOT NULL,
		before_sha TEXT NOT NULL DEFAULT '',
		after_sha TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		retry_count INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		last_error TEXT,
		scheduled_at DATETIME NOT NULL,
		started_at DATETIME,
		lease_token TEXT NOT NULL DEFAULT '',
		finished_at DATETIME,
		created_at DATETIME NOT NULL,
		CONSTRAINT uq_git_capability_sync_jobs_delivery
			UNIQUE (git_server_id, delivery_id)
	)`
	if err := database.GetDB().Exec(ddl).Error; err != nil {
		t.Fatalf("create git_capability_sync_jobs: %v", err)
	}
}

// Forking creates a repository but pushes nothing, so Gitea never delivers a
// push webhook for it — and the webhook ingress is the only other producer of
// sync jobs. Without the fork queueing its own first sync the row stays
// git_sync_status='pending' with an empty git_sha forever, which the
// Marketplace projection filters out: the fork would be silently unusable.
func TestForkItem_Git_QueuesInitialSyncJob(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-1", "cospowers-requirements", "costrict-plugins-repo/cospowers-requirements")
	fx.gitea.repos["costrict-plugins-repo/cospowers-requirements"] = "main"
	createGitCapabilitySyncJobTable(t)

	w := forkReq(newForkRouter("bob"), "plug-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var jobs []models.GitCapabilitySyncJob
	if err := database.GetDB().Find(&jobs).Error; err != nil {
		t.Fatalf("load sync jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want exactly one queued sync job, got %d", len(jobs))
	}
	job := jobs[0]

	// Derived from the item ID so a retried fork collapses onto the same job
	// through the (git_server_id, delivery_id) unique index.
	if want := "fork:" + resp.ID; job.DeliveryID != want {
		t.Errorf("delivery id: want %q, got %q", want, job.DeliveryID)
	}
	// The worker locates the repository by stable numeric ID, not by name.
	if job.GitServerID != "gs-1" || job.RepoID != 501 {
		t.Errorf("job identity: server=%q repo=%d", job.GitServerID, job.RepoID)
	}
	if job.RepoFullName != "10001/cospowers-requirements" {
		t.Errorf("repo full name: got %q", job.RepoFullName)
	}
	if job.DefaultBranch != "main" || job.Ref != "refs/heads/main" {
		t.Errorf("branch: default=%q ref=%q", job.DefaultBranch, job.Ref)
	}
	if job.Status != models.GitCapabilitySyncJobStatusPending {
		t.Errorf("status: want pending, got %q", job.Status)
	}
	// An all-zero SHA is the wire encoding for "default branch deleted"; the
	// worker would archive the very item this fork just published.
	if job.AfterSHA == strings.Repeat("0", 40) {
		t.Error("after_sha must not be the branch-deletion sentinel")
	}
}

// The legacy DB fork owns its content outright — there is no repository to
// index, so queueing a sync job would leave the worker chasing a repo that
// does not exist.
func TestForkItem_DBFork_QueuesNoSyncJob(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedPluginSource(t, "plug-10", "not-mirrored", "u/r") // metadata, but no repo on Gitea
	createGitCapabilitySyncJobTable(t)

	w := forkReq(newForkRouter("bob"), "plug-10")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp gitForkResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ContentBackend == "git" {
		t.Fatalf("precondition: expected a db-backed fork, got %q", resp.ContentBackend)
	}

	var count int64
	if err := database.GetDB().Model(&models.GitCapabilitySyncJob{}).Count(&count).Error; err != nil {
		t.Fatalf("count sync jobs: %v", err)
	}
	if count != 0 {
		t.Errorf("db-backed fork must not queue a sync job, got %d", count)
	}
}
