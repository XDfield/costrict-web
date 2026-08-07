// Authorization tests for the Git visibility gate.
//
// The scenario every case here is built around is the one the deployed Gitea
// 1.24.6 reports through no webhook at all: a repository stops being public,
// and the local `repositories.visibility` column still says it is. Until the
// periodic reconcile runs, the local answer is wrong — so a caller whose only
// claim is "the repository is public" must have that claim re-checked against
// the Git server before anything is served.
//
// The Git edge is a real httptest server (fakeContentGitea), so these count
// actual HTTP requests rather than stub invocations.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Item ids are real UUIDs because capability_items.id is a PostgreSQL `uuid`
// and the history endpoint rejects anything that cannot name an item before it
// reaches the gate.
const (
	gvPrivatisedItemID  = "a0000000-0000-4000-8000-000000000001"
	gvInternalItemID    = "a0000000-0000-4000-8000-000000000002"
	gvVanishedItemID    = "a0000000-0000-4000-8000-000000000003"
	gvOwnedItemID       = "a0000000-0000-4000-8000-000000000004"
	gvUnreachableItemID = "a0000000-0000-4000-8000-000000000005"
	gvDBItemID          = "a0000000-0000-4000-8000-000000000006"
	gvBurstItemID       = "a0000000-0000-4000-8000-000000000007"
	gvTTLItemID         = "a0000000-0000-4000-8000-000000000008"
)

// newGuardedItemRouter mounts every DETAIL-scoped read of one item behind the
// gate. They are tested together on purpose: fixing only the endpoint a report
// happened to name leaves the same disclosure reachable through its siblings.
func newGuardedItemRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/items/:id", injectUser, GetItem)
	r.GET("/api/items/:id/assets", injectUser, ListItemAssets)
	r.GET("/api/items/:id/versions", injectUser, ListItemVersions)
	r.GET("/api/items/:id/git-history", injectUser, ListItemGitHistory)
	r.GET("/api/items/:id/download", injectUser, DownloadItem)
	return r
}

// guardedItemPaths is every read that must refuse together.
func guardedItemPaths(itemID string) []string {
	return []string{
		"/api/items/" + itemID,
		"/api/items/" + itemID + "/assets",
		"/api/items/" + itemID + "/versions",
		"/api/items/" + itemID + "/git-history",
		"/api/items/" + itemID + "/download",
	}
}

func TestApplyGitBrowseVisibilityFilter_UsesTextRepositoryIDBoundary(t *testing.T) {
	defer setupTestDB(t)()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(middleware.UserIDKey, "usr_550e8400-e29b-41d4-a716-446655440000")

	db := database.GetDB()
	query := applyGitBrowseVisibilityFilter(
		db.Session(&gorm.Session{DryRun: true}).Model(&models.CapabilityItem{}),
		c,
		db,
	)
	result := query.Find(&[]models.CapabilityItem{})
	if result.Error != nil {
		t.Fatalf("build visibility query: %v", result.Error)
	}
	const boundary = "CAST(repo_members.repo_id AS TEXT) = capability_items.repo_id"
	if sql := result.Statement.SQL.String(); !strings.Contains(sql, boundary) {
		t.Fatalf("visibility query omitted UUID-to-text repository boundary: %s", sql)
	}
}

// seedGuardedGitItem is a public-repo, Git-backed row bound to the fake Git
// server, plus one revision so the history endpoint has something it could leak.
func seedGuardedGitItem(t *testing.T, id string) {
	t.Helper()
	seedGitContentItem(t, id, "skill", "skill.md")
	if err := database.GetDB().Exec(`INSERT INTO capability_item_git_revisions
		(id, item_id, revision_no, git_server_id, git_repo_id, git_ref, manifest_path, entry_key,
		 git_sha, version_label, source, observed_at, created_at)
		VALUES (?, ?, 1, ?, ?, 'main', 'skill.md', '', ?, '1.0.0', ?, ?, ?)`,
		"rev-"+id, id, gitContentTestServerID, gitContentTestRepoID,
		strings.Repeat("a", 40), models.GitRevisionSourceBackfill,
		time.Unix(1770000000, 0).UTC(), time.Now()).Error; err != nil {
		t.Fatalf("seed revision: %v", err)
	}
}

