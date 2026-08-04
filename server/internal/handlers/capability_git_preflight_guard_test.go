package handlers

// Gate matrix — stage 3 (pre-flight 409). One test per refused entry point plus
// a DB-backed control proving the refusal is scoped to content_backend='git'
// and changes nothing for existing rows:
//
//	W9  PUT    /api/items/:id/move
//	W10 PUT    /api/items/:id/transfer     (public branch and repo branch)
//	W11 DELETE /api/items/:id
//	W12 DELETE /api/items                  (batch)
//	W15 DELETE /api/repositories/:id
//	W28 PUT    /api/registries/:id/transfer
//	W29 DELETE /api/registries/:id
//
// The itemdelete backstop (a Git-backed sub-skill reached only by the cascade's
// recursion, which no pre-flight names) is covered at the bottom.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

// seedPreflightItem creates one capability_items row with the given backing.
func seedPreflightItem(t *testing.T, id, registryID, repoID, backend string) {
	t.Helper()
	item := models.CapabilityItem{
		ID: id, RegistryID: registryID, RepoID: repoID, Slug: id, ItemType: "skill",
		Name: "Preflight " + id, Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), Content: "seed content",
		CurrentRevision: 1, ContentBackend: backend,
	}
	if backend == models.ContentBackendGit {
		item.SourceRepoURL = "https://gitea.example.test/u1/repo"
		item.SourceRepoRef = "main"
		item.SourceRepoPath = "skill.md"
		item.SourceGitServerID = "preflight-srv"
		item.SourceGitRepoID = 77
		item.GitSyncStatus = "synced"
	}
	if err := database.DB.Create(&item).Error; err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
}

// assertGitBackedConflict checks the 409 contract every refusal shares: the
// GIT_BACKED_ITEM code plus a message the caller can act on.
func assertGitBackedConflict(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", code, body)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode 409 body: %v (%s)", err, body)
	}
	if payload["error_code"] != "GIT_BACKED_ITEM" {
		t.Fatalf("expected error_code GIT_BACKED_ITEM, got %v", payload["error_code"])
	}
	if msg, _ := payload["error"].(string); msg == "" {
		t.Fatalf("409 body carries no actionable message: %s", body)
	}
}

func itemCount(t *testing.T, id string) int64 {
	t.Helper()
	var count int64
	if err := database.DB.Model(&models.CapabilityItem{}).Where("id = ?", id).Count(&count).Error; err != nil {
		t.Fatalf("count item %s: %v", id, err)
	}
	return count
}

func loadItem(t *testing.T, id string) models.CapabilityItem {
	t.Helper()
	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", id).Error; err != nil {
		t.Fatalf("load item %s: %v", id, err)
	}
	return item
}

