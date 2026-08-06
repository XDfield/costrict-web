package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// End-to-end lifecycle convergence against a REAL Gitea.
//
// Everything below runs through the production gitsync client and the production
// SyncRepository — no fakes — because the whole premise of Phase C came from
// measuring what the deployed Gitea actually does, and three of the five
// transitions it covers (rename, transfer, default-branch change) emit no
// webhook at all. A test that mocks the Git server can only re-assert the
// assumption; this one re-measures it.
//
// Opt-in: it creates and destroys repositories, so it never runs by accident.
//
//	GITEA_E2E_ENDPOINT=http://127.0.0.1:3001 \
//	GITEA_E2E_TOKEN=<admin token> \
//	DATABASE_URL=postgres://... \
//	go test ./internal/services -run TestGitCapabilityLifecycle_GiteaE2E -v
//
// Safety: every repository, branch and organisation it creates carries the
// e2eLifecyclePrefix and is removed in a t.Cleanup, including on failure. It
// never reads, writes or deletes anything it did not create.
const e2eLifecyclePrefix = "costrict-lifecycle-e2e-"

type giteaE2E struct {
	t        *testing.T
	endpoint string
	token    string
	owner    string
}

func newGiteaE2E(t *testing.T) *giteaE2E {
	t.Helper()
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("GITEA_E2E_ENDPOINT")), "/")
	token := strings.TrimSpace(os.Getenv("GITEA_E2E_TOKEN"))
	if endpoint == "" || token == "" {
		t.Skip("GITEA_E2E_ENDPOINT/GITEA_E2E_TOKEN not set; skipping real-Gitea lifecycle E2E")
	}
	owner := strings.TrimSpace(os.Getenv("GITEA_E2E_OWNER"))
	if owner == "" {
		owner = "gitadmin"
	}
	return &giteaE2E{t: t, endpoint: endpoint, token: token, owner: owner}
}

func (g *giteaE2E) do(method, path string, body any, want ...int) []byte {
	g.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			g.t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, g.endpoint+path, reader)
	if err != nil {
		g.t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	for _, status := range want {
		if resp.StatusCode == status {
			return payload
		}
	}
	g.t.Fatalf("%s %s -> %d %s", method, path, resp.StatusCode, string(payload))
	return nil
}

// createRepo makes a throw-away repository and registers its removal. The name
// is prefixed and timestamped so a leaked one is unmistakably ours.
func (g *giteaE2E) createRepo(name string) (int64, string) {
	g.t.Helper()
	payload := g.do(http.MethodPost, "/api/v1/user/repos", map[string]any{
		"name": name, "description": "costrict lifecycle E2E; safe to delete",
		"private": false, "auto_init": true, "default_branch": "main",
	}, http.StatusCreated)
	var repo struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(payload, &repo); err != nil {
		g.t.Fatalf("decode created repo: %v", err)
	}
	g.t.Cleanup(func() { g.deleteRepoByID(repo.ID) })
	return repo.ID, repo.FullName
}

// deleteRepoByID removes a repository by its NUMERIC id, resolving the current
// name first. Cleanup must not assume the name it was created with: these tests
// rename and transfer, and both are invisible to everything except the id.
func (g *giteaE2E) deleteRepoByID(repoID int64) {
	g.t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/repositories/%d", g.endpoint, repoID), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "token "+g.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return // already gone
	}
	var repo struct {
		FullName string `json:"full_name"`
	}
	if json.Unmarshal(payload, &repo) != nil || !strings.Contains(repo.FullName, e2eLifecyclePrefix) {
		// Refuse to delete anything that is not ours, whatever the id says.
		return
	}
	g.do(http.MethodDelete, "/api/v1/repos/"+repo.FullName, nil,
		http.StatusNoContent, http.StatusNotFound)
}