// waitForDownloadLogs blocks until a successful /download's asynchronous
// behaviour log has landed. Without it the goroutine outlives the test and
// dereferences a DB handle the cleanup already dropped — the same reason the
// existing download tests wait rather than sleep.
func waitForDownloadLogs(t *testing.T, itemID string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var logged int64
		database.GetDB().Model(&models.BehaviorLog{}).
			Where("item_id = ? AND action_type = ?", itemID, string(models.ActionInstall)).
			Count(&logged)
		if logged >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d download logs for %s, saw %d", want, itemID, logged)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AC-LH16: a public-to-private change blocks content, history and repository
// coordinates on the next attempt — through every detail-scoped endpoint, for
// an anonymous caller and for a signed-in stranger alike.
func TestItemReads_RepositoryTurnedPrivateStopsServingEverything(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvPrivatisedItemID)

	// Before: the repository is public and every endpoint answers.
	for _, path := range guardedItemPaths(gvPrivatisedItemID) {
		if w := get(newGuardedItemRouter(""), path); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 while public, got %d (%s)", path, w.Code, w.Body.String())
		}
	}

	// The successful download above logs asynchronously; let it land before the
	// test DB is torn down under it.
	waitForDownloadLogs(t, gvPrivatisedItemID, 1)

	// The repository goes private on Gitea. Nothing tells us: no webhook exists
	// for this, and repositories.visibility still says "public".
	gitea.goPrivate()
	resetGitVisibilityCache()

	for _, caller := range []string{"", "stranger"} {
		for _, path := range guardedItemPaths(gvPrivatisedItemID) {
			w := get(newGuardedItemRouter(caller), path)
			if w.Code != http.StatusNotFound {
				t.Fatalf("caller %q %s: expected 404 after the repository went private, got %d (%s)",
					caller, path, w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, leak := range []string{
				gitContentTestRepoName,  // the repository coordinate
				"skill.md",              // the manifest path
				strings.Repeat("a", 40), // the commit sha in the history
				"from git",              // the manifest body
				gitContentStaleDBValue,  // the stale DB column
				"1.0.0",                 // the version label
			} {
				if strings.Contains(body, leak) {
					t.Fatalf("caller %q %s: refusal leaked %q: %s", caller, path, leak, body)
				}
			}
		}
	}
}

// A limited organisation's repository reports private=false but is still closed
// to anonymous visitors. Reading only `private` would call it world-readable.
func TestItemReads_InternalRepositoryIsNotPublic(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvInternalItemID)
	gitea.goInternal()

	w := get(newGuardedItemRouter(""), "/api/items/"+gvInternalItemID+"/git-history")
	if w.Code != http.StatusNotFound {
		t.Fatalf("an internal (limited-org) repository was treated as public: %d (%s)", w.Code, w.Body.String())
	}
}

// A repository the git server no longer knows is a definite answer, not a
// failure: it is not public, so it is not served.
func TestItemReads_VanishedRepositoryIsRefused(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvVanishedItemID)
	gitea.vanish()

	w := get(newGuardedItemRouter(""), "/api/items/"+gvVanishedItemID+"/git-history")
	if w.Code != http.StatusNotFound {
		t.Fatalf("a vanished repository was still served: %d (%s)", w.Code, w.Body.String())
	}
}

// The owner and a repository member are not authorized by public visibility, so
// the repository going private cannot revoke their access to their own item's
// history. Fail-closed must not mean "the owner loses their own capability".
func TestItemHistory_OwnerAndMemberStillReadAPrivatisedRepository(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvOwnedItemID) // created_by = u1
	gitea.goPrivate()

	// seedGitContentItem puts the item in repo-gc-<id>; membership there is the
	// second permission that survives.
	if err := database.GetDB().Exec(`INSERT INTO repo_members (id, repo_id, user_id, role, created_at)
		VALUES ('gv-m1', ?, 'teammate', 'member', ?)`, "repo-gc-"+gvOwnedItemID, time.Now()).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}

	for _, caller := range []string{"u1", "teammate"} {
		w := get(newGuardedItemRouter(caller), "/api/items/"+gvOwnedItemID+"/git-history")
		if w.Code != http.StatusOK {
			t.Fatalf("caller %q lost access to their own item: %d (%s)", caller, w.Code, w.Body.String())
		}
		if got := len(decodeGitHistory(t, w).Revisions); got != 1 {
			t.Fatalf("caller %q got %d revisions, want 1", caller, got)
		}
	}
}

// Fail closed, not open: when the git server cannot be reached the claim
// "this repository is public" is unproven, and unproven is not permission.
func TestItemHistory_UnreachableGitServerFailsClosed(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvUnreachableItemID)
	stopFakeGitea(t)

	w := get(newGuardedItemRouter(""), "/api/items/"+gvUnreachableItemID+"/git-history")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the git server cannot answer, got %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "GIT_VISIBILITY_UNVERIFIED") {
		t.Fatalf("refusal does not identify the unverified visibility: %s", body)
	}
	if strings.Contains(body, gitContentTestRepoName) || strings.Contains(body, "skill.md") {
		t.Fatalf("an unverified refusal handed out the repository coordinate: %s", body)
	}
	// The owner is unaffected by the failure to verify: their permission never
	// depended on the answer.
	if w := get(newGuardedItemRouter("u1"), "/api/items/"+gvUnreachableItemID+"/git-history"); w.Code != http.StatusOK {
		t.Fatalf("the owner was blocked by an unverifiable visibility: %d (%s)", w.Code, w.Body.String())
	}
}

