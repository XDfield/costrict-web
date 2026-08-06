package services

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// The lifecycle state machine on a real PostgreSQL.
//
// SQLite cannot prove any of this. The CHECK constraint that pairs the reason
// with its transition time only exists here; SELECT ... FOR UPDATE, which is
// what makes the "was this row still on the shelf" test a compare-and-set rather
// than a hope, is a no-op there; and the CASE expressions that decide status,
// reason and orphan marker all read pre-update values in one statement, which is
// a PostgreSQL semantic the test schema has to actually exercise.

type pgLifecycleState struct {
	Status                  string  `gorm:"column:status"`
	GitSyncStatus           string  `gorm:"column:git_sync_status"`
	GitLifecycleReason      *string `gorm:"column:git_lifecycle_reason"`
	GitLifecycleChangedAt   *string `gorm:"column:git_lifecycle_changed_at"`
	GitVisibilityVerifiedAt *string `gorm:"column:git_visibility_verified_at"`
}

func (s pgLifecycleState) reason() string {
	if s.GitLifecycleReason == nil {
		return ""
	}
	return *s.GitLifecycleReason
}

func readPGLifecycleState(t *testing.T, db *gorm.DB) pgLifecycleState {
	t.Helper()
	var state pgLifecycleState
	if err := db.Raw(`SELECT status, git_sync_status, git_lifecycle_reason,
		git_lifecycle_changed_at::text AS git_lifecycle_changed_at,
		git_visibility_verified_at::text AS git_visibility_verified_at
		FROM capability_items WHERE id = ?`, pgRevisionItemID).Scan(&state).Error; err != nil {
		t.Fatalf("read lifecycle state: %v", err)
	}
	if state.GitLifecycleReason != nil && state.GitLifecycleChangedAt == nil {
		t.Fatal("reason without a transition time survived the CHECK constraint")
	}
	return state
}

func newPGLifecycleReader(files map[string][]byte) *fakeGitCapabilityReader {
	return &fakeGitCapabilityReader{
		repo: &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/capabilities",
			DefaultBranch: "main", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}},
		branch: &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA1},
		files:  files,
	}
}

