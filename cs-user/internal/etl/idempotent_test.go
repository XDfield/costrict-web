//go:build cgo

package etl

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// TestImportUsers_SecondRunProducesZeroWrites is the headline idempotency
// guarantee: after one successful pass, re-running with the same source
// data must report unchanged=N and inserted=updated=0.
func TestImportUsers_SecondRunProducesZeroWrites(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	// Seed source with the data we'll be migrating.
	srcRows := []*models.User{
		freshUser("subj-1", "alice"),
		freshUser("subj-2", "bob"),
		freshUser("subj-3", "carol"),
	}
	for _, u := range srcRows {
		seedUser(t, source, u)
	}

	// First pass: copy source → target.
	var first Stats
	if err := ExportUsers(context.Background(), source, 2, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, false, 0, &first)
	}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Inserted != 3 || first.Updated != 0 || first.Unchanged != 0 {
		t.Fatalf("first pass stats = %+v, want inserted=3", first)
	}

	// Second pass: same source data, expect zero writes.
	var second Stats
	if err := ExportUsers(context.Background(), source, 2, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, false, 0, &second)
	}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Inserted != 0 || second.Updated != 0 {
		t.Errorf("second pass: writes occurred: %+v", second)
	}
	if second.Unchanged != 3 {
		t.Errorf("second pass: unchanged = %d, want 3", second.Unchanged)
	}
}

// TestImportUsers_SecondRunAfterMutationWritesOnlyChangedRow verifies that
// after a real source mutation, only the changed row updates — unchanged
// rows stay unchanged (so the report accurately distinguishes 1 update vs
// N bulk rewrites).
func TestImportUsers_SecondRunAfterMutationWritesOnlyChangedRow(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	seedUser(t, source, freshUser("subj-1", "alice"))
	seedUser(t, source, freshUser("subj-2", "bob"))
	seedUser(t, source, freshUser("subj-3", "carol"))

	// Initial pass.
	var first Stats
	if err := ExportUsers(context.Background(), source, 10, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, false, 0, &first)
	}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Inserted != 3 {
		t.Fatalf("first pass: inserted = %d, want 3", first.Inserted)
	}

	// Mutate one source row.
	if err := source.Unscoped().Model(&models.User{}).
		Where("subject_id = ?", "subj-2").
		Update("username", "bob-renamed").Error; err != nil {
		t.Fatalf("mutate source: %v", err)
	}

	// Second pass should report 1 update + 2 unchanged.
	var second Stats
	if err := ExportUsers(context.Background(), source, 10, func(batch []*models.User) error {
		return ImportUsers(context.Background(), target, batch, false, 0, &second)
	}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Updated != 1 || second.Unchanged != 2 {
		t.Errorf("second pass stats = %+v, want updated=1 unchanged=2", second)
	}

	// Verify target reflects the rename.
	var got models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-2").First(&got).Error; err != nil {
		t.Fatalf("load target: %v", err)
	}
	if got.Username != "bob-renamed" {
		t.Errorf("target username = %q, want bob-renamed", got.Username)
	}
}

func TestImportAuthIdentities_SecondRunProducesZeroWrites(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	seedAuthIdentity(t, source, freshAuthIdentity("k1", "subj-1", "casdoor"))
	seedAuthIdentity(t, source, freshAuthIdentity("k2", "subj-2", "oauth2"))

	var first Stats
	if err := ExportAuthIdentities(context.Background(), source, 10, func(batch []*models.UserAuthIdentity) error {
		return ImportAuthIdentities(context.Background(), target, batch, false, 0, &first)
	}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Inserted != 2 {
		t.Fatalf("first pass: inserted = %d, want 2", first.Inserted)
	}

	var second Stats
	if err := ExportAuthIdentities(context.Background(), source, 10, func(batch []*models.UserAuthIdentity) error {
		return ImportAuthIdentities(context.Background(), target, batch, false, 0, &second)
	}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Inserted != 0 || second.Updated != 0 {
		t.Errorf("second pass: writes occurred: %+v", second)
	}
	if second.Unchanged != 2 {
		t.Errorf("second pass: unchanged = %d, want 2", second.Unchanged)
	}
}

// TestEndToEnd_TwoTableMigrationParity runs the full source → target
// pipeline (both tables) and asserts row counts match at the end.
func TestEndToEnd_TwoTableMigrationParity(t *testing.T) {
	source := newDB(t)
	target := newDB(t)

	seedUser(t, source, freshUser("subj-1", "alice"))
	seedUser(t, source, freshUser("subj-2", "bob"))
	seedAuthIdentity(t, source, freshAuthIdentity("k1", "subj-1", "casdoor"))
	seedAuthIdentity(t, source, freshAuthIdentity("k2", "subj-1", "oauth2"))
	seedAuthIdentity(t, source, freshAuthIdentity("k3", "subj-2", "casdoor"))

	var userStats, aiStats Stats
	if err := ExportUsers(context.Background(), source, 10, func(b []*models.User) error {
		return ImportUsers(context.Background(), target, b, false, 0, &userStats)
	}); err != nil {
		t.Fatalf("users pass: %v", err)
	}
	if err := ExportAuthIdentities(context.Background(), source, 10, func(b []*models.UserAuthIdentity) error {
		return ImportAuthIdentities(context.Background(), target, b, false, 0, &aiStats)
	}); err != nil {
		t.Fatalf("auth-identities pass: %v", err)
	}

	srcU, _ := CountUsers(context.Background(), source)
	tgtU, _ := CountUsers(context.Background(), target)
	if srcU != tgtU {
		t.Errorf("users parity: source=%d target=%d", srcU, tgtU)
	}
	srcA, _ := CountAuthIdentities(context.Background(), source)
	tgtA, _ := CountAuthIdentities(context.Background(), target)
	if srcA != tgtA {
		t.Errorf("auth-identities parity: source=%d target=%d", srcA, tgtA)
	}
	if userStats.Inserted != 2 || aiStats.Inserted != 3 {
		t.Errorf("insert counts: users=%d (want 2), auth=%d (want 3)", userStats.Inserted, aiStats.Inserted)
	}
}
