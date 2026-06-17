package services

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// loadItemBySourcePath fetches the single row with an exact source_path. Used
// where a LIKE query would also match a nested child sharing the prefix.
func loadItemBySourcePath(t *testing.T, db *gorm.DB, sourcePath string) models.CapabilityItem {
	t.Helper()
	var item models.CapabilityItem
	if err := db.Where("source_path = ?", sourcePath).First(&item).Error; err != nil {
		t.Fatalf("load item by source_path %q: %v", sourcePath, err)
	}
	return item
}

// TestIngest_NestedSkillOrphan_ArchivedWhenParentCollapsed reproduces the core
// bug: a sub-skill that an OLD bundle expanded into its own row
// (skills/<parent>/<child>/SKILL.md) becomes an orphan once upstream collapses
// the parent into a single top-level skill (skills/<parent>/SKILL.md). The two
// share the same 2-segment entryDir, so the old entryDir-keyed archive pass
// spared the orphan forever. Keying on the exact source_path archives the
// orphan (its path is gone upstream) while keeping the still-present parent.
func TestIngest_NestedSkillOrphan_ArchivedWhenParentCollapsed(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// Legacy nested sub-skill row, exactly as an older bundle version created it.
	legacy := models.CapabilityItem{
		ID: "legacy-2d-games", RegistryID: PublicRegistryID, RepoID: PublicRepoID,
		Slug: "game-development-agskill-2d-games", ItemType: "skill", Name: "2d-games",
		SourcePath: "skills/game-development-agskill/2d-games/SKILL.md", SourceType: "direct",
		Status: "active", ExperienceScore: 34041,
		CreatedBy: "system", UpdatedBy: "system",
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatalf("seed legacy nested row: %v", err)
	}

	// Bundle ships ONLY the collapsed top-level parent skill.
	parent := catalogEntry{
		ID: "game-development-agskill", Type: "skill",
		Source: "antigravity-skills", Description: "game dev parent", Category: "tooling",
	}
	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{parent},
		map[string]string{"game-development-agskill": skillBodyFor("game-development-agskill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	orphan := loadItemByID(t, db, "legacy-2d-games")
	if orphan.Status != "archived" {
		t.Fatalf("nested orphan must be archived when its exact path leaves upstream; status=%q", orphan.Status)
	}
	parentRow := loadItemBySourcePath(t, db, "skills/game-development-agskill/SKILL.md")
	if parentRow.Status != "active" {
		t.Fatalf("collapsed parent (still upstream) must stay active; status=%q", parentRow.Status)
	}
}

// TestIngest_UserCreatedRows_NeverArchived guards the load-bearing scope: rows
// authored directly by users in the store carry empty / single-segment
// source_paths, which entryDirFromSourcePath drops, so they never enter the
// catalog archive scope. The archive pass must leave them untouched even though
// their source_path is (trivially) absent from the bundle. This is what stops
// the predicate from sweeping the ~22 live user/test items.
func TestIngest_UserCreatedRows_NeverArchived(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	users := []models.CapabilityItem{
		{ID: "u-empty", RegistryID: PublicRegistryID, RepoID: PublicRepoID, Slug: "u-empty",
			ItemType: "skill", Name: "empty path", SourcePath: "", SourceType: "direct",
			Status: "active", CreatedBy: "user-1", UpdatedBy: "user-1"},
		{ID: "u-flat", RegistryID: PublicRegistryID, RepoID: PublicRepoID, Slug: "u-flat",
			ItemType: "skill", Name: "flat path", SourcePath: "SKILL.md", SourceType: "direct",
			Status: "active", CreatedBy: "user-1", UpdatedBy: "user-1"},
		{ID: "u-subagent", RegistryID: PublicRegistryID, RepoID: PublicRepoID, Slug: "u-subagent",
			ItemType: "subagent", Name: "a subagent", SourcePath: "planner.md", SourceType: "direct",
			Status: "active", CreatedBy: "user-1", UpdatedBy: "user-1"},
	}
	for i := range users {
		if err := db.Create(&users[i]).Error; err != nil {
			t.Fatalf("seed user row %s: %v", users[i].ID, err)
		}
	}

	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "unrelated-skill", Type: "skill", Source: "catalog/x", Description: "x", Category: "tooling"}},
		map[string]string{"unrelated-skill": skillBodyFor("unrelated-skill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	for _, id := range []string{"u-empty", "u-flat", "u-subagent"} {
		r := loadItemByID(t, db, id)
		if r.Status != "active" {
			t.Fatalf("user-authored row %s must never be archived by catalog ingest; status=%q", id, r.Status)
		}
	}
}

// TestIngest_ForkAndArchiveRows_ProtectedFromArchive verifies the source_type
// guard: a fork or uploaded (archive) row can carry a catalog-shaped
// source_path, so it would reach the archive predicate. Even when that path is
// absent from the bundle, such user-owned rows must be kept. Mirrors
// reconcileParentPluginLinks' source_type exclusion.
func TestIngest_ForkAndArchiveRows_ProtectedFromArchive(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	rows := []models.CapabilityItem{
		{ID: "fork-1", RegistryID: PublicRegistryID, RepoID: PublicRepoID, Slug: "forked-thing",
			ItemType: "skill", Name: "forked", SourcePath: "skills/removed-upstream-a/SKILL.md",
			SourceType: "fork", Status: "active", CreatedBy: "user-2", UpdatedBy: "user-2"},
		{ID: "arch-1", RegistryID: PublicRegistryID, RepoID: PublicRepoID, Slug: "uploaded-thing",
			ItemType: "skill", Name: "uploaded", SourcePath: "skills/removed-upstream-b/SKILL.md",
			SourceType: "archive", Status: "active", CreatedBy: "user-2", UpdatedBy: "user-2"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed row %s: %v", rows[i].ID, err)
		}
	}

	bundle := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "some-other-skill", Type: "skill", Source: "catalog/x", Description: "x", Category: "tooling"}},
		map[string]string{"some-other-skill": skillBodyFor("some-other-skill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: bundle}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	for _, id := range []string{"fork-1", "arch-1"} {
		r := loadItemByID(t, db, id)
		if r.Status != "active" {
			t.Fatalf("user-owned row %s (source_type=%s) must be protected from catalog archive; status=%q", id, r.SourceType, r.Status)
		}
	}
}

