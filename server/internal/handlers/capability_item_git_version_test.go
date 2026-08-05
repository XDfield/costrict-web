// The `version` a Git-backed item reports.
//
// These tests exist because of a failure that unit tests on either endpoint
// alone cannot see: the device reads the version from the LIST and stores the
// version it got from the DETAIL, then compares the two on the next poll. If
// the two projections disagree the capability reinstalls forever; if neither
// moves when the repository does, an installed capability never updates at all.
// Both properties are therefore asserted across the endpoints, not inside one.

package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

const (
	gitVersionFirstSHA  = "e7dea05a1b2c3d4e5f60718293a4b5c6d7e8f901"
	gitVersionSecondSHA = "1f4b9c2d3e4f5061728394a5b6c7d8e9f0a1b2c3"
)

// seedGitVersionItem is seedGitContentItem with the two fields under test —
// manifest version and bound commit — chosen by the caller.
func seedGitVersionItem(t *testing.T, id, version, sha string) {
	t.Helper()
	createTestRepository(t, "repo-gv-"+id, "public")
	if err := database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gv-" + id, Name: "reg-gv-" + id, SourceType: "git",
		RepoID: "repo-gv-" + id, OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := database.DB.Create(&models.CapabilityItem{
		ID: id, RegistryID: "reg-gv-" + id, RepoID: "repo-gv-" + id, Slug: id,
		ItemType: "skill", Name: "Git " + id, Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), Content: gitContentStaleDBValue,
		CurrentRevision: 1, ContentBackend: models.ContentBackendGit,
		Version:       version,
		SourceRepoURL: "https://gitea.example.test/" + gitContentTestRepoName,
		SourceRepoRef: "main", SourceRepoPath: "skill.md",
		SourceGitServerID: gitContentTestServerID, SourceGitRepoID: gitContentTestRepoID,
		GitSHA: sha, GitSyncStatus: "synced",
	}).Error; err != nil {
		t.Fatalf("seed git-backed item: %v", err)
	}
}

// moveItemCommit is what a push looks like to the API: the Git writer advances
// git_sha (through the bypass only it is allowed to use) and nothing else.
func moveItemCommit(t *testing.T, id, sha string) {
	t.Helper()
	if err := database.DB.Set(models.GitSyncBypassSetting, true).
		Model(&models.CapabilityItem{}).
		Where("id = ?", id).
		Update("git_sha", sha).Error; err != nil {
		t.Fatalf("advance git_sha: %v", err)
	}
}

func favoriteItem(t *testing.T, favID, itemID, userID string) {
	t.Helper()
	if err := database.DB.Create(&models.ItemFavorite{
		ID: favID, ItemID: itemID, UserID: userID,
	}).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
}

// listedItemVersion reads the version the store list reports for one item —
// the value the device compares against its installed copy.
func listedItemVersion(t *testing.T, path, userID, itemID string) string {
	t.Helper()
	w := get(newItemRouter(userID), path)
	if w.Code != http.StatusOK {
		t.Fatalf("list %s: expected 200, got %d: %s", path, w.Code, w.Body.String())
	}
	return pickVersion(t, w.Body.Bytes(), itemID, path)
}

