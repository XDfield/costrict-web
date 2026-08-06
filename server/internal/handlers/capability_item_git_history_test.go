package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

const (
	historyRegistryID = "00000000-0000-0000-0000-0000000000a1"
	historyRepoID     = "00000000-0000-0000-0000-0000000000b1"
	historyItemID     = "00000000-0000-0000-0000-0000000000c1"
)

func newGitHistoryRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/items/:id/git-history", func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}, ListItemGitHistory)
	return r
}

// seedGitHistoryItem plants a repository/registry/item triple.
//
// visibility drives the local half of the authorization decision. The remote
// half — re-confirming that a Git-backed row's repository is still public
// before a caller who relies on that serves the timeline — needs a git server
// that can answer for the row's numeric coordinate, which is why the row is
// bound to the shared content fixture's server/repository ids rather than to
// invented ones.
func seedGitHistoryItem(t *testing.T, visibility, contentBackend string) {
	t.Helper()
	db := database.GetDB()
	if err := db.Exec(`INSERT INTO repositories (id, name, display_name, visibility, repo_type, owner_id, created_at, updated_at)
		VALUES (?, 'history-repo', 'history', ?, 'sync', 'owner-1', ?, ?)`,
		historyRepoID, visibility, time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_registries (id, name, source_type, repo_id, owner_id, created_at, updated_at)
		VALUES (?, 'history-registry', 'git', ?, 'owner-1', ?, ?)`,
		historyRegistryID, historyRepoID, time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_items
		(id, registry_id, repo_id, slug, item_type, name, version, content_backend,
		 source_git_server_id, source_git_repo_id, source_repo_ref, source_repo_path,
		 git_sha, git_sync_status, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, 'history-skill', 'skill', 'History Skill', '1.0.0', ?,
		        ?, ?, 'main', 'SKILL.md', ?, 'synced', 'active', 'owner-1', ?, ?)`,
		historyItemID, historyRegistryID, historyRepoID, contentBackend,
		gitContentTestServerID, gitContentTestRepoID,
		"1111111111111111111111111111111111111111", time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

// setItemStatus drives the hidden-state branch of the refusal.
func setItemStatus(t *testing.T, itemID, status string) {
	t.Helper()
	if err := database.GetDB().
		Exec(`UPDATE capability_items SET status = ? WHERE id = ?`, status, itemID).Error; err != nil {
		t.Fatalf("set status %q: %v", status, err)
	}
}

func seedGitRevision(t *testing.T, revisionNo int64, sha, version, source string) {
	t.Helper()
	if err := database.GetDB().Exec(`INSERT INTO capability_item_git_revisions
		(id, item_id, revision_no, git_server_id, git_repo_id, git_ref, manifest_path, entry_key,
		 git_sha, version_label, source, observed_at, created_at)
		VALUES (?, ?, ?, 'gs-1', 77, 'main', 'SKILL.md', '', ?, ?, ?, ?, ?)`,
		fmt.Sprintf("rev-%d", revisionNo), historyItemID, revisionNo, sha, version, source,
		time.Unix(1770000000+revisionNo, 0).UTC(), time.Now()).Error; err != nil {
		t.Fatalf("seed revision %d: %v", revisionNo, err)
	}
}

func decodeGitHistory(t *testing.T, w *httptest.ResponseRecorder) GitHistoryResponse {
	t.Helper()
	var payload GitHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return payload
}

func getHistory(r *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
	return w
}

func sha40(prefix byte) string {
	out := make([]byte, 40)
	for i := range out {
		out[i] = prefix
	}
	return string(out)
}

// TestListItemGitHistory_OrdersNewestFirstAndPagesByCursor covers the three
// paging rules together: newest-first ordering, the default page size, and a
// cursor that keeps its meaning when newer revisions arrive.
func TestListItemGitHistory_OrdersNewestFirstAndPagesByCursor(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedGitHistoryItem(t, "public", contentBackendGit)
	for i := int64(1); i <= 7; i++ {
		seedGitRevision(t, i, sha40(byte('a'+i-1)), "1."+string(rune('0'+i))+".0", models.GitRevisionSourcePush)
	}
	r := newGitHistoryRouter("")

	w := getHistory(r, "/api/items/"+historyItemID+"/git-history")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	page := decodeGitHistory(t, w)
	if len(page.Revisions) != 5 {
		t.Fatalf("default page size = %d, want 5", len(page.Revisions))
	}
	wantOrder := []int64{7, 6, 5, 4, 3}
	for i, want := range wantOrder {
		if page.Revisions[i].RevisionNo != want {
			t.Fatalf("revision[%d] = %d, want %d", i, page.Revisions[i].RevisionNo, want)
		}
	}
	if !page.HasMore || page.NextBeforeRevision != 3 {
		t.Fatalf("hasMore=%v nextBeforeRevision=%d, want true/3", page.HasMore, page.NextBeforeRevision)
	}
	if page.HistoryBackend != contentBackendGit {
		t.Fatalf("historyBackend = %q, want git", page.HistoryBackend)
	}

	// The cursor is item-bound and stable: appending revision 8 must not shift
	// the page that follows revision 3.
	seedGitRevision(t, 8, sha40('z'), "9.9.9", models.GitRevisionSourceReconcile)
	w = getHistory(r, "/api/items/"+historyItemID+"/git-history?before_revision=3")
	next := decodeGitHistory(t, w)
	if len(next.Revisions) != 2 {
		t.Fatalf("second page size = %d, want 2", len(next.Revisions))
	}
	if next.Revisions[0].RevisionNo != 2 || next.Revisions[1].RevisionNo != 1 {
		t.Fatalf("second page = %d,%d, want 2,1", next.Revisions[0].RevisionNo, next.Revisions[1].RevisionNo)
	}
	if next.HasMore || next.NextBeforeRevision != 0 {
		t.Fatalf("last page reported hasMore=%v cursor=%d", next.HasMore, next.NextBeforeRevision)
	}
}

func TestListItemGitHistory_LimitIsClampedNotRejected(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedGitHistoryItem(t, "public", contentBackendGit)
	for i := int64(1); i <= 25; i++ {
		seedGitRevision(t, i, sha40(byte('a'))[:38]+string(rune('0'+i/10))+string(rune('0'+i%10)),
			"1.0.0", models.GitRevisionSourcePush)
	}
	r := newGitHistoryRouter("")

	w := getHistory(r, "/api/items/"+historyItemID+"/git-history?limit=100")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := len(decodeGitHistory(t, w).Revisions); got != gitHistoryMaxLimit {
		t.Fatalf("clamped page size = %d, want %d", got, gitHistoryMaxLimit)
	}

	w = getHistory(r, "/api/items/"+historyItemID+"/git-history?limit=3")
	if got := len(decodeGitHistory(t, w).Revisions); got != 3 {
		t.Fatalf("explicit page size = %d, want 3", got)
	}

	for _, bad := range []string{"limit=0", "limit=-1", "limit=abc", "before_revision=0", "before_revision=x"} {
		w = getHistory(r, "/api/items/"+historyItemID+"/git-history?"+bad)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", bad, w.Code)
		}
	}
}

// TestListItemGitHistory_EmptyVersionFallsBackToTheShortSHA is the known gap in
// the schema: version_label is legitimately empty when a manifest declares no
// version, and a history row with a blank version is unrenderable.
func TestListItemGitHistory_EmptyVersionFallsBackToTheShortSHA(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedGitHistoryItem(t, "public", contentBackendGit)
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	seedGitRevision(t, 1, sha, "", models.GitRevisionSourceBackfill)
	seedGitRevision(t, 2, sha40('b'), "2.3.4", models.GitRevisionSourceProvision)

	page := decodeGitHistory(t, getHistory(newGitHistoryRouter(""), "/api/items/"+historyItemID+"/git-history"))
	if len(page.Revisions) != 2 {
		t.Fatalf("revisions = %d, want 2", len(page.Revisions))
	}
	labelled, unlabelled := page.Revisions[0], page.Revisions[1]
	if labelled.Version != "2.3.4" {
		t.Fatalf("declared version = %q, want 2.3.4", labelled.Version)
	}
	if labelled.Source != models.GitRevisionSourceProvision {
		t.Fatalf("source = %q, want provision", labelled.Source)
	}
	if unlabelled.Version != "abcdef0" {
		t.Fatalf("fallback version = %q, want the short SHA abcdef0", unlabelled.Version)
	}
	if unlabelled.ShortSHA != "abcdef0" || unlabelled.GitSHA != sha {
		t.Fatalf("sha fields = %q/%q", unlabelled.ShortSHA, unlabelled.GitSHA)
	}
	if unlabelled.Source != models.GitRevisionSourceBackfill {
		t.Fatalf("source = %q, want backfill", unlabelled.Source)
	}
	if unlabelled.ObservedAt == "" {
		t.Fatal("observedAt is empty")
	}
}

// TestListItemGitHistory_PrivateItemIsGatedLikeItemDetail is AC-LH11: history
// is the same disclosure as item detail and must not become the unauthenticated
// way around it.
func TestListItemGitHistory_PrivateItemIsGatedLikeItemDetail(t *testing.T) {
	defer setupTestDB(t)()
	seedGitHistoryItem(t, "private", contentBackendGit)
	seedGitRevision(t, 1, sha40('a'), "1.0.0", models.GitRevisionSourcePush)

	for _, tc := range []struct {
		name   string
		userID string
		want   int
	}{
		{"anonymous", "", http.StatusForbidden},
		{"a stranger", "user-outsider", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := getHistory(newGitHistoryRouter(tc.userID), "/api/items/"+historyItemID+"/git-history")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if body := w.Body.String(); strings.Contains(body, "SKILL.md") || strings.Contains(body, sha40('a')) {
				t.Fatalf("refusal leaked repository coordinates: %s", body)
			}
		})
	}

	// A member of the private repository may read it.
	if err := database.GetDB().Exec(`INSERT INTO repo_members (id, repo_id, user_id, role, created_at)
		VALUES ('m-1', ?, 'user-member', 'member', ?)`, historyRepoID, time.Now()).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}
	w := getHistory(newGitHistoryRouter("user-member"), "/api/items/"+historyItemID+"/git-history")
	if w.Code != http.StatusOK {
		t.Fatalf("member status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := len(decodeGitHistory(t, w).Revisions); got != 1 {
		t.Fatalf("member revisions = %d, want 1", got)
	}
}

