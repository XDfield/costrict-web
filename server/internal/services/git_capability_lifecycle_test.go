package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Lifecycle convergence: which reason Git records when it takes a row down, when
// it clears the claim again, and who gets a tombstone.
//
// The reason is not decoration. It is the recovery discriminator
// (gitCapabilityRecoverablePredicate) and the thing a manual moderation write
// clears to revoke Git's permission, so a missing or wrong value is either a
// capability that can never come back or one that comes back against a
// moderator's decision.

func lifecycleReasonOf(t *testing.T, db *gorm.DB, id string) string {
	t.Helper()
	item := loadGitCapabilityItem(t, db, id)
	if item.GitLifecycleReason == nil {
		return ""
	}
	if item.GitLifecycleChangedAt == nil {
		t.Fatalf("item %s carries reason %q with no transition time; the production CHECK forbids it",
			id, *item.GitLifecycleReason)
	}
	return *item.GitLifecycleReason
}

func seedFavorite(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO item_favorites (id, item_id, user_id, created_at) VALUES (?, ?, ?, ?)`,
		"fav-"+itemID+"-"+userID, itemID, userID, time.Now()).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
}

func loadTombstone(t *testing.T, db *gorm.DB, itemID, userID string) *models.CapabilitySyncTombstone {
	t.Helper()
	var rows []models.CapabilitySyncTombstone
	if err := db.Where("item_id = ? AND user_id = ?", itemID, userID).Find(&rows).Error; err != nil {
		t.Fatalf("load tombstone: %v", err)
	}
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

func TestGitCapabilityLifecycle_MissingManifestRecordsRecoverableReasonAndTombstones(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-manifest", "repo-manifest", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	seedFavorite(t, db, item.ID, "user-holder")

	svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{}))
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-archive", "lease-archive")); err != nil {
		t.Fatalf("archive sync: %v", err)
	}

	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != "archived" || after.GitSyncStatus != gitCapabilitySyncOrphaned {
		t.Fatalf("archive did not land: status=%q syncStatus=%q", after.Status, after.GitSyncStatus)
	}
	if reason := lifecycleReasonOf(t, db, item.ID); reason != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("reason = %q, want manifest_removed", reason)
	}
	// The entitlement side: the favorite is PRESERVED (a restore must reactivate
	// the same item for the same person) and the removal is expressed as an
	// explicit tombstone, because absence is never a removal instruction.
	tombstone := loadTombstone(t, db, item.ID, "user-holder")
	if tombstone == nil {
		t.Fatal("no tombstone for the favorite holder; csc would keep the capability installed forever")
	}
	if tombstone.Reason != models.SyncTombstoneReasonGitArchived ||
		tombstone.LifecycleReason == nil || *tombstone.LifecycleReason != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("tombstone cause = %+v", tombstone)
	}
	var favorites int64
	db.Table("item_favorites").Where("item_id = ?", item.ID).Count(&favorites)
	if favorites != 1 {
		t.Fatalf("favorites = %d, want the relationship preserved", favorites)
	}
}

// Repeating the archive is not a new transition: the reason's timestamp must not
// move, and — the part that actually matters — the tombstone's event id must not
// rotate. csc dedupes on that id, so a gratuitous rotation makes every poll look
// like a fresh removal.
func TestGitCapabilityLifecycle_RepeatedArchiveIsNotANewTransition(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-repeat", "repo-repeat", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	seedFavorite(t, db, item.ID, "user-holder")

	svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{}))
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-a1", "lease-a1")); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	first := loadGitCapabilityItem(t, db, item.ID)
	firstTombstone := loadTombstone(t, db, item.ID, "user-holder")
	if firstTombstone == nil {
		t.Fatal("first archive wrote no tombstone")
	}

	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-a2", "lease-a2")); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	second := loadGitCapabilityItem(t, db, item.ID)
	if !first.GitLifecycleChangedAt.Equal(*second.GitLifecycleChangedAt) {
		t.Errorf("transition time moved on a repeated archive: %v -> %v",
			first.GitLifecycleChangedAt, second.GitLifecycleChangedAt)
	}
	secondTombstone := loadTombstone(t, db, item.ID, "user-holder")
	if secondTombstone == nil || secondTombstone.EventID != firstTombstone.EventID {
		t.Errorf("tombstone event id rotated without a transition: %+v -> %+v", firstTombstone, secondTombstone)
	}
}

// A row a human had already hidden is not a transition either, so it produces no
// tombstone — but it DOES get the reason, because the reason describes what Git
// says about the capability, not who hid the row. Recording it is what stops a
// later Cloud activation from publishing an item with no reachable content.
func TestGitCapabilityLifecycle_HumanHiddenRowGetsTheReasonButNoTombstone(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-hidden", "repo-hidden", "skill", "skill", "SKILL.md")
	item.Status = "archived"
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)
	seedFavorite(t, db, item.ID, "user-holder")

	svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{}))
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-hidden", "lease-hidden")); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if reason := lifecycleReasonOf(t, db, item.ID); reason != models.GitLifecycleReasonManifestRemoved {
		t.Fatalf("reason = %q, want manifest_removed even on a human-hidden row", reason)
	}
	// Still not orphan-marked: the human hid it, so Git holds no republish
	// permission, which is what keeps the two halves of the recovery predicate
	// independent.
	if after := loadGitCapabilityItem(t, db, item.ID); after.GitSyncStatus == gitCapabilitySyncOrphaned {
		t.Error("Git claimed the orphan marker on a row a human had hidden")
	}
	if tombstone := loadTombstone(t, db, item.ID, "user-holder"); tombstone != nil {
		t.Errorf("tombstone rotated for a transition that did not happen: %+v", tombstone)
	}
}

// Recovery: the manifest returns, the row goes live, and the claim is dropped.
// The claim must be dropped even when the row stays hidden — otherwise a
// moderator who archived a capability while it was ALSO missing from Git could
// never re-activate it, because their own archive left the reason behind.
func TestGitCapabilityLifecycle_SuccessfulProjectionClearsTheClaim(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		wantStatus string
	}{
		{name: "git archived row is republished", status: "", wantStatus: "active"},
		{name: "human hidden row stays hidden", status: "inactive", wantStatus: "inactive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			item := newGitCapabilityItem("item-restore", "repo-restore", "skill", "skill", "SKILL.md")
			createGitCapabilityItem(t, db, item)

			reader := newGitCapabilityReader(map[string][]byte{})
			svc, cfg := newGitCapabilitySyncService(db, reader)
			if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
				createGitCapabilityLease(t, db, "job-down", "lease-down")); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if test.status != "" {
				// The moderator's own write, through the guarded path. It clears the
				// claim, so the assertion below that the claim is absent after the
				// restore holds for a different reason in this branch — which is the
				// point: whichever writer dropped it, a live manifest must leave no
				// claim behind for either of them.
				if err := db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).
					Update("status", test.status).Error; err != nil {
					t.Fatalf("manual %s: %v", test.status, err)
				}
			}

			reader.files["SKILL.md"] = []byte("---\nname: Restored\n---\nbody")
			if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
				createGitCapabilityLease(t, db, "job-up", "lease-up")); err != nil {
				t.Fatalf("restore: %v", err)
			}

			after := loadGitCapabilityItem(t, db, item.ID)
			if after.Status != test.wantStatus {
				t.Errorf("status = %q, want %q", after.Status, test.wantStatus)
			}
			if reason := lifecycleReasonOf(t, db, item.ID); reason != "" {
				t.Errorf("Git claim survived a successful projection: %q", reason)
			}
			if after.GitVisibilityVerifiedAt == nil {
				t.Error("a successful repository read did not refresh the visibility verification")
			}
		})
	}
}

// A manual hide revokes the republish permission for good: the manifest comes
// back and the row stays down. This is the whole reason the reason column is
// part of the recovery predicate rather than the orphan marker alone.
func TestGitCapabilityLifecycle_ManualHideSurvivesTheManifestReturning(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-moderated", "repo-moderated", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-down", "lease-down")); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// The moderator confirms the takedown while the row is orphaned. Before the
	// lifecycle reason existed, this was the documented residual hole: the marker
	// stayed, and the next returning manifest republished the capability.
	if err := db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).
		Update("status", "archived").Error; err != nil {
		t.Fatalf("moderator archive: %v", err)
	}

	reader.files["SKILL.md"] = []byte("---\nname: Back\n---\nbody")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-up", "lease-up")); err != nil {
		t.Fatalf("restore attempt: %v", err)
	}
	if after := loadGitCapabilityItem(t, db, item.ID); after.Status != "archived" {
		t.Fatalf("a returning manifest undid a moderator's takedown: status = %q", after.Status)
	}
	// The projection is still refreshed, so the row does not go stale while hidden.
	if after := loadGitCapabilityItem(t, db, item.ID); after.Name != "Back" {
		t.Errorf("hidden row was not re-projected: name = %q", after.Name)
	}
}

func TestGitCapabilityLifecycle_MissingRepositoryIsTerminal(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-gone", "repo-gone", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	seedFavorite(t, db, item.ID, "user-holder")

	reader := newGitCapabilityReader(map[string][]byte{})
	reader.repo = nil
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-gone", "lease-gone")); err != nil {
		t.Fatalf("repository deletion convergence: %v", err)
	}

	if reason := lifecycleReasonOf(t, db, item.ID); reason != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("reason = %q, want repository_deleted", reason)
	}
	if models.IsRecoverableGitLifecycleReason(models.GitLifecycleReasonRepositoryDeleted) {
		t.Fatal("repository_deleted must be terminal for automatic recovery")
	}
	tombstone := loadTombstone(t, db, item.ID, "user-holder")
	if tombstone == nil || tombstone.LifecycleReason == nil ||
		*tombstone.LifecycleReason != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("repository deletion tombstone = %+v", tombstone)
	}
	// Visibility verification is deliberately NOT refreshed: the repository could
	// not be read, so nothing about its visibility was confirmed. Letting the
	// stamp go stale is the fail-closed direction for public browse.
	if after := loadGitCapabilityItem(t, db, item.ID); after.GitVisibilityVerifiedAt != nil {
		t.Error("an unreadable repository refreshed the visibility verification")
	}
}

// A repository recreated with the same owner/name gets a new numeric id, so it
// is a different identity and must not resurrect the old rows. The binding is
// removed with the archive, so nothing points the old items at the new repo.
func TestGitCapabilityLifecycle_SameNameNewRepoIDDoesNotResurrect(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-gone", "repo-gone", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{})
	reader.repo = nil
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-gone", "lease-gone")); err != nil {
		t.Fatalf("repository deletion: %v", err)
	}

	// The replacement repository: same owner/name, different numeric id, and it
	// even carries the same manifest.
	replacement := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Recreated\n---\nbody"),
	})
	replacement.repo.ID = gitCapabilityTestRepoID + 1
	replacement.repo.FullName = "alice/capabilities"
	svc2, cfg2 := newGitCapabilitySyncService(db, replacement)
	lease := createGitCapabilityLease(t, db, "job-new", "lease-new")
	if _, err := svc2.SyncRepository(context.Background(), cfg2, replacement.repo.ID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("replacement sync: %v", err)
	}

	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != "archived" {
		t.Fatalf("a same-name replacement resurrected the old identity: status = %q", after.Status)
	}
	if reason := lifecycleReasonOf(t, db, item.ID); reason != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("terminal reason was cleared by an unrelated repository: %q", reason)
	}
}

func TestGitCapabilityLifecycle_MissingDefaultBranchIsRecoverable(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-branch", "repo-branch", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)
	seedFavorite(t, db, item.ID, "user-holder")

	reader := newGitCapabilityReader(map[string][]byte{})
	reader.branch = nil
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", true,
		createGitCapabilityLease(t, db, "job-branch", "lease-branch")); err != nil {
		t.Fatalf("default-branch convergence: %v", err)
	}

	if reason := lifecycleReasonOf(t, db, item.ID); reason != models.GitLifecycleReasonDefaultBranchMissing {
		t.Fatalf("reason = %q, want default_branch_missing", reason)
	}
	if !models.IsRecoverableGitLifecycleReason(models.GitLifecycleReasonDefaultBranchMissing) {
		t.Fatal("default_branch_missing must stay recoverable")
	}
	if tombstone := loadTombstone(t, db, item.ID, "user-holder"); tombstone == nil {
		t.Fatal("no tombstone for a default-branch archive")
	}
	// The repository itself was read successfully here, so visibility IS verified.
	if after := loadGitCapabilityItem(t, db, item.ID); after.GitVisibilityVerifiedAt == nil {
		t.Error("a successful repository read did not refresh the visibility verification")
	}
}

// A transient read failure must change nothing about availability — not the
// status, and not the reason. Losing the reason would leave the row archived
// forever once its manifest returned; inventing one would block activation for a
// capability that is perfectly fine.
func TestGitCapabilityLifecycle_TransientFailurePreservesTheClaim(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-flaky", "repo-flaky", "skill", "skill", "SKILL.md")
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-down", "lease-down")); err != nil {
		t.Fatalf("archive: %v", err)
	}
	before := loadGitCapabilityItem(t, db, item.ID)

	reader.branchErr = context.DeadlineExceeded
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-flaky", "lease-flaky")); err == nil {
		t.Fatal("a Gitea failure must surface as an error so the job retries")
	}

	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != before.Status {
		t.Errorf("status changed on a transient failure: %q -> %q", before.Status, after.Status)
	}
	if reason := lifecycleReasonOf(t, db, item.ID); reason != models.GitLifecycleReasonManifestRemoved {
		t.Errorf("transient failure lost the Git claim: %q", reason)
	}
	if after.GitSyncError == "" {
		t.Error("transient failure was not reported through git_sync_error")
	}
}

// seedDistributionHolder gives a user an entitlement through the OTHER path a
// tombstone must cover: a live distribution receipt. Favorites are the obvious
// holder; a distributed capability is the one that gets forgotten, and forgetting
// it has the same consequence — the item leaves every active query while the
// device keeps it installed forever.
func seedDistributionHolder(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	distributionID := "dist-" + itemID + "-" + userID
	if err := db.Exec(
		`INSERT INTO item_distributions (id, item_id, distributor_id, status, scope_type, target_id, created_at, updated_at)
		 VALUES (?, ?, 'distributor', 'active', 'user', ?, ?, ?)`,
		distributionID, itemID, userID, time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed distribution: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO item_distribution_receipts (id, distribution_id, user_id, receipt_status, created_at, updated_at)
		 VALUES (?, ?, ?, 'unread', ?, ?)`,
		"receipt-"+distributionID, distributionID, userID, time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
}

