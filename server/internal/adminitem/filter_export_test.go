package adminitem

import "testing"

// The "missing security eval" filter must be strictly security_status='unscanned',
// NOT the broader unknown bucket (pending/scanning/error/skipped): "never
// evaluated" ≠ "evaluation in progress or errored".
func TestListItems_MissingSecurityEval(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "a1", "Never Scanned", "skill", "active", "unscanned", "u1", 4.0)
	seedItem(t, db, "a2", "Clean", "skill", "active", "clean", "u1", 4.0)
	seedItem(t, db, "a3", "Pending", "skill", "active", "pending", "u1", 4.0)
	seedItem(t, db, "a4", "Errored", "skill", "active", "error", "u1", 4.0)

	svc := NewService(db)
	rows, total, err := svc.ListItems(ListParams{MissingSecurityEval: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("missingSecurityEval expected 1 (only unscanned), got total=%d len=%d", total, len(rows))
	}
	if rows[0].ID != "a1" {
		t.Fatalf("expected a1 (unscanned), got %s", rows[0].ID)
	}
}

func TestListItems_MissingScore(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "s1", "No Score", "skill", "active", "clean", "u1", 0)
	seedItem(t, db, "s2", "Neg Score", "skill", "active", "clean", "u1", -1)
	seedItem(t, db, "s3", "Has Score", "skill", "active", "clean", "u1", 3.5)

	svc := NewService(db)
	rows, total, err := svc.ListItems(ListParams{MissingScore: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("missingScore expected 2 (score<=0), got total=%d", total)
	}
	for _, r := range rows {
		if r.ExperienceScore > 0 {
			t.Fatalf("row %s has score %v > 0", r.ID, r.ExperienceScore)
		}
	}
}

// The two new filters compose (AND) with each other and existing ones.
func TestListItems_MissingBothCombined(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "b1", "Unscanned NoScore", "skill", "active", "unscanned", "u1", 0)
	seedItem(t, db, "b2", "Unscanned HasScore", "skill", "active", "unscanned", "u1", 4)
	seedItem(t, db, "b3", "Clean NoScore", "skill", "active", "clean", "u1", 0)

	svc := NewService(db)
	rows, total, err := svc.ListItems(ListParams{MissingSecurityEval: true, MissingScore: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || rows[0].ID != "b1" {
		t.Fatalf("combined expected only b1, got total=%d", total)
	}
}

// ExportRows returns ALL matching rows (no pagination) and carries the export
// fields (slug/category/source) added for the CSV.
func TestExportRows_FieldsAndFilter(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "e1", "Exp One", "skill", "active", "unscanned", "u1", 0)
	mustExec(t, db, `UPDATE capability_items SET category='dev', source='github' WHERE id='e1'`)
	seedItem(t, db, "e2", "Exp Two", "skill", "active", "clean", "u1", 5)

	svc := NewService(db)

	rows, err := svc.ExportRows(ListParams{MissingScore: true})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "e1" {
		t.Fatalf("export missingScore expected only e1, got %d", len(rows))
	}
	if rows[0].Slug == "" || rows[0].Category != "dev" || rows[0].Source != "github" {
		t.Fatalf("export row missing slug/category/source: %+v", rows[0])
	}

	all, err := svc.ExportRows(ListParams{})
	if err != nil {
		t.Fatalf("export all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("export all expected 2, got %d", len(all))
	}
}
