package models

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

// archiveGuardItem puts a seeded Git-backed row into the state Git leaves it in
// when it takes the capability down: archived, orphan-marked, and carrying the
// reason that is the recovery discriminator.
func archiveGuardItem(t *testing.T, db *gorm.DB, id, reason string) {
	t.Helper()
	if err := db.Exec(
		`UPDATE capability_items
		 SET status = 'archived', git_sync_status = 'orphaned',
		     git_lifecycle_reason = ?, git_lifecycle_changed_at = ?
		 WHERE id = ?`, reason, time.Now().UTC(), id).Error; err != nil {
		t.Fatalf("archive %s: %v", id, err)
	}
}

func lifecycleStateOf(t *testing.T, db *gorm.DB, id string) (status string, reason *string, syncStatus string) {
	t.Helper()
	var row struct {
		Status             string
		GitLifecycleReason *string
		GitSyncStatus      string
	}
	if err := db.Raw(
		`SELECT status, git_lifecycle_reason, git_sync_status FROM capability_items WHERE id = ?`, id,
	).Scan(&row).Error; err != nil {
		t.Fatalf("read lifecycle state of %s: %v", id, err)
	}
	return row.Status, row.GitLifecycleReason, row.GitSyncStatus
}

// The hole this guard closes: `status` is deliberately not a Git-owned column,
// so before this every writer in the codebase — and every writer that never
// learned Git backing exists — could flip a Git-archived row to 'active' while
// the archive claim was still on it. The result is a live marketplace entry with
// no reachable content that Git can never auto-recover, because recovery
// requires the row to be archived. Nothing errors; it is silent.
func TestGitLifecycleGuard_RefusesActivationWhileGitHoldsAClaim(t *testing.T) {
	for _, reason := range []string{
		GitLifecycleReasonManifestRemoved,
		GitLifecycleReasonDefaultBranchMissing,
		GitLifecycleReasonRepositoryDeleted,
	} {
		t.Run(reason, func(t *testing.T) {
			db := newGuardTestDB(t)
			item := seedGuardItem(t, db, "git-item", ContentBackendGit)
			archiveGuardItem(t, db, item.ID, reason)

			// The two shapes real writers use: a map update (adminitem.SetStatus)
			// and a struct Save (PUT /items/:id).
			err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).Update("status", "active").Error
			if !errors.Is(err, ErrGitLifecycleArchived) {
				t.Fatalf("map update error = %v, want ErrGitLifecycleArchived", err)
			}
			var current CapabilityItem
			if err := db.First(&current, "id = ?", item.ID).Error; err != nil {
				t.Fatal(err)
			}
			current.Status = "active"
			if err := db.Save(&current).Error; !errors.Is(err, ErrGitLifecycleArchived) {
				t.Fatalf("struct save error = %v, want ErrGitLifecycleArchived", err)
			}

			status, storedReason, _ := lifecycleStateOf(t, db, item.ID)
			if status != "archived" || storedReason == nil || *storedReason != reason {
				t.Fatalf("refused write still mutated the row: status=%q reason=%v", status, storedReason)
			}
		})
	}
}

// Taking a row DOWN stays allowed — moderation must work on a Git-backed row —
// and doing so clears Git's claim in the SAME statement. That is what makes the
// human's decision survive the manifest coming back: Git's recovery predicate
// needs a recoverable reason, and there no longer is one.
//
// Both real writer shapes are exercised: adminitem.SetStatus updates through a
// map, PUT /items/:id through Save(&item).
func TestGitLifecycleGuard_ManualHidingClearsTheGitClaimAtomically(t *testing.T) {
	for _, hidden := range CapabilityHiddenStatuses() {
		for _, shape := range []string{"map", "save"} {
			t.Run(hidden+"/"+shape, func(t *testing.T) {
				db := newGuardTestDB(t)
				item := seedGuardItem(t, db, "git-item", ContentBackendGit)
				archiveGuardItem(t, db, item.ID, GitLifecycleReasonManifestRemoved)

				if shape == "map" {
					if err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
						Update("status", hidden).Error; err != nil {
						t.Fatalf("manual %s: %v", hidden, err)
					}
				} else {
					var current CapabilityItem
					if err := db.First(&current, "id = ?", item.ID).Error; err != nil {
						t.Fatal(err)
					}
					current.Status = hidden
					if err := db.Save(&current).Error; err != nil {
						t.Fatalf("manual %s via Save: %v", hidden, err)
					}
				}

				status, reason, syncStatus := lifecycleStateOf(t, db, item.ID)
				if status != hidden {
					t.Errorf("status = %q, want %q", status, hidden)
				}
				if reason != nil {
					t.Errorf("git claim survived a manual hide: %q", *reason)
				}
				// The orphan marker is deliberately preserved: it is diagnostic
				// ("Git took this down originally"), and revoking the permission is
				// the reason's job alone.
				if syncStatus != "orphaned" {
					t.Errorf("git_sync_status = %q, want the diagnosis preserved", syncStatus)
				}

				// And afterwards the moderator can undo their own decision, because
				// Git no longer claims the row.
				if err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
					Update("status", "active").Error; err != nil {
					t.Fatalf("re-activation after a manual hide must be allowed: %v", err)
				}
			})
		}
	}
}