// seedRepoCascadeTables adds the three tables DeleteRepository deletes from
// that the shared fixture does not define. Minimal on purpose: the cascade only
// issues DELETE ... WHERE item_id / registry_id IN (...).
func seedRepoCascadeTables(t *testing.T) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS scan_jobs (id TEXT PRIMARY KEY, item_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_jobs (id TEXT PRIMARY KEY, registry_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_logs (id TEXT PRIMARY KEY, registry_id TEXT NOT NULL)`,
	} {
		if err := database.DB.Exec(stmt).Error; err != nil {
			t.Fatalf("create fixture table: %v", err)
		}
	}
}

// seedMoveFixture builds source + target registries in two repos, with the
// caller a member of the target.
func seedMoveFixture(t *testing.T) {
	t.Helper()
	createTestRepository(t, "repo-pf-src", "private")
	createTestRepository(t, "repo-pf-tgt", "private")
	database.DB.Create(&models.RepoMember{ID: "mem-pf-src", RepoID: "repo-pf-src", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.RepoMember{ID: "mem-pf-tgt", RepoID: "repo-pf-tgt", UserID: "u1", Role: "member"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-src", Name: "pf-src", SourceType: "internal", RepoID: "repo-pf-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-tgt", Name: "pf-tgt", SourceType: "internal", RepoID: "repo-pf-tgt", OwnerID: "u1",
	})
}

// ---------------------------------------------------------------------------
// W9 — PUT /api/items/:id/move
// ---------------------------------------------------------------------------

// Moving a Git-backed row rewrites registry_id/repo_id, which the
// git_capability_repositories binding still pins to the old registry: the next
// discovery would create its rows in one registry while this one sits in
// another.
func TestMoveItem_GitBackedRefused(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	seedPreflightItem(t, "pf-move-git", "reg-pf-src", "repo-pf-src", models.ContentBackendGit)

	w := putJSON(newItemRouter("u1"), "/api/items/pf-move-git/move", map[string]any{
		"targetRegistryId": "reg-pf-tgt",
	})
	assertGitBackedConflict(t, w.Code, w.Body.String())

	after := loadItem(t, "pf-move-git")
	if after.RegistryID != "reg-pf-src" || after.RepoID != "repo-pf-src" {
		t.Fatalf("git-backed item was moved: registry=%s repo=%s", after.RegistryID, after.RepoID)
	}
}

// Control (W9): a DB-backed row still moves.
func TestMoveItem_DBBackedStillMoves(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	seedPreflightItem(t, "pf-move-db", "reg-pf-src", "repo-pf-src", models.ContentBackendDB)

	w := putJSON(newItemRouter("u1"), "/api/items/pf-move-db/move", map[string]any{
		"targetRegistryId": "reg-pf-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	after := loadItem(t, "pf-move-db")
	if after.RegistryID != "reg-pf-tgt" || after.RepoID != "repo-pf-tgt" {
		t.Fatalf("db-backed item did not move: registry=%s repo=%s", after.RegistryID, after.RepoID)
	}
}

// ---------------------------------------------------------------------------
// W10 — PUT /api/items/:id/transfer
// ---------------------------------------------------------------------------

// The repo branch writes the same two columns as W9.
func TestTransferItem_GitBackedRefused(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	seedPreflightItem(t, "pf-xfer-git", "reg-pf-src", "repo-pf-src", models.ContentBackendGit)

	w := putJSON(newItemRouter("u1"), "/api/items/pf-xfer-git/transfer", map[string]any{
		"targetRepoId": "repo-pf-tgt",
	})
	assertGitBackedConflict(t, w.Code, w.Body.String())

	after := loadItem(t, "pf-xfer-git")
	if after.RegistryID != "reg-pf-src" || after.RepoID != "repo-pf-src" {
		t.Fatalf("git-backed item was transferred: registry=%s repo=%s", after.RegistryID, after.RepoID)
	}
}

// The "public" branch takes a different code path with its own Updates call;
// the guard sits ahead of the split so both are covered by one check.
func TestTransferItem_GitBackedToPublicRefused(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public-reg", SourceType: "internal", RepoID: "public", OwnerID: "system",
	})
	seedPreflightItem(t, "pf-xfer-git-pub", "reg-pf-src", "repo-pf-src", models.ContentBackendGit)

	w := putJSON(newItemRouter("u1"), "/api/items/pf-xfer-git-pub/transfer", map[string]any{
		"targetRepoId": "public",
	})
	assertGitBackedConflict(t, w.Code, w.Body.String())

	after := loadItem(t, "pf-xfer-git-pub")
	if after.RepoID != "repo-pf-src" {
		t.Fatalf("git-backed item was transferred to public: repo=%s", after.RepoID)
	}
}

// Control (W10): a DB-backed row still transfers.
func TestTransferItem_DBBackedStillTransfers(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	seedPreflightItem(t, "pf-xfer-db", "reg-pf-src", "repo-pf-src", models.ContentBackendDB)

	w := putJSON(newItemRouter("u1"), "/api/items/pf-xfer-db/transfer", map[string]any{
		"targetRepoId": "repo-pf-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	after := loadItem(t, "pf-xfer-db")
	if after.RegistryID != "reg-pf-tgt" || after.RepoID != "repo-pf-tgt" {
		t.Fatalf("db-backed item did not transfer: registry=%s repo=%s", after.RegistryID, after.RepoID)
	}
}

// ---------------------------------------------------------------------------
// W11 — DELETE /api/items/:id
// ---------------------------------------------------------------------------

// The cascade is a physical DELETE. With the repository still bound, the next
// push recreates the capability under a new uuid and every favourite,
// distribution and bookmark on the old id is left dangling.
func TestDeleteItem_GitBackedRefused(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-del", "private")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-del", Name: "pf-del", SourceType: "internal", RepoID: "repo-pf-del", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-del-git", "reg-pf-del", "repo-pf-del", models.ContentBackendGit)

	w := deleteReq(newItemRouter("u1"), "/api/items/pf-del-git")
	assertGitBackedConflict(t, w.Code, w.Body.String())

	if itemCount(t, "pf-del-git") != 1 {
		t.Fatal("git-backed item was deleted")
	}
}

// Control (W11): a DB-backed row still deletes.
func TestDeleteItem_DBBackedStillDeletes(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-del", "private")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-del", Name: "pf-del", SourceType: "internal", RepoID: "repo-pf-del", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-del-db", "reg-pf-del", "repo-pf-del", models.ContentBackendDB)

	w := deleteReq(newItemRouter("u1"), "/api/items/pf-del-db")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if itemCount(t, "pf-del-db") != 0 {
		t.Fatal("db-backed item was not deleted")
	}
}

// ---------------------------------------------------------------------------
// W12 — DELETE /api/items (batch)
// ---------------------------------------------------------------------------

// The batch is all-or-nothing by contract, so one Git-backed id refuses the
// whole request rather than deleting the rest and reporting a partial result.
// W12 — a Git-backed id is skipped rather than refusing the batch. This
// endpoint already reports per-id outcomes (skipped / forbidden), and the
// manager UI reads `skipped` to tell the user how many rows it left alone, so
// refusing outright would strand the caller with no success handler.
// gate-matrix §1.3 fixes this semantics.
func TestBatchDeleteItems_GitBackedIsSkippedNotRefused(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-del", "private")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-del", Name: "pf-del", SourceType: "internal", RepoID: "repo-pf-del", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-batch-db", "reg-pf-del", "repo-pf-del", models.ContentBackendDB)
	seedPreflightItem(t, "pf-batch-git", "reg-pf-del", "repo-pf-del", models.ContentBackendGit)

	w := doJSON(t, newItemRouter("u1"), http.MethodDelete, "/api/items", map[string]any{
		"ids": []string{"pf-batch-db", "pf-batch-git"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Deleted      int      `json:"deleted"`
		Skipped      int      `json:"skipped"`
		GitBackedIDs []string `json:"gitBackedIds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if body.Deleted != 1 || body.Skipped != 1 {
		t.Fatalf("expected 1 deleted and 1 skipped, got %+v", body)
	}
	if len(body.GitBackedIDs) != 1 || body.GitBackedIDs[0] != "pf-batch-git" {
		t.Fatalf("the git-backed id must be named, got %v", body.GitBackedIDs)
	}

	if itemCount(t, "pf-batch-git") != 1 {
		t.Fatal("git-backed item was deleted by the batch")
	}
	if itemCount(t, "pf-batch-db") != 0 {
		t.Fatal("the db-backed item should still have been deleted")
	}
}