// fullName resolves the repository's CURRENT name from its numeric id, and
// returns "" once it no longer exists. The empty case is not an error: after a
// deletion the platform still has to converge, and the only name it has left is
// the stale label stored on the job — which is exactly what a caller must fall
// back to.
func (g *giteaE2E) fullName(repoID int64) string {
	g.t.Helper()
	payload := g.do(http.MethodGet, fmt.Sprintf("/api/v1/repositories/%d", repoID), nil,
		http.StatusOK, http.StatusNotFound)
	var repo struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(payload, &repo); err != nil {
		return ""
	}
	return repo.FullName
}

func (g *giteaE2E) writeFile(fullName, branch, path string, content []byte, message string) string {
	g.t.Helper()
	// Gitea answers 201 for a create and 200 for an update, and an update needs
	// the current blob sha.
	existing := g.fileSHA(fullName, branch, path)
	body := map[string]any{
		"content": base64.StdEncoding.EncodeToString(content),
		"message": message, "branch": branch,
	}
	method, want := http.MethodPost, []int{http.StatusCreated}
	if existing != "" {
		method, want = http.MethodPut, []int{http.StatusOK}
		body["sha"] = existing
	}
	payload := g.do(method, "/api/v1/repos/"+fullName+"/contents/"+path, body, want...)
	var result struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		g.t.Fatalf("decode write result: %v", err)
	}
	return result.Commit.SHA
}

func (g *giteaE2E) fileSHA(fullName, branch, path string) string {
	g.t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		g.endpoint+"/api/v1/repos/"+fullName+"/contents/"+path+"?ref="+branch, nil)
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+g.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var file struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(payload, &file) != nil {
		return ""
	}
	return file.SHA
}

func (g *giteaE2E) deleteFile(fullName, branch, path string) {
	g.t.Helper()
	sha := g.fileSHA(fullName, branch, path)
	if sha == "" {
		g.t.Fatalf("cannot delete %s: not present on %s", path, branch)
	}
	g.do(http.MethodDelete, "/api/v1/repos/"+fullName+"/contents/"+path, map[string]any{
		"sha": sha, "message": "E2E: remove manifest", "branch": branch,
	}, http.StatusOK)
}

func (g *giteaE2E) headSHA(fullName, branch string) string {
	g.t.Helper()
	payload := g.do(http.MethodGet, "/api/v1/repos/"+fullName+"/branches/"+branch, nil, http.StatusOK)
	var result struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		g.t.Fatalf("decode branch: %v", err)
	}
	return result.Commit.ID
}

const e2eSkillManifest = "---\nname: Lifecycle E2E Skill\ndescription: real gitea\nversion: %s\n---\nbody\n"

