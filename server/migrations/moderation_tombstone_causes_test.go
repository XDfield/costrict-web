package migrations_test

import (
	"testing"
)

// moderationTombstoneCauseMigrations is the whole constraint history for this
// table. The last migration only makes sense against the one before it: it
// widens an enumeration the earlier migration wrote, and applying it alone
// would leave the three original triples undefined.
var moderationTombstoneCauseMigrations = []string{
	"20260805000200_create_capability_sync_tombstones.sql",
	"20260805000700_constrain_capability_sync_tombstone_triples.sql",
	"20260806000300_extend_capability_sync_tombstone_causes.sql",
}

const moderationTombstoneInsert = `INSERT INTO capability_sync_tombstones
	(user_id, item_id, reason, lifecycle_reason, source, event_id, removed_at)
	VALUES (?, ?, ?, ?, ?, ?, now())`

// TestModerationTombstoneCauses_TripleStaysExhaustive proves the widened
// constraint admits exactly the five contract triples and nothing adjacent to
// them.
//
// The cases that matter most are the NULL ones. A CHECK that evaluates to NULL
// PASSES, and in a disjunction one NULL branch is enough when no branch is TRUE
// — which is how a Git tombstone with no cause slipped through before
// 20260805000700 (`NULL IN (...)` is NULL, not false).
//
// The new branches face the mirror image of that trap. Spelled the tempting way
// — `... AND lifecycle_reason NOT IN ('manifest_removed', ...)` — the NULL case
// happens to come out right by accident, but an admin_archived row carrying an
// arbitrary string evaluates the branch to TRUE and is ACCEPTED, so the
// tombstone ships asserting a lifecycle event that has no meaning. That is what
// the "carrying an unknown lifecycle reason" case below pins down; the
// "carrying a lifecycle reason" cases pin down the named-but-wrong variant.
// Writing every branch as an explicit IS NULL / IS NOT NULL test before any
// membership test makes all four outcomes definite.
func TestModerationTombstoneCauses_TripleStaysExhaustive(t *testing.T) {
	tx := isolatedMigrationTx(t, "moderation_tombstone_causes", nil, moderationTombstoneCauseMigrations...)

	const itemID = "22222222-2222-2222-2222-222222222222"

	cases := []struct {
		name       string
		user       string
		reason     string
		lifecycle  any
		source     string
		eventID    string
		wantReject bool
	}{
		// The two new triples.
		{"moderation take-down", "m-1", "admin_archived", nil, "moderation", "m-evt-1", false},
		{"catalog hard delete", "m-2", "item_deleted", nil, "catalog", "m-evt-2", false},

		// The NULL trap, both new reasons. A CHECK written as a bare
		// membership test lets these through.
		{"admin_archived carrying a lifecycle reason", "m-3", "admin_archived", "manifest_removed", "moderation", "m-evt-3", true},
		{"item_deleted carrying a lifecycle reason", "m-4", "item_deleted", "repository_deleted", "catalog", "m-evt-4", true},
		{"admin_archived carrying an unknown lifecycle reason", "m-5", "admin_archived", "taken_down", "moderation", "m-evt-5", true},
		{"admin_archived carrying an empty lifecycle reason", "m-6", "admin_archived", "", "moderation", "m-evt-6", true},

		// Reason determines source. Every mispairing is a row whose three
		// fields make one false statement, which csc reads as a whole.
		{"admin_archived attributed to favorites", "m-7", "admin_archived", nil, "favorite", "m-evt-7", true},
		{"admin_archived attributed to the catalog", "m-8", "admin_archived", nil, "catalog", "m-evt-8", true},
		{"admin_archived attributed to the Git lifecycle", "m-9", "admin_archived", "manifest_removed", "git_lifecycle", "m-evt-9", true},
		{"item_deleted attributed to moderation", "m-10", "item_deleted", nil, "moderation", "m-evt-10", true},
		{"item_deleted attributed to distribution", "m-11", "item_deleted", nil, "distribution", "m-evt-11", true},
		{"git_archived attributed to moderation", "m-12", "git_archived", "manifest_removed", "moderation", "m-evt-12", true},
		{"unfavorited attributed to moderation", "m-13", "unfavorited", nil, "moderation", "m-evt-13", true},
		{"distribution_revoked attributed to the catalog", "m-14", "distribution_revoked", nil, "catalog", "m-evt-14", true},

		// The three original triples are untouched by the widening.
		{"git tombstone still legal", "m-15", "git_archived", "manifest_removed", "git_lifecycle", "m-evt-15", false},
		{"git tombstone still needs its cause", "m-16", "git_archived", nil, "git_lifecycle", "m-evt-16", true},
		{"unfavorite still legal", "m-17", "unfavorited", nil, "favorite", "m-evt-17", false},
		{"distribution revoke still legal", "m-18", "distribution_revoked", nil, "distribution", "m-evt-18", false},

		// The enumeration is still closed at the database, even though the
		// client-facing reason set is open: a reason the server does not know
		// how to produce is a bug, not a forward-compatible payload.
		{"reason nobody implements", "m-19", "moderation_archived", nil, "moderation", "m-evt-19", true},
		{"source nobody implements", "m-20", "admin_archived", nil, "admin", "m-evt-20", true},
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

// TestModerationTombstoneCauses_RollbackRestoresTheThreeTriples proves the Down
// block puts the previous constraint back — and that it REFUSES while a row
// carrying one of the new causes exists, rather than quietly dropping it.
//
// Deleting those rows to make a rollback succeed would tell every device that
// has not yet polled that the removal never happened, and the capability would
// stay installed. Failing loudly forces the operator to decide.
func TestModerationTombstoneCauses_RollbackRestoresTheThreeTriples(t *testing.T) {
	tx := isolatedMigrationTx(t, "moderation_tombstone_rollback", nil, moderationTombstoneCauseMigrations...)

	const itemID = "33333333-3333-3333-3333-333333333333"

	// A live moderation tombstone blocks the rollback.
	if err := tx.Exec(moderationTombstoneInsert,
		"r-1", itemID, "admin_archived", nil, "moderation", "r-evt-1").Error; err != nil {
		t.Fatalf("seed moderation tombstone: %v", err)
	}

	down, err := readGooseDown("20260806000300_extend_capability_sync_tombstone_causes.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}

	tx.Exec("SAVEPOINT blocked_rollback")
	if err := tx.Exec(down).Error; err == nil {
		t.Fatal("rollback must refuse while an admin_archived row exists; silently keeping it would leave a row no constraint describes, and deleting it would strand the removal on every device that has not polled")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT blocked_rollback")

	// With the row gone the rollback succeeds and the old vocabulary is back.
	if err := tx.Exec(`DELETE FROM capability_sync_tombstones WHERE user_id = ?`, "r-1").Error; err != nil {
		t.Fatalf("clear moderation tombstone: %v", err)
	}
	if err := tx.Exec(down).Error; err != nil {
		t.Fatalf("apply down migration: %v", err)
	}

	tx.Exec("SAVEPOINT after_down")
	if err := tx.Exec(moderationTombstoneInsert,
		"r-2", itemID, "admin_archived", nil, "moderation", "r-evt-2").Error; err == nil {
		t.Error("after rollback admin_archived must be rejected again")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT after_down")

	// ...and the three original triples still work, so the rollback restored the
	// previous constraint rather than a broken third variant.
	if err := tx.Exec(moderationTombstoneInsert,
		"r-3", itemID, "unfavorited", nil, "favorite", "r-evt-3").Error; err != nil {
		t.Errorf("after rollback the original triples must still be accepted: %v", err)
	}

	// Re-applying the Up restores the widened set on a schema that has been
	// rolled back once.
	up, err := readGooseUp("20260806000300_extend_capability_sync_tombstone_causes.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	if err := tx.Exec(up).Error; err != nil {
		t.Fatalf("re-apply up migration: %v", err)
	}
	if err := tx.Exec(moderationTombstoneInsert,
		"r-4", itemID, "item_deleted", nil, "catalog", "r-evt-4").Error; err != nil {
		t.Errorf("after re-applying up, item_deleted must be accepted: %v", err)
	}
}
