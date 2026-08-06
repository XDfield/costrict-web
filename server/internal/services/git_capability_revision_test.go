package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

func loadGitRevisions(t *testing.T, db *gorm.DB, itemID string) []models.CapabilityItemGitRevision {
	t.Helper()
	var revisions []models.CapabilityItemGitRevision
	if err := db.Where("item_id = ?", itemID).Order("revision_no ASC").Find(&revisions).Error; err != nil {
		t.Fatalf("load revisions for %s: %v", itemID, err)
	}
	return revisions
}

// createGitCapabilityLeaseWithDelivery mints a lease whose delivery id decides
// the revision source, so the trigger classification can be driven from a test
// the same way the worker drives it in production.
func createGitCapabilityLeaseWithDelivery(t *testing.T, db *gorm.DB, id, token, delivery string) GitCapabilitySyncLease {
	t.Helper()
	job := models.GitCapabilitySyncJob{
		ID: id, GitServerID: gitCapabilityTestServerID, DeliveryID: delivery,
		RepoID: gitCapabilityTestRepoID, RepoFullName: "alice/capabilities", DefaultBranch: "main",
		Ref: "refs/heads/main", AfterSHA: gitCapabilityTestSHA,
		Status: models.GitCapabilitySyncJobStatusRunning, MaxAttempts: 3,
		ScheduledAt: time.Now(), StartedAt: ptrTime(time.Now()), LeaseToken: token,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatalf("create lease: %v", err)
	}
	return GitCapabilitySyncLease{JobID: id, Token: token}
}

const (
	revisionSHAFirst  = "1111111111111111111111111111111111111111"
	revisionSHASecond = "2222222222222222222222222222222222222222"
)

func TestGitRevisionSourceForDelivery(t *testing.T) {
	cases := map[string]string{
		// Gitea's own delivery id (a UUID) is what a push looks like.
		"5a2ba9d1-32c0-4d02-8a1f-6d4b2b6c9f01": models.GitRevisionSourcePush,
		"reconcile:42:99":                      models.GitRevisionSourceReconcile,
		"manual:42:29123456":                   models.GitRevisionSourceReconcile,
		"":                                     models.GitRevisionSourcePush,
	}
	for delivery, want := range cases {
		if got := GitRevisionSourceForDelivery(delivery); got != want {
			t.Errorf("GitRevisionSourceForDelivery(%q) = %q, want %q", delivery, got, want)
		}
	}
}

func TestGitRevisionSourceForProjection(t *testing.T) {
	cases := []struct {
		name    string
		state   gitCapabilityProjectionState
		trigger string
		want    string
	}{
		{"a bound row that was never projected is a provision",
			gitCapabilityProjectionState{GitSHA: "", Status: "active"},
			models.GitRevisionSourcePush, models.GitRevisionSourceProvision},
		{"a row this sync orphaned is a restore",
			gitCapabilityProjectionState{GitSHA: revisionSHAFirst, Status: "archived", GitSyncStatus: gitCapabilitySyncOrphaned},
			models.GitRevisionSourcePush, models.GitRevisionSourceRestore},
		{"an inactive orphan is a restore too",
			gitCapabilityProjectionState{GitSHA: revisionSHAFirst, Status: "inactive", GitSyncStatus: gitCapabilitySyncOrphaned},
			models.GitRevisionSourceReconcile, models.GitRevisionSourceRestore},
		// 'banned' is absolute: the activate CASE never raises it, so calling the
		// projection a restore would name a transition that did not happen.
		{"a banned orphan is not a restore",
			gitCapabilityProjectionState{GitSHA: revisionSHAFirst, Status: "banned", GitSyncStatus: gitCapabilitySyncOrphaned},
			models.GitRevisionSourcePush, models.GitRevisionSourcePush},
		{"a human-archived row is not a restore",
			gitCapabilityProjectionState{GitSHA: revisionSHAFirst, Status: "archived", GitSyncStatus: gitCapabilitySyncSynced},
			models.GitRevisionSourcePush, models.GitRevisionSourcePush},
		{"an ordinary active row takes the trigger",
			gitCapabilityProjectionState{GitSHA: revisionSHAFirst, Status: "active", GitSyncStatus: gitCapabilitySyncSynced},
			models.GitRevisionSourceReconcile, models.GitRevisionSourceReconcile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitRevisionSourceForProjection(tc.state, tc.trigger); got != tc.want {
				t.Fatalf("source = %q, want %q", got, tc.want)
			}
		})
	}
}