func TestListItemGitHistory_UnknownItemIsNotFound(t *testing.T) {
	defer setupTestDB(t)()
	r := newGitHistoryRouter("user-1")
	for _, id := range []string{
		"00000000-0000-0000-0000-0000000000ff",
		// Not a UUID. On PostgreSQL the id column is `uuid`, so this makes the
		// lookup fail with 22P02 instead of missing; answering 500 would blame the
		// server for an id that can never name an item.
		"not-a-uuid",
		"' OR 1=1 --",
	} {
		w := getHistory(r, "/api/items/"+url.PathEscape(id)+"/git-history")
		if w.Code != http.StatusNotFound {
			t.Fatalf("%q: status = %d, want 404 (%s)", id, w.Code, w.Body.String())
		}
	}
}

// TestListItemGitHistory_DBBackedItemReportsAnEmptyGitHistory keeps the
// endpoint answerable for every item: a DB-backed capability is versioned
// through /items/{id}/versions and simply has no Git history.
func TestListItemGitHistory_DBBackedItemReportsAnEmptyGitHistory(t *testing.T) {
	defer setupTestDB(t)()
	seedGitHistoryItem(t, "public", contentBackendDB)
	seedGitRevision(t, 1, sha40('a'), "1.0.0", models.GitRevisionSourceBackfill)

	w := getHistory(newGitHistoryRouter(""), "/api/items/"+historyItemID+"/git-history")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	page := decodeGitHistory(t, w)
	if len(page.Revisions) != 0 || page.HasMore {
		t.Fatalf("db-backed history = %+v, want an empty page", page)
	}
	if page.HistoryBackend != contentBackendDB {
		t.Fatalf("historyBackend = %q, want db", page.HistoryBackend)
	}
}
