package itemdelete

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

// The cascade on a connection whose search_path reaches the tables but whose
// CURRENT_SCHEMA() does not name them.
//
// # Why this shape and why it matters here
//
// PostgreSQL's stock `search_path = "$user", public` produces this divergence
// the moment a schema named after the connecting role exists. gorm's
// Migrator().HasTable answers about CURRENT_SCHEMA() only, so every probe in
// this cascade used to report "table absent" while the statements they guard
// wrote to those very tables. The consequence is not symmetric across steps:
//
//   - step 1 (tombstones) wrote nothing and returned success — the capability
//     stays installed on every holder's device forever, which is F-27;
//   - steps 2-4 skipped every dependent delete while step 5 deleted the item
//     row unguarded, leaving sub-skills pointing at a missing parent and
//     favorites/receipts/versions pointing at a missing item.
//
// One deletion under a divergent search_path exercises all of it.

var reachabilityFixtureTables = []string{
	`CREATE TABLE capability_items (
		id TEXT PRIMARY KEY, registry_id TEXT, repo_id TEXT, slug TEXT,
		item_type TEXT, name TEXT, status TEXT NOT NULL DEFAULT 'active',
		forked_from_item_id TEXT, forked_from_owner_id TEXT, parent_plugin_id TEXT,
		created_by TEXT, created_at TIMESTAMPTZ, updated_at TIMESTAMPTZ
	)`,
	`CREATE TABLE capability_versions (id TEXT PRIMARY KEY, item_id TEXT)`,
	`CREATE TABLE capability_assets (id TEXT PRIMARY KEY, item_id TEXT)`,
	`CREATE TABLE capability_artifacts (id TEXT PRIMARY KEY, item_id TEXT)`,
	`CREATE TABLE capability_version_assets (id TEXT PRIMARY KEY, version_id TEXT)`,
	`CREATE TABLE item_favorites (id TEXT PRIMARY KEY, item_id TEXT, user_id TEXT)`,
	`CREATE TABLE item_tags (id TEXT PRIMARY KEY, item_id TEXT, tag_id TEXT, source TEXT NOT NULL DEFAULT 'legacy')`,
	`CREATE TABLE behavior_logs (id TEXT PRIMARY KEY, item_id TEXT, action_type TEXT)`,
	`CREATE TABLE scan_jobs (id TEXT PRIMARY KEY, item_id TEXT)`,
	`CREATE TABLE security_scans (id TEXT PRIMARY KEY, item_id TEXT)`,
	`CREATE TABLE mcp_user_configs (id TEXT PRIMARY KEY, user_id TEXT, item_id TEXT, field_values TEXT DEFAULT '{}')`,
	`CREATE TABLE item_distributions (id TEXT PRIMARY KEY, item_id TEXT, status TEXT DEFAULT 'active')`,
	`CREATE TABLE item_distribution_receipts (
		id TEXT PRIMARY KEY, distribution_id TEXT, user_id TEXT NOT NULL,
		receipt_status TEXT DEFAULT 'unread', forked_item_id TEXT
	)`,
	`CREATE TABLE capability_sync_tombstones (
		id TEXT PRIMARY KEY, user_id TEXT NOT NULL, item_id TEXT NOT NULL,
		reason TEXT NOT NULL, lifecycle_reason TEXT, source TEXT NOT NULL,
		event_id TEXT NOT NULL UNIQUE, removed_at TIMESTAMPTZ NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE(user_id, item_id)
	)`,
}