// skillManifest builds a SKILL.md whose frontmatter and body can each be varied
// independently, so a test can move exactly one input of the projection digest.
func skillManifest(version, body string) []byte {
	return []byte("---\nname: Skill\ndescription: d\nversion: " + version + "\n---\n" + body)
}

// TestGitCapabilityProjectionDigest_CoversTheProjectionAndNothingElse pins the
// digest's membership rule directly, field by field, without a database.
//
// Both halves matter. If a display field were missing, a manifest that renamed
// a capability would leave no revision. If a repository-level fact were
// present, every sibling in the repository would get a revision from every
// push — which is the defect this whole change removes, and the reason the
// exclusion is asserted here rather than left to the integration tests to
// notice.
func TestGitCapabilityProjectionDigest_CoversTheProjectionAndNothingElse(t *testing.T) {
	base := &ParsedItem{
		Name: "Skill", Description: "d", Category: "tools", Version: "1.0.0",
		Content: "body", Metadata: map[string]any{"a": 1},
	}
	baseDigest, err := gitCapabilityProjectionDigest("skill", "SKILL.md", base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if len(baseDigest) != 64 {
		t.Fatalf("digest = %q, want 64 hex characters", baseDigest)
	}

	// Deterministic: the same projection must never look like a change.
	repeat, err := gitCapabilityProjectionDigest("skill", "SKILL.md", &ParsedItem{
		Name: "Skill", Description: "d", Category: "tools", Version: "1.0.0",
		Content: "body", Metadata: map[string]any{"a": 1},
	})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if repeat != baseDigest {
		t.Fatalf("an identical projection digested differently: %q != %q", repeat, baseDigest)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ParsedItem)
	}{
		{"content", func(p *ParsedItem) { p.Content = "body edited" }},
		{"name", func(p *ParsedItem) { p.Name = "Renamed" }},
		{"description", func(p *ParsedItem) { p.Description = "d2" }},
		{"category", func(p *ParsedItem) { p.Category = "writing" }},
		{"version", func(p *ParsedItem) { p.Version = "1.0.1" }},
		{"metadata value", func(p *ParsedItem) { p.Metadata = map[string]any{"a": 2} }},
		{"metadata key", func(p *ParsedItem) { p.Metadata = map[string]any{"a": 1, "b": 1} }},
	} {
		t.Run(tc.name+" changes the digest", func(t *testing.T) {
			mutated := *base
			mutated.Metadata = map[string]any{"a": 1}
			tc.mutate(&mutated)
			got, err := gitCapabilityProjectionDigest("skill", "SKILL.md", &mutated)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if got == baseDigest {
				t.Fatalf("changing %s did not change the digest", tc.name)
			}
		})
	}

	// The excluded half. These are the facts every capability in a repository
	// shares, so none of them may reach the digest — a sibling's push moves all
	// of them.
	if _, err := gitCapabilityProjectionDigest("skill", "SKILL.md", nil); err == nil {
		t.Fatal("a nil projection must be reported, not digested as empty")
	}
	payload := gitCapabilityProjectionPayload{}
	for _, forbidden := range []string{"gitSha", "headSha", "repoUrl", "defaultBranch", "fullName", "status"} {
		if strings.Contains(fmt.Sprintf("%#v", payload), forbidden) {
			t.Fatalf("the digest payload carries the repository-level field %q", forbidden)
		}
	}
}

