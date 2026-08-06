package migrations_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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

// gitRevisionMigrations is the whole revision-table stack, applied together
// because the later two only make sense against the first: the digest column is
// the append trigger and the SHA constraint replaces the original one.
var gitRevisionMigrations = []string{
	"20260805000100_create_capability_item_git_revisions.sql",
	"20260805000500_add_capability_item_git_revision_content_digest.sql",
	"20260805000600_constrain_capability_item_git_revision_sha.sql",
}

func TestGitRevisionsMigration_RevisionNumberIsUniqueWithinItemAndSHAMayRepeat(t *testing.T) {
	tx := isolatedMigrationTx(t, "git_revisions_test",
		[]string{capabilityItemsFixture}, gitRevisionMigrations...)

	const itemA = "11111111-1111-1111-1111-111111111111"
	const itemB = "22222222-2222-2222-2222-222222222222"
	for _, id := range []string{itemA, itemB} {
		if err := tx.Exec(`INSERT INTO capability_items (id, content_backend) VALUES (?, 'git')`, id).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}

	insert := `INSERT INTO capability_item_git_revisions
		(item_id, revision_no, git_server_id, git_repo_id, git_sha, source, content_digest, observed_at)
		VALUES (?, ?, 'gs-1', 42, ?, ?, ?, now())`
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	digestA := strings.Repeat("1", 64)
	digestB := strings.Repeat("2", 64)

	if err := tx.Exec(insert, itemA, 1, shaA, "backfill", digestA).Error; err != nil {
		t.Fatalf("first revision: %v", err)
	}
	if err := tx.Exec(insert, itemA, 2, shaB, "push", digestB).Error; err != nil {
		t.Fatalf("second revision: %v", err)
	}

	// A force-push/revert back to an already observed SHA — and back to an
	// already observed DIGEST — is a NEW transition and must be recordable.
	// History is a sequence of transitions, not a set of states; only the
	// revision number is unique.
	if err := tx.Exec(insert, itemA, 3, shaA, "push", digestA).Error; err != nil {
		t.Fatalf("revisiting an earlier state must be a new revision: %v", err)
	}

	// Revision numbers are scoped to the item, not global.
	if err := tx.Exec(insert, itemB, 1, shaA, "provision", digestA).Error; err != nil {
		t.Fatalf("another item must start at revision 1: %v", err)
	}

	tx.Exec("SAVEPOINT dup")
	if err := tx.Exec(insert, itemA, 2, shaA, "reconcile", digestA).Error; err == nil {
		t.Fatal("a duplicate revision_no within one item must be rejected")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT dup")

	for _, bad := range []struct {
		name       string
		revision   int
		sha        string
		source     string
		digest     any
		wantReject bool
	}{
		{"unknown source", 9, shaA, "webhook", digestA, true},
		{"revision zero", 0, shaA, "push", digestA, true},

		// F-3: `git_sha <> ''` accepted all of these. A coordinate that cannot
		// be resolved against Gitea, or that is a different string from the
		// lowercase SHA the projection writes, is not a smaller problem than a
		// missing one.
		{"empty sha", 9, "", "push", digestA, true},
		{"short sha", 9, strings.Repeat("a", 39), "push", digestA, true},
		{"over-long sha", 9, strings.Repeat("a", 41), "push", digestA, true},
		{"non-hex sha", 9, strings.Repeat("x", 40), "push", digestA, true},
		{"uppercase sha", 9, strings.Repeat("A", 40), "push", digestA, true},
		{"lowercase 40-hex sha", 9, strings.Repeat("f", 40), "push", digestA, false},

		// The digest is the append trigger, so a row without one would leave the
		// next projection nothing to compare against. Only the synthesized
		// backfill baseline may omit it.
		{"push without a digest", 10, shaA, "push", nil, true},
		{"reconcile without a digest", 10, shaA, "reconcile", nil, true},
		{"backfill without a digest", 10, shaA, "backfill", nil, false},
		{"empty digest", 11, shaA, "push", "", true},
		{"short digest", 11, shaA, "push", strings.Repeat("1", 63), true},
		{"uppercase digest", 11, shaA, "push", strings.Repeat("A", 64), true},
	} {
		t.Run(bad.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT bad")
			err := tx.Exec(insert, itemA, bad.revision, bad.sha, bad.source, bad.digest).Error
			if (err != nil) != bad.wantReject {
				t.Errorf("err = %v, wantReject = %v", err, bad.wantReject)
			}
			if err != nil {
				tx.Exec("ROLLBACK TO SAVEPOINT bad")
			}
			tx.Exec("RELEASE SAVEPOINT bad")
		})
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

// TestGitRevisionsMigration_BackfilledBaselineIsTheOnlyDigestlessRow states the
// hole the digest column deliberately leaves and proves it is exactly one row
// shape wide.
//
// `migrate backfill-git-revisions` seeds revision 1 from the database alone,
// and a Git-backed row does not store its content, so the baseline digest is
// not computable at backfill time. Recording NULL says so; inventing a value
// would make the next projection look like a change. The constraint's job is to
// keep that exemption from spreading to any other writer.
func TestGitRevisionsMigration_BackfilledBaselineIsTheOnlyDigestlessRow(t *testing.T) {
	tx := isolatedMigrationTx(t, "git_revision_digest_test",
		[]string{capabilityItemsFixture}, gitRevisionMigrations...)

	const itemID = "11111111-1111-1111-1111-111111111111"
	if err := tx.Exec(`INSERT INTO capability_items (id, content_backend) VALUES (?, 'git')`, itemID).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// The backfill's statement, verbatim in shape: content_digest is not in the
	// column list at all.
	if err := tx.Exec(`INSERT INTO capability_item_git_revisions
		(item_id, revision_no, git_server_id, git_repo_id, git_sha, source, observed_at)
		VALUES (?, 1, 'gs-1', 42, ?, 'backfill', now())`, itemID, strings.Repeat("a", 40)).Error; err != nil {
		t.Fatalf("the backfill baseline must be insertable without a digest: %v", err)
	}

	// Adoption: the compare-and-set the sync writer performs on the first
	// successful projection. It fills the digest in and touches nothing else.
	digest := strings.Repeat("3", 64)
	result := tx.Exec(`UPDATE capability_item_git_revisions
		SET content_digest = ?
		WHERE item_id = ? AND revision_no = 1 AND source = 'backfill' AND content_digest IS NULL`,
		digest, itemID)
	if result.Error != nil {
		t.Fatalf("adopt: %v", result.Error)
	}
	if result.RowsAffected != 1 {
		t.Fatalf("adoption updated %d rows, want 1", result.RowsAffected)
	}

	// Re-running it cannot overwrite an observed digest.
	second := tx.Exec(`UPDATE capability_item_git_revisions
		SET content_digest = ?
		WHERE item_id = ? AND revision_no = 1 AND source = 'backfill' AND content_digest IS NULL`,
		strings.Repeat("4", 64), itemID)
	if second.Error != nil {
		t.Fatalf("second adopt: %v", second.Error)
	}
	if second.RowsAffected != 0 {
		t.Fatal("adoption overwrote an already observed digest")
	}
	var stored string
	if err := tx.Raw(`SELECT content_digest FROM capability_item_git_revisions WHERE item_id = ?`, itemID).
		Row().Scan(&stored); err != nil {
		t.Fatalf("read digest: %v", err)
	}
	if stored != digest {
		t.Fatalf("digest = %q, want the first adopted %q", stored, digest)
	}

	// A digest cannot be blanked back out on a row that has one, either.
	tx.Exec("SAVEPOINT unblank")
	if err := tx.Exec(`UPDATE capability_item_git_revisions SET content_digest = '' WHERE item_id = ?`, itemID).Error; err == nil {
		t.Error("an empty digest must be rejected; NULL is the only 'unobserved' spelling")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT unblank")
}

func TestSyncTombstonesMigration_GitTombstoneCannotOmitItsLifecycleReason(t *testing.T) {
	tx := isolatedMigrationTx(t, "sync_tombstones_test", nil,
		"20260805000200_create_capability_sync_tombstones.sql",
		"20260805000700_constrain_capability_sync_tombstone_triples.sql")

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

		// F-3 of the review, on this table: the three original CHECKs each
		// looked at one field, so a row could satisfy all of them and still
		// describe a removal that cannot have happened. Both rows below passed
		// before the triple constraint.
		//
		// The first is the lifecycle CHECK defeated by lying about `source`
		// instead of about `lifecycle_reason` — a git_archived tombstone with no
		// Git cause, which is the exact row that CHECK existed to prevent.
		{"git_archived attributed to the favorite subsystem", "u-9", "git_archived", nil, "favorite", "evt-9", true},
		// The second attributes a favourite the user removed to a repository
		// event. csc reads reason/source/lifecycleReason as one statement; a row
		// whose fields disagree is a false statement, not a partial one.
		{"unfavorited attributed to the Git lifecycle", "u-10", "unfavorited", "manifest_removed", "git_lifecycle", "evt-10", true},
		{"git_archived attributed to distribution", "u-11", "git_archived", "repository_deleted", "distribution", "evt-11", true},
		{"distribution_revoked attributed to favorites", "u-12", "distribution_revoked", nil, "favorite", "evt-12", true},
		{"unfavorited attributed to distribution", "u-13", "unfavorited", nil, "distribution", "evt-13", true},
		// The remaining legal Git causes stay legal.
		{"git tombstone for a missing default branch", "u-14", "git_archived", "default_branch_missing", "git_lifecycle", "evt-14", false},
		{"git tombstone for a deleted repository", "u-15", "git_archived", "repository_deleted", "git_lifecycle", "evt-15", false},
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

// --- Two-session concurrency (review finding F-9) ---------------------------
//
// Everything above runs inside ONE rolled-back transaction, which is the right
// shape for constraint tests and useless for concurrency: an uncommitted schema
// is invisible to a second session, and a single connection cannot contend with
// itself. The generation allocator's whole point is what happens when two
// materializations for one principal overlap, so it needs a committed schema
// and real sessions.

// newCommittedMigrationSchema creates a COMMITTED throwaway schema, applies the
// named migrations into it, and returns a *sql.DB whose every pooled connection
// starts there.
//
// search_path travels in the DSN rather than as a `SET` statement because the
// test opens several connections at once; a per-session SET would apply to
// whichever connection happened to run it and silently leave the others
// pointing at `public`.
func newCommittedMigrationSchema(t *testing.T, prefix string, migrationFiles ...string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL concurrency test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	quoted := quoteIdentifier(schema)

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + quoted + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL in %s: %v", schema, err)
	}
	for _, file := range migrationFiles {
		up, err := readGooseUp(file)
		if err != nil {
			t.Fatalf("read migration %s: %v", file, err)
		}
		if err := scoped.Exec(up).Error; err != nil {
			t.Fatalf("apply migration %s: %v", file, err)
		}
	}
	sqlDB, err := scoped.DB()
	if err != nil {
		t.Fatalf("unwrap sql.DB: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// snapshotGenerationAllocate is the documented allocation statement. The API
// layer must use this exact upsert, so the test must contend on it and not on a
// convenient paraphrase.
const snapshotGenerationAllocate = `INSERT INTO capability_sync_snapshot_generations (principal_id, generation, last_allocated_at)
	VALUES ($1, 1, now())
	ON CONFLICT (principal_id) DO UPDATE
	  SET generation = capability_sync_snapshot_generations.generation + 1,
	      last_allocated_at = now(), updated_at = now()
	RETURNING generation`

// sqlStater is implemented by pgx's *pgconn.PgError. Asserting the interface
// rather than importing pgconn keeps a test-only dependency out of go.mod while
// still matching on the SQLSTATE instead of on an error message that PostgreSQL
// is free to reword.
type sqlStater interface{ SQLState() string }

func sqlState(err error) string {
	var stater sqlStater
	if errors.As(err, &stater) {
		return stater.SQLState()
	}
	return ""
}

// waitForSnapshotGenerationLockWaiter blocks until some backend is waiting on a
// row lock while running the allocator.
//
// Without it the "concurrent" test would usually run its two transactions one
// after the other and prove nothing. The query text is part of the predicate on
// purpose: `go test ./...` runs packages in parallel against the same database,
// and a waiter belonging to another package's test would otherwise be accepted
// as this test's contention.
//
// It returns an error rather than failing, because its caller is holding a
// transaction open and must release it before reporting anything.
func waitForSnapshotGenerationLockWaiter(db *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int64
		err := db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query ILIKE '%capability_sync_snapshot_generations%'`).Scan(&waiting)
		if err != nil {
			return fmt.Errorf("query PostgreSQL waiters: %w", err)
		}
		if waiting > 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timed out waiting for an allocation to block on the principal row lock")
}

// TestSnapshotGenerationMigration_ConcurrentAllocationSerializesOrAborts builds
// the race the generation counter exists for, on two real sessions.
//
// It is not "start two goroutines and hope": the first transaction is held open
// on the row lock while the second is confirmed — through pg_stat_activity — to
// be BLOCKED on it, so the hand-off is the real, contended one.
//
// Two isolation levels, because the difference between them is the whole
// finding (F-4, deferred) and this test is what pins the behaviour it rests on:
//
//   - REPEATABLE READ: the loser cannot silently proceed. When the winner
//     commits, the blocked upsert aborts with SQLSTATE 40001 and the whole
//     transaction — including anything it had already read for the snapshot —
//     is discarded. Retrying gets the next number. This is the ONLY level at
//     which "allocate the generation, then materialize from that snapshot" is a
//     consistent build.
//   - READ COMMITTED (PostgreSQL's default, and therefore what a caller gets if
//     nobody sets the level): the loser does NOT abort. It re-reads the
//     committed row, returns the next generation, and carries on inside a
//     transaction whose earlier statements saw a different data state. The
//     counter is still monotonic — no inversion — but the snapshot built under
//     it is not internally consistent, which is exactly the defect F-4
//     describes.
func TestSnapshotGenerationMigration_ConcurrentAllocationSerializesOrAborts(t *testing.T) {
	db := newCommittedMigrationSchema(t, "snapshot_generation_race",
		"20260805000300_create_capability_sync_snapshot_generations.sql")

	// allocate opens its own session at `level`, takes the row lock, optionally
	// parks until released, and reports the generation it was given.
	allocate := func(
		level sql.IsolationLevel, principal string, holdUntil <-chan struct{}, locked chan<- struct{},
	) (int64, error) {
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()

		var generation int64
		if err := tx.QueryRowContext(ctx, snapshotGenerationAllocate, principal).Scan(&generation); err != nil {
			return 0, err
		}
		if locked != nil {
			close(locked)
		}
		if holdUntil != nil {
			<-holdUntil
		}
		if err := tx.Commit(); err != nil {
			return generation, err
		}
		return generation, nil
	}

	// race runs a held allocation and a blocked one, and returns both outcomes.
	// Nothing between opening the barrier and closing it may call t.Fatal:
	// Fatal exits the test goroutine, the barrier would never be released, and
	// the held transaction would block the schema teardown forever — a hang
	// instead of a failure.
	race := func(t *testing.T, level sql.IsolationLevel, principal string) (int64, int64, error, error) {
		t.Helper()
		hold := make(chan struct{})
		locked := make(chan struct{})
		var (
			wg              sync.WaitGroup
			winner, loser   int64
			winErr, loseErr error
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			winner, winErr = allocate(level, principal, hold, locked)
		}()

		var setupErr error
		select {
		case <-locked:
		case <-time.After(5 * time.Second):
			setupErr = errors.New("the first allocation never took the row lock")
		}
		if setupErr == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				loser, loseErr = allocate(level, principal, nil, nil)
			}()
			setupErr = waitForSnapshotGenerationLockWaiter(db)
		}

		close(hold)
		wg.Wait()
		if setupErr != nil {
			t.Fatalf("the contended hand-off was not constructed, so this test would prove nothing: %v", setupErr)
		}
		return winner, loser, winErr, loseErr
	}

	// --- REPEATABLE READ: the loser aborts with 40001 and retries. -----------
	if _, err := allocate(sql.LevelRepeatableRead, "u-rr", nil, nil); err != nil {
		t.Fatalf("seed the principal row: %v", err)
	}
	winner, _, winErr, loseErr := race(t, sql.LevelRepeatableRead, "u-rr")
	if winErr != nil {
		t.Fatalf("the winning allocation failed: %v", winErr)
	}
	if winner != 2 {
		t.Fatalf("winning generation = %d, want 2", winner)
	}
	if loseErr == nil {
		t.Fatal("under REPEATABLE READ the blocked allocation must abort, not silently take the next number")
	}
	if state := sqlState(loseErr); state != "40001" {
		t.Fatalf("blocked allocation failed with SQLSTATE %q (%v), want 40001 serialization_failure", state, loseErr)
	}

	// The aborted transaction wrote nothing: the counter is still where the
	// winner left it, so a retry is a clean allocation and not a repair.
	var afterAbort int64
	if err := db.QueryRow(`SELECT generation FROM capability_sync_snapshot_generations WHERE principal_id = 'u-rr'`).
		Scan(&afterAbort); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	if afterAbort != 2 {
		t.Fatalf("generation after the aborted allocation = %d, want 2", afterAbort)
	}

	// The retry path the caller must implement on 40001.
	retried, err := allocate(sql.LevelRepeatableRead, "u-rr", nil, nil)
	if err != nil {
		t.Fatalf("retry after 40001: %v", err)
	}
	if retried != 3 {
		t.Fatalf("retried generation = %d, want 3", retried)
	}

	// --- READ COMMITTED: the loser survives. --------------------------------
	//
	// Pinned as a hazard, not as an endorsement. It is why the snapshot build
	// must assert its isolation level (F-4) rather than trusting the session
	// default, and this assertion is what would notice if a future PostgreSQL
	// changed the behaviour the deferred fix is premised on.
	if _, err := allocate(sql.LevelReadCommitted, "u-rc", nil, nil); err != nil {
		t.Fatalf("seed the principal row: %v", err)
	}
	rcWinner, rcLoser, rcWinErr, rcLoseErr := race(t, sql.LevelReadCommitted, "u-rc")
	if rcWinErr != nil {
		t.Fatalf("the winning READ COMMITTED allocation failed: %v", rcWinErr)
	}
	if rcLoseErr != nil {
		t.Fatalf("under READ COMMITTED the blocked allocation is expected to survive, got: %v", rcLoseErr)
	}
	if rcWinner != 2 || rcLoser != 3 {
		t.Fatalf("READ COMMITTED generations = %d,%d, want 2,3", rcWinner, rcLoser)
	}
	// Monotonicity itself never breaks — the unique constraint and the row lock
	// see to that. What READ COMMITTED loses is the internal consistency of the
	// build that follows.
	if rcLoser <= rcWinner {
		t.Fatalf("generations were not strictly increasing: %d then %d", rcWinner, rcLoser)
	}
}
