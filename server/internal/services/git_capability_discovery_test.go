package services

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Both binding identity columns are `uuid` in PostgreSQL, so fixtures use real
// UUIDs: a readable placeholder would make the tests accept SQL the production
// database rejects with 22P02.
const (
	publicRegistryFixtureID = "00000000-0000-0000-0000-000000000001"
	legacyGitRegistryID     = "11111111-1111-4111-8111-111111111111"
	uuidGitRegistryID       = "22222222-2222-4222-8222-222222222222"
	gitRegistryAID          = "33333333-3333-4333-8333-333333333333"
	gitRegistryBID          = "44444444-4444-4444-8444-444444444444"
	takenRegistryID         = "55555555-5555-4555-8555-555555555555"
	takenProjectionID       = "66666666-6666-4666-8666-666666666666"
	otherRegistryID         = "77777777-7777-4777-8777-777777777777"
	otherProjectionID       = "88888888-8888-4888-8888-888888888888"
	freeRegistryID402       = "99999999-9999-4999-8999-999999999999"
	freeProjectionID402     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	freeRegistryID403       = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	freeRegistryID404       = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	freeProjectionID404     = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	boundGitRegistryID      = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
)

// A value that cannot be stored in a `uuid` column can never be a binding's
// identity, and must never be compared against one either: PostgreSQL answers
// 22P02 rather than "no match", which aborts the whole sync transaction. V4
// keeps exactly such a value — the virtual "public" repo id — on legacy rows.
func TestGitCapabilityIdentityAvailable_RejectsNonUUIDWithoutQuerying(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	for _, value := range []string{"public", "", "registry-legacy"} {
		available, err := gitCapabilityIdentityAvailable(db, gitCapabilityTestServerID, gitCapabilityTestRepoID, "repository_id", value)
		if err != nil {
			t.Fatalf("value %q: %v", value, err)
		}
		if available {
			t.Fatalf("value %q was accepted as a binding identity", value)
		}
	}
}

