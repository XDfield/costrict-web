// Git-backed forks beyond plugins: skill / subagent / command / mcp.
//
// The gate that kept these four on the DB fork path is gone, so what these
// tests pin is that opening it did not leave any of the four guards behind:
//
//	G1 the fork is signed with the USER's PAT — Gitea forks to whoever owns the
//	   token, so an admin token would build the repo in the wrong namespace;
//	G2 a 409 is only accepted as "already forked" after checking `parent`, since
//	   Gitea answers an unrelated same-name repo identically;
//	G3 a candidate repository must prove it holds THIS capability;
//	G4 the fork queues its own first index pass, because forking pushes nothing
//	   and no webhook will ever arrive for it.
//
// Each is asserted per type rather than once, which is the whole point: the
// guards were written for plugins and it is the sharing that has to be proven.

package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

// forkSlugFor mirrors the fork slug ForkItem derives. A provisioned fork names
// its repository after that slug, so a test that has to pre-create the target
// repository needs the same value.
func forkSlugFor(t *testing.T, srcSlug, userID string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(userID))
	return fmt.Sprintf("%s-fork-%x", srcSlug, sum[:4])
}

// gitBackedType describes one standalone capability type end to end: where its
// manifest lives in a repository, and what a valid manifest for it looks like.
type gitBackedType struct {
	itemType     string
	manifestPath string
	// capabilityName is what the manifest declares and what the row's `name`
	// carries — the pair G3 compares.
	capabilityName string
	entryKey       string
	manifest       string
}

func gitBackedTypeCases() []gitBackedType {
	return []gitBackedType{
		{
			itemType: "skill", manifestPath: "skill.md", capabilityName: "fix-01-skill",
			manifest: "---\nname: fix-01-skill\ndescription: a skill\nversion: 1.0.0\n---\n\n# body\n",
		},
		{
			itemType: "subagent", manifestPath: "agent.md", capabilityName: "fix-02-agent",
			manifest: "---\nname: fix-02-agent\ndescription: an agent\nversion: 1.0.0\n---\n\n# body\n",
		},
		{
			itemType: "command", manifestPath: "command.md", capabilityName: "fix-03-command",
			manifest: "---\nname: fix-03-command\ndescription: a command\nversion: 1.0.0\n---\n\n# body\n",
		},
		{
			itemType: "mcp", manifestPath: "mcp.json", capabilityName: "fix-04-mcp", entryKey: "fix-04-mcp",
			manifest: "{\n  \"mcpServers\": {\n    \"fix-04-mcp\": {\n      \"command\": \"npx\",\n      \"name\": \"fix-04-mcp\"\n    }\n  }\n}\n",
		},
	}
}

// seedGitBackedSource creates a source item whose content already lives in a
// repository, plus that repository on the fake Gitea.
func seedGitBackedSource(t *testing.T, fx *gitForkFixture, id, slug, repoFullName string, tc gitBackedType) {
	t.Helper()
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: id, RegistryID: PublicRegistryID, RepoID: "public", Slug: slug,
		ItemType: tc.itemType, Name: tc.capabilityName, Description: "d",
		Descriptions: datatypes.JSON([]byte(`{}`)), Category: "utilities", Version: "1.0.0",
		Metadata: datatypes.JSON([]byte(`{}`)), SourcePath: tc.manifestPath,
		SourceType: "git", CreatedBy: "alice", CurrentRevision: 1, Status: "active",
		ContentBackend:    "git",
		SourceRepoURL:     fx.srv.URL + "/" + repoFullName,
		SourceRepoRef:     "main",
		SourceRepoPath:    tc.manifestPath,
		SourceGitServerID: "gs-1",
		SourceGitRepoID:   4242,
		SourceGitEntryKey: tc.entryKey,
		GitSyncStatus:     "synced",
	}).Error; err != nil {
		t.Fatalf("seed %s source: %v", tc.itemType, err)
	}
	fx.gitea.repos[repoFullName] = "main"
	// The row names repository 4242; the fake has to answer for that id or the
	// visibility gate reads "this repository is gone" and refuses the fork.
	fx.gitea.registerRepoID(repoFullName, 4242)
	fx.gitea.putFile(repoFullName, tc.manifestPath, []byte(tc.manifest))
}

