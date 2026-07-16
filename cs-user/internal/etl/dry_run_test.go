//go:build cgo

package etl

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// TestImportUsers_DryRunLeavesTargetUntouched is the headline dry-run
// guarantee: with dryRun=true, target row count must stay 0 even though
// the stats report inserted=N.
func TestImportUsers_DryRunLeavesTargetUntouched(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	seedUser(t, source, freshUser("subj-1", "alice"))
	seedUser(t, source, freshUser("subj-2", "bob"))

	beforeN, _ := CountUsers(context.Background(), target)
	if beforeN != 0 {
		t.Fatalf("test setup: target not empty (%d rows)", beforeN)
	}

	var acc Stats
	if err := ExportUsers(context.Background(), source, 10, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, true, 0, &acc)
	}); err != nil {
		t.Fatalf("dry-run pass: %v", err)
	}

	if acc.Inserted != 2 {
		t.Errorf("stats inserted = %d, want 2 (dry-run still reports intent)", acc.Inserted)
	}
	if !acc.DryRun {
		t.Errorf("stats.DryRun = false, want true")
	}

	afterN, _ := CountUsers(context.Background(), target)
	if afterN != 0 {
		t.Errorf("dry-run wrote to target: %d rows after pass (want 0)", afterN)
	}
}

// TestImportUsers_DryRunReportsFieldDiffsAndSkipsWrites checks that a
// dry-run over a target with pre-existing rows produces diffs in the
// stats but performs no UPDATE.
func TestImportUsers_DryRunReportsFieldDiffsAndSkipsWrites(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	// Seed both sides with one matching row + one divergent row.
	seedUser(t, source, freshUser("subj-1", "alice"))
	seedUser(t, target, freshUser("subj-1", "alice"))

	seedUser(t, source, freshUser("subj-2", "bob-renamed"))
	seedUser(t, target, freshUser("subj-2", "bob"))

	// Snapshot target username for "subj-2" so we can assert it didn't change.
	var before models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-2").First(&before).Error; err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	var acc Stats
	if err := ExportUsers(context.Background(), source, 10, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, true, 100, &acc)
	}); err != nil {
		t.Fatalf("dry-run pass: %v", err)
	}

	if acc.Updated != 1 {
		t.Errorf("stats updated = %d, want 1 (subj-2 differs)", acc.Updated)
	}
	if acc.Unchanged != 1 {
		t.Errorf("stats unchanged = %d, want 1 (subj-1 identical)", acc.Unchanged)
	}
	if len(acc.FieldDiffs) != 1 {
		t.Fatalf("expected 1 FieldDiffRecord, got %d: %+v", len(acc.FieldDiffs), acc.FieldDiffs)
	}
	rec := acc.FieldDiffs[0]
	if rec.Kind != "user" || rec.Key != "subj-2" {
		t.Errorf("FieldDiffRecord = %+v, want kind=user key=subj-2", rec)
	}
	sawUsername := false
	for _, d := range rec.Diffs {
		if d.Field == "username" {
			sawUsername = true
			if d.SourceValue != "bob-renamed" || d.TargetValue != "bob" {
				t.Errorf("username diff values wrong: %+v", d)
			}
		}
	}
	if !sawUsername {
		t.Errorf("username diff not in record: %+v", rec.Diffs)
	}

	// Target must be untouched.
	var after models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-2").First(&after).Error; err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if after.Username != before.Username {
		t.Errorf("target was modified during dry-run: %q → %q", before.Username, after.Username)
	}
}

func TestImportUsers_DryRunRespectsMaxDiffRecords(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	// Seed 5 divergent rows.
	for i := 0; i < 5; i++ {
		s := freshUser("subj-"+itoa(i), "src-name-"+itoa(i))
		seedUser(t, source, s)
		tgt := freshUser("subj-"+itoa(i), "tgt-name-"+itoa(i))
		seedUser(t, target, tgt)
	}

	var acc Stats
	if err := ExportUsers(context.Background(), source, 10, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, true, 2, &acc)
	}); err != nil {
		t.Fatalf("dry-run pass: %v", err)
	}
	if acc.Updated != 5 {
		t.Errorf("stats updated = %d, want 5 (all divergent)", acc.Updated)
	}
	if len(acc.FieldDiffs) != 2 {
		t.Errorf("FieldDiffs len = %d, want 2 (capped by maxDiffRecords)", len(acc.FieldDiffs))
	}
}

func TestImportUsers_DryRunUnlimitedDiffRecords(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	for i := 0; i < 10; i++ {
		seedUser(t, source, freshUser("subj-"+itoa(i), "src-"+itoa(i)))
		seedUser(t, target, freshUser("subj-"+itoa(i), "tgt-"+itoa(i)))
	}

	var acc Stats
	if err := ExportUsers(context.Background(), source, 10, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, true, -1, &acc)
	}); err != nil {
		t.Fatalf("dry-run pass: %v", err)
	}
	if len(acc.FieldDiffs) != 10 {
		t.Errorf("FieldDiffs len = %d, want 10 (unlimited via -1)", len(acc.FieldDiffs))
	}
}

func TestImportAuthIdentities_DryRunLeavesTargetUntouched(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	seedAuthIdentity(t, source, freshAuthIdentity("k1", "subj-1", "casdoor"))
	seedAuthIdentity(t, source, freshAuthIdentity("k2", "subj-2", "oauth2"))

	var acc Stats
	if err := ExportAuthIdentities(context.Background(), source, 10, func(batch []*models.UserAuthIdentity) error {
		return ImportAuthIdentities(context.Background(), target, batch, true, 0, &acc)
	}); err != nil {
		t.Fatalf("dry-run pass: %v", err)
	}
	if acc.Inserted != 2 {
		t.Errorf("stats inserted = %d, want 2", acc.Inserted)
	}
	n, _ := CountAuthIdentities(context.Background(), target)
	if n != 0 {
		t.Errorf("dry-run wrote to target: %d rows", n)
	}
}