// Everything the guard must NOT interfere with: a clean Git-backed row, a
// db-backed row, and a statement that does not write status at all.
func TestGitLifecycleGuard_LeavesUnclaimedAndUnrelatedWritesAlone(t *testing.T) {
	db := newGuardTestDB(t)
	clean := seedGuardItem(t, db, "git-clean", ContentBackendGit)
	dbBacked := seedGuardItem(t, db, "db-item", ContentBackendDB)
	if err := db.Exec(
		`UPDATE capability_items SET status='archived', git_lifecycle_reason=?, git_lifecycle_changed_at=?
		 WHERE id = ?`, GitLifecycleReasonRepositoryDeleted, time.Now().UTC(), dbBacked.ID).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.Model(&CapabilityItem{}).Where("id = ?", clean.ID).
		Update("status", "active").Error; err != nil {
		t.Errorf("unclaimed Git-backed row was blocked: %v", err)
	}
	// A db-backed row has no Git lifecycle, so even a (nonsensical) reason on it
	// must not be enforced — the guard is scoped by content_backend.
	if err := db.Model(&CapabilityItem{}).Where("id = ?", dbBacked.ID).
		Update("status", "active").Error; err != nil {
		t.Errorf("db-backed row was blocked by a Git lifecycle claim: %v", err)
	}
	if err := db.Model(&CapabilityItem{}).Where("id = ?", clean.ID).
		Update("install_count", 7).Error; err != nil {
		t.Errorf("counter write was blocked: %v", err)
	}
}

// The Git writer is the one caller allowed to move a claimed row, and it says so
// explicitly on its own transaction. Without the bypass, the sync's own
// reactivation would be refused by this guard — the writer would be blocked from
// undoing a claim only it can hold.
func TestGitLifecycleGuard_HonoursTheAuthoritativeGitWriterBypass(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-item", ContentBackendGit)
	archiveGuardItem(t, db, item.ID, GitLifecycleReasonManifestRemoved)

	if err := db.Set(GitSyncBypassSetting, true).Model(&CapabilityItem{}).
		Where("id = ?", item.ID).
		Updates(map[string]any{"status": "active", "git_lifecycle_reason": nil}).Error; err != nil {
		t.Fatalf("Git writer was blocked by the lifecycle guard: %v", err)
	}
	if status, reason, _ := lifecycleStateOf(t, db, item.ID); status != "active" || reason != nil {
		t.Fatalf("Git reactivation did not land: status=%q reason=%v", status, reason)
	}
}

// A batch that touches one claimed row is refused whole. A partial success here
// is indistinguishable to the operator from a complete one, which is the worse
// failure: they would believe every item was published.
func TestGitLifecycleGuard_RefusesABatchContainingAClaimedRow(t *testing.T) {
	db := newGuardTestDB(t)
	clean := seedGuardItem(t, db, "git-clean", ContentBackendGit)
	claimed := seedGuardItem(t, db, "git-claimed", ContentBackendGit)
	archiveGuardItem(t, db, claimed.ID, GitLifecycleReasonRepositoryDeleted)

	err := db.Model(&CapabilityItem{}).
		Where("id IN ?", []string{clean.ID, claimed.ID}).
		Update("status", "active").Error
	if !errors.Is(err, ErrGitLifecycleArchived) {
		t.Fatalf("batch error = %v, want ErrGitLifecycleArchived", err)
	}
	if status, _, _ := lifecycleStateOf(t, db, claimed.ID); status != "archived" {
		t.Fatalf("claimed row status = %q, want unchanged", status)
	}
}

// An unrecognised status is treated as an activation, not as harmless. `status`
// is a free string on PUT /items/:id, so a future value that nobody thought to
// add to the hidden set must fail closed rather than silently re-open the hole.
func TestGitLifecycleGuard_TreatsUnknownStatusAsActivation(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-item", ContentBackendGit)
	archiveGuardItem(t, db, item.ID, GitLifecycleReasonManifestRemoved)

	err := db.Model(&CapabilityItem{}).Where("id = ?", item.ID).
		Update("status", "published").Error
	if !errors.Is(err, ErrGitLifecycleArchived) {
		t.Fatalf("unknown status error = %v, want ErrGitLifecycleArchived", err)
	}
}

// A struct update that writes only non-zero fields cannot carry the NULL that
// clears the claim, so the guard refuses it instead of applying a hide that
// leaves Git free to republish the row.
func TestGitLifecycleGuard_RefusesAHideItCannotMakeAtomic(t *testing.T) {
	db := newGuardTestDB(t)
	item := seedGuardItem(t, db, "git-item", ContentBackendGit)
	archiveGuardItem(t, db, item.ID, GitLifecycleReasonManifestRemoved)

	err := db.Model(&CapabilityItem{ID: item.ID}).
		Updates(CapabilityItem{Status: "inactive"}).Error
	if !errors.Is(err, ErrGitLifecycleClaimUnclearable) {
		t.Fatalf("error = %v, want ErrGitLifecycleClaimUnclearable", err)
	}
	if status, reason, _ := lifecycleStateOf(t, db, item.ID); status != "archived" || reason == nil {
		t.Fatalf("refused write still landed: status=%q reason=%v", status, reason)
	}
}