// A git-backed source of every type forks into the caller's Gitea namespace and
// produces a fully coordinated git-backed row — never a DB copy. After
// read-through a DB copy of a git-backed source is not merely stale: the source
// row carries no content at all, so the copy would be empty.
func TestForkItem_Git_AllTypesForkToGitea(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			createGitCapabilitySyncJobTable(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code != http.StatusCreated {
				t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
			}
			var resp gitForkResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}

			var stored models.CapabilityItem
			database.GetDB().First(&stored, "id = ?", resp.ID)
			if stored.ContentBackend != "git" {
				t.Fatalf("fork of a git-backed %s must stay git-backed, got %q", tc.itemType, stored.ContentBackend)
			}
			wantURL := fx.srv.URL + "/10001/" + tc.capabilityName
			if stored.SourceRepoURL != wantURL {
				t.Errorf("repo url: want %q, got %q", wantURL, stored.SourceRepoURL)
			}
			if stored.SourceRepoRef != "main" || stored.SourceRepoPath != tc.manifestPath {
				t.Errorf("coordinate: ref=%q path=%q", stored.SourceRepoRef, stored.SourceRepoPath)
			}
			if stored.SourceGitServerID != "gs-1" || stored.SourceGitRepoID <= 0 {
				t.Errorf("git identity: server=%q repo=%d", stored.SourceGitServerID, stored.SourceGitRepoID)
			}
			// An MCP row is matched on (path, entry key). Losing the key makes the
			// very next push find no match and archive the row.
			if stored.SourceGitEntryKey != tc.entryKey {
				t.Errorf("entry key: want %q, got %q", tc.entryKey, stored.SourceGitEntryKey)
			}
			// The row is an index entry, not a copy.
			if stored.Content != "" {
				t.Errorf("git-backed fork must store no content, got %q", stored.Content)
			}
			if len(stored.ContentMD5) != 64 {
				t.Errorf("content hash must be derived from the repository bytes, got %q", stored.ContentMD5)
			}

			// G1: signed with the user's PAT.
			if fx.gitea.forkCount() != 1 {
				t.Fatalf("fork calls: want 1, got %d", fx.gitea.forkCount())
			}
			if auth := fx.gitea.forkCalls[0].auth; auth != "token user-pat" {
				t.Errorf("G1: fork must use the user PAT, got %q", auth)
			}

			// G4: the first index pass is queued, keyed on the item id.
			var jobs []models.GitCapabilitySyncJob
			database.GetDB().Find(&jobs)
			if len(jobs) != 1 {
				t.Fatalf("G4: want exactly one queued sync job, got %d", len(jobs))
			}
			if want := "fork:" + resp.ID; jobs[0].DeliveryID != want {
				t.Errorf("G4: delivery id want %q, got %q", want, jobs[0].DeliveryID)
			}
			if jobs[0].RepoFullName != "10001/"+tc.capabilityName {
				t.Errorf("G4: job repo %q", jobs[0].RepoFullName)
			}
		})
	}
}

// G1, stated as its own case: the admin token is present and usable, and is
// still not what signs the fork. Gitea has no "fork into user X" parameter, so
// signing with the admin PAT would silently create the repository in the
// administrator's namespace.
func TestForkItem_Git_AllTypesForkWithUserPATNotAdminToken(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)

			if w := forkReq(newForkRouter("bob"), "src-1"); w.Code != http.StatusCreated {
				t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
			}
			call := fx.gitea.forkCalls[0]
			if call.auth == "token admin-token" {
				t.Fatalf("G1: fork was signed with the admin token")
			}
			if call.auth != "token user-pat" {
				t.Errorf("G1: expected the user PAT, got %q", call.auth)
			}
			// `organization` would redirect the fork to an org namespace.
			if strings.Contains(call.body, "organization") {
				t.Errorf("G1: fork body must never target an organization: %s", call.body)
			}
		})
	}
}

