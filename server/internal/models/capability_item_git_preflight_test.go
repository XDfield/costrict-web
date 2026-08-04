package models

// Unit tests for the shared pre-flight helper behind the stage-3 409s
// (W11/W12/W14/W15/W28/W29). The per-endpoint behaviour is asserted in
// internal/handlers and internal/adminitem; here the helper's own contract is
// pinned down: the scopes, the sentinel, and the two ways it must stay quiet.

import (
	"errors"
	"strconv"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRefuseGitBackedItems_ByIDRefusesOnlyGitRows(t *testing.T) {
	db := newGuardTestDB(t)
	seedGuardItem(t, db, "pf-db", ContentBackendDB)
	seedGuardItem(t, db, "pf-git", ContentBackendGit)

	if err := RefuseGitBackedItems(db, CapabilityItemsWithIDs("pf-db")); err != nil {
		t.Fatalf("db-backed id must not be refused: %v", err)
	}

	err := RefuseGitBackedItems(db, CapabilityItemsWithIDs("pf-db", "pf-git"))
	if !errors.Is(err, ErrGitBackedItemsPresent) {
		t.Fatalf("expected ErrGitBackedItemsPresent, got %v", err)
	}

	var blocked *GitBackedItemsError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected *GitBackedItemsError, got %T", err)
	}
	if blocked.Total != 1 || len(blocked.Items) != 1 {
		t.Fatalf("expected exactly the git row to block, got total=%d items=%v", blocked.Total, blocked.IDs())
	}
	// The repo coordinate is what makes the 409 actionable — without it the
	// caller is told "no" with nowhere to go.
	ref := blocked.Items[0]
	if ref.ID != "pf-git" || ref.Slug != "pf-git" || ref.SourceRepoURL == "" || ref.SourceRepoRef == "" {
		t.Fatalf("blocking row is under-described: %+v", ref)
	}
}

func TestRefuseGitBackedItems_ByRegistry(t *testing.T) {
	db := newGuardTestDB(t)
	git := seedGuardItem(t, db, "pf-reg-git", ContentBackendGit)
	if err := db.Model(&CapabilityItem{}).Where("id = ?", git.ID).
		Update("registry_id", "reg-other").Error; err != nil {
		t.Fatalf("re-home seed row: %v", err)
	}

	// W28/W29/W15 scope by registry membership, so a git row in another registry
	// must not block — refusing wider than the mutation writes would break
	// legitimate work.
	if err := RefuseGitBackedItems(db, CapabilityItemsInRegistries("reg-1")); err != nil {
		t.Fatalf("git row outside the scoped registry must not block: %v", err)
	}
	if err := RefuseGitBackedItems(db, CapabilityItemsInRegistries("reg-1", "reg-other")); !errors.Is(err, ErrGitBackedItemsPresent) {
		t.Fatalf("expected refusal for the registry holding the git row, got %v", err)
	}
}

func TestRefuseGitBackedItems_EmptyScopeAndUnknownIDsPass(t *testing.T) {
	db := newGuardTestDB(t)
	seedGuardItem(t, db, "pf-git-only", ContentBackendGit)

	// Deleting nothing blocks nothing; a global refusal here would turn every
	// empty batch into a 409.
	if err := RefuseGitBackedItems(db, CapabilityItemsWithIDs()); err != nil {
		t.Fatalf("empty id scope must pass: %v", err)
	}
	if err := RefuseGitBackedItems(db, CapabilityItemsInRegistries()); err != nil {
		t.Fatalf("empty registry scope must pass: %v", err)
	}
	if err := RefuseGitBackedItems(db, CapabilityItemsWithIDs("no-such-item")); err != nil {
		t.Fatalf("unknown id must pass: %v", err)
	}
}

// Total counts every blocking row while the reported list stays bounded: a
// repository-wide delete can match thousands, and the caller needs enough rows
// to act on, not the whole set.
func TestRefuseGitBackedItems_ReportIsCappedButTotalIsNot(t *testing.T) {
	db := newGuardTestDB(t)
	ids := make([]string, 0, gitBackedItemReportLimit+5)
	for i := 0; i < gitBackedItemReportLimit+5; i++ {
		id := "pf-many-" + strconv.Itoa(i)
		seedGuardItem(t, db, id, ContentBackendGit)
		ids = append(ids, id)
	}

	err := RefuseGitBackedItems(db, CapabilityItemsWithIDs(ids...))
	var blocked *GitBackedItemsError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected *GitBackedItemsError, got %v", err)
	}
	if blocked.Total != int64(len(ids)) {
		t.Fatalf("total should count every blocking row: got %d want %d", blocked.Total, len(ids))
	}
	if len(blocked.Items) != gitBackedItemReportLimit {
		t.Fatalf("report should be capped at %d, got %d", gitBackedItemReportLimit, len(blocked.Items))
	}
}

// A schema without content_backend cannot hold Git-backed rows, and querying
// the column there would turn a first-run deployment migration into a hard
// stop. Mirrors the same allowance in the update guard.
func TestRefuseGitBackedItems_SchemaWithoutContentBackendPasses(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE capability_items (
		id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, repo_id TEXT NOT NULL,
		slug TEXT NOT NULL, item_type TEXT NOT NULL, name TEXT NOT NULL,
		status TEXT DEFAULT 'active', created_by TEXT NOT NULL,
		created_at DATETIME, updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create legacy capability_items: %v", err)
	}
	if err := RefuseGitBackedItems(db, CapabilityItemsWithIDs("anything")); err != nil {
		t.Fatalf("legacy schema must pass: %v", err)
	}
	if err := RefuseGitBackedItems(db, CapabilityItemsInRegistries("reg-1")); err != nil {
		t.Fatalf("legacy schema must pass: %v", err)
	}
}

// The helper is used inside transactions (DeleteRepository, TransferRegistry)
// so that the check and the mutation read the same snapshot.
func TestRefuseGitBackedItems_SeesUncommittedTransactionState(t *testing.T) {
	db := newGuardTestDB(t)
	seedGuardItem(t, db, "pf-tx", ContentBackendDB)

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&CapabilityItem{}).Where("id = ?", "pf-tx").
			Update("content_backend", ContentBackendGit).Error; err != nil {
			return err
		}
		return RefuseGitBackedItems(tx, CapabilityItemsWithIDs("pf-tx"))
	})
	if !errors.Is(err, ErrGitBackedItemsPresent) {
		t.Fatalf("expected the in-transaction write to be visible, got %v", err)
	}
}
