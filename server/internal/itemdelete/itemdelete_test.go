package itemdelete

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupFullSchema builds an in-memory sqlite DB with the full set of tables the
// cascade touches (including the distribution + mcp-config tables that the
// adminitem unit fixture omits), so the orphan-cleanup paths are exercised.
func setupFullSchema(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	stmts := []string{
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY, registry_id TEXT, repo_id TEXT, slug TEXT,
			item_type TEXT, name TEXT, status TEXT DEFAULT 'active',
			forked_from_item_id TEXT, forked_from_owner_id TEXT, parent_plugin_id TEXT,
			created_by TEXT, created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE capability_versions (id TEXT PRIMARY KEY, item_id TEXT)`,
		`CREATE TABLE capability_assets (id TEXT PRIMARY KEY, item_id TEXT)`,
		`CREATE TABLE capability_artifacts (id TEXT PRIMARY KEY, item_id TEXT)`,
		`CREATE TABLE capability_version_assets (id TEXT PRIMARY KEY, version_id TEXT)`,
		`CREATE TABLE item_favorites (id TEXT PRIMARY KEY, item_id TEXT, user_id TEXT)`,
		`CREATE TABLE item_tags (id TEXT PRIMARY KEY, item_id TEXT, tag_id TEXT)`,
		`CREATE TABLE behavior_logs (id TEXT PRIMARY KEY, item_id TEXT, action_type TEXT)`,
		`CREATE TABLE scan_jobs (id TEXT PRIMARY KEY, item_id TEXT)`,
		`CREATE TABLE security_scans (id TEXT PRIMARY KEY, item_id TEXT)`,
		`CREATE TABLE mcp_user_configs (id TEXT PRIMARY KEY, user_id TEXT, item_id TEXT, field_values TEXT DEFAULT '{}')`,
		`CREATE TABLE item_distributions (id TEXT PRIMARY KEY, item_id TEXT)`,
		`CREATE TABLE item_distribution_receipts (id TEXT PRIMARY KEY, distribution_id TEXT, forked_item_id TEXT)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func count(t *testing.T, db *gorm.DB, sql string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(sql).Scan(&n).Error; err != nil {
		t.Fatalf("count query %q: %v", sql, err)
	}
	return n
}

func TestCascadeDelete_PluginSubskillAndOrphans(t *testing.T) {
	db := setupFullSchema(t)

	// Plugin P, its bundled sub-skill S1, and another user's fork F of P.
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('P','plugin','Plug','active','u1')`)
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by, parent_plugin_id) VALUES ('S1','skill','Sub','active','u1','P')`)
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by, forked_from_item_id, forked_from_owner_id) VALUES ('F','plugin','Fork','active','u2','P','u1')`)

	// P's dependents across every cascade table.
	db.Exec(`INSERT INTO capability_versions (id, item_id) VALUES ('pv','P')`)
	db.Exec(`INSERT INTO capability_version_assets (id, version_id) VALUES ('pva','pv')`)
	db.Exec(`INSERT INTO capability_assets (id, item_id) VALUES ('pa','P')`)
	db.Exec(`INSERT INTO capability_artifacts (id, item_id) VALUES ('part','P')`)
	db.Exec(`INSERT INTO item_favorites (id, item_id, user_id) VALUES ('pf','P','u9')`)
	db.Exec(`INSERT INTO item_tags (id, item_id, tag_id) VALUES ('pt','P','t1')`)
	db.Exec(`INSERT INTO behavior_logs (id, item_id, action_type) VALUES ('pb','P','view')`)
	db.Exec(`INSERT INTO scan_jobs (id, item_id) VALUES ('psj','P')`)
	db.Exec(`INSERT INTO security_scans (id, item_id) VALUES ('pss','P')`)
	db.Exec(`INSERT INTO mcp_user_configs (id, user_id, item_id) VALUES ('pmc','u9','P')`)
	db.Exec(`INSERT INTO item_distributions (id, item_id) VALUES ('pd','P')`)
	db.Exec(`INSERT INTO item_distribution_receipts (id, distribution_id, forked_item_id) VALUES ('pdr','pd','F')`)

	// S1's own dependents (must be cleaned when S1 is hard-deleted).
	db.Exec(`INSERT INTO capability_versions (id, item_id) VALUES ('sv','S1')`)
	db.Exec(`INSERT INTO mcp_user_configs (id, user_id, item_id) VALUES ('smc','u9','S1')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "P")
	}); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	// Plugin + sub-skill are gone; the fork survives.
	if n := count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id='P'`); n != 0 {
		t.Fatalf("plugin not deleted, count=%d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id='S1'`); n != 0 {
		t.Fatalf("sub-skill not hard-deleted, count=%d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id='F'`); n != 1 {
		t.Fatalf("fork must survive source deletion, count=%d", n)
	}

	// All of P's dependents cleared.
	for _, c := range []struct{ label, sql string }{
		{"versions", `SELECT COUNT(*) FROM capability_versions WHERE item_id='P'`},
		{"version_assets", `SELECT COUNT(*) FROM capability_version_assets WHERE version_id='pv'`},
		{"assets", `SELECT COUNT(*) FROM capability_assets WHERE item_id='P'`},
		{"artifacts", `SELECT COUNT(*) FROM capability_artifacts WHERE item_id='P'`},
		{"favorites", `SELECT COUNT(*) FROM item_favorites WHERE item_id='P'`},
		{"tags", `SELECT COUNT(*) FROM item_tags WHERE item_id='P'`},
		{"behavior_logs", `SELECT COUNT(*) FROM behavior_logs WHERE item_id='P'`},
		{"scan_jobs", `SELECT COUNT(*) FROM scan_jobs WHERE item_id='P'`},
		{"security_scans", `SELECT COUNT(*) FROM security_scans WHERE item_id='P'`},
		{"mcp_configs(P)", `SELECT COUNT(*) FROM mcp_user_configs WHERE item_id='P'`},
		{"distributions", `SELECT COUNT(*) FROM item_distributions WHERE item_id='P'`},
		{"receipts", `SELECT COUNT(*) FROM item_distribution_receipts WHERE id='pdr'`},
		{"subskill versions", `SELECT COUNT(*) FROM capability_versions WHERE item_id='S1'`},
		{"subskill mcp_configs", `SELECT COUNT(*) FROM mcp_user_configs WHERE item_id='S1'`},
	} {
		if n := count(t, db, c.sql); n != 0 {
			t.Fatalf("%s not cleaned, count=%d", c.label, n)
		}
	}
}