// G2: Gitea answers "you already forked this" and "you own an unrelated repo of
// that name" with the same 409, so the lineage in `parent` is the only thing
// that tells them apart. Accepting the unrelated repo would bind this item to
// somebody else's content and record it as truth.
func TestForkItem_Git_AllTypesRejectUnrelatedSameNameRepo(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)

			// Same bare name in the caller's namespace, but a fork of something else.
			fx.gitea.repos["10001/"+tc.capabilityName] = "main"
			fx.gitea.forkParents["10001/"+tc.capabilityName] = "someone-else/" + tc.capabilityName
			fx.gitea.forkStatus = http.StatusConflict
			fx.gitea.forkCreatesRepo = false

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code == http.StatusCreated {
				t.Fatalf("G2: must not adopt an unrelated same-name repo, got 201 (%s)", w.Body.String())
			}
			assertNoForkPersisted(t, "src-1", "bob")
		})
	}
}

// G2, recovery half: a repository that IS a fork of this source is adopted, so
// a retry after a failed DB write converges instead of piling up repositories.
func TestForkItem_Git_AllTypesRecoverRealForkOnConflict(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)

			fx.gitea.repos["10001/"+tc.capabilityName] = "main"
			fx.gitea.forkParents["10001/"+tc.capabilityName] = "gitadmin/" + tc.capabilityName
			fx.gitea.forkStatus = http.StatusConflict
			fx.gitea.forkCreatesRepo = false

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code != http.StatusCreated {
				t.Fatalf("G2: an existing fork of this source must be reused, got %d (%s)", w.Code, w.Body.String())
			}
			var resp gitForkResp
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if want := fx.srv.URL + "/10001/" + tc.capabilityName; resp.SourceRepoURL != want {
				t.Errorf("G2: want the existing fork %q, got %q", want, resp.SourceRepoURL)
			}
			if fx.gitea.forkCount() != 1 {
				t.Errorf("G2: fork attempts want 1, got %d", fx.gitea.forkCount())
			}
		})
	}
}

// G3: a repository that exists but declares a DIFFERENT capability must not be
// forked. The source coordinate is trusted for routing only — a later
// force-push or a manual repository replacement can put anything there — so the
// manifest is re-read and compared on every fork, for every type.
//
// The parent task decided (D8) to keep this a 409 and fix the data, rather than
// treating a mismatched manifest as "no repository" and degrading to a DB copy.
func TestForkItem_Git_AllTypesRejectManifestForAnotherCapability(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)

			// The repository now holds a valid manifest for something else.
			var impostor string
			if tc.itemType == "mcp" {
				impostor = `{"mcpServers":{"someone-elses-server":{"command":"x"}}}`
			} else {
				impostor = "---\nname: someone-elses-capability\ndescription: d\n---\n\n# body\n"
			}
			fx.gitea.putFile("gitadmin/"+tc.capabilityName, tc.manifestPath, []byte(impostor))

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code != http.StatusConflict {
				t.Fatalf("G3: expected 409, got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "GIT_SOURCE_MANIFEST_INVALID") {
				t.Errorf("G3: expected GIT_SOURCE_MANIFEST_INVALID, got %s", w.Body.String())
			}
			if fx.gitea.forkCount() != 0 {
				t.Errorf("G3: must reject before forking, got %d calls", fx.gitea.forkCount())
			}
			assertNoForkPersisted(t, "src-1", "bob")
		})
	}
}

// G3, empty-repository half: a source repository whose manifest is gone is not
// "not mirrored". Falling back to a DB copy there would hide a broken git
// source behind a fork that looks successful.
func TestForkItem_Git_AllTypesRejectEmptySourceRepo(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)
			fx.gitea.unreadableManifests["gitadmin/"+tc.capabilityName] = true
			// Drop the seeded file so the repo really has nothing to read.
			fx.gitea.files["gitadmin/"+tc.capabilityName] = map[string][]byte{}

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
			}
			assertNoForkPersisted(t, "src-1", "bob")
		})
	}
}

// The account guard applies to all four types too: a caller with no provisioned
// Gitea identity gets an explicit 409, never a DB copy that looks like success
// while producing no repository.
func TestForkItem_Git_AllTypesRequireReadyGitAccount(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "GIT_ACCOUNT_NOT_READY") {
				t.Errorf("expected GIT_ACCOUNT_NOT_READY, got %s", w.Body.String())
			}
			assertNoForkPersisted(t, "src-1", "bob")
			if fx.gitea.forkCount() != 0 {
				t.Errorf("must not call Gitea without a ready account: %d", fx.gitea.forkCount())
			}
		})
	}
}

