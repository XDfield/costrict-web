//go:build cgo

// Phase A migration smoke test: verifies the goose runner picks up files
// with the Phase A naming convention (20260716150000_create_employment_identities.sql
// and 20260716160000_create_tenant_configs.sql) and applies them cleanly.
//
// This is a plumbing smoke test, not a schema-fidelity test: sqlite can't
// enforce Postgres-specific types (BIGSERIAL, timestamptz, jsonb) or partial
// unique indexes (`WHERE deleted_at IS NULL`). The synthetic DDL mirrors the
// shape (table + indexes + a column default) so the runner's file-discovery,
// version-tracking, and Up/Down transitions are exercised end-to-end.
// Schema fidelity against Postgres is verified out-of-band via the pre-deploy
// Helm migrate job against a real database.

package migration

import (
	"context"
	"testing"
	"testing/fstest"
)

// Phase A synthetic migrations: same file names as production, sqlite-friendly
// DDL inside. The partial-unique-index clause is intentionally omitted — see
// file header for rationale.
const (
	phaseAMig1 = `-- +goose Up
CREATE TABLE employment_identities (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_subject_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	sync_status TEXT NOT NULL DEFAULT 'fresh'
);
CREATE INDEX idx_employment_identities_user_subject_id ON employment_identities(user_subject_id);
-- +goose Down
DROP INDEX idx_employment_identities_user_subject_id;
DROP TABLE employment_identities;
`
	phaseAMig2 = `-- +goose Up
CREATE TABLE tenant_configs (
	tenant_id TEXT PRIMARY KEY,
	config_yaml TEXT NOT NULL DEFAULT '{}',
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose Down
DROP TABLE tenant_configs;
`
)

func phaseAFS() fstest.MapFS {
	return fstest.MapFS{
		"20260716150000_create_employment_identities.sql": {Data: []byte(phaseAMig1)},
		"20260716160000_create_tenant_configs.sql":        {Data: []byte(phaseAMig2)},
	}
}

func TestRunner_PhaseA_UpAppliesBothTables(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", phaseAFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	for _, tbl := range []string{"employment_identities", "tenant_configs"} {
		var name string
		err = db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after Phase A Up: %v", tbl, err)
		}
	}
}

func TestRunner_PhaseA_VersionAdvances(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", phaseAFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	v, err := r.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	const want int64 = 20260716160000
	if v != want {
		t.Errorf("post-Up version: got %d want %d", v, want)
	}
}

func TestRunner_PhaseA_DownReverts(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", phaseAFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// tenant_configs is the latest migration — Down reverts it first.
	if err := r.Down(context.Background()); err != nil {
		t.Fatalf("Down (tenant_configs): %v", err)
	}

	var name string
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='tenant_configs'",
	).Scan(&name)
	if err == nil {
		t.Error("tenant_configs should be gone after Down")
	}

	// employment_identities must still be present.
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='employment_identities'",
	).Scan(&name)
	if err != nil {
		t.Errorf("employment_identities missing after single Down (should still exist): %v", err)
	}
}
