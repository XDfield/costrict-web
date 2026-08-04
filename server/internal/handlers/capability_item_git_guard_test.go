package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

// seedGuardedItem creates one capability_items row plus a newer
// capability_versions row, which is what makes reconcileItemCurrentRevision
// want to write during a plain GET.
func seedGuardedItem(t *testing.T, id, backend string) {
	t.Helper()
	item := models.CapabilityItem{
		ID: id, RegistryID: "reg-guard", RepoID: "repo-guard", Slug: id, ItemType: "skill",
		Name: "Guarded " + id, Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), Content: "seed content",
		CurrentRevision: 1, ContentBackend: backend,
	}
	if backend == models.ContentBackendGit {
		item.SourceRepoURL = "https://gitea.example.test/owner/repo"
		item.SourceRepoRef = "main"
		item.SourceRepoPath = "skill.md"
		item.SourceGitServerID = "guard-srv"
		item.SourceGitRepoID = 4242
		item.GitSyncStatus = "synced"
	}
	if err := database.DB.Create(&item).Error; err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
	// A version ahead of current_revision is the trigger for the reconcile.
	if err := database.DB.Create(&models.CapabilityVersion{
		ID: id + "-v3", ItemID: id, Revision: 3, Name: item.Name,
		Content: "seed content", Metadata: datatypes.JSON([]byte("{}")), CreatedBy: "u1",
	}).Error; err != nil {
		t.Fatalf("seed version for %s: %v", id, err)
	}
}

// W23: buildItemResponse reconciles current_revision, which turns every
// GET /api/items/:id into an UPDATE. current_revision is Git-owned, so on a
// Git-backed row that write must not be attempted at all — otherwise the
// guard rejects it and the detail page fails.
func TestGetItem_GitBackedRowIsNotWrittenByRead(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-guard", "public")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-guard", Name: "guard-reg", SourceType: "internal", RepoID: "repo-guard", OwnerID: "u1",
	})
	seedGuardedItem(t, "guard-git", models.ContentBackendGit)

	var before models.CapabilityItem
	if err := database.DB.First(&before, "id = ?", "guard-git").Error; err != nil {
		t.Fatalf("load seeded row: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	w := get(newItemRouter(""), "/api/items/guard-git")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Without the early return the reconcile bumps the in-memory value to 3,
	// serves it, and then has its UPDATE refused — the response would advertise
	// a revision the database does not have.
	if body["currentRevision"] != float64(1) {
		t.Fatalf("response revision diverged from the stored row: %v", body["currentRevision"])
	}

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", "guard-git").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.CurrentRevision != before.CurrentRevision {
		t.Fatalf("GET rewrote current_revision: %d -> %d", before.CurrentRevision, after.CurrentRevision)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("GET produced an UPDATE: updated_at %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
}

// Control: the same read on a DB-backed row must still reconcile, so the
// early return above is scoped to Git backing and not a behaviour change.
func TestGetItem_DBBackedRowStillReconcilesRevision(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-guard", "public")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-guard", Name: "guard-reg", SourceType: "internal", RepoID: "repo-guard", OwnerID: "u1",
	})
	seedGuardedItem(t, "guard-db", models.ContentBackendDB)

	if w := get(newItemRouter(""), "/api/items/guard-db"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", "guard-db").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.CurrentRevision != 3 {
		t.Fatalf("db-backed row was not reconciled: current_revision=%d", after.CurrentRevision)
	}
}

// R1.6: status/isBuiltIn are deliberately outside the Git-owned set, and
// updateItemFromJSON persists them with db.Save(&item) — which rewrites every
// column. The guard must diff rather than reject on the write shape alone.
func TestUpdateItem_StatusOnlyOnGitBackedRowIsAllowed(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-guard", "public")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-guard", Name: "guard-reg", SourceType: "internal", RepoID: "repo-guard", OwnerID: "u1",
	})
	seedGuardedItem(t, "guard-status", models.ContentBackendGit)

	w := putJSON(newItemRouter("u1"), "/api/items/guard-status", map[string]any{"status": "archived"})
	if w.Code != http.StatusOK {
		t.Fatalf("status-only update should succeed, got %d: %s", w.Code, w.Body.String())
	}

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", "guard-status").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != "archived" {
		t.Fatalf("status did not land: %q", after.Status)
	}
	if after.Content != "seed content" || after.GitSyncStatus != "synced" || after.SourceRepoPath != "skill.md" {
		t.Fatalf("git-owned columns drifted: %+v", after)
	}
}

// The content path stays a 409 (pre-existing handler guard), so the new
// backstop did not change the user-visible contract.
func TestUpdateItem_ContentOnGitBackedRowStill409s(t *testing.T) {
	defer setupTestDB(t)()
	createTestRepository(t, "repo-guard", "public")
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-guard", Name: "guard-reg", SourceType: "internal", RepoID: "repo-guard", OwnerID: "u1",
	})
	seedGuardedItem(t, "guard-content", models.ContentBackendGit)

	w := putJSON(newItemRouter("u1"), "/api/items/guard-content", map[string]any{"content": "rewritten"})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var after models.CapabilityItem
	if err := database.DB.First(&after, "id = ?", "guard-content").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Content != "seed content" {
		t.Fatalf("content was written: %q", after.Content)
	}
}