// TestGitCapabilityLifecycle_GiteaE2E drives one capability through every
// lifecycle transition the platform claims to converge on, against a live Gitea.
//
// The subtests share one repository and run in order on purpose: the interesting
// failures are transitions BETWEEN states (a restore after an archive, a rename
// after a transfer), and an independent-fixture-per-case design cannot express
// them.
func TestGitCapabilityLifecycle_GiteaE2E(t *testing.T) {
	gitea := newGiteaE2E(t)
	db, _ := newGitRevisionPostgresDB(t)

	repoName := e2eLifecyclePrefix + fmt.Sprint(time.Now().UnixNano())
	repoID, fullName := gitea.createRepo(repoName)
	t.Logf("created %s (numeric id %d)", fullName, repoID)
	gitea.writeFile(fullName, "main", "SKILL.md", []byte(fmt.Sprintf(e2eSkillManifest, "1.0.0")), "E2E: seed manifest")

	// One capability row bound to the REAL repository by numeric id. Discovery is
	// covered elsewhere; what is under test here is convergence of an existing
	// binding, which is where every lifecycle rule lives.
	if err := db.Exec(`INSERT INTO capability_items
		(id, registry_id, repo_id, slug, item_type, name, version, source_repo_ref, source_repo_path,
		 content_backend, source_git_server_id, source_git_repo_id, git_sha, git_sync_status, status, created_by)
		VALUES (?, 'reg', 'repo', 'skill', 'skill', 'placeholder', '0.0.0', 'main', 'SKILL.md',
		        'git', ?, ?, '', 'pending', 'active', 'user-1')`,
		pgRevisionItemID, pgRevisionServerID, repoID).Error; err != nil {
		t.Fatalf("seed bound item: %v", err)
	}
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, 'e2e-holder')`,
		pgRevisionItemID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}

	// The production client, pointed at the real server.
	cfg := &gitserver.Config{
		ServerID: pgRevisionServerID,
		Endpoint: gitea.endpoint,
		WebURL:   gitea.endpoint,
	}
	svc := &GitCapabilitySyncService{
		DB: db, Parser: &ParserService{},
		NewReader: func(c *gitserver.Config) GitCapabilityReader {
			return gitsync.NewClient(c.Endpoint, gitea.token)
		},
	}
	sync := func(t *testing.T, label string) error {
		t.Helper()
		// The name handed to the sync is a stale display label by construction —
		// rename and transfer are silent, so it is wrong the moment it is written,
		// and after a deletion there is no current name at all. Resolution goes
		// through the numeric id inside SyncRepository; this argument only ever
		// reaches log lines.
		staleName := gitea.fullName(repoID)
		if staleName == "" {
			staleName = fullName
		}
		lease := seedPostgresLease(t, db, uuid.NewString(), "tok-"+label, "delivery-"+label+"-"+uuid.NewString())
		_, err := svc.SyncRepository(context.Background(), cfg, repoID, staleName, "main", false, lease)
		return err
	}
	state := func(t *testing.T) (models.CapabilityItem, string) {
		t.Helper()
		var item models.CapabilityItem
		if err := db.First(&item, "id = ?", pgRevisionItemID).Error; err != nil {
			t.Fatalf("load item: %v", err)
		}
		reason := ""
		if item.GitLifecycleReason != nil {
			reason = *item.GitLifecycleReason
		}
		return item, reason
	}

	t.Run("baseline projection", func(t *testing.T) {
		if err := sync(t, "baseline"); err != nil {
			t.Fatalf("baseline sync: %v", err)
		}
		item, reason := state(t)
		if item.Status != "active" || reason != "" {
			t.Fatalf("baseline: status=%q reason=%q", item.Status, reason)
		}
		if item.Name != "Lifecycle E2E Skill" {
			t.Fatalf("manifest was not projected: name=%q", item.Name)
		}
		if item.GitSHA != gitea.headSHA(fullName, "main") {
			t.Fatalf("git_sha=%q does not match the real head", item.GitSHA)
		}
		if item.GitVisibilityVerifiedAt == nil {
			t.Fatal("a real repository read did not stamp the visibility verification")
		}
	})

	t.Run("manifest removed archives with a recoverable reason", func(t *testing.T) {
		gitea.deleteFile(gitea.fullName(repoID), "main", "SKILL.md")
		if err := sync(t, "archive"); err != nil {
			t.Fatalf("archive sync: %v", err)
		}
		item, reason := state(t)
		if item.Status != "archived" || item.GitSyncStatus != gitCapabilitySyncOrphaned ||
			reason != models.GitLifecycleReasonManifestRemoved {
			t.Fatalf("archive: status=%q sync=%q reason=%q", item.Status, item.GitSyncStatus, reason)
		}
		var tombstones int64
		db.Table("capability_sync_tombstones").
			Where("item_id = ? AND user_id = 'e2e-holder' AND lifecycle_reason = ?",
				pgRevisionItemID, models.GitLifecycleReasonManifestRemoved).Count(&tombstones)
		if tombstones != 1 {
			t.Fatalf("tombstones = %d, want 1 — csc would keep the capability installed", tombstones)
		}
	})

	t.Run("restoring the manifest reuses the same item", func(t *testing.T) {
		gitea.writeFile(gitea.fullName(repoID), "main", "SKILL.md",
			[]byte(fmt.Sprintf(e2eSkillManifest, "1.1.0")), "E2E: restore manifest")
		if err := sync(t, "restore"); err != nil {
			t.Fatalf("restore sync: %v", err)
		}
		item, reason := state(t)
		if item.ID != pgRevisionItemID {
			t.Fatalf("restore created a new identity: %s", item.ID)
		}
		if item.Status != "active" || reason != "" || item.Version != "1.1.0" {
			t.Fatalf("restore: status=%q reason=%q version=%q", item.Status, reason, item.Version)
		}
	})

	t.Run("rename is invisible to events and converges by numeric id", func(t *testing.T) {
		renamed := repoName + "-renamed"
		gitea.do(http.MethodPatch, "/api/v1/repos/"+gitea.fullName(repoID),
			map[string]any{"name": renamed}, http.StatusOK)
		if got := gitea.fullName(repoID); !strings.HasSuffix(got, renamed) {
			t.Fatalf("rename did not take: %s", got)
		}
		if err := sync(t, "rename"); err != nil {
			t.Fatalf("post-rename sync: %v", err)
		}
		item, _ := state(t)
		if item.ID != pgRevisionItemID {
			t.Fatalf("rename changed the item identity: %s", item.ID)
		}
		if !strings.HasSuffix(item.SourceRepoURL, renamed) {
			t.Fatalf("repository coordinate was not updated: %q", item.SourceRepoURL)
		}
		if item.Status != "active" {
			t.Fatalf("rename archived the capability: status=%q", item.Status)
		}
	})

	t.Run("transfer is invisible to events and converges by numeric id", func(t *testing.T) {
		// Gitea caps a username at 40 characters, so the org gets a shortened but
		// still unmistakable name.
		orgName := fmt.Sprintf("cs-lifecycle-e2e-org-%d", time.Now().UnixNano()%1e9)
		gitea.do(http.MethodPost, "/api/v1/orgs", map[string]any{
			"username": orgName, "visibility": "public",
		}, http.StatusCreated)
		t.Cleanup(func() {
			// The repository is moved back before the org is removed, so cleanup
			// never depends on cascade behaviour.
			gitea.do(http.MethodPost, "/api/v1/repos/"+gitea.fullName(repoID)+"/transfer",
				map[string]any{"new_owner": gitea.owner}, http.StatusAccepted, http.StatusCreated, http.StatusOK, http.StatusNotFound)
			gitea.do(http.MethodDelete, "/api/v1/orgs/"+orgName, nil, http.StatusNoContent, http.StatusNotFound)
		})

		gitea.do(http.MethodPost, "/api/v1/repos/"+gitea.fullName(repoID)+"/transfer",
			map[string]any{"new_owner": orgName}, http.StatusAccepted, http.StatusCreated, http.StatusOK)
		if got := gitea.fullName(repoID); !strings.HasPrefix(got, orgName+"/") {
			t.Fatalf("transfer did not take: %s", got)
		}
		if err := sync(t, "transfer"); err != nil {
			t.Fatalf("post-transfer sync: %v", err)
		}
		item, _ := state(t)
		if item.ID != pgRevisionItemID || item.Status != "active" {
			t.Fatalf("transfer disturbed the item: id=%s status=%q", item.ID, item.Status)
		}
		if !strings.Contains(item.SourceRepoURL, orgName) {
			t.Fatalf("owner coordinate was not updated: %q", item.SourceRepoURL)
		}
	})

	t.Run("default branch change moves the read-through", func(t *testing.T) {
		current := gitea.fullName(repoID)
		gitea.do(http.MethodPost, "/api/v1/repos/"+current+"/branches",
			map[string]any{"new_branch_name": "v2", "old_ref_name": "main"}, http.StatusCreated)
		gitea.writeFile(current, "v2", "SKILL.md",
			[]byte(fmt.Sprintf(e2eSkillManifest, "2.0.0")), "E2E: v2 manifest")
		// Silent on 1.24.6: this emits no webhook of any kind.
		gitea.do(http.MethodPatch, "/api/v1/repos/"+current,
			map[string]any{"default_branch": "v2"}, http.StatusOK)

		if err := sync(t, "default-branch"); err != nil {
			t.Fatalf("post-default-branch sync: %v", err)
		}
		item, _ := state(t)
		if item.SourceRepoRef != "v2" {
			t.Fatalf("read-through ref = %q, want v2", item.SourceRepoRef)
		}
		if item.Version != "2.0.0" {
			t.Fatalf("projection did not follow the new default branch: version=%q", item.Version)
		}
		if item.GitSHA != gitea.headSHA(gitea.fullName(repoID), "v2") {
			t.Fatalf("git_sha did not move to the new default branch head")
		}
	})

	t.Run("repository deletion is terminal and same-name recreation does not resurrect", func(t *testing.T) {
		deleted := gitea.fullName(repoID)
		gitea.do(http.MethodDelete, "/api/v1/repos/"+deleted, nil, http.StatusNoContent)

		if err := sync(t, "deleted"); err != nil {
			t.Fatalf("deletion convergence: %v", err)
		}
		item, reason := state(t)
		if item.Status != "archived" || reason != models.GitLifecycleReasonRepositoryDeleted {
			t.Fatalf("deletion: status=%q reason=%q", item.Status, reason)
		}
		var bindings int64
		db.Table("git_capability_repositories").
			Where("git_server_id = ? AND git_repo_id = ?", pgRevisionServerID, repoID).Count(&bindings)
		if bindings != 0 {
			t.Fatalf("binding survived repository deletion: %d", bindings)
		}

		// The replacement: same owner AND same bare name, carrying the same
		// manifest at the same path. Everything a name-based identity would match
		// on is identical; only the numeric id differs.
		replacementID, replacementFullName := gitea.createRepo(repoName)
		if replacementID == repoID {
			t.Fatal("Gitea reused a numeric repository id; the identity model assumes it does not")
		}
		t.Logf("recreated %s as numeric id %d (was %d)", replacementFullName, replacementID, repoID)
		gitea.writeFile(replacementFullName, "main", "SKILL.md",
			[]byte(fmt.Sprintf(e2eSkillManifest, "3.0.0")), "E2E: replacement manifest")

		// Converge the OLD identity again, which is what a retried job or the next
		// reconcile pass would do. It must still find nothing: the archived rows
		// are bound to 1296, and 1297 is a different repository no matter what it
		// is called.
		if err := sync(t, "after-recreate"); err != nil {
			t.Fatalf("post-recreation convergence of the old identity: %v", err)
		}
		after, afterReason := state(t)
		if after.Status != "archived" || afterReason != models.GitLifecycleReasonRepositoryDeleted {
			t.Fatalf("a same-name replacement resurrected the old identity: status=%q reason=%q",
				after.Status, afterReason)
		}
		if after.Version != "2.0.0" {
			t.Fatalf("the replacement's manifest was projected onto the dead item: version=%q", after.Version)
		}
		if after.SourceGitRepoID != repoID {
			t.Fatalf("the dead item was re-pointed at another repository: %d", after.SourceGitRepoID)
		}
	})
}

// TestGitCapabilityLifecycle_GiteaE2ELostEventIsRepairedByReconcile is the claim
// the whole reconcile rewrite rests on: for the five transitions Gitea 1.24.6
// emits nothing for, and for any delivery that is simply lost, re-reading the
// repository by numeric id converges anyway.
//
// It deliberately performs the mutation WITHOUT any webhook in the picture — no
// delivery is generated, none is replayed — and then asserts that a plain
// convergence pass produces the correct state.
func TestGitCapabilityLifecycle_GiteaE2ELostEventIsRepairedByReconcile(t *testing.T) {
	gitea := newGiteaE2E(t)
	db, _ := newGitRevisionPostgresDB(t)

	repoName := e2eLifecyclePrefix + "lost-" + fmt.Sprint(time.Now().UnixNano())
	repoID, fullName := gitea.createRepo(repoName)
	gitea.writeFile(fullName, "main", "SKILL.md", []byte(fmt.Sprintf(e2eSkillManifest, "1.0.0")), "E2E: seed")

	if err := db.Exec(`INSERT INTO capability_items
		(id, registry_id, repo_id, slug, item_type, name, version, source_repo_ref, source_repo_path,
		 content_backend, source_git_server_id, source_git_repo_id, git_sha, git_sync_status, status, created_by)
		VALUES (?, 'reg', 'repo', 'skill', 'skill', 'placeholder', '0.0.0', 'main', 'SKILL.md',
		        'git', ?, ?, '', 'pending', 'active', 'user-1')`,
		pgRevisionItemID, pgRevisionServerID, repoID).Error; err != nil {
		t.Fatalf("seed bound item: %v", err)
	}

	cfg := &gitserver.Config{ServerID: pgRevisionServerID, Endpoint: gitea.endpoint, WebURL: gitea.endpoint}
	svc := &GitCapabilitySyncService{
		DB: db, Parser: &ParserService{},
		NewReader: func(c *gitserver.Config) GitCapabilityReader {
			return gitsync.NewClient(c.Endpoint, gitea.token)
		},
	}
	reconcile := func(t *testing.T, label string) {
		t.Helper()
		// The delivery id carries the reconcile prefix, which is what labels the
		// resulting revision `reconcile` rather than `push`.
		lease := seedPostgresLease(t, db, uuid.NewString(), "tok-"+label,
			models.GitCapabilitySyncDeliveryPrefixReconcile+label+":"+uuid.NewString())
		if _, err := svc.SyncRepository(context.Background(), cfg, repoID,
			gitea.fullName(repoID), "main", false, lease); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	reconcile(t, "baseline")

	// Two silent mutations at once, with no delivery of any kind in between:
	// rename, then take the repository private. Neither emits a webhook.
	renamed := repoName + "-silent"
	gitea.do(http.MethodPatch, "/api/v1/repos/"+gitea.fullName(repoID),
		map[string]any{"name": renamed, "private": true}, http.StatusOK)

	reconcile(t, "repair")

	var item models.CapabilityItem
	if err := db.First(&item, "id = ?", pgRevisionItemID).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(item.SourceRepoURL, renamed) {
		t.Fatalf("reconcile did not repair the renamed coordinate: %q", item.SourceRepoURL)
	}
	if item.Status != "active" {
		t.Fatalf("a rename+visibility change archived the capability: status=%q", item.Status)
	}
	if item.GitVisibilityVerifiedAt == nil {
		t.Fatal("reconcile did not refresh the visibility verification")
	}
	assertNoLifecycleClaim(t, db)

	// The authorization gate, asked of the live server rather than of our own
	// row: a repository that went private must stop reading as public
	// immediately, because a caller authorized ONLY by "the repository is
	// public" would otherwise keep receiving its content until the next
	// reconcile — and a visibility change emits nothing to shorten that window.
	content := &GitCapabilityContentService{
		Resolver: staticGitServerResolver{cfg: &gitserver.Config{
			ServerID: pgRevisionServerID, Endpoint: gitea.endpoint, WebURL: gitea.endpoint,
			AdminToken: gitea.token,
		}},
	}
	public, err := content.ItemRepositoryIsPublic(context.Background(), &item)
	if err != nil {
		t.Fatalf("visibility probe: %v", err)
	}
	if public {
		t.Fatal("a private repository still reads as public; anonymous callers would keep receiving its content")
	}
}

// staticGitServerResolver hands the content service the same live coordinates
// the sync used, without going through the git_servers table (the throwaway
// schema does not have one).
type staticGitServerResolver struct{ cfg *gitserver.Config }

func (r staticGitServerResolver) ResolveByServerID(context.Context, string) (*gitserver.Config, error) {
	return r.cfg, nil
}

func assertNoLifecycleClaim(t *testing.T, db *gorm.DB) {
	t.Helper()
	var reason *string
	if err := db.Raw(`SELECT git_lifecycle_reason FROM capability_items WHERE id = ?`,
		pgRevisionItemID).Scan(&reason).Error; err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		t.Fatalf("a healthy repository left a Git archive claim: %q", *reason)
	}
}