func TestEnsureGitCapabilityReconciliationBinding_ProjectsLegacyPublicRepoID(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	// A Git-owned registry no other repository holds, whose repo_id still names
	// the projection this repository resolves to, is this repository's to adopt —
	// the case reconciliation inheritance exists for (its binding was dropped by
	// the deleted-repository converge, its registry and its rows stayed behind).
	// Re-discovery finds that projection again through the deterministic
	// discoveredRepositoryName, which is why the identity survives at all.
	projectionID := seedDiscoveredProjection(t, db, gitCapabilityTestServerID, gitCapabilityTestRepoID)
	seedGitCapabilityRegistry(t, db, legacyGitRegistryID, GitRegistrySourceType, projectionID)
	bound := []models.CapabilityItem{
		{RepoID: "public", RegistryID: legacyGitRegistryID},
		{RepoID: "public", RegistryID: legacyGitRegistryID},
	}
	repo := &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/capabilities", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}}
	now := time.Now().UTC()
	var first *models.GitCapabilityRepository
	err := db.Transaction(func(tx *gorm.DB) error {
		owner := newGitCapabilityOwnerResolver(tx, gitCapabilityTestServerID, 1001, "alice")
		var err error
		first, err = ensureGitCapabilityReconciliationBinding(tx, gitCapabilityTestServerID, repo, "https://git.example/alice/capabilities", "main", gitCapabilityTestSHA, "standalone", owner, bound, now)
		return err
	})
	if err != nil {
		t.Fatalf("ensure legacy binding: %v", err)
	}
	if _, err := uuid.Parse(first.RepositoryID); err != nil {
		t.Fatalf("repository_id = %q is not UUID: %v", first.RepositoryID, err)
	}
	if first.RepositoryID == "public" || first.RegistryID != legacyGitRegistryID {
		t.Fatalf("unexpected binding identities: %+v", first)
	}
	var projection models.Repository
	if err := db.First(&projection, "id = ?", first.RepositoryID).Error; err != nil {
		t.Fatalf("load projection: %v", err)
	}
	if projection.OwnerID != "user-alice" || projection.Name == "" {
		t.Fatalf("unexpected projection: %+v", projection)
	}
	var ownerMembers []models.RepoMember
	if err := db.Where("repo_id = ? AND user_id = ? AND role = ?", first.RepositoryID, "user-alice", "owner").Find(&ownerMembers).Error; err != nil {
		t.Fatalf("load projection owner membership: %v", err)
	}
	if len(ownerMembers) != 1 || ownerMembers[0].Username != "alice" {
		t.Fatalf("projection owner memberships = %+v, want exactly alice owner", ownerMembers)
	}
	var ownerCount int64
	if err := db.Model(&models.RepoMember{}).Where("repo_id = ? AND role = ?", first.RepositoryID, "owner").Count(&ownerCount).Error; err != nil {
		t.Fatalf("count projection owners: %v", err)
	}
	if ownerCount != 1 {
		t.Fatalf("projection owner count = %d, want 1", ownerCount)
	}
	var second *models.GitCapabilityRepository
	err = db.Transaction(func(tx *gorm.DB) error {
		owner := newGitCapabilityOwnerResolver(tx, gitCapabilityTestServerID, 1001, "alice")
		var err error
		second, err = ensureGitCapabilityReconciliationBinding(tx, gitCapabilityTestServerID, repo, "https://git.example/alice/capabilities", "main", gitCapabilityTestSHA, "standalone", owner, bound, now.Add(time.Minute))
		return err
	})
	if err != nil || second.ID != first.ID || second.RepositoryID != first.RepositoryID || second.RegistryID != first.RegistryID {
		t.Fatalf("reconciliation not idempotent: first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestEnsureGitCapabilityReconciliationBinding_PreservesUUIDRepoID(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	repoID := uuid.NewString()
	if err := db.Create(&models.Repository{ID: repoID, Name: "existing-projection", DisplayName: "Existing", OwnerID: "user-alice", RepoType: "sync"}).Error; err != nil {
		t.Fatal(err)
	}
	seedGitCapabilityRegistry(t, db, uuidGitRegistryID, GitRegistrySourceType, repoID)
	bound := []models.CapabilityItem{{RepoID: repoID, RegistryID: uuidGitRegistryID}}
	repo := &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/capabilities", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}}
	var binding *models.GitCapabilityRepository
	err := db.Transaction(func(tx *gorm.DB) error {
		owner := newGitCapabilityOwnerResolver(tx, gitCapabilityTestServerID, 1001, "alice")
		var err error
		binding, err = ensureGitCapabilityReconciliationBinding(tx, gitCapabilityTestServerID, repo, "https://git.example/alice/capabilities", "main", gitCapabilityTestSHA, "standalone", owner, bound, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.RepositoryID != repoID {
		t.Fatalf("UUID repository identity changed: got %q want %q", binding.RepositoryID, repoID)
	}
}

// seedGitCapabilityRegistry takes repoID explicitly because
// capability_registries.repo_id is now part of what makes a registry ownable.
// mintGitCapabilityRegistry is its only writer and always stores the binding's
// repository projection, so a fixture that hard-coded "public" for a Git-owned
// registry described a row production cannot produce.
func seedGitCapabilityRegistry(t *testing.T, db *gorm.DB, id, sourceType, repoID string) {
	t.Helper()
	if err := db.Create(&models.CapabilityRegistry{
		ID: id, Name: id, SourceType: sourceType, RepoID: repoID, OwnerID: "user-alice",
	}).Error; err != nil {
		t.Fatalf("seed capability registry %s: %v", id, err)
	}
}

// seedDiscoveredProjection creates the repository row re-discovery resolves by
// its deterministic name, which is what makes an orphaned registry inheritable
// again after the deleted-repository converge dropped its binding.
func seedDiscoveredProjection(t *testing.T, db *gorm.DB, serverID string, gitRepoID int64) string {
	t.Helper()
	id := uuid.NewString()
	if err := db.Create(&models.Repository{
		ID: id, Name: discoveredRepositoryName(serverID, gitRepoID), DisplayName: "alice/capabilities",
		OwnerID: "user-alice", RepoType: "sync", Visibility: "public",
	}).Error; err != nil {
		t.Fatalf("seed discovered projection: %v", err)
	}
	return id
}

func reconcileBindingForRepo(
	t *testing.T, db *gorm.DB, repo *gitsync.Repo, bound []models.CapabilityItem,
) (*models.GitCapabilityRepository, error) {
	t.Helper()
	var binding *models.GitCapabilityRepository
	err := db.Transaction(func(tx *gorm.DB) error {
		owner := newGitCapabilityOwnerResolver(tx, gitCapabilityTestServerID, 1001, "alice")
		var innerErr error
		binding, innerErr = ensureGitCapabilityReconciliationBinding(
			tx, gitCapabilityTestServerID, repo, "https://git.example/"+repo.FullName,
			"main", gitCapabilityTestSHA, "standalone", owner, bound, time.Now().UTC(),
		)
		return innerErr
	})
	return binding, err
}

// The shared public registry holds essentially the whole catalog, and every fork
// and migrated row lands in it. A repository binding claiming it would make one
// repository the exclusive owner of the marketplace registry — the UNIQUE
// registry_id index then rejects every later repository, which is precisely how
// 27 of 30 forks died with "reload ... after conflict: record not found".
func TestEnsureGitCapabilityReconciliationBinding_NeverClaimsSharedRegistry(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	seedGitCapabilityRegistry(t, db, publicRegistryFixtureID, "internal", "public")
	repo := &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/first-fork", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}}

	binding, err := reconcileBindingForRepo(t, db, repo, []models.CapabilityItem{{RepoID: "public", RegistryID: publicRegistryFixtureID}})
	if err != nil {
		t.Fatalf("reconcile binding: %v", err)
	}
	if binding.RegistryID == publicRegistryFixtureID {
		t.Fatalf("binding claimed the shared public registry: %+v", binding)
	}
	var minted models.CapabilityRegistry
	if err := db.First(&minted, "id = ?", binding.RegistryID).Error; err != nil {
		t.Fatalf("load minted registry: %v", err)
	}
	if minted.SourceType != GitRegistrySourceType || minted.Name != repo.FullName {
		t.Fatalf("minted registry is not this repository's: %+v", minted)
	}
	var shared models.CapabilityRegistry
	if err := db.First(&shared, "id = ?", publicRegistryFixtureID).Error; err != nil {
		t.Fatalf("load shared registry: %v", err)
	}
	if shared.Name != publicRegistryFixtureID || shared.SourceType != "internal" {
		t.Fatalf("shared registry was rewritten by a repository binding: %+v", shared)
	}
}

// The reported failure verbatim: a second repository reconciled under the same
// user namespace whose rows carry the same shared registry. Both must bind.
func TestEnsureGitCapabilityReconciliationBinding_SecondRepositorySharingRegistry(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	seedGitCapabilityRegistry(t, db, publicRegistryFixtureID, "internal", "public")
	bound := []models.CapabilityItem{{RepoID: "public", RegistryID: publicRegistryFixtureID}}

	first, err := reconcileBindingForRepo(t, db,
		&gitsync.Repo{ID: 201, FullName: "alice/fork-one", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}}, bound)
	if err != nil {
		t.Fatalf("first repository: %v", err)
	}
	second, err := reconcileBindingForRepo(t, db,
		&gitsync.Repo{ID: 202, FullName: "alice/fork-two", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}}, bound)
	if err != nil {
		t.Fatalf("second repository in the same registry: %v", err)
	}
	if first.RegistryID == second.RegistryID {
		t.Fatalf("two repositories share one registry identity: %q", first.RegistryID)
	}
	if first.RepositoryID == second.RepositoryID {
		t.Fatalf("two repositories share one projection: %q", first.RepositoryID)
	}
}

// Same shape one level down: the rows point at a real platform repository UUID
// that another Git repository's binding already holds (several capabilities
// migrated out of one personal space into separate repositories).
func TestEnsureGitCapabilityReconciliationBinding_MintsProjectionWhenUUIDClaimed(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	sharedRepoID := uuid.NewString()
	if err := db.Create(&models.Repository{ID: sharedRepoID, Name: "personal-space", OwnerID: "user-alice", RepoType: "sync"}).Error; err != nil {
		t.Fatal(err)
	}
	// Registry A indexes the shared personal space; registry B was minted for the
	// repository that moved out, so it names that repository's own projection —
	// the one re-discovery resolves by deterministic name.
	movedProjectionID := seedDiscoveredProjection(t, db, gitCapabilityTestServerID, 302)
	seedGitCapabilityRegistry(t, db, gitRegistryAID, GitRegistrySourceType, sharedRepoID)
	seedGitCapabilityRegistry(t, db, gitRegistryBID, GitRegistrySourceType, movedProjectionID)

	first, err := reconcileBindingForRepo(t, db,
		&gitsync.Repo{ID: 301, FullName: "alice/moved-one", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}},
		[]models.CapabilityItem{{RepoID: sharedRepoID, RegistryID: gitRegistryAID}})
	if err != nil {
		t.Fatalf("first repository: %v", err)
	}
	if first.RepositoryID != sharedRepoID {
		t.Fatalf("unclaimed projection should have been adopted: %+v", first)
	}
	second, err := reconcileBindingForRepo(t, db,
		&gitsync.Repo{ID: 302, FullName: "alice/moved-two", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}},
		[]models.CapabilityItem{{RepoID: sharedRepoID, RegistryID: gitRegistryBID}})
	if err != nil {
		t.Fatalf("second repository sharing a projection: %v", err)
	}
	if second.RepositoryID == sharedRepoID {
		t.Fatalf("second repository borrowed a claimed projection: %+v", second)
	}
	if second.RepositoryID != movedProjectionID {
		t.Fatalf("second repository should have resolved its own discovered projection: %+v", second)
	}
	if second.RegistryID != gitRegistryBID {
		t.Fatalf("unclaimed Git registry should have been adopted: %+v", second)
	}
}

// The boundary 534103b left open. A Git-owned registry that no binding holds
// still says, in its own repo_id column, which repository projection it indexes.
// Ownability used to ignore that column, so an orphan left behind by one
// repository could be inherited by an unrelated one — which then renames it and
// writes its own projection through it, leaving one registry carrying two
// repositories' identities. mintGitCapabilityRegistry never produces such a row,
// so the only way to reach it is a stale or hand-edited one; the answer is to
// mint a dedicated registry rather than to repair the mismatch in place, because
// the rows already filed under the orphan belong to the other repository.
func TestEnsureGitCapabilityReconciliationBinding_RejectsRegistryOfAnotherRepository(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	foreignProjectionID := uuid.NewString()
	if err := db.Create(&models.Repository{
		ID: foreignProjectionID, Name: "someone-elses-projection", OwnerID: "user-alice", RepoType: "sync",
	}).Error; err != nil {
		t.Fatal(err)
	}
	// Orphaned: git-owned, held by no binding, but its repo_id names a repository
	// that is not the one this reconciliation is about to record.
	seedGitCapabilityRegistry(t, db, otherRegistryID, GitRegistrySourceType, foreignProjectionID)
	repo := &gitsync.Repo{ID: 501, FullName: "alice/newcomer", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}}

	binding, err := reconcileBindingForRepo(t, db, repo,
		[]models.CapabilityItem{{RepoID: "public", RegistryID: otherRegistryID}})
	if err != nil {
		t.Fatalf("reconcile binding: %v", err)
	}
	if binding.RegistryID == otherRegistryID {
		t.Fatalf("binding claimed a registry belonging to repository %s: %+v", foreignProjectionID, binding)
	}
	var orphan models.CapabilityRegistry
	if err := db.First(&orphan, "id = ?", otherRegistryID).Error; err != nil {
		t.Fatalf("load orphan registry: %v", err)
	}
	if orphan.Name != otherRegistryID || orphan.RepoID != foreignProjectionID || orphan.ExternalURL != "" {
		t.Fatalf("orphan registry was renamed onto another repository: %+v", orphan)
	}
	var minted models.CapabilityRegistry
	if err := db.First(&minted, "id = ?", binding.RegistryID).Error; err != nil {
		t.Fatalf("load minted registry: %v", err)
	}
	if minted.RepoID != binding.RepositoryID || minted.Name != repo.FullName {
		t.Fatalf("minted registry does not belong to this binding: %+v (binding %+v)", minted, binding)
	}
}

