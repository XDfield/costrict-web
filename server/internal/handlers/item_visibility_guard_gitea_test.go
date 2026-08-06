// End-to-end verification of the Git visibility gate against a REAL Gitea.
//
// The unit tests above run against a fake that we wrote, so they can only prove
// the gate behaves the way we believe Gitea does. This one proves the belief:
// it drives a real Gitea instance through the exact transition the gate exists
// for — a public repository becoming private with no webhook, no event and no
// change to the local visibility column — and checks that the reads stop.
//
// It is opt-in, like the PostgreSQL tests:
//
//	GITEA_E2E_URL=http://127.0.0.1:3001 \
//	GITEA_E2E_TOKEN=<admin token from git_servers.config> \
//	go test ./internal/handlers/ -run TestGitVisibilityGate_RealGitea -v
//
// It creates its OWN repository (prefixed so it is unmistakable) and deletes it
// on the way out. It never touches a repository it did not create, and never
// changes the visibility of one — a shared Gitea holds sample sets whose
// visibility other work depends on.

package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

const realGiteaItemID = "b0000000-0000-4000-8000-000000000001"

type realGitea struct {
	t     *testing.T
	base  string
	token string
}

func (g *realGitea) do(method, path string, body any) (int, []byte) {
	g.t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			g.t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, g.base+path, payload)
	if err != nil {
		g.t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestGitVisibilityGate_RealGitea is the acceptance test for AC-LH16 against
// the deployed Gitea version, not against our model of it.
func TestGitVisibilityGate_RealGitea(t *testing.T) {
	base := strings.TrimRight(os.Getenv("GITEA_E2E_URL"), "/")
	token := os.Getenv("GITEA_E2E_TOKEN")
	if base == "" || token == "" {
		t.Skip("GITEA_E2E_URL / GITEA_E2E_TOKEN not set; skipping the real-Gitea visibility test")
	}
	gitea := &realGitea{t: t, base: base, token: token}

	// Own repository, obvious name, deleted on the way out. Never an existing one.
	repoName := fmt.Sprintf("costrict-visibility-probe-%d", time.Now().UnixNano())
	status, raw := gitea.do(http.MethodPost, "/api/v1/user/repos", map[string]any{
		"name": repoName, "private": false, "auto_init": true, "default_branch": "main",
	})
	if status != http.StatusCreated {
		t.Fatalf("create probe repository: %d %s", status, raw)
	}
	var created struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode created repository: %v (%s)", err, raw)
	}
	t.Cleanup(func() {
		if code, body := gitea.do(http.MethodDelete, "/api/v1/repos/"+created.FullName, nil); code != http.StatusNoContent {
			t.Errorf("probe repository %s was not cleaned up: %d %s", created.FullName, code, body)
		}
	})
	if created.Private {
		t.Fatalf("probe repository was created private; the test needs a public one")
	}

	// Write the capability manifest the row points at.
	if code, body := gitea.do(http.MethodPost,
		"/api/v1/repos/"+created.FullName+"/contents/SKILL.md",
		map[string]any{
			"message": "probe",
			// base64("---\nname: probe\n---\n\n# probe body\n")
			"content": "LS0tCm5hbWU6IHByb2JlCi0tLQoKIyBwcm9iZSBib2R5Cg==",
			"branch":  "main",
		}); code != http.StatusCreated {
		t.Fatalf("write probe manifest: %d %s", code, body)
	}

	defer setupTestDB(t)()
	if err := database.GetDB().Exec(`CREATE TABLE IF NOT EXISTS git_servers (
		server_id TEXT PRIMARY KEY, kind TEXT NOT NULL, endpoint TEXT NOT NULL,
		display_name TEXT NOT NULL, config TEXT NOT NULL DEFAULT '{}',
		is_template INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`).Error; err != nil {
		t.Fatalf("create git_servers: %v", err)
	}
	if err := database.GetDB().Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		 VALUES ('e2e-gitea','gitea',?,'e2e',?,0,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		base, fmt.Sprintf(`{"admin_token":%q}`, token)).Error; err != nil {
		t.Fatalf("seed git_servers: %v", err)
	}

	createTestRepository(t, "repo-e2e-vis", "public")
	if err := database.GetDB().Create(&models.CapabilityRegistry{
		ID: "reg-e2e-vis", Name: "reg-e2e-vis", SourceType: "git", RepoID: "repo-e2e-vis", OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := database.GetDB().Create(&models.CapabilityItem{
		ID: realGiteaItemID, RegistryID: "reg-e2e-vis", RepoID: "repo-e2e-vis", Slug: "e2e-vis",
		ItemType: "skill", Name: "E2E Visibility", Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), CurrentRevision: 1,
		ContentBackend: models.ContentBackendGit, SourceRepoURL: base + "/" + created.FullName,
		SourceRepoRef: "main", SourceRepoPath: "SKILL.md",
		SourceGitServerID: "e2e-gitea", SourceGitRepoID: created.ID,
		GitSHA: strings.Repeat("a", 40), GitSyncStatus: "synced",
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := database.GetDB().Exec(`INSERT INTO capability_item_git_revisions
		(id, item_id, revision_no, git_server_id, git_repo_id, git_ref, manifest_path, entry_key,
		 git_sha, version_label, source, observed_at, created_at)
		VALUES ('rev-e2e-vis', ?, 1, 'e2e-gitea', ?, 'main', 'SKILL.md', '', ?, '1.0.0', ?, ?, ?)`,
		realGiteaItemID, created.ID, strings.Repeat("a", 40),
		models.GitRevisionSourceBackfill, time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed revision: %v", err)
	}

	// Public: every detail-scoped read answers.
	for _, path := range guardedItemPaths(realGiteaItemID) {
		if w := get(newGuardedItemRouter(""), path); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 against a public repository, got %d (%s)",
				path, w.Code, w.Body.String())
		}
	}
	waitForDownloadLogs(t, realGiteaItemID, 1)

	// The transition. Gitea emits nothing for it and repositories.visibility is
	// deliberately left saying "public" — that is the whole point.
	if code, body := gitea.do(http.MethodPatch, "/api/v1/repos/"+created.FullName,
		map[string]any{"private": true}); code != http.StatusOK {
		t.Fatalf("make the probe repository private: %d %s", code, body)
	}
	var stillPublic int64
	database.GetDB().Model(&models.Repository{}).
		Where("id = ? AND visibility = ?", "repo-e2e-vis", "public").Count(&stillPublic)
	if stillPublic != 1 {
		t.Fatal("the local visibility column changed; this test is no longer testing the stale-column case")
	}
	resetGitVisibilityCache()

	for _, caller := range []string{"", "stranger"} {
		for _, path := range guardedItemPaths(realGiteaItemID) {
			w := get(newGuardedItemRouter(caller), path)
			if w.Code != http.StatusNotFound {
				t.Fatalf("caller %q %s: expected 404 once the real repository went private, got %d (%s)",
					caller, path, w.Code, w.Body.String())
			}
			for _, leak := range []string{created.FullName, "SKILL.md", "probe body", strings.Repeat("a", 40)} {
				if strings.Contains(w.Body.String(), leak) {
					t.Fatalf("caller %q %s: refusal leaked %q: %s", caller, path, leak, w.Body.String())
				}
			}
		}
	}

	// The owner is not authorized by public visibility, so they keep their item.
	if w := get(newGuardedItemRouter("u1"), "/api/items/"+realGiteaItemID+"/git-history"); w.Code != http.StatusOK {
		t.Fatalf("the owner lost their own item's history: %d (%s)", w.Code, w.Body.String())
	}

	// And back: a repository made public again is served again, with no local
	// write anywhere in between.
	if code, body := gitea.do(http.MethodPatch, "/api/v1/repos/"+created.FullName,
		map[string]any{"private": false}); code != http.StatusOK {
		t.Fatalf("make the probe repository public again: %d %s", code, body)
	}
	resetGitVisibilityCache()
	if w := get(newGuardedItemRouter(""), "/api/items/"+realGiteaItemID+"/git-history"); w.Code != http.StatusOK {
		t.Fatalf("a repository made public again is still refused: %d (%s)", w.Code, w.Body.String())
	}
}