// TestGitCapabilitySync_AppendsRevisionOnlyWhenTheItemsOwnContentChanges is the
// core contract: the trigger is this item's projected digest, not the
// repository head.
func TestGitCapabilitySync_AppendsRevisionOnlyWhenTheItemsOwnContentChanges(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-history", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = revisionSHAFirst
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{"SKILL.md": skillManifest("2.0.0", "body")})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	lease := createGitCapabilityLease(t, db, "job-1", "lease-1")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	revisions := loadGitRevisions(t, db, item.ID)
	if len(revisions) != 1 {
		t.Fatalf("revisions after the first projection = %d, want 1", len(revisions))
	}
	first := revisions[0]
	if first.RevisionNo != 1 || first.GitSHA != revisionSHASecond {
		t.Fatalf("revision = %+v, want revision 1 at %s", first, revisionSHASecond)
	}
	if first.Source != models.GitRevisionSourcePush {
		t.Fatalf("source = %q, want push", first.Source)
	}
	if first.VersionLabel != "2.0.0" {
		t.Fatalf("version label = %q, want 2.0.0", first.VersionLabel)
	}
	if first.GitRef != "main" || first.ManifestPath != "SKILL.md" {
		t.Fatalf("coordinate = %+v, want main/SKILL.md", first)
	}
	if len(first.ContentDigest) != 64 {
		t.Fatalf("recorded digest = %q, want 64 hex characters", first.ContentDigest)
	}

	// A duplicate delivery and a reconcile at the same head are both replays of
	// a transition that already happened.
	replay := createGitCapabilityLease(t, db, "job-2", "lease-2")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, replay); err != nil {
		t.Fatalf("replay sync: %v", err)
	}
	reconcile := createGitCapabilityLeaseWithDelivery(t, db, "job-3", "lease-3",
		models.GitCapabilitySyncDeliveryPrefixReconcile+"101:7")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, reconcile); err != nil {
		t.Fatalf("reconcile sync: %v", err)
	}
	if got := len(loadGitRevisions(t, db, item.ID)); got != 1 {
		t.Fatalf("revisions after same-content replays = %d, want 1", got)
	}

	// The repository head moves while this manifest is untouched — a commit to
	// something else in the repository. Nothing is appended, and the item still
	// tracks the new head.
	const siblingSHA = "4444444444444444444444444444444444444444"
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: siblingSHA}
	sibling := createGitCapabilityLease(t, db, "job-sibling", "lease-sibling")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, sibling); err != nil {
		t.Fatalf("sibling-commit sync: %v", err)
	}
	if got := len(loadGitRevisions(t, db, item.ID)); got != 1 {
		t.Fatalf("a commit that did not touch this manifest appended %d extra revision(s)", got-1)
	}
	if sha := loadGitCapabilityItem(t, db, item.ID).GitSHA; sha != siblingSHA {
		t.Fatalf("git_sha = %q, want the projected head %s", sha, siblingSHA)
	}

	// A revert back to content that was already recorded IS a new transition:
	// the test is inequality against the CURRENT digest, not absence from the
	// set of digests ever seen. The head goes back to an earlier SHA too, which
	// under the old trigger is what would have been measured.
	reader.files["SKILL.md"] = skillManifest("1.0.0", "body")
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHAFirst}
	revert := createGitCapabilityLeaseWithDelivery(t, db, "job-4", "lease-4",
		models.GitCapabilitySyncDeliveryPrefixManual+"101:9")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, revert); err != nil {
		t.Fatalf("revert sync: %v", err)
	}
	revisions = loadGitRevisions(t, db, item.ID)
	if len(revisions) != 2 {
		t.Fatalf("revisions after revert = %d, want 2", len(revisions))
	}
	if revisions[1].RevisionNo != 2 || revisions[1].GitSHA != revisionSHAFirst {
		t.Fatalf("revert revision = %+v, want revision 2 at %s", revisions[1], revisionSHAFirst)
	}
	// A manual resync is a forced re-read of current state, i.e. a reconcile.
	if revisions[1].Source != models.GitRevisionSourceReconcile {
		t.Fatalf("manual resync source = %q, want reconcile", revisions[1].Source)
	}
	if revisions[1].ContentDigest == revisions[0].ContentDigest {
		t.Fatal("the reverted revision recorded the digest it replaced")
	}
}