// Control (W12): an all-DB batch still deletes.
func TestBatchDeleteItems_DBBackedStillDeletes(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-del", "private")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-del", Name: "pf-del", SourceType: "internal", RepoID: "repo-pf-del", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-batch-db1", "reg-pf-del", "repo-pf-del", models.ContentBackendDB)
	seedPreflightItem(t, "pf-batch-db2", "reg-pf-del", "repo-pf-del", models.ContentBackendDB)

	w := doJSON(t, newItemRouter("u1"), http.MethodDelete, "/api/items", map[string]any{
		"ids": []string{"pf-batch-db1", "pf-batch-db2"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if itemCount(t, "pf-batch-db1")+itemCount(t, "pf-batch-db2") != 0 {
		t.Fatal("db-backed batch was not deleted")
	}
}

// ---------------------------------------------------------------------------
// W15 — DELETE /api/repositories/:id
// ---------------------------------------------------------------------------

// The repo delete hard-deletes every item under its registries, which would
// also orphan the git_capability_repositories binding pointing at them.
func TestDeleteRepository_GitBackedItemRefused(t *testing.T) {
	defer setupTestDB(t)()
	seedRepoCascadeTables(t)
	createTestRepository(t, "repo-pf-cascade", "private")
	database.DB.Create(&models.RepoMember{ID: "mem-pf-cascade", RepoID: "repo-pf-cascade", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-cascade", Name: "pf-cascade", SourceType: "internal", RepoID: "repo-pf-cascade", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-cascade-db", "reg-pf-cascade", "repo-pf-cascade", models.ContentBackendDB)
	seedPreflightItem(t, "pf-cascade-git", "reg-pf-cascade", "repo-pf-cascade", models.ContentBackendGit)

	w := deleteReq(newRepoRouter("u1"), "/api/repositories/repo-pf-cascade")
	assertGitBackedConflict(t, w.Code, w.Body.String())

	if itemCount(t, "pf-cascade-git") != 1 || itemCount(t, "pf-cascade-db") != 1 {
		t.Fatal("the refused repository delete still removed items")
	}
	var repoCount int64
	database.DB.Model(&models.Repository{}).Where("id = ?", "repo-pf-cascade").Count(&repoCount)
	if repoCount != 1 {
		t.Fatal("repository was deleted despite the refusal")
	}
}

// Control (W15): a repository holding only DB-backed items still deletes.
func TestDeleteRepository_DBBackedStillDeletes(t *testing.T) {
	defer setupTestDB(t)()
	seedRepoCascadeTables(t)
	createTestRepository(t, "repo-pf-cascade", "private")
	database.DB.Create(&models.RepoMember{ID: "mem-pf-cascade", RepoID: "repo-pf-cascade", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-cascade", Name: "pf-cascade", SourceType: "internal", RepoID: "repo-pf-cascade", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-cascade-db", "reg-pf-cascade", "repo-pf-cascade", models.ContentBackendDB)

	w := deleteReq(newRepoRouter("u1"), "/api/repositories/repo-pf-cascade")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if itemCount(t, "pf-cascade-db") != 0 {
		t.Fatal("db-backed item survived the repository delete")
	}
}

// ---------------------------------------------------------------------------
// W28 — PUT /api/registries/:id/transfer
// ---------------------------------------------------------------------------

// This is W9 applied to every row under the registry at once, so it is refused
// on the same grounds — and refused whole, because a partial transfer would
// leave the registry and its items disagreeing about repo_id.
func TestTransferRegistry_GitBackedItemRefused(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	seedPreflightItem(t, "pf-regxfer-db", "reg-pf-src", "repo-pf-src", models.ContentBackendDB)
	seedPreflightItem(t, "pf-regxfer-git", "reg-pf-src", "repo-pf-src", models.ContentBackendGit)

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-pf-src/transfer", map[string]any{
		"targetRepoId": "repo-pf-tgt",
	})
	assertGitBackedConflict(t, w.Code, w.Body.String())

	var reg models.CapabilityRegistry
	database.DB.First(&reg, "id = ?", "reg-pf-src")
	if reg.RepoID != "repo-pf-src" {
		t.Fatalf("registry was transferred: repo=%s", reg.RepoID)
	}
	if loadItem(t, "pf-regxfer-git").RepoID != "repo-pf-src" ||
		loadItem(t, "pf-regxfer-db").RepoID != "repo-pf-src" {
		t.Fatal("the refused transfer still re-homed items")
	}
}

// Control (W28): a registry holding only DB-backed items still transfers.
func TestTransferRegistry_DBBackedStillTransfers(t *testing.T) {
	defer setupTestDB(t)()
	seedMoveFixture(t)
	seedPreflightItem(t, "pf-regxfer-db", "reg-pf-src", "repo-pf-src", models.ContentBackendDB)

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-pf-src/transfer", map[string]any{
		"targetRepoId": "repo-pf-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if loadItem(t, "pf-regxfer-db").RepoID != "repo-pf-tgt" {
		t.Fatal("db-backed item was not re-homed")
	}
}

// ---------------------------------------------------------------------------
// W29 — DELETE /api/registries/:id
// ---------------------------------------------------------------------------

// This delete does not cascade into items: it strands them. For a Git-backed
// row both its registry_id and the repository binding would point at a registry
// that no longer exists.
func TestDeleteRegistry_GitBackedItemRefused(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-regdel", "private")
	database.DB.Create(&models.RepoMember{ID: "mem-pf-regdel", RepoID: "repo-pf-regdel", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-regdel", Name: "pf-regdel", SourceType: "internal", RepoID: "repo-pf-regdel", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-regdel-git", "reg-pf-regdel", "repo-pf-regdel", models.ContentBackendGit)

	w := deleteReq(newRegistryRouter("u1"), "/api/registries/reg-pf-regdel")
	assertGitBackedConflict(t, w.Code, w.Body.String())

	var regCount int64
	database.DB.Model(&models.CapabilityRegistry{}).Where("id = ?", "reg-pf-regdel").Count(&regCount)
	if regCount != 1 {
		t.Fatal("registry was deleted despite holding a git-backed item")
	}
}

// Control (W29): a registry holding only DB-backed items still deletes.
func TestDeleteRegistry_DBBackedItemStillDeletes(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-regdel", "private")
	database.DB.Create(&models.RepoMember{ID: "mem-pf-regdel", RepoID: "repo-pf-regdel", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-regdel", Name: "pf-regdel", SourceType: "internal", RepoID: "repo-pf-regdel", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-regdel-db", "reg-pf-regdel", "repo-pf-regdel", models.ContentBackendDB)

	w := deleteReq(newRegistryRouter("u1"), "/api/registries/reg-pf-regdel")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var regCount int64
	database.DB.Model(&models.CapabilityRegistry{}).Where("id = ?", "reg-pf-regdel").Count(&regCount)
	if regCount != 0 {
		t.Fatal("db-only registry was not deleted")
	}
}

// ---------------------------------------------------------------------------
// W11 backstop — the cascade reaches ids the pre-flight never named
// ---------------------------------------------------------------------------

// A DB-backed plugin passes every pre-flight, but the cascade recurses into its
// bundled sub-skills. The per-row check inside itemdelete is what stops a
// Git-backed child from being hard-deleted through its parent.
func TestDeleteItem_GitBackedSubSkillBlocksParentDelete(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-pf-sub", "private")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-pf-sub", Name: "pf-sub", SourceType: "internal", RepoID: "repo-pf-sub", OwnerID: "u1",
	})
	seedPreflightItem(t, "pf-sub-parent", "reg-pf-sub", "repo-pf-sub", models.ContentBackendDB)
	seedPreflightItem(t, "pf-sub-child", "reg-pf-sub", "repo-pf-sub", models.ContentBackendGit)
	parentID := "pf-sub-parent"
	if err := database.DB.Model(&models.CapabilityItem{}).
		Where("id = ?", "pf-sub-child").
		Update("parent_plugin_id", &parentID).Error; err != nil {
		t.Fatalf("link sub-skill: %v", err)
	}

	w := deleteReq(newItemRouter("u1"), "/api/items/pf-sub-parent")
	assertGitBackedConflict(t, w.Code, w.Body.String())

	if itemCount(t, "pf-sub-parent") != 1 || itemCount(t, "pf-sub-child") != 1 {
		t.Fatal("the cascade removed rows despite the git-backed sub-skill")
	}
}
