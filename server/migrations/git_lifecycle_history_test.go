package migrations_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"gorm.io/gorm"
)

// isolatedMigrationTx opens an isolated schema inside a rolled-back transaction
// and applies the named migrations onto the supplied fixture tables. It follows
// the same shape as the sync-jobs/repositories migration tests in this package.
func isolatedMigrationTx(t *testing.T, prefix string, fixtures []string, migrationFiles ...string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL migration constraint test")
	}
	db, err := database.Initialize(dsn)
	if err != nil {
		t.Fatalf("initialize PostgreSQL: %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	schemaName := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	quoted := quoteIdentifier(schemaName)
	if err := tx.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	if err := tx.Exec("SET LOCAL search_path TO " + quoted).Error; err != nil {
		t.Fatalf("set isolated search path: %v", err)
	}
	for _, ddl := range fixtures {
		if err := tx.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture: %v", err)
		}
	}
	for _, file := range migrationFiles {
		sql, err := readGooseUp(file)
		if err != nil {
			t.Fatalf("read migration %s: %v", file, err)
		}
		if err := tx.Exec(sql).Error; err != nil {
			t.Fatalf("apply migration %s: %v", file, err)
		}
	}
	return tx
}

// capabilityItemsFixture is the minimum shape the lifecycle migration touches:
// the primary key the revision table references and the content_backend column
// the visibility index is partial on.
const capabilityItemsFixture = `CREATE TABLE capability_items (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	content_backend VARCHAR(16) NOT NULL DEFAULT 'db'
)`

func TestGitLifecycleMigration_ReasonEnumAndTimestampArePaired(t *testing.T) {
	tx := isolatedMigrationTx(t, "git_lifecycle_reason_test",
		[]string{capabilityItemsFixture},
		"20260805000000_add_capability_items_git_lifecycle.sql")

	const itemID = "11111111-1111-1111-1111-111111111111"
	if err := tx.Exec(`INSERT INTO capability_items (id, content_backend) VALUES (?, 'git')`, itemID).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	cases := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{"unknown reason is rejected",
			`UPDATE capability_items SET git_lifecycle_reason='repo_deleted', git_lifecycle_changed_at=now() WHERE id=?`, true},
		{"reason without a transition time is rejected",
			`UPDATE capability_items SET git_lifecycle_reason='repository_deleted', git_lifecycle_changed_at=NULL WHERE id=?`, true},
		{"a recoverable reason with its time is accepted",
			`UPDATE capability_items SET git_lifecycle_reason='manifest_removed', git_lifecycle_changed_at=now() WHERE id=?`, false},
		{"a terminal reason with its time is accepted",
			`UPDATE capability_items SET git_lifecycle_reason='repository_deleted', git_lifecycle_changed_at=now() WHERE id=?`, false},
		// Manual moderation clears the reason (revoking Git's permission to
		// auto-reactivate) without having to clear the audit timestamp.
		{"clearing only the reason is accepted",
			`UPDATE capability_items SET git_lifecycle_reason=NULL WHERE id=?`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT probe")
			err := tx.Exec(tc.sql, itemID).Error
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				tx.Exec("ROLLBACK TO SAVEPOINT probe")
			}
			tx.Exec("RELEASE SAVEPOINT probe")
		})
	}
}

