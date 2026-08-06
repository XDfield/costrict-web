package migrations_test

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// snapshotMaterializeMigrations is the whole snapshot stack. The later
// migration only makes sense against the earlier one — it adds the frozen
// payload, the change-detection digest and the three guard triggers to tables
// the first one created.
var snapshotMaterializeMigrations = []string{
	"20260805000300_create_capability_sync_snapshot_generations.sql",
	"20260805000800_materialize_capability_sync_snapshots.sql",
}

const (
	snapshotTestPrincipal = "principal-a"
	snapshotTestDigestA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapshotTestDigestB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// allocateTestGeneration takes a generation through the only path the schema
// permits, so every later insert in a test is describing a number that was
// actually issued.
func allocateTestGeneration(t *testing.T, tx *gorm.DB, principal string) int64 {
	t.Helper()
	var generation int64
	err := tx.Raw(`
		INSERT INTO capability_sync_snapshot_generations (principal_id, generation, last_allocated_at)
		VALUES (?, 1, now())
		ON CONFLICT (principal_id) DO UPDATE
			SET generation = capability_sync_snapshot_generations.generation + 1,
			    last_allocated_at = now(), updated_at = now()
		RETURNING generation`, principal).Row().Scan(&generation)
	if err != nil {
		t.Fatalf("allocate generation: %v", err)
	}
	return generation
}

func insertCompleteSnapshot(t *testing.T, tx *gorm.DB, id, principal string, generation int64) {
	t.Helper()
	if err := tx.Exec(`INSERT INTO capability_sync_snapshots
		(id, principal_id, generation, page_count, page_size, item_count, tombstone_count,
		 snapshot_digest, content_digest, complete, expires_at)
		VALUES (?, ?, ?, 1, 200, 0, 0, ?, ?, true, now() + interval '1 hour')`,
		id, principal, generation, snapshotTestDigestA, snapshotTestDigestB).Error; err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

// F-5, in the database. The unique constraint stops a number being REUSED; it
// says nothing about a number being INVENTED, and one invented number is a real
// inversion — csc accepts only strictly greater generations, so a manifest
// written at 42 while the counter holds 5 makes every generation from 6 to 41
// permanently unacceptable. Silent, and visible only as "stale generation" in a
// log.
func TestSnapshotMaterializeMigration_GenerationCannotBeInvented(t *testing.T) {
	tx := isolatedMigrationTx(t, "snapshot_generation_guard", nil, snapshotMaterializeMigrations...)

	if got := allocateTestGeneration(t, tx, snapshotTestPrincipal); got != 1 {
		t.Fatalf("first allocation = %d, want 1", got)
	}
	if got := allocateTestGeneration(t, tx, snapshotTestPrincipal); got != 2 {
		t.Fatalf("second allocation = %d, want 2", got)
	}

	cases := []struct {
		name       string
		sql        string
		args       []any
		wantReject bool
	}{
		{
			"the counter may not jump forward",
			`UPDATE capability_sync_snapshot_generations SET generation = 42 WHERE principal_id = ?`,
			[]any{snapshotTestPrincipal}, true,
		},
		{
			"the counter may not go backwards",
			`UPDATE capability_sync_snapshot_generations SET generation = 1 WHERE principal_id = ?`,
			[]any{snapshotTestPrincipal}, true,
		},
		{
			"a principal may not be seeded above 1",
			`INSERT INTO capability_sync_snapshot_generations (principal_id, generation) VALUES ('seeded', 7)`,
			nil, true,
		},
		{
			"a principal may be created at 0",
			`INSERT INTO capability_sync_snapshot_generations (principal_id, generation) VALUES ('fresh-zero', 0)`,
			nil, false,
		},
		{
			"a principal may be created at 1",
			`INSERT INTO capability_sync_snapshot_generations (principal_id, generation) VALUES ('fresh-one', 1)`,
			nil, false,
		},
		{
			"metadata-only updates do not have to burn a generation",
			`UPDATE capability_sync_snapshot_generations SET last_allocated_at = now() WHERE principal_id = ?`,
			[]any{snapshotTestPrincipal}, false,
		},
		{
			"a row may not change principal",
			`UPDATE capability_sync_snapshot_generations SET principal_id = 'stolen' WHERE principal_id = ?`,
			[]any{snapshotTestPrincipal}, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT probe")
			err := tx.Exec(tc.sql, tc.args...).Error
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

// The other half of F-5: a manifest may only carry the number the allocator
// currently holds, so "allocate, then write the manifest" is the only
// expressible sequence.
func TestSnapshotMaterializeMigration_ManifestMustMatchTheAllocator(t *testing.T) {
	tx := isolatedMigrationTx(t, "snapshot_manifest_guard", nil, snapshotMaterializeMigrations...)

	const snapshotID = "11111111-1111-4111-8111-111111111111"

	tx.Exec("SAVEPOINT unallocated")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshots
		(id, principal_id, generation, page_count, page_size, item_count, tombstone_count,
		 snapshot_digest, content_digest, complete, expires_at)
		VALUES (?, 'never-allocated', 1, 1, 200, 0, 0, ?, ?, true, now() + interval '1 hour')`,
		snapshotID, snapshotTestDigestA, snapshotTestDigestB).Error; err == nil {
		t.Fatal("a manifest for a principal with no allocator row was accepted")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT unallocated")

	generation := allocateTestGeneration(t, tx, snapshotTestPrincipal)

	tx.Exec("SAVEPOINT ahead")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshots
		(id, principal_id, generation, page_count, page_size, item_count, tombstone_count,
		 snapshot_digest, content_digest, complete, expires_at)
		VALUES (?, ?, ?, 1, 200, 0, 0, ?, ?, true, now() + interval '1 hour')`,
		snapshotID, snapshotTestPrincipal, generation+41, snapshotTestDigestA, snapshotTestDigestB).Error; err == nil {
		t.Fatal("a manifest carrying an unallocated generation was accepted; csc would then reject every number in between")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT ahead")

	insertCompleteSnapshot(t, tx, snapshotID, snapshotTestPrincipal, generation)

	// Frozen: everything a client's verification depends on is immutable once
	// the snapshot is complete.
	frozen := []struct {
		name string
		sql  string
	}{
		{"generation", `UPDATE capability_sync_snapshots SET generation = generation + 1 WHERE id = ?`},
		{"page count", `UPDATE capability_sync_snapshots SET page_count = 2 WHERE id = ?`},
		{"page size", `UPDATE capability_sync_snapshots SET page_size = 50 WHERE id = ?`},
		{"item count", `UPDATE capability_sync_snapshots SET item_count = 1 WHERE id = ?`},
		{"tombstone count", `UPDATE capability_sync_snapshots SET tombstone_count = 1 WHERE id = ?`},
		{"snapshot digest", `UPDATE capability_sync_snapshots SET snapshot_digest = ? WHERE id = ?`},
		{"completeness", `UPDATE capability_sync_snapshots SET complete = false WHERE id = ?`},
		{"generated at", `UPDATE capability_sync_snapshots SET generated_at = generated_at + interval '1 second' WHERE id = ?`},
	}
	for _, tc := range frozen {
		t.Run("frozen: "+tc.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT probe")
			var err error
			if strings.Count(tc.sql, "?") == 2 {
				err = tx.Exec(tc.sql, snapshotTestDigestB, snapshotID).Error
			} else {
				err = tx.Exec(tc.sql, snapshotID).Error
			}
			if err == nil {
				t.Fatalf("%s was mutable on a complete snapshot", tc.name)
			}
			tx.Exec("ROLLBACK TO SAVEPOINT probe")
			tx.Exec("RELEASE SAVEPOINT probe")
		})
	}

	// expires_at is the one exception, and it has to be: re-serving unchanged
	// content under its original generation is what stops a polling fleet from
	// burning numbers, and that requires extending the lease of a frozen row.
	if err := tx.Exec(`UPDATE capability_sync_snapshots SET expires_at = now() + interval '2 hours' WHERE id = ?`,
		snapshotID).Error; err != nil {
		t.Fatalf("expires_at must remain extendable: %v", err)
	}
}

// A complete snapshot must be fully described. Without page_size a reader would
// have to guess how to slice the stored artifact, and a wrong guess produces
// pages that reassemble to a different digest than the one advertised.
func TestSnapshotMaterializeMigration_CompleteRequiresAFullDescription(t *testing.T) {
	tx := isolatedMigrationTx(t, "snapshot_complete_guard", nil, snapshotMaterializeMigrations...)
	generation := allocateTestGeneration(t, tx, snapshotTestPrincipal)

	cases := []struct {
		name       string
		columns    string
		values     string
		args       []any
		wantReject bool
	}{
		{
			"complete without a content digest",
			"snapshot_digest, page_size", "?, 200",
			[]any{snapshotTestDigestA}, true,
		},
		{
			"complete without a snapshot digest",
			"content_digest, page_size", "?, 200",
			[]any{snapshotTestDigestB}, true,
		},
		{
			"complete without a page size",
			"snapshot_digest, content_digest", "?, ?",
			[]any{snapshotTestDigestA, snapshotTestDigestB}, true,
		},
		{
			"complete with everything",
			"snapshot_digest, content_digest, page_size", "?, ?, 200",
			[]any{snapshotTestDigestA, snapshotTestDigestB}, false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx.Exec("SAVEPOINT probe")
			args := append([]any{}, tc.args...)
			sql := `INSERT INTO capability_sync_snapshots
				(id, principal_id, generation, page_count, item_count, tombstone_count, complete, expires_at, ` +
				tc.columns + `) VALUES (gen_random_uuid(), ?, ?, 1, 0, 0, true, now() + interval '1 hour', ` +
				tc.values + `)`
			err := tx.Exec(sql, append([]any{snapshotTestPrincipal, generation + int64(i)}, args...)...).Error
			// Only the last case allocates its own generation legitimately;
			// the rejected ones must fail on the CHECK, not on the guard.
			if tc.wantReject && err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if err != nil {
				tx.Exec("ROLLBACK TO SAVEPOINT probe")
			}
			tx.Exec("RELEASE SAVEPOINT probe")
		})
	}

	// A malformed content digest is rejected by format, like the snapshot
	// digest already was.
	tx.Exec("SAVEPOINT format")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshots
		(id, principal_id, generation, page_count, page_size, item_count, tombstone_count,
		 snapshot_digest, content_digest, complete, expires_at)
		VALUES (gen_random_uuid(), ?, ?, 1, 200, 0, 0, ?, 'NOTHEX', true, now() + interval '1 hour')`,
		snapshotTestPrincipal, generation, snapshotTestDigestA).Error; err == nil {
		t.Fatal("a non-hex content digest was accepted")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT format")
}

// The frozen artifact: a payload's recorded size must match its bytes, it may
// never be rewritten, and it disappears with its manifest.
func TestSnapshotMaterializeMigration_PayloadIsFrozenAndSelfConsistent(t *testing.T) {
	tx := isolatedMigrationTx(t, "snapshot_payload_guard", nil, snapshotMaterializeMigrations...)
	generation := allocateTestGeneration(t, tx, snapshotTestPrincipal)
	const snapshotID = "22222222-2222-4222-8222-222222222222"
	insertCompleteSnapshot(t, tx, snapshotID, snapshotTestPrincipal, generation)

	tx.Exec("SAVEPOINT orphan")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshot_payloads (snapshot_id, byte_size, payload)
		VALUES (gen_random_uuid(), 2, '{}'::bytea)`).Error; err == nil {
		t.Fatal("a payload with no manifest was accepted; it could never be served")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT orphan")

	tx.Exec("SAVEPOINT size")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshot_payloads (snapshot_id, byte_size, payload)
		VALUES (?, 99, '{}'::bytea)`, snapshotID).Error; err == nil {
		t.Fatal("byte_size was allowed to disagree with the payload it describes")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT size")

	tx.Exec("SAVEPOINT empty")
	if err := tx.Exec(`INSERT INTO capability_sync_snapshot_payloads (snapshot_id, byte_size, payload)
		VALUES (?, 0, ''::bytea)`, snapshotID).Error; err == nil {
		t.Fatal("an empty payload was accepted; a complete snapshot always serializes to at least a document")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT empty")

	if err := tx.Exec(`INSERT INTO capability_sync_snapshot_payloads (snapshot_id, byte_size, payload)
		VALUES (?, 2, '{}'::bytea)`, snapshotID).Error; err != nil {
		t.Fatalf("a well-formed payload must be insertable: %v", err)
	}

	tx.Exec("SAVEPOINT rewrite")
	if err := tx.Exec(`UPDATE capability_sync_snapshot_payloads SET payload = '[]'::bytea, byte_size = 2
		WHERE snapshot_id = ?`, snapshotID).Error; err == nil {
		t.Fatal("a stored payload was rewritable; its manifest's digest would then describe content that is gone, " +
			"and every client would discard every page forever with no server-side symptom")
	}
	tx.Exec("ROLLBACK TO SAVEPOINT rewrite")

	// Cascade: unlike a tombstone (which must outlive its item), a payload
	// without its manifest is unservable, so it goes with it.
	if err := tx.Exec(`DELETE FROM capability_sync_snapshots WHERE id = ?`, snapshotID).Error; err != nil {
		t.Fatalf("delete manifest: %v", err)
	}
	var remaining int64
	if err := tx.Raw(`SELECT COUNT(*) FROM capability_sync_snapshot_payloads WHERE snapshot_id = ?`,
		snapshotID).Scan(&remaining).Error; err != nil {
		t.Fatalf("count payloads: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("payloads after manifest delete = %d, want 0 (cascade)", remaining)
	}
}
