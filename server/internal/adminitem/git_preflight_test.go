package adminitem

// W14 — DELETE /api/admin/items/:id and POST /admin/items/batch-delete.
//
// Admin moderation shares internal/itemdelete's physical cascade with the
// public endpoints, so it inherits the same problem: deleting a Git-backed row
// does not remove the capability, it detaches it. The repository stays bound
// and the next push recreates the row under a fresh uuid, stranding every
// favourite, distribution and bookmark on the old id.
//
// Each refusal is paired with a DB-backed control: the existing admin delete
// behaviour must be unchanged.

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// addGitBackingColumns brings the admin fixture's capability_items up to the
// columns the pre-flight reads. The shared fixture predates Git backing, which
// is itself the legacy-schema case the helper stays inert on — so a test that
// wants a Git-backed row has to add them.
func addGitBackingColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE capability_items ADD COLUMN content_backend TEXT NOT NULL DEFAULT 'db'`,
		`ALTER TABLE capability_items ADD COLUMN source_repo_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE capability_items ADD COLUMN source_repo_ref TEXT NOT NULL DEFAULT 'main'`,
	} {
		mustExec(t, db, stmt)
	}
}

func markGitBacked(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	if err := db.Exec(`UPDATE capability_items
		SET content_backend = 'git', source_repo_url = 'https://gitea.example.test/u2/repo', source_repo_ref = 'main'
		WHERE id = ?`, id).Error; err != nil {
		t.Fatalf("mark %s git-backed: %v", id, err)
	}
}

func adminItemCount(t *testing.T, db *gorm.DB, id string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM capability_items WHERE id = ?`, id).Scan(&n).Error; err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n
}

func assertGitBackedConflictBody(t *testing.T, code int, body string) {
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

func TestHandler_DeleteItem_GitBackedRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	addGitBackingColumns(t, db)
	seedRepoRegistry(t, db)
	seedItem(t, db, "gi1", "Git Alpha", "skill", "active", "clean", "u2", 4.5)
	markGitBacked(t, db, "gi1")
	m := New(db)

	c, rec := newCtx(t, http.MethodDelete, "/admin/items/gi1", "admin1", "")
	c.Params = gin.Params{{Key: "id", Value: "gi1"}}
	m.DeleteItemHandler()(c)
	assertGitBackedConflictBody(t, rec.Code, rec.Body.String())

	if adminItemCount(t, db, "gi1") != 1 {
		t.Fatal("git-backed item was deleted by the admin endpoint")
	}
}

// Control (W14): with the Git columns present, a DB-backed row still deletes —
// the refusal is scoped to content_backend, not to the schema.
func TestHandler_DeleteItem_DBBackedStillDeletes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	addGitBackingColumns(t, db)
	seedRepoRegistry(t, db)
	seedItem(t, db, "di1", "DB Alpha", "skill", "active", "clean", "u2", 4.5)
	m := New(db)

	c, rec := newCtx(t, http.MethodDelete, "/admin/items/di1", "admin1", "")
	c.Params = gin.Params{{Key: "id", Value: "di1"}}
	m.DeleteItemHandler()(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if adminItemCount(t, db, "di1") != 0 {
		t.Fatal("db-backed item was not deleted")
	}
}

// A Git-backed id is skipped, not refused: the endpoint already reports per-id
// outcomes, so one undeletable row must not cost the admin the rest of the
// batch. gate-matrix §1.3 fixes this semantics for W12/W14, and the manager UI
// reads `skipped` to tell the user how many rows it left alone.
func TestHandler_BatchDeleteItems_GitBackedIsSkippedNotRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	addGitBackingColumns(t, db)
	seedRepoRegistry(t, db)
	seedItem(t, db, "bd1", "DB One", "skill", "active", "clean", "u2", 4.0)
	seedItem(t, db, "bg1", "Git One", "skill", "active", "clean", "u2", 4.0)
	markGitBacked(t, db, "bg1")
	m := New(db)

	c, rec := newCtx(t, http.MethodPost, "/admin/items/batch-delete", "admin1", `{"ids":["bd1","bg1"]}`)
	m.BatchDeleteItemsHandler()(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Deleted    int      `json:"deleted"`
		Skipped    int      `json:"skipped"`
		SkippedIDs []string `json:"skippedIds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Deleted != 1 || body.Skipped != 1 {
		t.Fatalf("expected 1 deleted and 1 skipped, got %+v", body)
	}
	if len(body.SkippedIDs) != 1 || body.SkippedIDs[0] != "bg1" {
		t.Fatalf("the git-backed id must be named in skippedIds, got %v", body.SkippedIDs)
	}

	if adminItemCount(t, db, "bg1") != 1 {
		t.Fatal("git-backed item was deleted by the admin batch")
	}
	if adminItemCount(t, db, "bd1") != 0 {
		t.Fatal("the db-backed item should still have been deleted")
	}
}

// Control (W14 batch): an all-DB batch still deletes.
func TestHandler_BatchDeleteItems_DBBackedStillDeletes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	addGitBackingColumns(t, db)
	seedRepoRegistry(t, db)
	seedItem(t, db, "bd1", "DB One", "skill", "active", "clean", "u2", 4.0)
	seedItem(t, db, "bd2", "DB Two", "skill", "active", "clean", "u2", 4.0)
	m := New(db)

	c, rec := newCtx(t, http.MethodPost, "/admin/items/batch-delete", "admin1", `{"ids":["bd1","bd2"]}`)
	m.BatchDeleteItemsHandler()(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if adminItemCount(t, db, "bd1")+adminItemCount(t, db, "bd2") != 0 {
		t.Fatal("db-backed batch was not deleted")
	}
}

// The service-level sentinel is what the handler keys on; assert it directly so
// a future caller outside internal/adminitem gets a matchable error rather than
// an opaque failure.
func TestService_DeleteItem_GitBackedReturnsSentinel(t *testing.T) {
	db := setupTestDB(t)
	addGitBackingColumns(t, db)
	seedRepoRegistry(t, db)
	seedItem(t, db, "gs1", "Git Sentinel", "skill", "active", "clean", "u2", 4.0)
	markGitBacked(t, db, "gs1")

	svc := NewService(db)
	if err := svc.DeleteItem("gs1"); !errors.Is(err, models.ErrGitBackedItemsPresent) {
		t.Fatalf("expected ErrGitBackedItemsPresent, got %v", err)
	}
	// The batch reports the id as skipped instead of failing — same outcome as
	// an id that no longer exists, which this endpoint already handles that way.
	deleted, skipped, err := svc.BatchDeleteItems([]string{"gs1"})
	if err != nil {
		t.Fatalf("batch must not fail on a git-backed id: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("nothing should have been deleted, got %v", deleted)
	}
	if len(skipped) != 1 || skipped[0] != "gs1" {
		t.Fatalf("expected the git-backed id in skipped, got %v", skipped)
	}
}
