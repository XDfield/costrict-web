// Does the Git lifecycle claim actually reach a client?
//
// capability_items has carried git_lifecycle_reason / git_lifecycle_changed_at
// since the lifecycle work landed, and the hub's detail page renders a block
// off them. Between the two sits ItemResponse, which projected git_sync_status
// and git_last_synced_at and stopped there — so the columns existed, the UI
// existed, and nothing joined them. These tests pin the join, and pin who is
// allowed to see it.

package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
)

// archiveItemByGit writes the lifecycle claim the way the sync service writes
// it: status + orphan marker + reason + transition time, in one statement.
// Raw SQL rather than the model, because the models layer guards this column
// against exactly the kind of write a test would otherwise make casually.
func archiveItemByGit(t *testing.T, itemID, reason string, at time.Time) {
	t.Helper()
	if err := database.GetDB().Exec(
		`UPDATE capability_items
		    SET status = 'archived', git_sync_status = 'orphaned',
		        git_lifecycle_reason = ?, git_lifecycle_changed_at = ?
		  WHERE id = ?`, reason, at.UTC(), itemID).Error; err != nil {
		t.Fatalf("archive %s by git: %v", itemID, err)
	}
}

// setGitLifecycleClaim writes only the two lifecycle columns, leaving status
// and the orphan marker alone. Not a state the sync service produces — it is
// the isolation the serialization claim needs: with the row still readable,
// a missing field in the response can only be the projection's fault.
func setGitLifecycleClaim(t *testing.T, itemID, reason string, at time.Time) {
	t.Helper()
	if err := database.GetDB().Exec(
		`UPDATE capability_items SET git_lifecycle_reason = ?, git_lifecycle_changed_at = ? WHERE id = ?`,
		reason, at.UTC(), itemID).Error; err != nil {
		t.Fatalf("set lifecycle claim on %s: %v", itemID, err)
	}
}

// The projection itself: a Git-backed row carrying a lifecycle claim serves
// both fields, and gitLifecycleChangedAt is a parseable RFC 3339 timestamp
// rather than whatever a driver felt like rendering.
func TestGetItem_GitLifecycleClaimIsProjected(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-lifecycle", "skill", "skill.md")

	changedAt := time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC)
	setGitLifecycleClaim(t, "gc-lifecycle", models.GitLifecycleReasonManifestRemoved, changedAt)

	w := get(newItemRouter(""), "/api/items/gc-lifecycle")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())

	if body["gitLifecycleReason"] != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("gitLifecycleReason missing from the response: %v", body["gitLifecycleReason"])
	}
	raw, ok := body["gitLifecycleChangedAt"].(string)
	if !ok {
		t.Fatalf("gitLifecycleChangedAt missing from the response: %v", body["gitLifecycleChangedAt"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("gitLifecycleChangedAt is not RFC 3339: %q (%v)", raw, err)
	}
	if !parsed.UTC().Equal(changedAt) {
		t.Fatalf("gitLifecycleChangedAt round-tripped wrong: got %s want %s", parsed.UTC(), changedAt)
	}
}

// A row with no claim must not grow empty keys: the frontend reads
// `item.gitLifecycleReason &&` as "Git archived this", so an empty string
// present on every healthy row would be one truthiness bug away from showing
// the archive banner on live capabilities.
func TestGetItem_HealthyGitRowOmitsLifecycleFields(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-healthy", "skill", "skill.md")

	w := get(newItemRouter(""), "/api/items/gc-healthy")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if _, present := body["gitLifecycleReason"]; present {
		t.Fatalf("healthy row emitted gitLifecycleReason: %v", body["gitLifecycleReason"])
	}
	if _, present := body["gitLifecycleChangedAt"]; present {
		t.Fatalf("healthy row emitted gitLifecycleChangedAt: %v", body["gitLifecycleChangedAt"])
	}
}

// A DB-backed row never carries a Git claim, and must not start reporting one
// even if the columns hold residue. Same rule the other git* fields follow:
// the projection is gated on the backend, not on the column being non-empty.
func TestGetItem_DBBackedRowNeverProjectsLifecycleFields(t *testing.T) {
	defer setupTestDB(t)()
	seedDBContentItem(t, "db-lifecycle")
	setGitLifecycleClaim(t, "db-lifecycle", models.GitLifecycleReasonRepositoryDeleted,
		time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC))

	w := get(newItemRouter(""), "/api/items/db-lifecycle")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if _, present := body["gitLifecycleReason"]; present {
		t.Fatalf("db-backed row projected a Git lifecycle reason: %v", body["gitLifecycleReason"])
	}
	if _, present := body["gitLifecycleChangedAt"]; present {
		t.Fatalf("db-backed row projected a Git lifecycle time: %v", body["gitLifecycleChangedAt"])
	}
}