func TestGitRevisionsMigration_RevisionNumberIsUniqueWithinItemAndSHAMayRepeat(t *testing.T) {
	tx := isolatedMigrationTx(t, "git_revisions_test",
		[]string{capabilityItemsFixture},
		"20260805000100_create_capability_item_git_revisions.sql")

	const itemA = "11111111-1111-1111-1111-111111111111"
	const itemB = "22222222-2222-2222-2222-222222222222"
	for _, id := range []string{itemA, itemB} {
		if err := tx.Exec(`INSERT INTO capability_items (id, content_backend) VALUES (?, 'git')`, id).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}

	insert := `INSERT INTO capability_item_git_revisions
		(item_id, revision_no, git_server_id, git_repo_id, git_sha, source, observed_at)
		VALUES (?, ?, 'gs-1', 42, ?, ?, now())`
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)

	if err := tx.Exec(insert, itemA, 1, shaA, "backfill").Error; err != nil {
		t.Fatalf("first revision: %v", err)
	}
	if err := tx.Exec(insert, itemA, 2, shaB, "push").Error; err != nil {
		t.Fatalf("second revision: %v", err)
	}

	// A force-push/revert back to an already observed SHA is a NEW transition
	// and must be recordable; only the revision number is unique.
	if err := tx.Exec(insert, itemA, 3, shaA, "push").Error; err != nil {
		t.Fatalf("revisiting an earlier SHA must be a new revision: %v", err)
	}

	// Revision numbers are scoped to the item, not global.
	if err := tx.Exec(insert, itemB, 1, shaA, "provision").Error; err != nil {
		t.Fatalf("another item must start at revision 1: %v", err)
	}

	tx.Exec("SAVEPOINT dup")
	if err := tx.Exec(insert, itemA, 2, shaA, "reconcile").Error; err == nil {
		t.Fatal("a duplicate revision_no within one item must be rejected")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT dup")

	for _, bad := range []struct {
		name       string
		revision   int
		sha        string
		source     string
		wantReject bool
	}{
		{"unknown source", 9, shaA, "webhook", true},
		{"empty sha", 9, "", "push", true},
		{"revision zero", 0, shaA, "push", true},
	} {
		tx.Exec("SAVEPOINT bad")
		err := tx.Exec(insert, itemA, bad.revision, bad.sha, bad.source).Error
		if (err != nil) != bad.wantReject {
			t.Errorf("%s: err = %v, wantReject = %v", bad.name, err, bad.wantReject)
		}
		if err != nil {
			tx.Exec("ROLLBACK TO SAVEPOINT bad")
		}
		tx.Exec("RELEASE SAVEPOINT bad")
	}

	// Archiving an item must not remove its history; only a hard delete does.
	if err := tx.Exec(`DELETE FROM capability_items WHERE id = ?`, itemB).Error; err != nil {
		t.Fatalf("delete item: %v", err)
	}
	var remaining int64
	if err := tx.Raw(`SELECT COUNT(*) FROM capability_item_git_revisions WHERE item_id = ?`, itemB).Scan(&remaining).Error; err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("revisions after item hard delete = %d, want 0 (cascade)", remaining)
	}
}