// TestGitCapabilityLifecycle_PostgresTransitionTable walks the contract's state
// table end to end on one row, in the order an operator would actually produce
// it. Each step asserts all three lifecycle facts at once (status, orphan
// marker, reason) because the bugs this guards against are always a
// disagreement BETWEEN them, never a single wrong value.
func TestGitCapabilityLifecycle_PostgresTransitionTable(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)

	reader := newPGLifecycleReader(map[string][]byte{"SKILL.md": pgSkillManifest("1.0.0")})
	svc, cfg := newPostgresSyncService(db, reader)
	sync := func(t *testing.T, name string) error {
		t.Helper()
		lease := seedPostgresLease(t, db, uuid.NewString(), name, "delivery-"+name)
		_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
			"alice/capabilities", "main", false, lease)
		return err
	}

	// 1. active + manifest present -> no claim, visibility verified.
	if err := sync(t, "baseline"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if state := readPGLifecycleState(t, db); state.Status != "active" || state.reason() != "" ||
		state.GitVisibilityVerifiedAt == nil {
		t.Fatalf("baseline state = %+v", state)
	}

	// 2. manifest disappears -> archived / orphaned / manifest_removed.
	delete(reader.files, "SKILL.md")
	if err := sync(t, "archive"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived := readPGLifecycleState(t, db)
	if archived.Status != "archived" || archived.GitSyncStatus != gitCapabilitySyncOrphaned ||
		archived.reason() != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("archived state = %+v", archived)
	}

	// 3. Cloud may not put it back while the claim stands. The models guard is
	// the enforcement point and it must hold against a real UPDATE.
	err := db.Model(&models.CapabilityItem{}).Where("id = ?", pgRevisionItemID).
		Update("status", "active").Error
	if !errors.Is(err, models.ErrGitLifecycleArchived) {
		t.Fatalf("manual activation error = %v, want ErrGitLifecycleArchived", err)
	}
	if state := readPGLifecycleState(t, db); state.Status != "archived" {
		t.Fatalf("refused activation mutated the row: %+v", state)
	}

	// 4. the manifest returns -> republished, claim dropped.
	reader.files["SKILL.md"] = pgSkillManifest("1.1.0")
	if err := sync(t, "restore"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored := readPGLifecycleState(t, db)
	if restored.Status != "active" || restored.reason() != "" {
		t.Fatalf("restored state = %+v", restored)
	}
	// The transition time is kept rather than nulled: it is the audit record of
	// when Git last changed its mind about this capability.
	if restored.GitLifecycleChangedAt == nil {
		t.Error("clearing the reason erased the transition audit trail")
	}

	// 5. archive again, then a MANUAL takedown revokes the republish permission.
	delete(reader.files, "SKILL.md")
	if err := sync(t, "archive-again"); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if err := db.Model(&models.CapabilityItem{}).Where("id = ?", pgRevisionItemID).
		Update("status", "archived").Error; err != nil {
		t.Fatalf("moderator archive: %v", err)
	}
	if state := readPGLifecycleState(t, db); state.reason() != "" {
		t.Fatalf("moderator write did not clear the Git claim: %+v", state)
	}

	// 6. ...and the manifest returning no longer undoes it.
	reader.files["SKILL.md"] = pgSkillManifest("1.2.0")
	if err := sync(t, "restore-blocked"); err != nil {
		t.Fatalf("restore attempt: %v", err)
	}
	if state := readPGLifecycleState(t, db); state.Status != "archived" {
		t.Fatalf("a returning manifest undid a moderator's takedown: %+v", state)
	}

	// 7. the moderator can undo their OWN decision, because Git no longer claims
	// the row. Without this, step 5 would be a one-way door.
	if err := db.Model(&models.CapabilityItem{}).Where("id = ?", pgRevisionItemID).
		Update("status", "active").Error; err != nil {
		t.Fatalf("moderator could not reverse their own takedown: %v", err)
	}
	if state := readPGLifecycleState(t, db); state.Status != "active" {
		t.Fatalf("final state = %+v", state)
	}
}

// A repository that is gone is terminal: the reason survives, and it survives
// specifically the path that would otherwise erase it — a later successful sync
// of a DIFFERENT repository that happens to carry the same name.
func TestGitCapabilityLifecycle_PostgresRepositoryDeletionIsTerminal(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, 'user-holder')`,
		pgRevisionItemID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}

	reader := newPGLifecycleReader(map[string][]byte{"SKILL.md": pgSkillManifest("1.0.0")})
	reader.repo = nil
	svc, cfg := newPostgresSyncService(db, reader)
	lease := seedPostgresLease(t, db, uuid.NewString(), "gone", "delivery-gone")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
		"alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("repository deletion convergence: %v", err)
	}

	state := readPGLifecycleState(t, db)
	if state.Status != "archived" || state.reason() != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("terminal state = %+v", state)
	}
	// The tombstone's CHECK constraint pairs reason/source/lifecycle_reason, so
	// its presence here also proves the writer produced a legal triple.
	var tombstones int64
	if err := db.Table("capability_sync_tombstones").
		Where("item_id = ? AND reason = ? AND lifecycle_reason = ?",
			pgRevisionItemID, models.SyncTombstoneReasonGitArchived,
			models.GitLifecycleReasonRepositoryDeleted).Count(&tombstones).Error; err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 {
		t.Fatalf("tombstones = %d, want 1", tombstones)
	}
	// The favorite is preserved: the contract expresses removal through the
	// tombstone, never by deleting the relationship.
	var favorites int64
	db.Table("item_favorites").Where("item_id = ?", pgRevisionItemID).Count(&favorites)
	if favorites != 1 {
		t.Fatalf("favorites = %d, want the relationship preserved", favorites)
	}
}

// A worker whose lease was reclaimed must not land a lifecycle transition. The
// archive, the binding deletion and the tombstones share the lease-fenced
// transaction, so losing the lease has to leave all three untouched — a partial
// apply here is the worst outcome available, because the tombstone would tell
// csc to uninstall a capability the platform still considers live.
func TestGitCapabilityLifecycle_PostgresLostLeaseAppliesNothing(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, 'user-holder')`,
		pgRevisionItemID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
	bindingID := uuid.NewString()
	if err := db.Exec(`INSERT INTO git_capability_repositories (id, git_server_id, git_repo_id)
		VALUES (?, ?, ?)`, bindingID, pgRevisionServerID, gitCapabilityTestRepoID).Error; err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	reader := newPGLifecycleReader(map[string][]byte{})
	reader.repo = nil
	svc, cfg := newPostgresSyncService(db, reader)

	// The lease is minted and then reclaimed before the transaction commits,
	// exactly as reclaimExpiredLeases does after a timeout.
	jobID := uuid.NewString()
	lease := seedPostgresLease(t, db, jobID, "stale", "delivery-stale")
	if err := db.Exec(`UPDATE git_capability_sync_jobs SET lease_token = ? WHERE id = ?`,
		uuid.NewString(), jobID).Error; err != nil {
		t.Fatalf("reclaim lease: %v", err)
	}

	_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
		"alice/capabilities", "main", false, lease)
	if !errors.Is(err, ErrGitCapabilityLeaseLost) {
		t.Fatalf("error = %v, want ErrGitCapabilityLeaseLost", err)
	}

	state := readPGLifecycleState(t, db)
	if state.Status != "active" || state.reason() != "" {
		t.Fatalf("a reclaimed worker mutated the item: %+v", state)
	}
	var tombstones, bindings int64
	db.Table("capability_sync_tombstones").Where("item_id = ?", pgRevisionItemID).Count(&tombstones)
	db.Table("git_capability_repositories").Where("id = ?", bindingID).Count(&bindings)
	if tombstones != 0 {
		t.Errorf("tombstones = %d, want 0 — csc would uninstall a live capability", tombstones)
	}
	if bindings != 1 {
		t.Errorf("bindings = %d, want the binding untouched", bindings)
	}
}

// The enum is enforced by the database, not only by application code: a
// misspelled reason would silently turn a terminal 'repository_deleted' into a
// value the recovery guard does not recognise, which reads as "recoverable" to
// nothing and as "unknown" to an operator.
func TestGitCapabilityLifecycle_PostgresRejectsAnUnknownReason(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "archived", gitCapabilitySyncOrphaned)

	err := db.Exec(`UPDATE capability_items
		SET git_lifecycle_reason = 'repo_deleted', git_lifecycle_changed_at = now() WHERE id = ?`,
		pgRevisionItemID).Error
	if err == nil {
		t.Fatal("an unknown lifecycle reason was accepted")
	}
	err = db.Exec(`UPDATE capability_items
		SET git_lifecycle_reason = 'manifest_removed', git_lifecycle_changed_at = NULL WHERE id = ?`,
		pgRevisionItemID).Error
	if err == nil {
		t.Fatal("a reason without a transition time was accepted")
	}
}
