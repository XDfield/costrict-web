//go:build cgo

// End-to-end migration plumbing tests (Up idempotency + Version accuracy)
// require a SQL driver. We use sqlite via cgo to avoid standing up a real
// Postgres in unit tests. The real Postgres-only migrations (BIGSERIAL etc.)
// are exercised by the integration test layer (Helm pre-deploy job + manual
// `cs-user-migrate up` verification).

package migration

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	_ "github.com/mattn/go-sqlite3"
)

// syntheticMigrations is a minimal goose-compatible fs.FS that lets us test
// Up/Version wiring without depending on the real Postgres-only schema.
//
// Goose parses the leading numeric prefix as the version, so file names must
// match `YYYYMMDDhhmmss_*.sql` and contain `-- +goose Up` / `-- +goose Down`
// markers.
const (
	mig1 = `-- +goose Up
CREATE TABLE t1 (id INTEGER PRIMARY KEY);
-- +goose Down
DROP TABLE t1;
`
	mig2 = `-- +goose Up
CREATE TABLE t2 (id INTEGER PRIMARY KEY);
-- +goose Down
DROP TABLE t2;
`
)

func syntheticFS() fs.FS {
	return fstest.MapFS{
		"20260101000000_create_t1.sql": {Data: []byte(mig1)},
		"20260102000000_create_t2.sql": {Data: []byte(mig2)},
	}
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRunner_Up_AppliesAllMigrations runs Up against a fresh sqlite db and
// verifies both synthetic tables exist afterwards.
func TestRunner_Up_AppliesAllMigrations(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", syntheticFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	for _, tbl := range []string{"t1", "t2"} {
		var name string
		err = db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after Up: %v", tbl, err)
		}
	}
}

// TestRunner_Up_IsIdempotent re-runs Up on an already-migrated db; goose
// should treat it as a no-op (no error, no extra work).
func TestRunner_Up_IsIdempotent(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", syntheticFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("second Up (should be no-op): %v", err)
	}
}

// TestRunner_Version_Advances confirms the version table advances from 0 → 2
// after running both synthetic migrations.
func TestRunner_Version_Advances(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", syntheticFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	v0, err := r.Version()
	if err != nil {
		t.Fatalf("Version before Up: %v", err)
	}
	if v0 != 0 {
		t.Errorf("initial version: got %d want 0", v0)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	v2, err := r.Version()
	if err != nil {
		t.Fatalf("Version after Up: %v", err)
	}
	// Goose encodes version as the YYYYMMDDhhmmss prefix; our two files are
	// 20260101000000 and 20260102000000. Latest applied is the latter.
	const wantLatest int64 = 20260102000000
	if v2 != wantLatest {
		t.Errorf("post-Up version: got %d want %d", v2, wantLatest)
	}
}

// TestRunner_Down_RollsBackAllMigrations is the inverse of Up — verifies
// schema returns to a clean state. Useful as a test helper and to document
// expected behaviour of Down.
func TestRunner_Down_RollsBackAllMigrations(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", syntheticFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// Down rolls back one migration at a time — call it twice to clear both.
	if err := r.Down(context.Background()); err != nil {
		t.Fatalf("Down #1: %v", err)
	}
	if err := r.Down(context.Background()); err != nil {
		t.Fatalf("Down #2: %v", err)
	}

	var name string
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='t2'",
	).Scan(&name)
	if err == nil {
		t.Error("t2 still exists after Down (expected sql.ErrNoRows)")
	}
}