// TestIngest_TopLevelSkillRemoved_StillArchived confirms the refactor preserves
// the original behavior: a top-level catalog skill that drops out of a later
// bundle is still soft-archived (its exact path is gone), while the surviving
// sibling stays active.
func TestIngest_TopLevelSkillRemoved_StillArchived(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	full := writeMultiEntryBundle(t,
		[]catalogEntry{
			{ID: "keep-skill", Type: "skill", Source: "catalog/k", Description: "k", Category: "tooling"},
			{ID: "drop-skill", Type: "skill", Source: "catalog/d", Description: "d", Category: "tooling"},
		},
		map[string]string{"keep-skill": skillBodyFor("keep-skill"), "drop-skill": skillBodyFor("drop-skill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: full}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	reduced := writeMultiEntryBundle(t,
		[]catalogEntry{{ID: "keep-skill", Type: "skill", Source: "catalog/k", Description: "k", Category: "tooling"}},
		map[string]string{"keep-skill": skillBodyFor("keep-skill")})
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: reduced}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	dropped := loadItemBySourcePath(t, db, "skills/drop-skill/SKILL.md")
	if dropped.Status != "archived" {
		t.Fatalf("removed top-level skill must be archived; status=%q", dropped.Status)
	}
	kept := loadItemBySourcePath(t, db, "skills/keep-skill/SKILL.md")
	if kept.Status != "active" {
		t.Fatalf("surviving skill must stay active; status=%q", kept.Status)
	}
}
