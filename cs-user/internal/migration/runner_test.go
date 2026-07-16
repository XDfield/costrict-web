package migration

import (
	"database/sql"
	"testing"
)

// TestNewRunner_RejectsNilDB guards against nil-db footguns — goose calls
// would panic later with a confusing message otherwise.
func TestNewRunner_RejectsNilDB(t *testing.T) {
	if _, err := NewRunner(nil, "postgres"); err == nil {
		t.Error("NewRunner(nil, ...) should error")
	}
}

// TestNewRunner_RejectsEmptyDialect ensures the dialect contract is enforced
// before any goose state is mutated.
func TestNewRunner_RejectsEmptyDialect(t *testing.T) {
	// Pass a non-nil db shim — we only care the constructor short-circuits.
	if _, err := NewRunner(&sql.DB{}, ""); err == nil {
		t.Error("NewRunner(db, '') should error")
	}
}

func TestNewRunner_RejectsNilFS(t *testing.T) {
	if _, err := NewRunnerWithFS(&sql.DB{}, "sqlite3", nil); err == nil {
		t.Error("NewRunnerWithFS(..., nil) should error")
	}
}

// TestNewRunner_RejectsUnknownDialect confirms goose's dialect allow-list
// surfaces bad input as an error rather than a deferred runtime failure.
func TestNewRunner_RejectsUnknownDialect(t *testing.T) {
	if _, err := NewRunner(&sql.DB{}, "definitely-not-a-real-dialect"); err == nil {
		t.Error("expected error for unknown dialect")
	}
}