// TestGitCapabilitySync_SiblingManifestCommitAppendsNothing is the multi-item
// form of the same rule, and the shape 94% of Git-backed capabilities are in.
//
// One repository, two manifests, one push that rewrites exactly one of them.
// Both items are projected by the same pass — the untouched one is visited, its
// row is updated to the new head, and its history is left alone because its own
// digest did not move.
func TestGitCapabilitySync_SiblingManifestCommitAppendsNothing(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	for _, seed := range []struct{ id, slug, path string }{
		{"item-alpha", "alpha", "skills/alpha/SKILL.md"},
		{"item-beta", "beta", "skills/beta/SKILL.md"},
	} {
		seeded := newGitCapabilityItem(seed.id, "repo-git", seed.slug, "skill", seed.path)
		seeded.GitSHA = revisionSHAFirst
		seeded.GitSyncStatus = gitCapabilitySyncSynced
		createGitCapabilityItem(t, db, seeded)
	}

	reader := newGitCapabilityReader(map[string][]byte{
		"skills/alpha/SKILL.md": skillManifest("1.0.0", "alpha body"),
		"skills/beta/SKILL.md":  skillManifest("1.0.0", "beta body"),
	})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHAFirst}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	sync := func(job string) {
		t.Helper()
		lease := createGitCapabilityLease(t, db, job, "lease-"+job)
		if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
			"alice/capabilities", "main", false, lease); err != nil {
			t.Fatalf("%s: %v", job, err)
		}
	}

	sync("job-baseline")
	if got := len(loadGitRevisions(t, db, "item-alpha")); got != 1 {
		t.Fatalf("alpha baseline revisions = %d, want 1", got)
	}
	if got := len(loadGitRevisions(t, db, "item-beta")); got != 1 {
		t.Fatalf("beta baseline revisions = %d, want 1", got)
	}
	// Two capabilities from one manifest set must not share a digest, or "only
	// mine changed" could never be distinguished from "nothing changed".
	if loadGitRevisions(t, db, "item-alpha")[0].ContentDigest ==
		loadGitRevisions(t, db, "item-beta")[0].ContentDigest {
		t.Fatal("two different manifests produced the same projection digest")
	}

	// One push: shared head moves, only alpha's manifest is rewritten.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	reader.files["skills/alpha/SKILL.md"] = skillManifest("2.0.0", "alpha body v2")
	sync("job-alpha-push")

	if got := len(loadGitRevisions(t, db, "item-alpha")); got != 2 {
		t.Fatalf("the edited capability has %d revisions, want 2", got)
	}
	betaRevisions := loadGitRevisions(t, db, "item-beta")
	if len(betaRevisions) != 1 {
		t.Fatalf("a commit that never touched beta's manifest gave it %d revisions, want 1", len(betaRevisions))
	}
	// beta WAS projected — it tracks the new head — so the assertion above is
	// about the trigger, not about the item having been skipped.
	if sha := loadGitCapabilityItem(t, db, "item-beta").GitSHA; sha != revisionSHASecond {
		t.Fatalf("beta git_sha = %q, want the projected head %s", sha, revisionSHASecond)
	}
	if betaRevisions[0].GitSHA != revisionSHAFirst {
		t.Fatalf("beta's history moved to %q; the head is a coordinate of ITS OWN transition",
			betaRevisions[0].GitSHA)
	}
}

// TestGitCapabilitySync_MetadataOnlyChangeAppendsARevision covers the half of
// the digest that is not the body: a manifest whose frontmatter changes while
// its prose does not still changes what item detail displays.
func TestGitCapabilitySync_MetadataOnlyChangeAppendsARevision(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-metadata", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = revisionSHAFirst
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Skill\ndescription: original\nversion: 1.0.0\n---\nbody"),
	})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHAFirst}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	base := createGitCapabilityLease(t, db, "job-meta-base", "lease-meta-base")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, base); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}
	if got := len(loadGitRevisions(t, db, item.ID)); got != 1 {
		t.Fatalf("baseline revisions = %d, want 1", got)
	}

	// Same body, different frontmatter description.
	reader.files["SKILL.md"] = []byte("---\nname: Skill\ndescription: rewritten\nversion: 1.0.0\n---\nbody")
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	changed := createGitCapabilityLease(t, db, "job-meta-change", "lease-meta-change")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, changed); err != nil {
		t.Fatalf("metadata-change sync: %v", err)
	}
	revisions := loadGitRevisions(t, db, item.ID)
	if len(revisions) != 2 {
		t.Fatalf("a frontmatter-only change appended %d revision(s), want 2 total", len(revisions))
	}
	if loadGitCapabilityItem(t, db, item.ID).Description != "rewritten" {
		t.Fatal("the projection did not actually change, so this test proves nothing")
	}
}

