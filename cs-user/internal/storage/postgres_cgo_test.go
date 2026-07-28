//go:build cgo

// Ping/SQLDB behaviour is exercised through an in-memory sqlite handle —
// mattn/go-sqlite3 requires cgo, so these tests are tagged out when CGO_ENABLED=0
// (e.g. on minimal CI runners without a C toolchain, or on Windows hosts
// where the local cgo toolchain is unavailable). The Linux dev/CI flow has
// cgo on by default, so coverage is preserved where it matters.

package storage

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestPool_Ping_OK wires a Pool over an in-memory sqlite DB and verifies Ping
// returns nil. Exercises the real sql.DB.PingContext code path without
// requiring a running Postgres.
func TestPool_Ping_OK(t *testing.T) {
	db := openSQLite(t)
	p := &Pool{sql: db}
	if err := p.Ping(context.Background()); err != nil {
		t.Errorf("Ping healthy sqlite: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Errorf("Ready healthy sqlite: %v", err)
	}
}

func TestPool_Ping_ClosedDBFails(t *testing.T) {
	db := openSQLite(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	p := &Pool{sql: db}
	if err := p.Ping(context.Background()); err == nil {
		t.Error("Ping on closed DB should fail")
	}
}

func TestPool_Ping_NilPoolErrors(t *testing.T) {
	var p *Pool
	if err := p.Ping(context.Background()); err == nil {
		t.Error("Ping on nil Pool should error")
	}
}

func TestPool_Ready_NilPoolErrors(t *testing.T) {
	var p *Pool
	if err := p.Ready(); err == nil {
		t.Error("Ready on nil Pool should error")
	}
}

func TestPool_SQLDB(t *testing.T) {
	db := openSQLite(t)
	p := &Pool{sql: db}
	got, err := p.SQLDB()
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	if got != db {
		t.Error("SQLDB did not return the wired *sql.DB")
	}

	var np *Pool
	if _, err := np.SQLDB(); err == nil {
		t.Error("nil Pool.SQLDB should error")
	}
}