// AC-LH11 / the archived-item rule: a hidden Git-backed row answers not-found
// to a caller who may not see it. 403 would confirm that it exists.
func TestItemHistory_ArchivedPrivateItemIsNotFoundNotForbidden(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedGitHistoryItem(t, "private", contentBackendGit)
	seedGitRevision(t, 1, sha40('a'), "1.0.0", models.GitRevisionSourcePush)

	// Active + private: a stranger is told it exists but is off-limits, which is
	// the pre-existing contract for a private repository and stays as it was.
	if w := getHistory(newGitHistoryRouter("stranger"), "/api/items/"+historyItemID+"/git-history"); w.Code != http.StatusForbidden {
		t.Fatalf("active private item: expected 403, got %d (%s)", w.Code, w.Body.String())
	}

	// Archived (however it got there — Git lifecycle convergence or manual
	// moderation): the row is visible to its owner and platform operators only,
	// and everyone else is told nothing.
	setItemStatus(t, historyItemID, "archived")
	for _, caller := range []string{"", "stranger"} {
		w := getHistory(newGitHistoryRouter(caller), "/api/items/"+historyItemID+"/git-history")
		if w.Code != http.StatusNotFound {
			t.Fatalf("archived private item, caller %q: expected 404, got %d (%s)",
				caller, w.Code, w.Body.String())
		}
		if body := w.Body.String(); strings.Contains(body, "SKILL.md") || strings.Contains(body, sha40('a')) {
			t.Fatalf("archived refusal leaked coordinates: %s", body)
		}
	}

	// Someone who may see the private repository keeps the archived row's
	// history — that is the point of archiving rather than deleting, and it is
	// what makes the 404 above a disclosure decision rather than a deletion.
	//
	// Membership is one of three permissions that grant it; since F-24 the
	// item's creator and platform operators are admitted too, without a member
	// row (see TestItemReads_OwnerWithoutMembershipAndAdminAreNotLockedOut).
	// This case keeps a member row anyway so the original membership path
	// stays pinned on its own.
	if err := database.GetDB().Exec(`INSERT INTO repo_members (id, repo_id, user_id, role, created_at)
		VALUES ('gv-arch-m1', ?, 'member-9', 'member', ?)`, historyRepoID, time.Now()).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}
	w := getHistory(newGitHistoryRouter("member-9"), "/api/items/"+historyItemID+"/git-history")
	if w.Code != http.StatusOK {
		t.Fatalf("a repository member lost the archived item's history: %d (%s)", w.Code, w.Body.String())
	}
	if got := len(decodeGitHistory(t, w).Revisions); got != 1 {
		t.Fatalf("archived history rows = %d, want 1", got)
	}
}