func TestCascadeDelete_SkipsMissingTablesAndCycles(t *testing.T) {
	db := setupFullSchema(t)
	// A pathological self-parent cycle must not loop forever.
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by, parent_plugin_id) VALUES ('A','plugin','A','active','u1','B')`)
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by, parent_plugin_id) VALUES ('B','plugin','B','active','u1','A')`)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return CascadeDelete(tx, "A")
	}); err != nil {
		t.Fatalf("cascade delete with cycle: %v", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM capability_items WHERE id IN ('A','B')`); n != 0 {
		t.Fatalf("expected both cyclic rows deleted, count=%d", n)
	}
}

func TestCascadeDeleteMany_DedupsAndSkips(t *testing.T) {
	db := setupFullSchema(t)
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('P','plugin','P','active','u1')`)
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by, parent_plugin_id) VALUES ('S','skill','S','active','u1','P')`)
	db.Exec(`INSERT INTO capability_items (id, item_type, name, status, created_by) VALUES ('X','skill','X','active','u1')`)

	var deleted, skipped []string
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		deleted, skipped, err = CascadeDeleteMany(tx, []string{"P", "S", "X", "ghost", "P"})
		return err
	}); err != nil {
		t.Fatalf("cascade delete many: %v", err)
	}
	// P deleted (cascades S); X deleted. S, ghost, duplicate P → skipped.
	if len(deleted) != 2 {
		t.Fatalf("expected 2 deleted (P,X), got %v", deleted)
	}
	if len(skipped) != 3 {
		t.Fatalf("expected 3 skipped (S,ghost,dup-P), got %v", skipped)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM capability_items`); n != 0 {
		t.Fatalf("expected all rows gone, count=%d", n)
	}
}