func TestSyncTombstonesMigration_GitTombstoneCannotOmitItsLifecycleReason(t *testing.T) {
	tx := isolatedMigrationTx(t, "sync_tombstones_test", nil,
		"20260805000200_create_capability_sync_tombstones.sql")

	const itemID = "11111111-1111-1111-1111-111111111111"
	insert := `INSERT INTO capability_sync_tombstones
		(user_id, item_id, reason, lifecycle_reason, source, event_id, removed_at)
		VALUES (?, ?, ?, ?, ?, ?, now())`

	cases := []struct {
		name       string
		user       string
		reason     string
		lifecycle  any
		source     string
		eventID    string
		wantReject bool
	}{
		// Regression: written as `lifecycle_reason IN (...)` alone, this row is
		// ACCEPTED, because "NULL IN (...)" is NULL and a CHECK evaluating to
		// NULL passes. The Git tombstone that must carry a cause would then
		// carry none.
		{"git tombstone without a lifecycle reason", "u-1", "git_archived", nil, "git_lifecycle", "evt-1", true},
		{"git tombstone with a lifecycle reason", "u-2", "git_archived", "manifest_removed", "git_lifecycle", "evt-2", false},
		{"non-git tombstone carrying a lifecycle reason", "u-3", "unfavorited", "manifest_removed", "favorite", "evt-3", true},
		{"unfavorite tombstone", "u-4", "unfavorited", nil, "favorite", "evt-4", false},
		{"distribution revoke tombstone", "u-5", "distribution_revoked", nil, "distribution", "evt-5", false},
		{"unknown reason", "u-6", "deleted", nil, "favorite", "evt-6", true},
		{"unknown source", "u-7", "unfavorited", nil, "manual", "evt-7", true},
		{"unknown lifecycle reason", "u-8", "git_archived", "repo_gone", "git_lifecycle", "evt-8", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT probe")
			err := tx.Exec(insert, tc.user, itemID, tc.reason, tc.lifecycle, tc.source, tc.eventID).Error
			if (err != nil) != tc.wantReject {
				t.Fatalf("err = %v, wantReject = %v", err, tc.wantReject)
			}
			if err != nil {
				tx.Exec("ROLLBACK TO SAVEPOINT probe")
			}
			tx.Exec("RELEASE SAVEPOINT probe")
		})
	}

	// One terminal record per user/item, and event ids never collide.
	tx.Exec("SAVEPOINT dup_pair")
	if err := tx.Exec(insert, "u-4", itemID, "unfavorited", nil, "favorite", "evt-9").Error; err == nil {
		t.Error("a second tombstone for the same user/item must be rejected")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT dup_pair")

	tx.Exec("SAVEPOINT dup_event")
	if err := tx.Exec(insert, "u-9", itemID, "unfavorited", nil, "favorite", "evt-4").Error; err == nil {
		t.Error("a reused event id must be rejected")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT dup_event")
}

func TestSnapshotGenerationMigration_AllocatesStrictlyIncreasingPerPrincipal(t *testing.T) {
	tx := isolatedMigrationTx(t, "snapshot_generation_test", nil,
		"20260805000300_create_capability_sync_snapshot_generations.sql")

	// The documented allocation statement; the API layer must use this exact
	// upsert so the row lock serializes one principal's materialization.
	const allocate = `INSERT INTO capability_sync_snapshot_generations (principal_id, generation, last_allocated_at)
		VALUES (?, 1, now())
		ON CONFLICT (principal_id) DO UPDATE
		  SET generation = capability_sync_snapshot_generations.generation + 1,
		      last_allocated_at = now(), updated_at = now()
		RETURNING generation`

	for _, want := range []int64{1, 2, 3} {
		var got int64
		if err := tx.Raw(allocate, "u-1").Scan(&got).Error; err != nil {
			t.Fatalf("allocate: %v", err)
		}
		if got != want {
			t.Fatalf("generation = %d, want %d", got, want)
		}
	}
	// A different principal has its own sequence.
	var other int64
	if err := tx.Raw(allocate, "u-2").Scan(&other).Error; err != nil {
		t.Fatalf("allocate for second principal: %v", err)
	}
	if other != 1 {
		t.Fatalf("second principal generation = %d, want 1", other)
	}

	const snapshotID = "11111111-1111-1111-1111-111111111111"
	if err := tx.Exec(`INSERT INTO capability_sync_snapshots (id, principal_id, generation, expires_at)
		VALUES (?, 'u-1', 3, now() + interval '10 minutes')`, snapshotID).Error; err != nil {
		t.Fatalf("reserve snapshot: %v", err)
	}

	tx.Exec("SAVEPOINT incomplete")
	if err := tx.Exec(`UPDATE capability_sync_snapshots SET complete=true, page_count=1 WHERE id=?`, snapshotID).Error; err == nil {
		t.Error("a snapshot cannot be complete without a digest")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT incomplete")

	tx.Exec("SAVEPOINT uppercase")
	if err := tx.Exec(`UPDATE capability_sync_snapshots SET snapshot_digest=upper(repeat('a',64)) WHERE id=?`, snapshotID).Error; err == nil {
		t.Error("the digest must be lowercase sha256 hex")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT uppercase")

	if err := tx.Exec(`UPDATE capability_sync_snapshots
		SET complete=true, page_count=2, item_count=7, tombstone_count=1, snapshot_digest=repeat('a',64)
		WHERE id=?`, snapshotID).Error; err != nil {
		t.Fatalf("finalize snapshot: %v", err)
	}

	tx.Exec("SAVEPOINT reuse")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshots (principal_id, generation, expires_at)
		VALUES ('u-1', 3, now())`).Error; err == nil {
		t.Error("a served generation must never be reused for the same principal")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT reuse")
}