// TestGitCapabilitySync_BackfilledBaselineAdoptsItsDigestInsteadOfAppending
// covers the rows a deployed index already has: seeded by
// `migrate backfill-git-revisions` with no digest, because a Git-backed row
// does not store its content and that command does not talk to Gitea.
func TestGitCapabilitySync_BackfilledBaselineAdoptsItsDigestInsteadOfAppending(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-backfilled", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = revisionSHAFirst
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	// Exactly the row the backfill writes: no content_digest in the INSERT.
	if err := db.Exec(`INSERT INTO capability_item_git_revisions
		(id, item_id, revision_no, git_server_id, git_repo_id, git_ref, manifest_path, entry_key,
		 git_sha, version_label, source, observed_at, created_at)
		VALUES ('rev-backfill', ?, 1, ?, ?, 'main', 'SKILL.md', '', ?, '1.0.0', 'backfill', ?, ?)`,
		item.ID, gitCapabilityTestServerID, gitCapabilityTestRepoID, revisionSHAFirst,
		time.Now().Add(-72*time.Hour), time.Now().Add(-72*time.Hour)).Error; err != nil {
		t.Fatalf("seed backfilled baseline: %v", err)
	}

	reader := newGitCapabilityReader(map[string][]byte{"SKILL.md": skillManifest("1.0.0", "body")})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	adopt := createGitCapabilityLease(t, db, "job-adopt", "lease-adopt")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, adopt); err != nil {
		t.Fatalf("adopting sync: %v", err)
	}
	revisions := loadGitRevisions(t, db, item.ID)
	if len(revisions) != 1 {
		t.Fatalf("revisions after adoption = %d, want 1 (the baseline, completed)", len(revisions))
	}
	adopted := revisions[0]
	if adopted.Source != models.GitRevisionSourceBackfill || adopted.RevisionNo != 1 ||
		adopted.GitSHA != revisionSHAFirst || adopted.VersionLabel != "1.0.0" {
		t.Fatalf("adoption rewrote the synthesized baseline: %+v", adopted)
	}
	if len(adopted.ContentDigest) != 64 {
		t.Fatalf("baseline digest after adoption = %q, want an observed digest", adopted.ContentDigest)
	}

	// From here it behaves like any observed revision: a real change appends,
	// and it appends exactly once.
	reader.files["SKILL.md"] = skillManifest("2.0.0", "body")
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: "5555555555555555555555555555555555555555"}
	changed := createGitCapabilityLease(t, db, "job-after-adopt", "lease-after-adopt")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, changed); err != nil {
		t.Fatalf("post-adoption sync: %v", err)
	}
	revisions = loadGitRevisions(t, db, item.ID)
	if len(revisions) != 2 || revisions[1].RevisionNo != 2 {
		t.Fatalf("revisions after the first real change = %+v, want a revision 2", revisions)
	}
	if revisions[1].ContentDigest == adopted.ContentDigest {
		t.Fatal("the appended revision reused the adopted digest")
	}
}

