package migrations_test

import (
	"testing"
)

// flattenTombstoneCauseMigrations is the table's whole constraint history. The
// last migration only makes sense against the ones before it: it widens an
// enumeration they wrote, and applying it alone would leave the five earlier
// triples undefined.
var flattenTombstoneCauseMigrations = []string{
	"20260805000200_create_capability_sync_tombstones.sql",
	"20260805000700_constrain_capability_sync_tombstone_triples.sql",
	"20260806000300_extend_capability_sync_tombstone_causes.sql",
	"20260806000500_add_capability_sync_tombstone_flatten_cause.sql",
}

const flattenTombstoneCauseFile = "20260806000500_add_capability_sync_tombstone_flatten_cause.sql"

// TestFlattenTombstoneCause_TripleStaysExhaustive proves the widened constraint
// admits exactly the six contract triples and nothing adjacent to them.
//
// The NULL cases are the ones that matter. A CHECK that evaluates to NULL
// PASSES, and in a disjunction one NULL branch is enough when no branch is TRUE
// — which is how a Git tombstone with no cause slipped through before
// 20260805000700 (`NULL IN (...)` is NULL, not false).
//
// `package_flattened` faces the mirror image of that trap. Written the tempting
// way — `... AND lifecycle_reason NOT IN ('manifest_removed', ...)` — the NULL
// case comes out right by accident, but a row carrying an arbitrary string
// evaluates the branch to TRUE and is ACCEPTED, so a data-migration tombstone
// ships asserting a Git lifecycle event that never happened. The branch is
// written as a bare `IS NULL` test for exactly that reason, and the cases below
// pin all four outcomes.
func TestFlattenTombstoneCause_TripleStaysExhaustive(t *testing.T) {
	tx := isolatedMigrationTx(t, "flatten_tombstone_causes", nil, flattenTombstoneCauseMigrations...)

	const itemID = "44444444-4444-4444-4444-444444444444"

	cases := []struct {
		name       string
		user       string
		reason     string
		lifecycle  any
		source     string
		eventID    string
		wantReject bool
	}{
		// The new triple.
		{"package flattened by a data migration", "f-1", "package_flattened", nil, "data_migration", "f-evt-1", false},

		// The NULL trap. A CHECK written as a membership test lets these through.
		{"package_flattened carrying a lifecycle reason", "f-2", "package_flattened", "manifest_removed", "data_migration", "f-evt-2", true},
		{"package_flattened carrying a terminal lifecycle reason", "f-3", "package_flattened", "repository_deleted", "data_migration", "f-evt-3", true},
		{"package_flattened carrying an unknown lifecycle reason", "f-4", "package_flattened", "flattened", "data_migration", "f-evt-4", true},
		{"package_flattened carrying an empty lifecycle reason", "f-5", "package_flattened", "", "data_migration", "f-evt-5", true},

		// Reason determines source. The moderation mispairing is the one that
		// matters most here: it is the exact row this migration exists to stop
		// being written, so it must not become legal in the other direction.
		{"package_flattened attributed to moderation", "f-6", "package_flattened", nil, "moderation", "f-evt-6", true},
		{"package_flattened attributed to the catalog", "f-7", "package_flattened", nil, "catalog", "f-evt-7", true},
		{"package_flattened attributed to favorites", "f-8", "package_flattened", nil, "favorite", "f-evt-8", true},
		{"package_flattened attributed to the Git lifecycle", "f-9", "package_flattened", "manifest_removed", "git_lifecycle", "f-evt-9", true},
		{"admin_archived attributed to the data migration", "f-10", "admin_archived", nil, "data_migration", "f-evt-10", true},
		{"item_deleted attributed to the data migration", "f-11", "item_deleted", nil, "data_migration", "f-evt-11", true},
		{"git_archived attributed to the data migration", "f-12", "git_archived", "manifest_removed", "data_migration", "f-evt-12", true},
		{"unfavorited attributed to the data migration", "f-13", "unfavorited", nil, "data_migration", "f-evt-13", true},

		// The five existing triples are untouched by the widening.
		{"git tombstone still legal", "f-14", "git_archived", "manifest_removed", "git_lifecycle", "f-evt-14", false},
		{"git tombstone still needs its cause", "f-15", "git_archived", nil, "git_lifecycle", "f-evt-15", true},
		{"unfavorite still legal", "f-16", "unfavorited", nil, "favorite", "f-evt-16", false},
		{"distribution revoke still legal", "f-17", "distribution_revoked", nil, "distribution", "f-evt-17", false},
		{"moderation take-down still legal", "f-18", "admin_archived", nil, "moderation", "f-evt-18", false},
		{"catalog hard delete still legal", "f-19", "item_deleted", nil, "catalog", "f-evt-19", false},

		// The enumeration is still closed at the database, even though the
		// client-facing reason set is open: a reason the server does not know how
		// to produce is a bug, not a forward-compatible payload. `migration`
		// specifically, because it is the name this source was NOT given.
		{"reason nobody implements", "f-20", "catalog_flattened", nil, "data_migration", "f-evt-20", true},
		{"source nobody implements", "f-21", "package_flattened", nil, "migration", "f-evt-21", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT probe")
			err := tx.Exec(moderationTombstoneInsert, tc.user, itemID, tc.reason, tc.lifecycle, tc.source, tc.eventID).Error
			if (err != nil) != tc.wantReject {
				t.Fatalf("err = %v, wantReject = %v", err, tc.wantReject)
			}
			if err != nil {
				tx.Exec("ROLLBACK TO SAVEPOINT probe")
			}
			tx.Exec("RELEASE SAVEPOINT probe")
		})
	}
}