// Same mismatch reached through the other caller: an EXISTING binding whose
// registry_id points at a registry filed under a different repository must
// re-point itself, exactly as the shared-registry converge already does.
func TestEnsureOwnedGitCapabilityRegistry_RepointsRegistryOfAnotherRepository(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	ownProjectionID := uuid.NewString()
	foreignProjectionID := uuid.NewString()
	for _, id := range []string{ownProjectionID, foreignProjectionID} {
		if err := db.Create(&models.Repository{
			ID: id, Name: "projection-" + id[:8], OwnerID: "user-alice", RepoType: "sync",
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	seedGitCapabilityRegistry(t, db, gitRegistryAID, GitRegistrySourceType, foreignProjectionID)
	now := time.Now().UTC()
	binding := models.GitCapabilityRepository{
		ID: uuid.NewString(), GitServerID: gitCapabilityTestServerID, GitRepoID: 601,
		RepositoryID: ownProjectionID, RegistryID: gitRegistryAID, FullName: "alice/mismatched",
		RepoKind: "standalone", IdentificationStatus: models.GitCapabilityIdentificationClean,
		Visibility: "public", GitRemoteURL: "https://git.example/alice/mismatched", DefaultBranch: "main",
		LastSyncedCommit: gitCapabilityTestSHA, CreatedBy: "user-alice", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}

	var registryID string
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		registryID, err = ensureOwnedGitCapabilityRegistry(tx, &binding, "alice/mismatched",
			"https://git.example/alice/mismatched", "main", gitCapabilityTestSHA, "user-alice", now)
		return err
	}); err != nil {
		t.Fatalf("ensure owned registry: %v", err)
	}
	if registryID == gitRegistryAID {
		t.Fatalf("binding kept a registry filed under repository %s", foreignProjectionID)
	}
	var reloaded models.GitCapabilityRepository
	if err := db.First(&reloaded, "id = ?", binding.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.RegistryID != registryID {
		t.Fatalf("binding was not re-pointed: %+v", reloaded)
	}
	var minted models.CapabilityRegistry
	if err := db.First(&minted, "id = ?", registryID).Error; err != nil {
		t.Fatal(err)
	}
	if minted.RepoID != ownProjectionID {
		t.Fatalf("minted registry repo_id = %q, want %q", minted.RepoID, ownProjectionID)
	}
	// A second pass must be a no-op: the converge has to settle, not re-mint on
	// every sync.
	var settled string
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		settled, err = ensureOwnedGitCapabilityRegistry(tx, &reloaded, "alice/mismatched",
			"https://git.example/alice/mismatched", "main", gitCapabilityTestSHA, "user-alice", now)
		return err
	}); err != nil {
		t.Fatalf("second converge pass: %v", err)
	}
	if settled != registryID {
		t.Fatalf("converge did not settle: %q then %q", registryID, settled)
	}
}