// newDivergentCascadeDB builds the cascade's tables in `back` and returns a
// connection whose CURRENT_SCHEMA() is the empty `front`.
func newDivergentCascadeDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL cascade reachability test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	stamp := time.Now().UnixNano()
	front := fmt.Sprintf("cascade_front_%d", stamp)
	back := fmt.Sprintf("cascade_back_%d", stamp)
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	for _, schema := range []string{front, back} {
		if err := admin.Exec("CREATE SCHEMA " + quote(schema)).Error; err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
	}
	t.Cleanup(func() {
		for _, schema := range []string{front, back} {
			_ = admin.Exec("DROP SCHEMA " + quote(schema) + " CASCADE").Error
		}
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	open := func(searchPath string) *gorm.DB {
		copied := *parsed
		query := copied.Query()
		query.Set("search_path", searchPath)
		copied.RawQuery = query.Encode()
		db, err := gorm.Open(postgres.Open(copied.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatalf("open PostgreSQL with search_path=%s: %v", searchPath, err)
		}
		return db
	}

	builder := open(back)
	for _, ddl := range reachabilityFixtureTables {
		if err := builder.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture table: %v\nSQL: %s", err, ddl)
		}
	}
	if sqlDB, err := builder.DB(); err == nil {
		_ = sqlDB.Close()
	}

	db := open(front + "," + back)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	var current string
	if err := db.Raw("SELECT current_schema()").Scan(&current).Error; err != nil {
		t.Fatalf("read current_schema(): %v", err)
	}
	if current != front {
		t.Fatalf("fixture did not reproduce the divergence: current_schema() = %q, want %q", current, front)
	}
	return db
}

// TestCascadeDelete_CompletesUnderDivergentSearchPath deletes one plugin on a
// connection where every probe used to answer "table absent", and asserts the
// cascade did the whole job rather than only its unguarded last step.
func TestCascadeDelete_CompletesUnderDivergentSearchPath(t *testing.T) {
	db := newDivergentCascadeDB(t)

	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('P','plugin','Plug','active','u1')`)
	mustExec(t, db, `INSERT INTO capability_items (id, item_type, name, status, created_by, parent_plugin_id) VALUES ('S1','skill','Sub','active','u1','P')`)
	mustExec(t, db, `INSERT INTO capability_versions (id, item_id) VALUES ('pv','P')`)
	mustExec(t, db, `INSERT INTO capability_version_assets (id, version_id) VALUES ('pva','pv')`)
	mustExec(t, db, `INSERT INTO item_favorites (id, item_id, user_id) VALUES ('pf','P','u9')`)
	mustExec(t, db, `INSERT INTO item_distributions (id, item_id) VALUES ('pd','P')`)
	mustExec(t, db, `INSERT INTO item_distribution_receipts (id, distribution_id, user_id) VALUES ('pdr','pd','u8')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "P")
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	// The reason the whole cascade exists: both holders must be told.
	holders := map[string]bool{}
	rows, err := db.Raw(`SELECT user_id FROM capability_sync_tombstones WHERE item_id = 'P'`).Rows()
	if err != nil {
		t.Fatalf("read tombstones: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			t.Fatalf("scan tombstone: %v", err)
		}
		holders[user] = true
	}
	if !holders["u9"] || !holders["u8"] {
		t.Errorf("tombstoned holders = %v, want both the favorite (u9) and the receipt (u8): "+
			"a holder that is not told keeps the capability installed forever", holders)
	}

	// And no half-completed cascade: the item is gone, so nothing may still
	// point at it.
	for _, check := range []struct {
		what string
		sql  string
	}{
		{"the plugin row", `SELECT COUNT(*) FROM capability_items WHERE id='P'`},
		{"its bundled sub-skill", `SELECT COUNT(*) FROM capability_items WHERE id='S1'`},
		{"its versions", `SELECT COUNT(*) FROM capability_versions WHERE item_id='P'`},
		{"its version assets", `SELECT COUNT(*) FROM capability_version_assets WHERE version_id='pv'`},
		{"its favorites", `SELECT COUNT(*) FROM item_favorites WHERE item_id='P'`},
		{"its distributions", `SELECT COUNT(*) FROM item_distributions WHERE item_id='P'`},
		{"its distribution receipts", `SELECT COUNT(*) FROM item_distribution_receipts WHERE distribution_id='pd'`},
	} {
		if n := count(t, db, check.sql); n != 0 {
			t.Errorf("%s survived the cascade (count=%d); the item row is gone, so this is orphaned data", check.what, n)
		}
	}
}
