package itemdelete

// The cascade's own refusal of Git-backed rows (W11/W12/W14 backstop).
//
// Callers pre-flight the ids they were handed, which produces a better message,
// but the recursion below reaches ids nobody named: a DB-backed plugin's
// bundled sub-skills. The per-row check here is what covers those — and any
// future caller that forgets to pre-flight at all.

import (
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// addGitBackingColumns brings the shared fixture up to the columns the refusal
// reads. Without them the schema cannot hold Git-backed rows at all, which is
// the legacy-schema case the helper deliberately stays inert on.
func addGitBackingColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE capability_items ADD COLUMN content_backend TEXT NOT NULL DEFAULT 'db'`,
		`ALTER TABLE capability_items ADD COLUMN source_repo_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE capability_items ADD COLUMN source_repo_ref TEXT NOT NULL DEFAULT 'main'`,
	} {
		mustExec(t, db, stmt)
	}
}

func TestCascadeDelete_RefusesGitBackedRow(t *testing.T) {
	db := setupFullSchema(t)
	addGitBackingColumns(t, db)
	mustExec(t, db, `INSERT INTO capability_items (id, slug, item_type, name, status, created_by, content_backend, source_repo_url)
		VALUES ('G','g-slug','skill','Git Skill','active','u1','git','https://gitea.example.test/u1/repo')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('gf','G','u9')`)

	err := db.Transaction(func(tx *gorm.DB) error { return CascadeDelete(tx, "G") })
	if !errors.Is(err, models.ErrGitBackedItemsPresent) {
		t.Fatalf("expected ErrGitBackedItemsPresent, got %v", err)
	}
	if count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id = 'G'`) != 1 {
		t.Fatal("git-backed row was deleted")
	}
	// The transaction rolled back, so the dependent rows the cascade would have
	// cleared are still there too.
	if count(t, db, `SELECT COUNT(*) FROM item_favorites WHERE item_id = 'G'`) != 1 {
		t.Fatal("dependent rows were cleared despite the refusal")
	}
}

// The recursion is the reason this check lives per-row rather than only in the
// callers: nothing about "delete plugin P" names its Git-backed child.
func TestCascadeDelete_RefusesGitBackedSubSkill(t *testing.T) {
	db := setupFullSchema(t)
	addGitBackingColumns(t, db)
	mustExec(t, db, `INSERT INTO capability_items (id, slug, item_type, name, status, created_by)
		VALUES ('P','p-slug','plugin','Plug','active','u1')`)
	mustExec(t, db, `INSERT INTO capability_items (id, slug, item_type, name, status, created_by, parent_plugin_id, content_backend)
		VALUES ('S','s-slug','skill','Sub','active','u1','P','git')`)

	err := db.Transaction(func(tx *gorm.DB) error { return CascadeDelete(tx, "P") })
	if !errors.Is(err, models.ErrGitBackedItemsPresent) {
		t.Fatalf("expected ErrGitBackedItemsPresent, got %v", err)
	}
	if count(t, db, `SELECT COUNT(*) FROM capability_items`) != 2 {
		t.Fatal("the plugin or its git-backed sub-skill was deleted")
	}
}

// CascadeDeleteMany rolls the whole batch back, matching the all-or-nothing
// contract both batch endpoints document.
func TestCascadeDeleteMany_RefusesWholeBatchOnGitBackedRow(t *testing.T) {
	db := setupFullSchema(t)
	addGitBackingColumns(t, db)
	mustExec(t, db, `INSERT INTO capability_items (id, slug, item_type, name, status, created_by)
		VALUES ('D','d-slug','skill','DB Skill','active','u1')`)
	mustExec(t, db, `INSERT INTO capability_items (id, slug, item_type, name, status, created_by, content_backend)
		VALUES ('G','g-slug','skill','Git Skill','active','u1','git')`)

	err := db.Transaction(func(tx *gorm.DB) error {
		_, _, e := CascadeDeleteMany(tx, []string{"D", "G"})
		return e
	})
	if !errors.Is(err, models.ErrGitBackedItemsPresent) {
		t.Fatalf("expected ErrGitBackedItemsPresent, got %v", err)
	}
	if count(t, db, `SELECT COUNT(*) FROM capability_items`) != 2 {
		t.Fatal("the refused batch still deleted a row")
	}
}

// Control: with the Git columns present, a DB-backed row still cascades. The
// refusal is scoped to content_backend, not to the schema.
func TestCascadeDelete_DBBackedRowStillDeletes(t *testing.T) {
	db := setupFullSchema(t)
	addGitBackingColumns(t, db)
	mustExec(t, db, `INSERT INTO capability_items (id, slug, item_type, name, status, created_by)
		VALUES ('D','d-slug','skill','DB Skill','active','u1')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('df','D','u9')`)

	if err := db.Transaction(func(tx *gorm.DB) error { return CascadeDelete(tx, "D") }); err != nil {
		t.Fatalf("db-backed delete failed: %v", err)
	}
	if count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id = 'D'`) != 0 {
		t.Fatal("db-backed row was not deleted")
	}
	if count(t, db, `SELECT COUNT(*) FROM item_favorites WHERE item_id = 'D'`) != 0 {
		t.Fatal("db-backed dependents were not cleared")
	}
}