// TestGitCapabilitySync_ArchiveAdvancesTheSHAWithoutAppendingAndRestoreAppendsOnce
// pins the asymmetry the contract calls out by name: the commit that removes a
// manifest moves the item's authoritative SHA but is not content history, and
// the restoring commit that follows appends exactly one row.
func TestGitCapabilitySync_ArchiveAdvancesTheSHAWithoutAppendingAndRestoreAppendsOnce(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-archive-restore", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = revisionSHAFirst
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	manifest := []byte("---\nname: Skill\ndescription: d\nversion: 1.5.0\n---\nbody")
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	archive := createGitCapabilityLease(t, db, "job-archive", "lease-archive")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, archive); err != nil {
		t.Fatalf("archiving sync: %v", err)
	}
	archived := loadGitCapabilityItem(t, db, item.ID)
	if archived.Status != "archived" || archived.GitSyncStatus != gitCapabilitySyncOrphaned {
		t.Fatalf("archived item = %s/%s, want archived/orphaned", archived.Status, archived.GitSyncStatus)
	}
	if archived.GitSHA != revisionSHASecond {
		t.Fatalf("archive did not advance git_sha: %q", archived.GitSHA)
	}
	if got := len(loadGitRevisions(t, db, item.ID)); got != 0 {
		t.Fatalf("revisions after archive = %d, want 0", got)
	}

	// The manifest comes back on a new commit.
	const restoreSHA = "3333333333333333333333333333333333333333"
	reader.files = map[string][]byte{"SKILL.md": manifest}
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: restoreSHA}
	restore := createGitCapabilityLease(t, db, "job-restore", "lease-restore")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, restore); err != nil {
		t.Fatalf("restoring sync: %v", err)
	}
	restored := loadGitCapabilityItem(t, db, item.ID)
	if restored.Status != "active" {
		t.Fatalf("restored status = %q, want active", restored.Status)
	}
	revisions := loadGitRevisions(t, db, item.ID)
	if len(revisions) != 1 {
		t.Fatalf("revisions after restore = %d, want exactly 1", len(revisions))
	}
	if revisions[0].Source != models.GitRevisionSourceRestore || revisions[0].GitSHA != restoreSHA {
		t.Fatalf("restore revision = %+v, want source=restore at %s", revisions[0], restoreSHA)
	}

	// Replaying the restoring head appends nothing more.
	replay := createGitCapabilityLease(t, db, "job-restore-replay", "lease-restore-replay")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, replay); err != nil {
		t.Fatalf("restore replay: %v", err)
	}
	if got := len(loadGitRevisions(t, db, item.ID)); got != 1 {
		t.Fatalf("revisions after restore replay = %d, want 1", got)
	}
}

// TestGitCapabilitySync_HumanArchivedRowIsNotRestored proves the source
// classifier does not narrate a transition the status CASE refuses to make: a
// row a moderator hid keeps its status, so its projection is a plain push.
func TestGitCapabilitySync_HumanArchivedRowRecordsAPushNotARestore(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-human-archived", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = revisionSHAFirst
	item.GitSyncStatus = gitCapabilitySyncSynced
	item.Status = "archived"
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Skill\ndescription: d\nversion: 1.0.0\n---\nbody"),
	})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-human", "lease-human")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if status := loadGitCapabilityItem(t, db, item.ID).Status; status != "archived" {
		t.Fatalf("human decision overwritten: status = %q", status)
	}
	revisions := loadGitRevisions(t, db, item.ID)
	if len(revisions) != 1 || revisions[0].Source != models.GitRevisionSourcePush {
		t.Fatalf("revisions = %+v, want a single push revision", revisions)
	}
}

// TestGitCapabilitySync_FailedProjectionAppendsNoRevision covers both halves of
// "failed or unreachable sync attempts do not create successful revisions": a
// read that fails before the transaction, and a failure inside it that rolls
// the whole projection back.
func TestGitCapabilitySync_FailedProjectionAppendsNoRevision(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-failed", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = revisionSHAFirst
	item.GitSyncStatus = gitCapabilitySyncSynced
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	reader.readErrs = map[string]error{"SKILL.md": errors.New("git server unreachable")}
	reader.files = map[string][]byte{"SKILL.md": nil}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-failed", "lease-failed")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err == nil {
		t.Fatal("sync should have failed")
	}
	if got := len(loadGitRevisions(t, db, item.ID)); got != 0 {
		t.Fatalf("revisions after a failed read = %d, want 0", got)
	}
	if sha := loadGitCapabilityItem(t, db, item.ID).GitSHA; sha != revisionSHAFirst {
		t.Fatalf("failed sync moved git_sha to %q", sha)
	}
}