// Fail closed: Gitea unreachable leaves no row of any kind — not a git-backed
// one, and not a DB copy standing in for it.
func TestForkItem_Git_AllTypesFailClosedWhenGiteaDown(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedGitBackedSource(t, fx, "src-1", "src-"+tc.itemType, "gitadmin/"+tc.capabilityName, tc)
			fx.srv.Close()

			w := forkReq(newForkRouter("bob"), "src-1")
			if w.Code < 400 {
				t.Fatalf("expected a failure status, got %d (%s)", w.Code, w.Body.String())
			}
			assertNoForkPersisted(t, "src-1", "bob")
			var count int64
			database.GetDB().Model(&models.CapabilityItem{}).
				Where("forked_from_item_id = ?", "src-1").Count(&count)
			if count != 0 {
				t.Errorf("a failed fork must leave no row at all, found %d", count)
			}
		})
	}
}

// ---------------------------------------------------- db-backed source forks

// seedDBBackedSource creates an ordinary DB-backed item of the given type,
// carrying real content the way ingest would.
func seedDBBackedSource(t *testing.T, id, slug string, tc gitBackedType) {
	t.Helper()
	metadata := datatypes.JSON([]byte(`{}`))
	if tc.itemType == "mcp" {
		metadata = datatypes.JSON([]byte(fmt.Sprintf(`{"key":%q}`, tc.entryKey)))
	}
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: id, RegistryID: PublicRegistryID, RepoID: "public", Slug: slug,
		ItemType: tc.itemType, Name: tc.capabilityName, Description: "d",
		Descriptions: datatypes.JSON([]byte(`{}`)), Category: "utilities", Version: "1.0.0",
		Content: tc.manifest, Metadata: metadata, SourcePath: tc.manifestPath,
		SourceType: "direct", CreatedBy: "alice", CurrentRevision: 1, Status: "active",
	}).Error; err != nil {
		t.Fatalf("seed db-backed %s: %v", tc.itemType, err)
	}
}

// A DB-backed source of any of the four types has no repository to fork, so one
// is provisioned and the source text copied into it byte for byte.
//
// Byte-for-byte matters concretely: the device writes this text straight to
// SKILL.md / <slug>.md, its frontmatter carries keys with no column at all
// (allowed-tools, hooks, disable-model-invocation), and content_md5 is computed
// over the original bytes — a regenerated manifest would drop fields and make
// every later sync see a change that never happened.
func TestForkItem_Git_DBBackedSourceProvisionsRepoWithVerbatimContent(t *testing.T) {
	for _, tc := range gitBackedTypeCases() {
		t.Run(tc.itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			createGitCapabilitySyncJobTable(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedDBBackedSource(t, "db-src", "db-"+tc.itemType, tc)

			w := forkReq(newForkRouter("bob"), "db-src")
			if w.Code != http.StatusCreated {
				t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
			}
			var resp gitForkResp
			_ = json.Unmarshal(w.Body.Bytes(), &resp)

			var stored models.CapabilityItem
			database.GetDB().First(&stored, "id = ?", resp.ID)
			if stored.ContentBackend != "git" {
				t.Fatalf("db-backed %s fork must be provisioned into git, got %q", tc.itemType, stored.ContentBackend)
			}
			// V4 §5.1 naming — the only names the discovery classifier knows at
			// the root of a repository.
			if stored.SourceRepoPath != tc.manifestPath {
				t.Errorf("manifest path: want %q, got %q", tc.manifestPath, stored.SourceRepoPath)
			}
			if stored.SourceGitEntryKey != tc.entryKey {
				t.Errorf("entry key: want %q, got %q", tc.entryKey, stored.SourceGitEntryKey)
			}
			repoName := strings.TrimPrefix(stored.SourceRepoURL, fx.srv.URL+"/")
			if !strings.HasPrefix(repoName, "10001/") {
				t.Fatalf("repository must live in the caller's namespace, got %q", repoName)
			}
			if got := string(fx.gitea.fileOf(repoName, tc.manifestPath)); got != tc.manifest {
				t.Errorf("repository content must be the source verbatim:\n got %q\nwant %q", got, tc.manifest)
			}
			if stored.Content != "" {
				t.Errorf("the row must keep no content copy, got %q", stored.Content)
			}

			// The write is authored by the user, not by the admin identity.
			if len(fx.gitea.writeCalls) == 0 {
				t.Fatal("no file was written")
			}
			if auth := fx.gitea.writeCalls[0].auth; auth != "token user-pat" {
				t.Errorf("manifest write should use the user PAT, got %q", auth)
			}

			// G4 applies to provisioned repositories too: the push webhook is
			// opt-in per deployment, so the row must not depend on it.
			var jobs []models.GitCapabilitySyncJob
			database.GetDB().Find(&jobs)
			if len(jobs) != 1 || jobs[0].DeliveryID != "fork:"+resp.ID {
				t.Errorf("G4: want one job fork:%s, got %+v", resp.ID, jobs)
			}
		})
	}
}

