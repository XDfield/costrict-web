package migrations_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
)

// This migration is the ONLY definition of the two flatten tool tables:
// `migrate flatten-plugins` bootstraps itself by reading this same file rather
// than carrying its own copy (the copy it used to carry had already drifted by
// two indexes and every column comment). Two properties therefore have to hold
// here and not only in cmd/migrate's own suite:
//
//  1. goose's shape still works. goose sends the whole StatementBegin/End block
//     as one Exec, and this file now contains ALTER statements after the CREATEs.
//  2. Re-running the Up block CONVERGES an existing table. `CREATE TABLE IF NOT
//     EXISTS` is a no-op on a table that already exists, so a CHECK that gained
//     a value would never reach an environment where an earlier draft ran — the
//     bootstrap path relies on the re-statement to fix that.
func TestPluginFlattenRunsMigration_IsConvergentNotJustIdempotent(t *testing.T) {
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
	defer func() { _ = tx.Rollback().Error }()

	schemaName := fmt.Sprintf("plugin_flatten_migration_test_%d", time.Now().UnixNano())
	quotedSchema := quoteIdentifier(schemaName)
	if err := tx.Exec("CREATE SCHEMA " + quotedSchema).Error; err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	if err := tx.Exec("SET LOCAL search_path TO " + quotedSchema).Error; err != nil {
		t.Fatalf("set isolated search path: %v", err)
	}

	migrationSQL, err := readGooseUp(filepath.Join("20260806000400_create_plugin_flatten_migration_runs.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	// goose applies the block exactly like this: one Exec, statements included.
	if err := tx.Exec(migrationSQL).Error; err != nil {
		t.Fatalf("apply plugin flatten migration: %v", err)
	}

	// Stand in for an environment that ran an earlier draft of this file: the
	// tables exist with all their columns, but the two indexes that were added
	// later are absent and the row_state CHECK predates `already_at_target`.
	// (That is the real drift: the command's hand-copied DDL, which this file
	// replaced, created both tables in full and was only missing those.)
	for _, downgrade := range []string{
		`DROP INDEX idx_plugin_flatten_runs_source`,
		`DROP INDEX idx_plugin_flatten_rows_item`,
		`ALTER TABLE plugin_flatten_migration_rows DROP CONSTRAINT chk_plugin_flatten_rows_state`,
		`ALTER TABLE plugin_flatten_migration_rows ADD CONSTRAINT chk_plugin_flatten_rows_state
			CHECK (row_state IN ('pending','applied','skipped','failed'))`,
	} {
		if err := tx.Exec(downgrade).Error; err != nil {
			t.Fatalf("simulate an earlier draft (%s): %v", downgrade, err)
		}
	}

	// Re-applying must CONVERGE it, not no-op through CREATE TABLE IF NOT EXISTS.
	if err := tx.Exec(migrationSQL).Error; err != nil {
		t.Fatalf("re-apply over a drifted table: %v", err)
	}

	if err := tx.Exec(`INSERT INTO plugin_flatten_migration_runs (id, mode, status)
		VALUES ('44444444-4444-4444-8444-000000000001', 'migrate', 'planned')`).Error; err != nil {
		t.Fatalf("seed run: %v", err)
	}
	insertRow := func(seq int, itemID, state string) error {
		return tx.Exec(`INSERT INTO plugin_flatten_migration_rows
			(run_id, seq, item_id, before_status, after_status, classification, action, row_state)
			VALUES ('44444444-4444-4444-8444-000000000001', ?, ?::uuid,
			        'active', 'archived', 'derived_catalog', 'archive_and_unlink', ?)`,
			seq, itemID, state).Error
	}
	// The converged CHECK must accept the state the tool now writes...
	if err := insertRow(1, "55555555-5555-4555-8555-000000000001", "already_at_target"); err != nil {
		t.Fatalf("CHECK did not converge; already_at_target rejected: %v", err)
	}
	// ...and still reject anything else. Behind a savepoint: the rejection aborts
	// the transaction, and everything after it would then fail for that reason
	// rather than its own.
	if err := tx.Exec("SAVEPOINT unknown_state").Error; err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if err := insertRow(2, "55555555-5555-4555-8555-000000000002", "whatever"); err == nil {
		t.Fatal("CHECK accepted an unknown row_state")
	}
	if err := tx.Exec("ROLLBACK TO SAVEPOINT unknown_state").Error; err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}

	// Applying the block a second time must stay a no-op, since the bootstrap
	// path runs it on every invocation of the command.
	if err := tx.Exec(migrationSQL).Error; err != nil {
		t.Fatalf("second application is not idempotent: %v", err)
	}

	var indexes []string
	if err := tx.Raw(`SELECT indexname FROM pg_indexes
		WHERE schemaname = ? AND tablename LIKE 'plugin_flatten_migration_%'`, schemaName).
		Scan(&indexes).Error; err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	joined := strings.Join(indexes, " ")
	for _, want := range []string{"idx_plugin_flatten_runs_source", "idx_plugin_flatten_rows_item"} {
		if !strings.Contains(joined, want) {
			t.Errorf("index %s missing after the migration ran: %v", want, indexes)
		}
	}
}