// F-21: all THREE production archive entry points must tombstone, in the same
// lease-fenced transaction that archived the item.
//
// The failure this prevents is the one the whole snapshot contract is built
// around, and it is silent on both sides: an archived item drops out of every
// active query, so the server believes it is gone, while csc — which is
// forbidden from treating absence as a removal instruction — keeps the
// capability installed on the device forever. Nothing errors. The only thing
// that closes the loop is an explicit tombstone, and it has to be written by
// whichever of the three paths did the archiving.
func TestGitCapabilityLifecycle_EveryArchivePathTombstonesItsHolders(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantReason string
		// arrange puts the reader into the state that triggers this archive path.
		arrange func(reader *fakeGitCapabilityReader)
		// deletedBranch is the delivery hint the default-branch path needs.
		deletedBranch bool
	}{
		{
			name:       "manifest removed",
			wantReason: models.GitLifecycleReasonManifestRemoved,
			arrange:    func(reader *fakeGitCapabilityReader) { reader.files = map[string][]byte{} },
		},
		{
			name:       "repository deleted",
			wantReason: models.GitLifecycleReasonRepositoryDeleted,
			arrange:    func(reader *fakeGitCapabilityReader) { reader.repo = nil },
		},
		{
			name:          "default branch missing",
			wantReason:    models.GitLifecycleReasonDefaultBranchMissing,
			arrange:       func(reader *fakeGitCapabilityReader) { reader.branch = nil },
			deletedBranch: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			item := newGitCapabilityItem("item-holders", "repo-holders", "skill", "skill", "SKILL.md")
			createGitCapabilityItem(t, db, item)
			seedFavorite(t, db, item.ID, "user-favorite")
			seedDistributionHolder(t, db, item.ID, "user-distributed")

			reader := newGitCapabilityReader(map[string][]byte{"SKILL.md": []byte("---\nname: x\n---\nbody")})
			test.arrange(reader)
			svc, cfg := newGitCapabilitySyncService(db, reader)

			// --- rollback first: a reclaimed lease must leave NO tombstone -------
			// A tombstone that outlived its rolled-back archive is worse than a
			// missing one: it instructs every device to uninstall a capability the
			// platform still considers live, and nothing ever retracts it.
			staleLease := createGitCapabilityLease(t, db, "job-stale", "lease-stale")
			if err := db.Model(&models.GitCapabilitySyncJob{}).Where("id = ?", staleLease.JobID).
				Update("lease_token", "reclaimed-by-another-worker").Error; err != nil {
				t.Fatalf("reclaim lease: %v", err)
			}
			if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
				"alice/capabilities", "main", test.deletedBranch, staleLease); !errors.Is(err, ErrGitCapabilityLeaseLost) {
				t.Fatalf("stale lease error = %v, want ErrGitCapabilityLeaseLost", err)
			}
			for _, user := range []string{"user-favorite", "user-distributed"} {
				if tombstone := loadTombstone(t, db, item.ID, user); tombstone != nil {
					t.Fatalf("rolled-back archive left a tombstone for %s: %+v", user, tombstone)
				}
			}
			if after := loadGitCapabilityItem(t, db, item.ID); after.Status != "active" {
				t.Fatalf("rolled-back archive still changed status to %q", after.Status)
			}

			// --- then the real archive ------------------------------------------
			if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
				"alice/capabilities", "main", test.deletedBranch,
				createGitCapabilityLease(t, db, "job-archive", "lease-archive")); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if after := loadGitCapabilityItem(t, db, item.ID); after.Status != "archived" {
				t.Fatalf("archive did not land: status = %q", after.Status)
			}
			if reason := lifecycleReasonOf(t, db, item.ID); reason != test.wantReason {
				t.Fatalf("reason = %q, want %q", reason, test.wantReason)
			}

			for _, user := range []string{"user-favorite", "user-distributed"} {
				tombstone := loadTombstone(t, db, item.ID, user)
				if tombstone == nil {
					t.Fatalf("no tombstone for %s; csc would keep this capability installed forever", user)
				}
				if tombstone.Reason != models.SyncTombstoneReasonGitArchived {
					t.Errorf("%s tombstone reason = %q, want git_archived", user, tombstone.Reason)
				}
				if tombstone.Source != models.SyncTombstoneSourceGitLifecycle {
					t.Errorf("%s tombstone source = %q, want git_lifecycle", user, tombstone.Source)
				}
				if tombstone.LifecycleReason == nil || *tombstone.LifecycleReason != test.wantReason {
					t.Errorf("%s tombstone lifecycle reason = %v, want %q", user, tombstone.LifecycleReason, test.wantReason)
				}
				if tombstone.EventID == "" {
					t.Errorf("%s tombstone has no event id; csc has nothing to dedupe on", user)
				}
			}
		})
	}
}
