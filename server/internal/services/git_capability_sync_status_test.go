package services

import (
	"context"
	"errors"
	"testing"
)

// R1.5 — a push must not raise a row out of a status a human put it in.
// PUT /items/:id deliberately leaves `status` writable on a Git-backed row
// (R1.6), so without this the two combine into a resurrection hole: the
// moderator takes the capability down and the next commit puts it back.
func TestGitCapabilitySyncService_DoesNotResurrectHumanHiddenStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
	}{
		{name: "archived", status: "archived"},
		{name: "inactive", status: "inactive"},
		{name: "banned", status: "banned"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			item := newGitCapabilityItem("item-hidden", "repo-hidden", "skill", "skill", "SKILL.md")
			item.Status = test.status
			item.GitSyncStatus = gitCapabilitySyncSynced
			createGitCapabilityItem(t, db, item)

			svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{
				"SKILL.md": []byte("---\nname: Updated name\ndescription: updated\n---\nbody"),
			}))
			if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
				createGitCapabilityLease(t, db, "job-hidden", "lease-hidden")); err != nil {
				t.Fatalf("sync: %v", err)
			}

			after := loadGitCapabilityItem(t, db, item.ID)
			if after.Status != test.status {
				t.Errorf("push resurrected a hidden item: status = %q, want %q", after.Status, test.status)
			}
			// The row must still be indexed — the guard is on the shelf state,
			// not on the projection. A skipped projection would go stale.
			if after.Name != "Updated name" || after.GitSHA != gitCapabilityTestSHA {
				t.Errorf("projection was not refreshed on a hidden row: name=%q git_sha=%q", after.Name, after.GitSHA)
			}
		})
	}
}

// Control for the test above: the ordinary path still publishes. Without this
// the protection could be "nothing is ever activated" and still pass.
func TestGitCapabilitySyncService_ActiveItemStaysActive(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-active", "repo-active", "skill", "skill", "SKILL.md")
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Updated name\n---\nbody"),
	}))
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-active", "lease-active")); err != nil {
		t.Fatalf("sync: %v", err)
	}

	after := loadGitCapabilityItem(t, db, item.ID)
	if after.Status != "active" || after.GitSyncStatus != gitCapabilitySyncSynced {
		t.Fatalf("active item drifted: status=%q git_sync_status=%q", after.Status, after.GitSyncStatus)
	}
}

// A row a human already hid keeps that human's status when the manifest goes
// away, and is not claimed as an orphan — claiming it would hand the next push
// permission to undo the human's decision.
func TestGitCapabilitySyncService_MissingManifestKeepsHumanHiddenStatus(t *testing.T) {
	for _, status := range []string{"archived", "inactive", "banned"} {
		t.Run(status, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			item := newGitCapabilityItem("item-gone", "repo-gone", "skill", "skill", "SKILL.md")
			item.Status = status
			item.GitSyncStatus = gitCapabilitySyncSynced
			createGitCapabilityItem(t, db, item)

			svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{}))
			result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
				createGitCapabilityLease(t, db, "job-gone", "lease-gone"))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if result.Archived != 0 {
				t.Errorf("already-hidden row counted as archived: %d", result.Archived)
			}

			after := loadGitCapabilityItem(t, db, item.ID)
			if after.Status != status {
				t.Errorf("status rewritten: %q, want %q", after.Status, status)
			}
			if after.GitSyncStatus != gitCapabilitySyncSynced {
				t.Errorf("sync claimed a human's takedown as its own: git_sync_status = %q", after.GitSyncStatus)
			}
		})
	}
}

// The other half of the same rule: sync must still be able to undo its *own*
// archival, or a file that is deleted and re-added leaves the capability dark
// forever with no Git-side recourse.
func TestGitCapabilitySyncService_RepublishesItsOwnArchivalWhenManifestReturns(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-restore", "repo-restore", "skill", "skill", "SKILL.md")
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-restore-1", "lease-restore-1"))
	if err != nil {
		t.Fatalf("sync with manifest removed: %v", err)
	}
	if result.Archived != 1 {
		t.Fatalf("archived count = %d, want 1", result.Archived)
	}
	archived := loadGitCapabilityItem(t, db, item.ID)
	if archived.Status != "archived" || archived.GitSyncStatus != gitCapabilitySyncOrphaned {
		t.Fatalf("removal did not mark the row as sync-owned: status=%q git_sync_status=%q",
			archived.Status, archived.GitSyncStatus)
	}

	reader.files["SKILL.md"] = []byte("---\nname: Restored\n---\nbody")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-restore-2", "lease-restore-2")); err != nil {
		t.Fatalf("sync with manifest restored: %v", err)
	}
	restored := loadGitCapabilityItem(t, db, item.ID)
	if restored.Status != "active" || restored.GitSyncStatus != gitCapabilitySyncSynced {
		t.Fatalf("restored manifest did not republish: status=%q git_sync_status=%q",
			restored.Status, restored.GitSyncStatus)
	}
}