// No middle state: a failure at the write step must not leave a row claiming
// content_backend='git'. A row flipped before its repository holds the content
// resolves to a 404 on every read and nothing repairs it.
func TestForkItem_Git_ProvisionWriteFailureLeavesNoRow(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedDBBackedSource(t, "db-src", "db-skill", gitBackedTypeCases()[0])
	fx.gitea.writeStatus = http.StatusInternalServerError

	w := forkReq(newForkRouter("bob"), "db-src")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_REPO_WRITE_FAILED") {
		t.Errorf("expected GIT_REPO_WRITE_FAILED, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "db-src", "bob")

	// The failed attempt's newly created repository is removed. Retrying creates
	// a fresh marked repository rather than adopting an unowned empty shell.
	fx.gitea.writeStatus = 0
	retry := forkReq(newForkRouter("bob"), "db-src")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry: expected 201, got %d (%s)", retry.Code, retry.Body.String())
	}
	if len(fx.gitea.createCalls) != 2 {
		t.Errorf("retry must recreate the rolled-back repository: creates=%d", len(fx.gitea.createCalls))
	}
	var stored models.CapabilityItem
	var resp gitForkResp
	_ = json.Unmarshal(retry.Body.Bytes(), &resp)
	database.GetDB().First(&stored, "id = ?", resp.ID)
	if want := fx.srv.URL + "/10001/" + forkSlugFor(t, "db-skill", "bob"); stored.SourceRepoURL != want {
		t.Errorf("retry repo url: want %q, got %q", want, stored.SourceRepoURL)
	}
}