// Each of the three unique keys has its own recovery path, and only the identity
// one may reuse the row it finds.
func TestReloadGitCapabilityBindingAfterConflict(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	now := time.Now().UTC()
	existing := models.GitCapabilityRepository{
		ID: uuid.NewString(), GitServerID: gitCapabilityTestServerID, GitRepoID: 401,
		RepositoryID: takenProjectionID, RegistryID: takenRegistryID, FullName: "alice/taken",
		RepoKind: "standalone", IdentificationStatus: models.GitCapabilityIdentificationClean,
		Visibility: "public", GitRemoteURL: "https://git.example/alice/taken", DefaultBranch: "main",
		LastSyncedCommit: gitCapabilityTestSHA, CreatedBy: "user-alice", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	cause := errors.New("duplicate key value violates unique constraint")

	t.Run("identity conflict reuses the racing writer's row", func(t *testing.T) {
		got, err := reloadGitCapabilityBindingAfterConflict(db, gitCapabilityTestServerID, 401, otherProjectionID, otherRegistryID, cause)
		if err != nil {
			t.Fatalf("identity recovery failed: %v", err)
		}
		if got.ID != existing.ID {
			t.Fatalf("recovered %q, want %q", got.ID, existing.ID)
		}
	})

	t.Run("registry conflict fails instead of sharing an identity", func(t *testing.T) {
		got, err := reloadGitCapabilityBindingAfterConflict(db, gitCapabilityTestServerID, 402, freeProjectionID402, takenRegistryID, cause)
		if err == nil {
			t.Fatalf("foreign registry was reused: %+v", got)
		}
		for _, want := range []string{"capability registry", takenRegistryID, "alice/taken"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("repository conflict fails instead of sharing an identity", func(t *testing.T) {
		got, err := reloadGitCapabilityBindingAfterConflict(db, gitCapabilityTestServerID, 403, takenProjectionID, freeRegistryID403, cause)
		if err == nil {
			t.Fatalf("foreign projection was reused: %+v", got)
		}
		for _, want := range []string{"repository projection", takenProjectionID, "alice/taken"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("no key matches surfaces the original violation", func(t *testing.T) {
		_, err := reloadGitCapabilityBindingAfterConflict(db, gitCapabilityTestServerID, 404, freeProjectionID404, freeRegistryID404, cause)
		if !errors.Is(err, cause) {
			t.Fatalf("original cause was dropped: %v", err)
		}
	})
}

// A binding written before the ownership rule existed keeps rewriting the shared
// registry's name/URL on every pass. It has to heal itself, because the rule
// alone does not undo a claim that was already recorded.
func TestUpdateGitCapabilityRepositoryProjection_ConvergesHijackedRegistry(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	seedGitCapabilityRegistry(t, db, publicRegistryFixtureID, "internal", "public")
	projectionID := uuid.NewString()
	if err := db.Create(&models.Repository{ID: projectionID, Name: "projection", OwnerID: "user-alice", RepoType: "sync"}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hijacked := models.GitCapabilityRepository{
		ID: uuid.NewString(), GitServerID: gitCapabilityTestServerID, GitRepoID: gitCapabilityTestRepoID,
		RepositoryID: projectionID, RegistryID: publicRegistryFixtureID, FullName: "alice/hijacker",
		RepoKind: "standalone", IdentificationStatus: models.GitCapabilityIdentificationClean,
		Visibility: "public", GitRemoteURL: "https://git.example/alice/hijacker", DefaultBranch: "main",
		LastSyncedCommit: gitCapabilityTestSHA, CreatedBy: "user-alice", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&hijacked).Error; err != nil {
		t.Fatal(err)
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		owner := newGitCapabilityOwnerResolver(tx, gitCapabilityTestServerID, 1001, "alice")
		return updateGitCapabilityRepositoryProjection(tx, gitCapabilityTestServerID, gitCapabilityTestRepoID,
			"alice/hijacker", "https://git.example/alice/hijacker", "main", gitCapabilityTestSHA, false, owner, "alice", now)
	})
	if err != nil {
		t.Fatalf("update projection: %v", err)
	}

	var converged models.GitCapabilityRepository
	if err := db.First(&converged, "id = ?", hijacked.ID).Error; err != nil {
		t.Fatal(err)
	}
	if converged.RegistryID == publicRegistryFixtureID {
		t.Fatalf("binding still claims the shared registry: %+v", converged)
	}
	var shared models.CapabilityRegistry
	if err := db.First(&shared, "id = ?", publicRegistryFixtureID).Error; err != nil {
		t.Fatal(err)
	}
	if shared.Name != publicRegistryFixtureID || shared.ExternalURL != "" {
		t.Fatalf("shared registry was still rewritten: %+v", shared)
	}
	var minted models.CapabilityRegistry
	if err := db.First(&minted, "id = ?", converged.RegistryID).Error; err != nil {
		t.Fatal(err)
	}
	if minted.SourceType != GitRegistrySourceType || minted.ExternalURL != "https://git.example/alice/hijacker" {
		t.Fatalf("converged registry is not this repository's: %+v", minted)
	}
}

func seedGitDiscoveryOwner(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	if err := db.Exec(`INSERT INTO tenant_git_server_binding (tenant_id, git_server_id, bound_at, updated_at) VALUES (?, ?, ?, ?)`,
		"tenant-1", gitCapabilityTestServerID, now, now).Error; err != nil {
		t.Fatalf("seed tenant Git server binding: %v", err)
	}
	if err := db.Exec(`INSERT INTO user_git_binding (
		user_subject_id, tenant_id, git_uid, git_username, provider_kind, sync_status, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, "user-alice", "tenant-1", 1001, "alice", "gitea", models.GitSyncStatusSynced, now, now).Error; err != nil {
		t.Fatalf("seed user Git binding: %v", err)
	}
}

func TestGitCapabilityDiscovery_RepairsOwnerProjectionAfterBindingAppears(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Ownership Recovery\n---\n\nBody"),
	})
	reader.tree = []gitsync.GitTreeEntry{{Path: "SKILL.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	if _, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-owner-system", "lease-owner-system")); err != nil {
		t.Fatalf("discover before user binding: %v", err)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load discovery binding: %v", err)
	}
	if binding.CreatedBy != gitCapabilityDiscoverySystemOwner {
		t.Fatalf("initial created_by = %q, want system", binding.CreatedBy)
	}

	seedGitDiscoveryOwner(t, db)
	if _, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-owner-user", "lease-owner-user")); err != nil {
		t.Fatalf("sync after user binding: %v", err)
	}

	var platformRepo models.Repository
	if err := db.First(&platformRepo, "id = ?", binding.RepositoryID).Error; err != nil {
		t.Fatalf("load repository projection: %v", err)
	}
	if platformRepo.OwnerID != "user-alice" {
		t.Fatalf("repository owner = %q, want user-alice", platformRepo.OwnerID)
	}
	var registry models.CapabilityRegistry
	if err := db.First(&registry, "id = ?", binding.RegistryID).Error; err != nil {
		t.Fatalf("load registry projection: %v", err)
	}
	if registry.OwnerID != "user-alice" {
		t.Fatalf("registry owner = %q, want user-alice", registry.OwnerID)
	}
	var members []models.RepoMember
	if err := db.Where("repo_id = ? AND role = ?", binding.RepositoryID, "owner").Find(&members).Error; err != nil {
		t.Fatalf("load owner membership: %v", err)
	}
	if len(members) != 1 || members[0].UserID != "user-alice" || members[0].Username != "alice" {
		t.Fatalf("owner memberships = %+v, want user-alice", members)
	}
	if err := db.First(&binding, "id = ?", binding.ID).Error; err != nil {
		t.Fatalf("reload discovery binding: %v", err)
	}
	if binding.CreatedBy != gitCapabilityDiscoverySystemOwner {
		t.Fatalf("created_by changed to %q; creation identity must remain immutable", binding.CreatedBy)
	}
}

func TestGitCapabilityDiscovery_CreatesCompoundRepositoryAndLocksTypes(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	seedGitDiscoveryOwner(t, db)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md":           []byte("---\nslug: repo-skill\nname: Repo Skill\ndescription: Root skill\nversion: 1.2.0\n---\n\nBody"),
		"commands/review.md": []byte("---\nname: Review Command\ndescription: Review code\n---\n\nRun review"),
	})
	reader.repo.Private = true
	reader.tree = []gitsync.GitTreeEntry{
		{Path: "README.md", Type: "blob"},
		{Path: "SKILL.md", Type: "blob"},
		{Path: "commands/review.md", Type: "blob"},
	}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	lease := createGitCapabilityLease(t, db, "discover-compound", "lease-compound")

	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, "stale/name", "main", false, lease)
	if err != nil {
		t.Fatalf("SyncRepository discovery: %v", err)
	}
	if result.Created != 2 || result.Updated != 0 || result.CommitSHA != gitCapabilityTestSHA {
		t.Fatalf("unexpected result: %+v", result)
	}

	var binding models.GitCapabilityRepository
	if err := db.First(&binding, "git_server_id = ? AND git_repo_id = ?", gitCapabilityTestServerID, gitCapabilityTestRepoID).Error; err != nil {
		t.Fatalf("load discovery binding: %v", err)
	}
	if binding.RepoKind != "standalone" || binding.IdentificationStatus != models.GitCapabilityIdentificationClean || binding.Visibility != "private" {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	if binding.CreatedBy != "user-alice" {
		t.Fatalf("created_by = %q, want user-alice", binding.CreatedBy)
	}

	var items []models.CapabilityItem
	if err := db.Order("item_type ASC").Find(&items).Error; err != nil {
		t.Fatalf("load discovered items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("item count = %d, want 2", len(items))
	}
	types := []string{items[0].ItemType, items[1].ItemType}
	sort.Strings(types)
	if strings.Join(types, ",") != "command,skill" {
		t.Fatalf("types = %v", types)
	}
	for _, item := range items {
		if item.ContentBackend != "git" || item.SourceGitRepoID != gitCapabilityTestRepoID || item.GitSyncStatus != gitCapabilitySyncSynced {
			t.Fatalf("item was not fully Git-bound: %+v", item)
		}
	}

	// A later manifest attempts to change the existing skill type. Bound rows
	// must preserve their locked item types during set reconciliation.
	reader.tree = []gitsync.GitTreeEntry{
		{Path: "SKILL.md", Type: "blob"},
		{Path: "commands/review.md", Type: "blob"},
	}
	reader.files["SKILL.md"] = []byte("---\nname: Still Skill\ntype: plugin\n---\n\nChanged")
	secondLease := createGitCapabilityLease(t, db, "discover-compound-2", "lease-compound-2")
	second, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false, secondLease)
	if err != nil {
		t.Fatalf("SyncRepository locked refresh: %v", err)
	}
	if second.Created != 0 || second.Updated != 2 {
		t.Fatalf("unexpected second result: %+v", second)
	}
	items = nil
	if err := db.Order("item_type ASC").Find(&items).Error; err != nil {
		t.Fatalf("reload locked items: %v", err)
	}
	if len(items) != 2 || items[0].ItemType != "command" || items[1].ItemType != "skill" {
		t.Fatalf("locked types changed: %+v", items)
	}
}

func TestGitCapabilityDiscovery_SkipsExcludedUnboundOwner(t *testing.T) {
	t.Setenv("PLUGIN_GIT_MIRROR_OWNER", "mirror-owner")
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"SKILL.md": []byte("---\nname: Duplicate\n---\nbody"),
	})
	reader.repo.FullName = "mirror-owner/plugin-one"
	reader.tree = []gitsync.GitTreeEntry{{Path: "SKILL.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)

	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-excluded", "lease-excluded"))
	if err != nil {
		t.Fatalf("excluded discovery: %v", err)
	}
	if result.Created != 0 || result.Updated != 0 || result.CommitSHA != gitCapabilityTestSHA {
		t.Fatalf("unexpected excluded result: %+v", result)
	}
	var itemCount int64
	if err := db.Model(&models.CapabilityItem{}).Count(&itemCount).Error; err != nil {
		t.Fatalf("count items: %v", err)
	}
	if itemCount != 0 {
		t.Fatalf("excluded owner created %d capability items", itemCount)
	}
}

func TestGitCapabilityDiscovery_CreatesPluginPack(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"plugins/alpha/.plugin.json": []byte(discoveryPluginJSON("Alpha", "alpha")),
		"plugins/beta/.plugin.json":  []byte(discoveryPluginJSON("Beta", "beta")),
	})
	reader.tree = []gitsync.GitTreeEntry{
		{Path: "plugins/alpha/.plugin.json", Type: "blob"},
		{Path: "plugins/alpha/skills/internal/SKILL.md", Type: "blob"},
		{Path: "plugins/beta/.plugin.json", Type: "blob"},
	}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-pack", "lease-pack"))
	if err != nil {
		t.Fatalf("discover pack: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("created = %d, want 2", result.Created)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load binding: %v", err)
	}
	if binding.RepoKind != "pack" || binding.IdentificationStatus != models.GitCapabilityIdentificationClean {
		t.Fatalf("unexpected pack binding: %+v", binding)
	}
	var items []models.CapabilityItem
	if err := db.Order("slug ASC").Find(&items).Error; err != nil {
		t.Fatalf("load pack items: %v", err)
	}
	if len(items) != 2 || items[0].Slug != "alpha" || items[1].Slug != "beta" {
		t.Fatalf("plugin internals must not be indexed: %+v", items)
	}
}

func TestGitCapabilityDiscovery_ExpandsMCPEntries(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"github":{"command":"gh"},"postgres":{"command":"psql"}}}`),
	})
	reader.tree = []gitsync.GitTreeEntry{{Path: ".mcp.json", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-mcp", "lease-mcp"))
	if err != nil {
		t.Fatalf("discover MCP: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("created = %d, want 2", result.Created)
	}
	var items []models.CapabilityItem
	if err := db.Order("source_git_entry_key ASC").Find(&items).Error; err != nil {
		t.Fatalf("load MCP items: %v", err)
	}
	if len(items) != 2 || items[0].SourceGitEntryKey != "github" || items[1].SourceGitEntryKey != "postgres" {
		t.Fatalf("unexpected MCP identities: %+v", items)
	}
}

func TestGitCapabilityDiscovery_RecordsUnknownRepository(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.tree = []gitsync.GitTreeEntry{{Path: "README.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-unknown", "lease-unknown"))
	if err != nil {
		t.Fatalf("discover unknown: %v", err)
	}
	if result.Created != 0 || result.Skipped == 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load unknown binding: %v", err)
	}
	if binding.IdentificationStatus != models.GitCapabilityIdentificationUnknown || !strings.Contains(binding.LastError, "no supported") {
		t.Fatalf("unexpected unknown binding: %+v", binding)
	}
	var count int64
	if err := db.Model(&models.CapabilityItem{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unknown repo item count=%d err=%v", count, err)
	}
}

func TestGitCapabilityDiscovery_UnknownRepositoryCanBeIdentifiedByLaterPush(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.tree = []gitsync.GitTreeEntry{{Path: "README.md", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	first, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-later-unknown", "lease-later-unknown"))
	if err != nil {
		t.Fatalf("record unknown repository: %v", err)
	}
	if first.Created != 0 {
		t.Fatalf("first discovery created = %d, want 0", first.Created)
	}

	var original models.GitCapabilityRepository
	if err := db.First(&original).Error; err != nil {
		t.Fatalf("load original binding: %v", err)
	}
	seedGitDiscoveryOwner(t, db)
	reader.tree = []gitsync.GitTreeEntry{{Path: "SKILL.md", Type: "blob"}}
	reader.files["SKILL.md"] = []byte("---\nname: Later Skill\ndescription: Added after the first push\n---\n\nBody")
	second, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-later-clean", "lease-later-clean"))
	if err != nil {
		t.Fatalf("identify repository on later push: %v", err)
	}
	if second.Created != 1 {
		t.Fatalf("second discovery created = %d, want 1", second.Created)
	}

	var current models.GitCapabilityRepository
	if err := db.First(&current).Error; err != nil {
		t.Fatalf("reload binding: %v", err)
	}
	if current.ID != original.ID || current.RepositoryID != original.RepositoryID || current.RegistryID != original.RegistryID {
		t.Fatalf("later discovery replaced stable binding: original=%+v current=%+v", original, current)
	}
	if current.IdentificationStatus != models.GitCapabilityIdentificationClean || current.LastError != "" {
		t.Fatalf("later discovery did not become clean: %+v", current)
	}
	var platformRepo models.Repository
	if err := db.First(&platformRepo, "id = ?", current.RepositoryID).Error; err != nil {
		t.Fatalf("load recovered repository owner: %v", err)
	}
	var ownerMembers int64
	if err := db.Model(&models.RepoMember{}).
		Where("repo_id = ? AND user_id = ? AND role = ?", current.RepositoryID, "user-alice", "owner").
		Count(&ownerMembers).Error; err != nil {
		t.Fatalf("count recovered owner membership: %v", err)
	}
	if platformRepo.OwnerID != "user-alice" || ownerMembers != 1 {
		t.Fatalf("later discovery did not recover owner projection: repo=%+v owner_members=%d", platformRepo, ownerMembers)
	}
}

func TestGitCapabilityDiscovery_V4OptionalHeuristics(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		content  string
		itemType string
		version  string
	}{
		{
			name: "agent yaml", path: ".agent/agent.yaml", itemType: "subagent",
			content: "name: Release Agent\ndescription: Coordinates releases\nversion: 2.0.0\ntags: [release]\n", version: "2.0.0",
		},
		{
			name: "package name", path: "package.json", itemType: "mcp",
			content: `{"name":"github-mcp","description":"GitHub MCP","version":"1.4.0"}`, version: "1.4.0",
		},
		{
			name: "pep 621 project", path: "pyproject.toml", itemType: "mcp",
			content: "[project]\nname = \"postgres-mcp\"\nversion = \"3.2.1\"\ndescription = \"Postgres MCP\"\n", version: "3.2.1",
		},
		{
			name: "mcp manifest", path: "manifest.json", itemType: "mcp",
			content: `{"name":"manifest-mcp","version":"4.0.0","mcp":{"transport":"stdio"}}`, version: "4.0.0",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupGitCapabilitySyncDB(t)
			reader := newGitCapabilityReader(map[string][]byte{tc.path: []byte(tc.content)})
			reader.tree = []gitsync.GitTreeEntry{{Path: tc.path, Type: "blob"}}
			svc, cfg := newGitCapabilitySyncService(db, reader)
			result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
				createGitCapabilityLease(t, db, fmt.Sprintf("discover-heuristic-%d", i), fmt.Sprintf("lease-heuristic-%d", i)))
			if err != nil {
				t.Fatalf("discover %s: %v", tc.path, err)
			}
			if result.Created != 1 {
				t.Fatalf("created = %d, want 1", result.Created)
			}
			var item models.CapabilityItem
			if err := db.First(&item).Error; err != nil {
				t.Fatalf("load discovered item: %v", err)
			}
			if item.ItemType != tc.itemType || item.Version != tc.version {
				t.Fatalf("unexpected item: type=%q version=%q", item.ItemType, item.Version)
			}
		})
	}
}

func TestGitCapabilityDiscovery_UnmatchedOptionalCandidateHasDiagnostic(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{
		"package.json": []byte(`{"name":"ordinary-web-app"}`),
	})
	reader.tree = []gitsync.GitTreeEntry{{Path: "package.json", Type: "blob"}}
	svc, cfg := newGitCapabilitySyncService(db, reader)
	result, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-unmatched", "lease-unmatched"))
	if err != nil {
		t.Fatalf("discover unmatched package: %v", err)
	}
	if result.Created != 0 || result.Skipped == 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var binding models.GitCapabilityRepository
	if err := db.First(&binding).Error; err != nil {
		t.Fatalf("load unknown binding: %v", err)
	}
	if binding.IdentificationStatus != models.GitCapabilityIdentificationUnknown ||
		!strings.Contains(binding.LastError, "did not match") {
		t.Fatalf("unmatched candidate has no diagnostic: %+v", binding)
	}
}

func TestGitCapabilityDiscovery_RetriesTreeTransportFailure(t *testing.T) {
	db := setupGitCapabilitySyncDB(t)
	reader := newGitCapabilityReader(map[string][]byte{})
	reader.treeErr = errors.New("tree unavailable")
	svc, cfg := newGitCapabilitySyncService(db, reader)
	_, err := svc.SyncRepository(t.Context(), cfg, gitCapabilityTestRepoID, reader.repo.FullName, "main", false,
		createGitCapabilityLease(t, db, "discover-tree-error", "lease-tree-error"))
	if err == nil || !strings.Contains(err.Error(), "tree unavailable") {
		t.Fatalf("expected retryable tree failure, got %v", err)
	}
	var count int64
	if err := db.Model(&models.GitCapabilityRepository{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("transport failure must not commit binding: count=%d err=%v", count, err)
	}
}

func discoveryPluginJSON(name, pluginName string) string {
	return `{"name":"` + name + `","description":"Discovered plugin","install":{"method":"plugin_marketplace","plugin_name":"` +
		pluginName + `","marketplace_name":"native","marketplace_repo":"owner/marketplace"}}`
}