// Repeated pushes with the manifest still missing must not lose the marker,
// otherwise the row silently becomes unrestorable after the second push.
func TestGitCapabilitySyncService_KeepsOrphanMarkerAcrossRepeatedRemovals(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-repeat", "repo-repeat", "skill", "skill", "SKILL.md")
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	svc, cfg := newGitCapabilitySyncService(db, newGitCapabilityReader(map[string][]byte{}))
	for _, job := range []string{"job-repeat-1", "job-repeat-2"} {
		if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
			createGitCapabilityLease(t, db, job, "lease-"+job)); err != nil {
			t.Fatalf("%s: %v", job, err)
		}
	}
	after := loadGitCapabilityItem(t, db, item.ID)
	if after.GitSyncStatus != gitCapabilitySyncOrphaned {
		t.Fatalf("second removal pass lost the orphan marker: %q", after.GitSyncStatus)
	}
}

// A transient read failure must not erase the marker either: the failure is
// reported through git_sync_error, and the row stays restorable.
func TestGitCapabilitySyncService_SyncFailureKeepsOrphanMarker(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-orphan-err", "repo-orphan-err", "skill", "skill", "SKILL.md")
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-orphan-err-1", "lease-orphan-err-1")); err != nil {
		t.Fatalf("sync with manifest removed: %v", err)
	}

	reader.treeErr = errors.New("Gitea timed out")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false,
		createGitCapabilityLease(t, db, "job-orphan-err-2", "lease-orphan-err-2")); err == nil {
		t.Fatal("sync succeeded, want tree failure")
	}

	after := loadGitCapabilityItem(t, db, item.ID)
	if after.GitSyncStatus != gitCapabilitySyncOrphaned {
		t.Errorf("failure overwrote the orphan marker: %q", after.GitSyncStatus)
	}
	if after.GitSyncError == "" {
		t.Error("failure was not recorded in git_sync_error")
	}
}

// The same rule on the whole-repository path: deleting the default branch must
// not let a later restore republish something a human had taken down, and must
// not permanently bury the rows it archives itself.
func TestGitCapabilitySyncService_MissingDefaultBranchRespectsHumanStatus(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	hidden := newGitCapabilityItem("item-branch-hidden", "repo-branch", "hidden-skill", "skill", "HIDDEN.md")
	hidden.Status = "archived"
	hidden.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, hidden)
	live := newGitCapabilityItem("item-branch-live", "repo-branch", "live-skill", "skill", "LIVE.md")
	live.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, live)

	reader := newGitCapabilityReader(map[string][]byte{})
	reader.branch = nil
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", true,
		createGitCapabilityLease(t, db, "job-branch", "lease-branch"))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Archived != 1 {
		t.Errorf("archived count = %d, want 1 (the already-hidden row must not be counted)", result.Archived)
	}

	afterHidden := loadGitCapabilityItem(t, db, hidden.ID)
	if afterHidden.Status != "archived" || afterHidden.GitSyncStatus != gitCapabilitySyncSynced {
		t.Errorf("human-hidden row was claimed: status=%q git_sync_status=%q", afterHidden.Status, afterHidden.GitSyncStatus)
	}
	afterLive := loadGitCapabilityItem(t, db, live.ID)
	if afterLive.Status != "archived" || afterLive.GitSyncStatus != gitCapabilitySyncOrphaned {
		t.Errorf("live row was not archived as sync-owned: status=%q git_sync_status=%q", afterLive.Status, afterLive.GitSyncStatus)
	}
}

// The status guard is expressed in SQL against the stored row, so the Go-side
// helper that drives the counters must agree with it.
func TestIsGitCapabilityHiddenStatus(t *testing.T) {
	for _, status := range gitCapabilityHiddenStatuses {
		if !isGitCapabilityHiddenStatus(status) {
			t.Errorf("%q should be hidden", status)
		}
	}
	for _, status := range []string{"active", "", "pending"} {
		if isGitCapabilityHiddenStatus(status) {
			t.Errorf("%q should not be hidden", status)
		}
	}
	// Guarding the value the marker uses would make an orphaned row's status
	// unreadable as a status; keep the two namespaces disjoint.
	if isGitCapabilityHiddenStatus(gitCapabilitySyncOrphaned) {
		t.Error("the orphan marker must not double as a status value")
	}
}