// F-24. itemAccessDecision judges a private repository by repo_members alone,
// and the guard used to return its refusal without ever consulting the two
// permissions that live elsewhere: capability_items.created_by and the
// platform-admin role. The item's own creator — who in the Git-backed world
// routinely has no repo_members row, because the repository is theirs on Gitea
// rather than shared through membership — and a platform operator were both
// told their locally-private item does not exist (LH-7 violated).
//
// Neither permission depends on the repository being public, so neither caller
// is probed: the git server is not contacted at all on this path.
func TestItemReads_OwnerWithoutMembershipAndAdminAreNotLockedOut(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	seedGitHistoryItem(t, "private", contentBackendGit) // created_by = owner-1; NO member rows
	seedGitRevision(t, 1, sha40('a'), "1.0.0", models.GitRevisionSourcePush)

	if err := database.GetDB().Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at)
		VALUES ('gv-admin-role', 'platform-op', 'platform_admin', CURRENT_TIMESTAMP)`).Error; err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}

	// Active: the creator (no member row) and the platform admin both read it.
	for _, caller := range []string{"owner-1", "platform-op"} {
		w := getHistory(newGitHistoryRouter(caller), "/api/items/"+historyItemID+"/git-history")
		if w.Code != http.StatusOK {
			t.Fatalf("caller %q is locked out of the locally-private item: %d (%s)", caller, w.Code, w.Body.String())
		}
		if got := len(decodeGitHistory(t, w).Revisions); got != 1 {
			t.Fatalf("caller %q got %d revisions, want 1", caller, got)
		}
	}

	// Archived: LH-7's actual sentence — visible to the owner and platform
	// operators, not-found (not forbidden) to everyone else.
	setItemStatus(t, historyItemID, "archived")
	for _, caller := range []string{"owner-1", "platform-op"} {
		w := getHistory(newGitHistoryRouter(caller), "/api/items/"+historyItemID+"/git-history")
		if w.Code != http.StatusOK {
			t.Fatalf("caller %q lost their own archived item: %d (%s)", caller, w.Code, w.Body.String())
		}
	}
	for _, caller := range []string{"", "stranger"} {
		w := getHistory(newGitHistoryRouter(caller), "/api/items/"+historyItemID+"/git-history")
		if w.Code != http.StatusNotFound {
			t.Fatalf("caller %q on the archived private item: %d, want 404 — 403 confirms existence (%s)",
				caller, w.Code, w.Body.String())
		}
	}

	// None of the four callers above relied on public visibility, so none of
	// them owed the git server a question.
	if repoLookups, rawReads := gitea.counts(); repoLookups != 0 || rawReads != 0 {
		t.Fatalf("locally-authorized reads contacted the git server: %d lookups, %d raw reads", repoLookups, rawReads)
	}
}

// The control group. A DB-backed row has no remote visibility, so the gate must
// never contact the git server for one — not once, on any endpoint.
func TestItemReads_DBBackedRowsNeverProbeTheGitServer(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	seedDBContentItem(t, gvDBItemID)
	if err := database.GetDB().Exec(`INSERT INTO capability_versions
		(id, item_id, revision, version, content, content_md5, created_by, created_at)
		VALUES ('gv-db-v1', ?, 1, '1.0.0', 'body', 'md5', 'u1', ?)`, gvDBItemID, time.Now()).Error; err != nil {
		t.Fatalf("seed version: %v", err)
	}

	for _, path := range guardedItemPaths(gvDBItemID) {
		if w := get(newGuardedItemRouter(""), path); w.Code != http.StatusOK {
			t.Fatalf("%s: db-backed read broke, got %d (%s)", path, w.Code, w.Body.String())
		}
	}
	if repoLookups, rawReads := gitea.counts(); repoLookups != 0 || rawReads != 0 {
		t.Fatalf("db-backed reads contacted the git server: %d lookups, %d raw reads", repoLookups, rawReads)
	}
	waitForDownloadLogs(t, gvDBItemID, 1)
}

// A burst of concurrent readers of the same repository asks the git server
// once. Without this the gate turns one repository's 55 bound capabilities into
// 55 round trips per page view — an authorization check that doubles as a load
// amplifier is not one anybody keeps switched on.
func TestGitVisibilityGate_ConcurrentReadersShareOneProbe(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvBurstItemID)

	// One router, built before the goroutines start: gin.SetMode writes package
	// state, so building a router per goroutine would report a race in the test
	// harness rather than in what is being tested.
	router := newGuardedItemRouter("")

	const readers = 12
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			if w := get(router, "/api/items/"+gvBurstItemID+"/git-history"); w.Code != http.StatusOK {
				t.Errorf("concurrent read failed: %d (%s)", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if repoLookups, _ := gitea.counts(); repoLookups != 1 {
		t.Fatalf("%d concurrent readers produced %d repository lookups, want 1", readers, repoLookups)
	}
}

// The memoized answer has a bounded life. A cache with no expiry would turn
// "the repository went private" into "the repository went private until the
// process restarts".
func TestGitVisibilityGate_MemoizedAnswerExpires(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGuardedGitItem(t, gvTTLItemID)

	original := gitVisibilityTTL
	gitVisibilityTTL = 20 * time.Millisecond
	defer func() { gitVisibilityTTL = original }()

	if w := get(newGuardedItemRouter(""), "/api/items/"+gvTTLItemID+"/git-history"); w.Code != http.StatusOK {
		t.Fatalf("first read: %d (%s)", w.Code, w.Body.String())
	}
	gitea.goPrivate()
	// Still inside the TTL: the memoized "public" answer is deliberately reused.
	if w := get(newGuardedItemRouter(""), "/api/items/"+gvTTLItemID+"/git-history"); w.Code != http.StatusOK {
		t.Fatalf("within the TTL the answer should be reused, got %d", w.Code)
	}
	time.Sleep(30 * time.Millisecond)
	if w := get(newGuardedItemRouter(""), "/api/items/"+gvTTLItemID+"/git-history"); w.Code != http.StatusNotFound {
		t.Fatalf("the memoized answer never expired: %d (%s)", w.Code, w.Body.String())
	}
}