// TestGitCapabilityDiscovery_SeedsRevisionOneForEveryNewRow pins that a row
// born in Git gets its history in the transaction that created it, with the
// provision source, so it never depends on the backfill command.
func TestGitCapabilityDiscovery_SeedsRevisionOneForEveryNewRow(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Discovered\ndescription: d\nversion: 3.1.0\n---\nbody"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-discover", "lease-discover")
	result, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease)
	if err != nil {
		t.Fatalf("discovery sync: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}
	var created models.CapabilityItem
	if err := db.Where("content_backend = ?", "git").First(&created).Error; err != nil {
		t.Fatalf("load discovered item: %v", err)
	}
	revisions := loadGitRevisions(t, db, created.ID)
	if len(revisions) != 1 {
		t.Fatalf("revisions for a discovered row = %d, want 1", len(revisions))
	}
	if revisions[0].RevisionNo != 1 || revisions[0].Source != models.GitRevisionSourceProvision {
		t.Fatalf("revision = %+v, want revision 1 with source=provision", revisions[0])
	}
	if revisions[0].GitSHA != gitCapabilityTestSHA || revisions[0].VersionLabel != "3.1.0" {
		t.Fatalf("revision = %+v, want the discovered head and version", revisions[0])
	}

	// The very next sync at the same head adds nothing.
	replay := createGitCapabilityLease(t, db, "job-discover-replay", "lease-discover-replay")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, replay); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(loadGitRevisions(t, db, created.ID)); got != 1 {
		t.Fatalf("revisions after replay = %d, want 1", got)
	}
}

// TestGitCapabilitySync_FirstProjectionOfABoundRowIsAProvision covers the row
// shape `migrate capability-to-git` and repository provisioning leave behind:
// bound to a repository, but never yet projected.
func TestGitCapabilitySync_FirstProjectionOfABoundRowIsAProvision(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	item := newGitCapabilityItem("item-fresh-binding", "repo-git", "skill", "skill", "SKILL.md")
	item.GitSHA = ""
	item.SourceSHA = ""
	item.GitSyncStatus = gitCapabilitySyncPending
	createGitCapabilityItem(t, db, item)

	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Skill\ndescription: d\nversion: 1.0.0\n---\nbody"),
	})
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-fresh", "lease-fresh")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}
	revisions := loadGitRevisions(t, db, item.ID)
	if len(revisions) != 1 || revisions[0].Source != models.GitRevisionSourceProvision {
		t.Fatalf("revisions = %+v, want a single provision revision", revisions)
	}
}

// TestGitCapabilitySync_RevisionNumbersAreScopedToTheItem proves two manifests
// in one repository each keep their own sequence rather than sharing one.
func TestGitCapabilitySync_RevisionNumbersAreScopedToTheItem(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	for _, seed := range []struct{ id, slug, path string }{
		{"item-a", "skill-a", "skills/a/SKILL.md"},
		{"item-b", "skill-b", "skills/b/SKILL.md"},
	} {
		item := newGitCapabilityItem(seed.id, "repo-git", seed.slug, "skill", seed.path)
		item.GitSHA = revisionSHAFirst
		item.GitSyncStatus = gitCapabilitySyncSynced
		createGitCapabilityItem(t, db, item)
	}
	reader := newGitCapabilityReader(map[string][]byte{
		"skills/a/SKILL.md": []byte("---\nname: A\ndescription: d\nversion: 1.0.0\n---\nbody"),
		"skills/b/SKILL.md": []byte("---\nname: B\ndescription: d\nversion: 1.0.0\n---\nbody"),
	})
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: revisionSHASecond}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "job-multi", "lease-multi")
	if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID, "alice/capabilities", "main", false, lease); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, id := range []string{"item-a", "item-b"} {
		revisions := loadGitRevisions(t, db, id)
		if len(revisions) != 1 || revisions[0].RevisionNo != 1 {
			t.Fatalf("%s revisions = %+v, want a single revision 1", id, revisions)
		}
	}
}
