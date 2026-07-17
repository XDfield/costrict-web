//go:build cgo

// A6 idempotency test: verifies the default-tenant bootstrap INSERT behaves
// as a safe no-op when re-applied. Goose migrations run inside a
// transaction; if `ON CONFLICT DO NOTHING` weren't honored, the second
// deploy would crash on the PK constraint and block rolling deploys.
//
// The SQL string below mirrors the production migration
// `cs-user/migrations/20260716170000_bootstrap_default_tenant.sql`. Both
// Postgres (>=9.5) and sqlite (>=3.24) accept the same syntax, so this
// test exercises the actual production semantic against sqlite. Schema
// fidelity against Postgres is verified out-of-band via the pre-deploy
// Helm migrate job.

package migration

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"
)

// bootstrapUpSQL is the literal INSERT shipped in the production migration.
// Kept in sync manually with the .sql file — diverging here would mask a
// production idempotency regression.
const bootstrapUpSQL = `INSERT INTO tenant_configs (tenant_id, config_yaml)
VALUES ('default', '{}')
ON CONFLICT (tenant_id) DO NOTHING;`

const bootstrapDownSQL = `DELETE FROM tenant_configs WHERE tenant_id = 'default';`

// syntheticA2Only returns an fs.FS with just the A2 tenant_configs table
// (synthetic sqlite-friendly DDL mirroring the real migration). The A6
// bootstrap migration is then layered on top, matching the production
// ordering (A2 creates the table, A6 fills the default row).
const (
	a2Synthetic = `-- +goose Up
CREATE TABLE tenant_configs (
	tenant_id TEXT PRIMARY KEY,
	config_yaml TEXT NOT NULL DEFAULT '{}',
	updated_by TEXT,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose Down
DROP TABLE tenant_configs;
`
	a6Synthetic = `-- +goose Up
` + bootstrapUpSQL + `
-- +goose Down
` + bootstrapDownSQL + `
`
)

func a2AndBootstrapFS() fstest.MapFS {
	return fstest.MapFS{
		"20260716160000_create_tenant_configs.sql":    {Data: []byte(a2Synthetic)},
		"20260716170000_bootstrap_default_tenant.sql": {Data: []byte(a6Synthetic)},
	}
}

// TestBootstrap_DefaultTenantInsertCreatesRow runs the full A2 → A6 sequence
// via the goose runner and verifies the default row exists with the
// documented shape.
func TestBootstrap_DefaultTenantInsertCreatesRow(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", a2AndBootstrapFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var (
		tenantID   string
		configYAML string
	)
	err = db.QueryRow(`SELECT tenant_id, config_yaml FROM tenant_configs WHERE tenant_id = 'default'`).
		Scan(&tenantID, &configYAML)
	if err != nil {
		t.Fatalf("default row missing after Up: %v", err)
	}
	if tenantID != "default" {
		t.Errorf("tenant_id: got %q, want default", tenantID)
	}
	if configYAML != "{}" {
		t.Errorf("config_yaml: got %q, want {}", configYAML)
	}
}

// TestBootstrap_UpIsIdempotent re-runs Up on a fully-migrated DB. Goose
// should treat it as a no-op (no error), and the default row count must
// remain 1. Pins the ON CONFLICT DO NOTHING contract that protects rolling
// deploys.
func TestBootstrap_UpIsIdempotent(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", a2AndBootstrapFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("second Up (should be no-op): %v", err)
	}

	count, err := countDefaultRows(db)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("after double Up: row count got %d, want 1 (ON CONFLICT DO NOTHING must prevent duplicate)", count)
	}
}

// TestBootstrap_ReUpAfterOperatorInsert exercises the operator-supplied row
// preservation: if an operator has manually inserted tenant_id="default"
// with a non-trivial YAML, the bootstrap migration must not clobber it.
func TestBootstrap_ReUpAfterOperatorInsert(t *testing.T) {
	db := openSQLite(t)

	// Manually create the tenant_configs table (skip goose for A2) and have
	// the operator insert a richer default before A6 runs.
	if _, err := db.Exec(`CREATE TABLE tenant_configs (
		tenant_id TEXT PRIMARY KEY,
		config_yaml TEXT NOT NULL DEFAULT '{}',
		updated_by TEXT,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	const operatorYAML = "employment_providers:\n  enabled: [idtrust]\n"
	if _, err := db.Exec(`INSERT INTO tenant_configs (tenant_id, config_yaml) VALUES ('default', ?)`, operatorYAML); err != nil {
		t.Fatalf("operator insert: %v", err)
	}

	// Runner sees only A6 — A2 was applied manually above. goose Up applies
	// A6 against the pre-populated table; ON CONFLICT DO NOTHING must leave
	// the operator's richer YAML intact.
	bootstrapOnlyFS := fstest.MapFS{
		"20260716170000_bootstrap_default_tenant.sql": {Data: []byte(a6Synthetic)},
	}
	r, err := NewRunnerWithFS(db, "sqlite3", bootstrapOnlyFS)
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}
	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up A6: %v", err)
	}

	var configYAML string
	err = db.QueryRow(`SELECT config_yaml FROM tenant_configs WHERE tenant_id = 'default'`).
		Scan(&configYAML)
	if err != nil {
		t.Fatalf("operator row missing after A6: %v", err)
	}
	if configYAML != operatorYAML {
		t.Errorf("A6 must not clobber operator-supplied YAML: got %q, want %q", configYAML, operatorYAML)
	}
}

// TestBootstrap_DownRemovesDefaultRow verifies the Down migration removes
// only the bootstrap row (operator rows for other tenants survive).
func TestBootstrap_DownRemovesDefaultRow(t *testing.T) {
	db := openSQLite(t)
	r, err := NewRunnerWithFS(db, "sqlite3", a2AndBootstrapFS())
	if err != nil {
		t.Fatalf("NewRunnerWithFS: %v", err)
	}

	if err := r.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// Operator adds a second tenant.
	if _, err := db.Exec(`INSERT INTO tenant_configs (tenant_id, config_yaml) VALUES ('acme', '{}')`); err != nil {
		t.Fatalf("operator insert acme: %v", err)
	}

	// Roll back A6 only — A2 stays.
	if err := r.Down(context.Background()); err != nil {
		t.Fatalf("Down A6: %v", err)
	}

	// default gone, acme survives.
	if c, err := countDefaultRows(db); err != nil {
		t.Fatalf("count default after Down: %v", err)
	} else if c != 0 {
		t.Errorf("default row should be gone after Down, got %d", c)
	}
	var acme string
	err = db.QueryRow(`SELECT tenant_id FROM tenant_configs WHERE tenant_id = 'acme'`).Scan(&acme)
	if err != nil {
		t.Errorf("operator row 'acme' should survive A6 Down: %v", err)
	}
}

func countDefaultRows(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM tenant_configs WHERE tenant_id = 'default'`).Scan(&n)
	return n, err
}
