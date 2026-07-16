// Package migration runs cs-user's goose SQL migrations against the service's
// independent PostgreSQL database (ADR D1). Migrations are embedded into the
// binary via migrations.FS so `go install` produces a self-contained artifact.
//
// Production wiring (ADR D7): the migrate binary (cmd/migrate) acquires a
// PostgreSQL advisory lock before running Up, then K8s job hooks call it as a
// pre-deploy step. In dev, main.go runs migrations inline at API boot.
package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/costrict/costrict-web/cs-user/migrations"
	"github.com/pressly/goose/v3"
)

// Runner owns a single *sql.DB and runs goose operations against it. Tests
// inject an in-memory sqlite handle plus a synthetic fs.FS here to verify
// Up/Version plumbing without requiring a real Postgres.
type Runner struct {
	db      *sql.DB
	dialect string
	fsys    fs.FS
}

// NewRunner constructs a Runner bound to the embedded production migrations.
// The dialect must match the underlying driver ("postgres" for prod). Goose
// refuses to operate on an unknown dialect, so surface that as a hard error
// early.
func NewRunner(db *sql.DB, dialect string) (*Runner, error) {
	return NewRunnerWithFS(db, dialect, migrations.FS)
}

// NewRunnerWithFS is the testable form of NewRunner — callers supply the
// migration source explicitly. Production code reaches for NewRunner; tests
// pass a synthetic fstest.MapFS so they don't depend on the real schema
// (which uses Postgres-only types like BIGSERIAL).
func NewRunnerWithFS(db *sql.DB, dialect string, fsys fs.FS) (*Runner, error) {
	if db == nil {
		return nil, errors.New("migration.NewRunner: nil db")
	}
	if dialect == "" {
		return nil, errors.New("migration.NewRunner: empty dialect")
	}
	if fsys == nil {
		return nil, errors.New("migration.NewRunner: nil fs")
	}

	// goose.SetBaseFS / SetDialect are package-level state. Re-calling per
	// Runner is fine — last call wins, and Runner construction is rare (once
	// per process at boot, once per test).
	goose.SetBaseFS(fsys)
	if err := goose.SetDialect(dialect); err != nil {
		return nil, fmt.Errorf("migration.NewRunner: set dialect %q: %w", dialect, err)
	}

	return &Runner{db: db, dialect: dialect, fsys: fsys}, nil
}

// Up applies all pending migrations. Idempotent: calling it twice in a row is
// a no-op once the schema is at the latest version. WithAllowMissing tolerates
// gaps in version history that may exist from manual ops — matches the
// server's policy.
func (r *Runner) Up(ctx context.Context) error {
	if err := goose.UpContext(ctx, r.db, ".", goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("migration.Up: %w", err)
	}
	return nil
}

// Down rolls back all migrations. Intended for tests only — production callers
// should never invoke this against a live database.
func (r *Runner) Down(ctx context.Context) error {
	if err := goose.DownContext(ctx, r.db, "."); err != nil {
		return fmt.Errorf("migration.Down: %w", err)
	}
	return nil
}

// Version returns the current schema version recorded in goose's version
// table (0 if no migrations applied yet).
func (r *Runner) Version() (int64, error) {
	v, err := goose.GetDBVersion(r.db)
	if err != nil {
		return 0, fmt.Errorf("migration.Version: %w", err)
	}
	if v < 0 {
		return 0, errors.New("migration.Version: negative version returned")
	}
	return v, nil
}