// TestFlattenTombstoneCause_RollbackRestoresTheFiveTriples proves the Down block
// puts the previous constraint back — and that it REFUSES while a row carrying
// the new cause exists, rather than quietly dropping it.
//
// Deleting those rows to make a rollback succeed would tell every device that
// has not yet polled that the removal never happened, and the capability would
// stay installed. Failing loudly forces the operator to decide. The supported
// undo for the archive itself is `migrate flatten-plugins rollback-apply`, which
// reactivates the item and lets the active row supersede its own tombstone.
func TestFlattenTombstoneCause_RollbackRestoresTheFiveTriples(t *testing.T) {
	tx := isolatedMigrationTx(t, "flatten_tombstone_rollback", nil, flattenTombstoneCauseMigrations...)

	const itemID = "55555555-5555-5555-5555-555555555555"

	if err := tx.Exec(moderationTombstoneInsert,
		"fr-1", itemID, "package_flattened", nil, "data_migration", "fr-evt-1").Error; err != nil {
		t.Fatalf("seed flatten tombstone: %v", err)
	}

	down, err := readGooseDown(flattenTombstoneCauseFile)
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}

	tx.Exec("SAVEPOINT blocked_rollback")
	if err := tx.Exec(down).Error; err == nil {
		t.Fatal("rollback must refuse while a package_flattened row exists; silently keeping it would leave a row no constraint describes, and deleting it would strand the removal on every device that has not polled")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT blocked_rollback")

	// With the row gone the rollback succeeds and the previous vocabulary is back.
	if err := tx.Exec(`DELETE FROM capability_sync_tombstones WHERE user_id = ?`, "fr-1").Error; err != nil {
		t.Fatalf("clear flatten tombstone: %v", err)
	}
	if err := tx.Exec(down).Error; err != nil {
		t.Fatalf("apply down migration: %v", err)
	}

	tx.Exec("SAVEPOINT after_down")
	if err := tx.Exec(moderationTombstoneInsert,
		"fr-2", itemID, "package_flattened", nil, "data_migration", "fr-evt-2").Error; err == nil {
		t.Error("after rollback package_flattened must be rejected again")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT after_down")

	// ...and the five earlier triples still work, so the rollback restored the
	// previous constraint rather than a broken seventh variant.
	for _, tc := range []struct {
		user      string
		reason    string
		lifecycle any
		source    string
		eventID   string
	}{
		{"fr-3", "unfavorited", nil, "favorite", "fr-evt-3"},
		{"fr-4", "admin_archived", nil, "moderation", "fr-evt-4"},
		{"fr-5", "item_deleted", nil, "catalog", "fr-evt-5"},
		{"fr-6", "git_archived", "manifest_removed", "git_lifecycle", "fr-evt-6"},
		{"fr-7", "distribution_revoked", nil, "distribution", "fr-evt-7"},
	} {
		if err := tx.Exec(moderationTombstoneInsert,
			tc.user, itemID, tc.reason, tc.lifecycle, tc.source, tc.eventID).Error; err != nil {
			t.Errorf("after rollback %s must still be accepted: %v", tc.reason, err)
		}
	}

	// Re-applying the Up restores the widened set on a schema that has been
	// rolled back once.
	up, err := readGooseUp(flattenTombstoneCauseFile)
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	if err := tx.Exec(up).Error; err != nil {
		t.Fatalf("re-apply up migration: %v", err)
	}
	if err := tx.Exec(moderationTombstoneInsert,
		"fr-8", itemID, "package_flattened", nil, "data_migration", "fr-evt-8").Error; err != nil {
		t.Errorf("after re-applying up, package_flattened must be accepted: %v", err)
	}
}
