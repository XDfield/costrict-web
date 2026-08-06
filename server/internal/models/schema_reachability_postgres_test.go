package models

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The property under test is a PostgreSQL name-resolution rule, so it can only
// be tested on PostgreSQL. SQLite has one unqualified namespace and therefore
// nothing to diverge.
//
// The shape reproduced here is not exotic. PostgreSQL's default search_path is
// `"$user", public`: the moment a schema named after the connecting role
// exists, CURRENT_SCHEMA() stops being `public` while every unqualified
// statement still reaches `public`. `front`/`back` below is that shape with
// deterministic names.

// newDivergentSchemaDB returns a connection whose search_path reaches `back`
// but whose CURRENT_SCHEMA() is the empty `front`.
func newDivergentSchemaDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL schema reachability test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	stamp := time.Now().UnixNano()
	front := fmt.Sprintf("reach_front_%d", stamp)
	back := fmt.Sprintf("reach_back_%d", stamp)

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	for _, schema := range []string{front, back} {
		if err := admin.Exec("CREATE SCHEMA " + quoteSchema(schema)).Error; err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
	}
	t.Cleanup(func() {
		for _, schema := range []string{front, back} {
			_ = admin.Exec("DROP SCHEMA " + quoteSchema(schema) + " CASCADE").Error
		}
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// The table is created in `back` explicitly: an unqualified CREATE would
	// land in `front`, which is the one schema this fixture needs to stay empty.
	if err := admin.Exec("CREATE TABLE " + quoteSchema(back) + `.capability_items (
		id TEXT PRIMARY KEY,
		status TEXT NOT NULL DEFAULT 'active',
		content_backend VARCHAR(16) NOT NULL DEFAULT 'db'
	)`).Error; err != nil {
		t.Fatalf("create table in %s: %v", back, err)
	}

	db := openWithSearchPath(t, parsed, front+","+back)
	var current string
	if err := db.Raw("SELECT current_schema()").Scan(&current).Error; err != nil {
		t.Fatalf("read current_schema(): %v", err)
	}
	if current != front {
		t.Fatalf("fixture did not reproduce the divergence: current_schema() = %q, want %q", current, front)
	}
	return db, back
}

func openWithSearchPath(t *testing.T, base *url.URL, searchPath string) *gorm.DB {
	t.Helper()
	parsed := *base
	query := parsed.Query()
	query.Set("search_path", searchPath)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL with search_path=%s: %v", searchPath, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func quoteSchema(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// TestTableReachable_AnswersAboutTheTableTheStatementReaches is the root-cause
// test for the whole file: gorm's Migrator().HasTable reports "absent" for a
// table an unqualified INSERT writes to without complaint.
//
// It asserts the disagreement rather than only the fixed answer, so if a future
// gorm release makes HasTable resolve through search_path the assertion fails
// and somebody re-reads the guards instead of inheriting a stale rationale.
func TestTableReachable_AnswersAboutTheTableTheStatementReaches(t *testing.T) {
	db, back := newDivergentSchemaDB(t)

	if db.Migrator().HasTable(&CapabilityItem{}) {
		t.Fatal("gorm's HasTable now resolves through search_path; re-read the guards in schema_reachability.go " +
			"— they were written around the opposite behaviour")
	}

	// What the statement actually does, which is the only thing that matters.
	if err := db.Exec(`INSERT INTO capability_items (id) VALUES ('reach-1')`).Error; err != nil {
		t.Fatalf("an unqualified INSERT must reach %s.capability_items: %v", back, err)
	}

	reachable, err := TableReachable(db, &CapabilityItem{})
	if err != nil {
		t.Fatalf("TableReachable: %v", err)
	}
	if !reachable {
		t.Fatal("TableReachable said the table is absent, but the INSERT above wrote a row to it")
	}
	if err := RequireTable(db, &CapabilityItem{}, "the test row"); err != nil {
		t.Fatalf("RequireTable refused a table the INSERT reaches: %v", err)
	}
}

func TestColumnReachable_AnswersAboutTheTableTheStatementReaches(t *testing.T) {
	db, _ := newDivergentSchemaDB(t)

	if db.Migrator().HasColumn(&CapabilityItem{}, "content_backend") {
		t.Fatal("gorm's HasColumn now resolves through search_path; re-read capabilityItemsHaveContentBackend")
	}

	present, err := ColumnReachable(db, &CapabilityItem{}, "content_backend")
	if err != nil {
		t.Fatalf("ColumnReachable: %v", err)
	}
	if !present {
		t.Fatal("ColumnReachable missed a column on the table the statement reaches; " +
			"the Git-backed write/delete guard would silently switch itself off here")
	}

	absent, err := ColumnReachable(db, &CapabilityItem{}, "column_that_does_not_exist")
	if err != nil {
		t.Fatalf("probing an absent column must not error: %v", err)
	}
	if absent {
		t.Fatal("ColumnReachable invented a column")
	}
}

// TestRequireTable_RefusesWhatItCannotFind pins the other half of the contract:
// a genuinely unreachable table is an error, never a quiet zero.
func TestRequireTable_RefusesWhatItCannotFind(t *testing.T) {
	db, _ := newDivergentSchemaDB(t)

	reachable, err := TableReachable(db, &CapabilitySyncTombstone{})
	if err != nil {
		t.Fatalf("TableReachable on an absent table must answer, not error: %v", err)
	}
	if reachable {
		t.Fatal("TableReachable claimed a table this fixture never created")
	}

	err = RequireTable(db, &CapabilitySyncTombstone{}, "the removal instruction for every holder")
	if err == nil {
		t.Fatal("RequireTable returned nil for an unreachable table")
	}
	if !strings.Contains(err.Error(), "capability_sync_tombstones") {
		t.Errorf("error must name the relation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "the removal instruction for every holder") {
		t.Errorf("error must name the work that did not happen, got: %v", err)
	}
}