func myItemVersion(t *testing.T, userID, itemID string) string {
	t.Helper()
	w := get(newRegistryRouter(userID), "/api/items/my")
	if w.Code != http.StatusOK {
		t.Fatalf("my items: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	return pickVersion(t, w.Body.Bytes(), itemID, "/api/items/my")
}

func pickVersion(t *testing.T, raw []byte, itemID, label string) string {
	t.Helper()
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	for _, entry := range payload.Items {
		if entry["id"] == itemID {
			version, _ := entry["version"].(string)
			return version
		}
	}
	t.Fatalf("%s did not contain item %s: %s", label, itemID, raw)
	return ""
}

func detailItemVersion(t *testing.T, userID, itemID string) string {
	t.Helper()
	w := get(newItemRouter(userID), "/api/items/"+itemID)
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	version, _ := decodeItemBody(t, w.Body.Bytes())["version"].(string)
	return version
}

// The core property: every projection reports the same version, that version
// follows the repository's commit, and it never moves on its own.
func TestGitBackedVersion_ListAndDetailAgreeAndFollowTheCommit(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitVersionItem(t, "gv-main", "1.4.0", gitVersionFirstSHA)
	favoriteItem(t, "fav-gv-main", "gv-main", "u1")

	const wantFirst = "1.4.0+e7dea05"
	listed := listedItemVersion(t, "/api/items?favorited=true", "u1", "gv-main")
	detail := detailItemVersion(t, "u1", "gv-main")
	mine := myItemVersion(t, "u1", "gv-main")
	if listed != wantFirst || detail != wantFirst || mine != wantFirst {
		t.Fatalf("projections disagree: list=%q detail=%q my=%q want %q", listed, detail, mine, wantFirst)
	}

	// Stable while the commit is. A value that moved per request would make the
	// device tear down and reinstall the capability on every poll.
	for i := 0; i < 3; i++ {
		if again := listedItemVersion(t, "/api/items?favorited=true", "u1", "gv-main"); again != wantFirst {
			t.Fatalf("list version changed without a push on read %d: %q", i, again)
		}
		if again := detailItemVersion(t, "u1", "gv-main"); again != wantFirst {
			t.Fatalf("detail version changed without a push on read %d: %q", i, again)
		}
	}

	// A push lands: the body changed but the manifest's version did not, which
	// is exactly the case the device used to miss.
	gitea.setFile("skill.md", "---\nname: FIX Skill\nversion: 1.4.0\n---\n\n# Rewritten body\n")
	moveItemCommit(t, "gv-main", gitVersionSecondSHA)

	const wantSecond = "1.4.0+1f4b9c2"
	listed = listedItemVersion(t, "/api/items?favorited=true", "u1", "gv-main")
	detail = detailItemVersion(t, "u1", "gv-main")
	mine = myItemVersion(t, "u1", "gv-main")
	if listed != wantSecond || detail != wantSecond || mine != wantSecond {
		t.Fatalf("after the push: list=%q detail=%q my=%q want %q", listed, detail, mine, wantSecond)
	}
	if listed == wantFirst {
		t.Fatal("the version did not move with the commit; an installed copy would never update")
	}

	// The stored column still holds the manifest's own value: this is a
	// projection, and capability_items.version remains the repository's truth.
	var stored models.CapabilityItem
	if err := database.DB.First(&stored, "id = ?", "gv-main").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Version != "1.4.0" {
		t.Fatalf("the suffix was persisted to the DB: %q", stored.Version)
	}
}

// Control group: a DB-backed row's version is the stored column, byte for byte,
// on every endpoint. Git backing is opt-in and must stay invisible here.
func TestDBBackedVersion_IsUnchanged(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedDBContentItem(t, "gv-dbctl")
	if err := database.DB.Model(&models.CapabilityItem{}).
		Where("id = ?", "gv-dbctl").
		Update("version", "2.3.1").Error; err != nil {
		t.Fatalf("set version: %v", err)
	}
	favoriteItem(t, "fav-gv-dbctl", "gv-dbctl", "u1")

	listed := listedItemVersion(t, "/api/items?favorited=true", "u1", "gv-dbctl")
	detail := detailItemVersion(t, "u1", "gv-dbctl")
	mine := myItemVersion(t, "u1", "gv-dbctl")
	if listed != "2.3.1" || detail != "2.3.1" || mine != "2.3.1" {
		t.Fatalf("db-backed version changed: list=%q detail=%q my=%q", listed, detail, mine)
	}
	if strings.Contains(listed+detail+mine, "+") {
		t.Fatal("a db-backed row grew a commit suffix")
	}
}

// The edges of the projection, stated one by one so a later change to the
// format cannot silently drop one of them.
func TestItemWireVersion_Edges(t *testing.T) {
	git := func(version, sha string) *models.CapabilityItem {
		return &models.CapabilityItem{
			ContentBackend: models.ContentBackendGit, Version: version, GitSHA: sha,
		}
	}

	cases := []struct {
		name string
		item *models.CapabilityItem
		want string
	}{
		{
			name: "db-backed rows pass through untouched",
			item: &models.CapabilityItem{Version: "1.0.0", GitSHA: gitVersionFirstSHA},
			want: "1.0.0",
		},
		{
			name: "git-backed rows carry the commit",
			item: git("1.0.0", gitVersionFirstSHA),
			want: "1.0.0+e7dea05",
		},
		{
			name: "a row that has never synced has no commit to anchor to",
			item: git("1.0.0", ""),
			want: "1.0.0",
		},
		{
			name: "a manifest without a version falls back to the commit alone",
			item: git("", gitVersionFirstSHA),
			want: "e7dea05",
		},
		{
			name: "existing build metadata is extended, not duplicated",
			item: git("1.0.0+ci.42", gitVersionFirstSHA),
			want: "1.0.0+ci.42.e7dea05",
		},
		{
			name: "projecting an already-projected value is a no-op",
			item: git("1.0.0+e7dea05", gitVersionFirstSHA),
			want: "1.0.0+e7dea05",
		},
		{
			name: "a pre-release keeps its identifier",
			item: git("2.0.0-rc.1", gitVersionFirstSHA),
			want: "2.0.0-rc.1+e7dea05",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := itemWireVersion(tc.item); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