// The provisioning primitive writes one file, so an item that is really a file
// tree is refused rather than published with everything but its manifest
// dropped. The DB fork this replaces copied every asset; losing them silently
// would be a worse trade than a fork the user cannot complete yet.
func TestForkItem_Git_ProvisionRefusesItemWithAssets(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedDBBackedSource(t, "db-src", "db-skill", gitBackedTypeCases()[0])
	body := "reference material"
	if err := database.GetDB().Create(&models.CapabilityAsset{
		ID: "asset-1", ItemID: "db-src", RelPath: "reference.md", TextContent: &body,
	}).Error; err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	w := forkReq(newForkRouter("bob"), "db-src")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_SOURCE_HAS_ASSETS") {
		t.Errorf("expected GIT_SOURCE_HAS_ASSETS, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "db-src", "bob")
	if len(fx.gitea.createCalls) != 0 {
		t.Errorf("must refuse before creating a repository: %d", len(fx.gitea.createCalls))
	}
}

// A repository already holding some other capability is never adopted. It is
// the create-path form of the fork lineage check: the user's namespace holds
// something we must not overwrite, and retrying can never make it right.
func TestForkItem_Git_ProvisionRefusesRepoHoldingAnotherCapability(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedDBBackedSource(t, "db-src", "db-skill", gitBackedTypeCases()[0])

	// Pre-create the target repository with an unrelated capability inside.
	// The slug carries a per-user hash, so derive it the way ForkItem does.
	forkSlug := forkSlugFor(t, "db-skill", "bob")
	fx.gitea.repos["10001/"+forkSlug] = "main"
	fx.gitea.putFile("10001/"+forkSlug, "agent.md", []byte("---\nname: unrelated\n---\n"))

	w := forkReq(newForkRouter("bob"), "db-src")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_REPO_NAME_TAKEN") {
		t.Errorf("expected GIT_REPO_NAME_TAKEN, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "db-src", "bob")
	if len(fx.gitea.writeCalls) != 0 {
		t.Errorf("must refuse before writing anything: %d writes", len(fx.gitea.writeCalls))
	}
}

// A repository with no matching ownership marker is rejected before any write,
// even if it happens to hold a manifest at the expected path.
func TestForkItem_Git_ProvisionRejectsPreexistingDifferentManifest(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	seedDBBackedSource(t, "db-src", "db-skill", gitBackedTypeCases()[0])

	forkSlug := forkSlugFor(t, "db-skill", "bob")
	fx.gitea.repos["10001/"+forkSlug] = "main"
	fx.gitea.putFile("10001/"+forkSlug, "skill.md", []byte("---\nname: fix-01-skill\n---\n\n# tampered\n"))

	w := forkReq(newForkRouter("bob"), "db-src")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GIT_REPO_NAME_TAKEN") {
		t.Errorf("expected GIT_REPO_NAME_TAKEN, got %s", w.Body.String())
	}
	assertNoForkPersisted(t, "db-src", "bob")
}

func TestForkItem_Git_ProvisionRefusesPrivateOrOrdinarySameNameRepo(t *testing.T) {
	for _, tc := range []struct {
		name        string
		makePrivate bool
	}{
		{name: "private", makePrivate: true},
		{name: "ordinary-files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			seedDBBackedSource(t, "db-src", "db-skill", gitBackedTypeCases()[0])
			forkSlug := forkSlugFor(t, "db-skill", "bob")
			fullName := "10001/" + forkSlug
			fx.gitea.repos[fullName] = "main"
			fx.gitea.privateRepos[fullName] = tc.makePrivate
			fx.gitea.putFile(fullName, "README.md", []byte("unrelated project"))

			w := forkReq(newForkRouter("bob"), "db-src")
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "GIT_REPO_NAME_TAKEN") {
				t.Fatalf("expected ownership conflict, got %d (%s)", w.Code, w.Body.String())
			}
			assertNoForkPersisted(t, "db-src", "bob")
			if len(fx.gitea.writeCalls) != 0 {
				t.Fatalf("unowned repository was modified: %+v", fx.gitea.writeCalls)
			}
		})
	}
}

// rule/template can be parsed out of a git index file but not out of a
// discovery file, so a git-backed row of either type could be created and then
// never re-indexed. They stay on the DB fork path.
func TestForkItem_Git_UnsupportedTypesKeepDBFork(t *testing.T) {
	for _, itemType := range []string{"rule", "template"} {
		t.Run(itemType, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			if err := database.GetDB().Create(&models.CapabilityItem{
				ID: "rt-src", RegistryID: PublicRegistryID, RepoID: "public", Slug: "rt-" + itemType,
				ItemType: itemType, Name: "rt", Descriptions: datatypes.JSON([]byte(`{}`)),
				Content: "# body", Metadata: datatypes.JSON([]byte(`{}`)),
				SourceType: "direct", CreatedBy: "alice", CurrentRevision: 1, Status: "active",
			}).Error; err != nil {
				t.Fatalf("seed: %v", err)
			}

			w := forkReq(newForkRouter("bob"), "rt-src")
			if w.Code != http.StatusCreated {
				t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
			}
			var resp gitForkResp
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.ContentBackend == "git" {
				t.Errorf("%s must stay db-backed, got %q", itemType, resp.ContentBackend)
			}
			if len(fx.gitea.createCalls) != 0 || fx.gitea.forkCount() != 0 {
				t.Errorf("%s must not touch Gitea: %d creates, %d forks",
					itemType, len(fx.gitea.createCalls), fx.gitea.forkCount())
			}
		})
	}
}