// Authorization: the two new fields ride the existing gate and open no door of
// their own. The repository goes private on the Git server while the local
// visibility column still says public — the case authorizeItemRead exists for —
// and a stranger must get the refusal, not a body carrying the lifecycle claim.
func TestGetItem_LifecycleFieldsAreNotServedToUnauthorizedCallers(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-private-lifecycle", "skill", "skill.md")
	setGitLifecycleClaim(t, "gc-private-lifecycle", models.GitLifecycleReasonManifestRemoved,
		time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC))
	gitea.goPrivate()

	w := get(newItemRouter("stranger"), "/api/items/gc-private-lifecycle")
	if w.Code == http.StatusOK {
		t.Fatalf("a stranger read a repository that went private: %s", w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if _, present := body["gitLifecycleReason"]; present {
		t.Fatalf("refusal leaked the lifecycle reason: %v", body["gitLifecycleReason"])
	}
	if _, present := body["gitLifecycleChangedAt"]; present {
		t.Fatalf("refusal leaked the lifecycle time: %v", body["gitLifecycleChangedAt"])
	}

	// The owner keeps their own row, claim included: LH-7 says an archived
	// private item stays visible to its owner and to platform operators.
	w = get(newItemRouter("u1"), "/api/items/gc-private-lifecycle")
	if w.Code != http.StatusOK {
		t.Fatalf("the owner lost their own item: %d %s", w.Code, w.Body.String())
	}
	if got := decodeItemBody(t, w.Body.Bytes())["gitLifecycleReason"]; got != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("owner did not receive the lifecycle reason: %v", got)
	}
}

// ── The gap the projection alone does not close ─────────────────────────────
//
// Characterization, not endorsement. In every state the sync service actually
// produces a reason, the manifest is gone from Git — that is what caused the
// archive. GetItem reads content through before it builds a response and fails
// the whole request when it cannot, so the detail endpoint answers 502
// GIT_CONTENT_MISSING for exactly the rows whose lifecycle claim the hub's
// detail page wants to render. The field is on the wire; the response carrying
// it is unreachable on this path.
//
// This test exists so the next change to that behaviour is a deliberate one.
// The 502 is also a factually wrong statement for `repository_deleted`: it
// tells the caller to retry a read that can never succeed again.
func TestGetItem_ArchivedByGitServesLifecycleClaimWithoutContent(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-archived", "skill", "skill.md")

	// The manifest disappears, which is what produced the archive in the first
	// place, and the archive is recorded exactly as the sync service records it.
	delete(gitea.files, "skill.md")
	archiveItemByGit(t, "gc-archived", models.GitLifecycleReasonManifestRemoved, time.Now())

	w := get(newItemRouter("u1"), "/api/items/gc-archived")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if body["gitLifecycleReason"] != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("missing lifecycle reason: %v", body["gitLifecycleReason"])
	}
	if body["content"] != "" {
		t.Fatalf("archived missing content must be empty: %v", body["content"])
	}
}

// The terminal case, which is where the wrong statement costs the most: the
// repository is gone, so the read cannot succeed on any retry, yet the caller
// is handed a 5xx that reads as "upstream trouble, come back later". The one
// field that would say "this is permanent" is the one the response cannot
// reach.
func TestGetItem_RepositoryDeletedServesTerminalLifecycleClaim(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-gone", "skill", "skill.md")

	gitea.vanish()
	archiveItemByGit(t, "gc-gone", models.GitLifecycleReasonRepositoryDeleted, time.Now())

	w := get(newItemRouter("u1"), "/api/items/gc-gone")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	if body["gitLifecycleReason"] != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("missing terminal lifecycle reason: %v", body["gitLifecycleReason"])
	}
	if body["content"] != "" {
		t.Fatalf("deleted repository content must be empty: %v", body["content"])
	}
}

func TestGetItem_ActiveMissingGitContentStillFails(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-active-missing", "skill", "skill.md")
	delete(gitea.files, "skill.md")
	w := get(newItemRouter("u1"), "/api/items/gc-active-missing")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for active missing content, got %d: %s", w.Code, w.Body.String())
	}
}

// The list path is where the claim is reachable today, because ListAllItems
// serializes models.CapabilityItem directly and the model has carried the JSON
// tags all along. Asserted so the two paths cannot silently diverge again: if
// a future refactor routes the list through ItemResponse, this fails unless the
// fields came along.
func TestListAllItems_ArchivedGitRowCarriesLifecycleClaim(t *testing.T) {
	defer setupTestDB(t)()
	gitea := setupGitContentFixture(t)
	gitea.setFile("skill.md", gitContentSkillFile)
	seedGitContentItem(t, "gc-listed", "skill", "skill.md")
	archiveItemByGit(t, "gc-listed", models.GitLifecycleReasonRepositoryDeleted, time.Now())

	w := get(newItemRouter("u1"), "/api/items?status=archived")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := decodeItemBody(t, w.Body.Bytes())
	items, _ := body["items"].([]any)
	var found map[string]any
	for _, raw := range items {
		entry, _ := raw.(map[string]any)
		if entry["id"] == "gc-listed" {
			found = entry
			break
		}
	}
	if found == nil {
		t.Fatalf("archived git row absent from the archived listing: %s", w.Body.String())
	}
	if found["gitLifecycleReason"] != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("list entry lost the lifecycle reason: %v", found["gitLifecycleReason"])
	}
	if found["gitLifecycleChangedAt"] == nil {
		t.Fatal("list entry lost the lifecycle transition time")
	}
}

// Guard against the seed drifting: the fixture must really be Git-backed, or
// every assertion above would pass vacuously against a DB row.
func TestGitLifecycleFixtureIsGitBacked(t *testing.T) {
	defer setupTestDB(t)()
	setupGitContentFixture(t)
	seedGitContentItem(t, "gc-fixture", "skill", "skill.md")
	var item models.CapabilityItem
	if err := database.GetDB().First(&item, "id = ?", "gc-fixture").Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !isGitBacked(&item) {
		t.Fatalf("fixture is not git-backed: content_backend=%q", item.ContentBackend)
	}
}
