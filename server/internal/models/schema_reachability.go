// Package models — how to ask "does this table/column exist?" without lying.
//
// # The rule this file exists to enforce
//
// A guard may only skip work the caller is allowed not to do, it must resolve
// the object the statement it guards will resolve, and it must never turn "I
// could not find out" into "there is nothing to do".
//
// gorm's Migrator().HasTable / HasColumn break the last two clauses on
// PostgreSQL and cannot be used to gate a write:
//
//  1. Both resolve against CURRENT_SCHEMA() (driver/postgres migrator.go), i.e.
//     the FIRST existing schema in search_path. The statement they guard is
//     unqualified and resolves against the WHOLE search_path. With the
//     PostgreSQL default `search_path = "$user", public` those two answers
//     differ the moment a schema named after the connecting role exists: the
//     probe reports "no table" while the very next INSERT writes to
//     public.<table> perfectly happily. Nothing needs to be misconfigured for
//     this — it is one `CREATE SCHEMA costrict` away on the deployment this
//     code runs against today.
//
//  2. Both return a bare bool and discard the query error, so a probe that
//     could not run at all is indistinguishable from a definite "absent".
//
// This is not hypothetical and it is not new: cmd/migrate's countByItem
// records the same divergence silently reporting zero favorites for the whole
// fleet under a non-default search_path, and plugin_flatten_apply's
// requirePluginFlattenTombstoneCause already switched to to_regclass for
// exactly this reason. This file generalises that fix so the next guard does
// not have to rediscover it.
package models

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ErrSchemaObjectUnreachable reports that an object a write requires cannot be
// resolved from this connection's search_path.
var ErrSchemaObjectUnreachable = errors.New("required schema object is not reachable")

// TableReachable reports whether an unqualified statement against model's table
// would resolve on this connection, and returns an error when the question
// could not be answered.
//
// Use it for genuinely optional data — a table the caller is allowed to find
// missing. For a table the caller's contract requires, use RequireTable: a
// bool answer invites the caller to write `if !ok { return nil }`, which is the
// shape this whole file exists to stop.
func TableReachable(db *gorm.DB, model any) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("%w: nil database handle", ErrSchemaObjectUnreachable)
	}
	table, err := tableNameOf(db, model)
	if err != nil {
		return false, err
	}
	if !isPostgres(db) {
		// SQLite (the unit fixtures) has a single unqualified namespace, so
		// there is no resolution to diverge from and the driver's probe is
		// exact. Kept as a delegation rather than hand-written sqlite_master
		// SQL so this stays correct if the fixtures ever move dialect.
		return db.Migrator().HasTable(model), nil
	}
	var reachable bool
	// to_regclass resolves the identifier the way the statement does: through
	// the whole search_path. It returns NULL (not an error) for an absent
	// object, so "absent" and "the probe failed" stay distinguishable.
	if err := db.Raw(`SELECT to_regclass(?) IS NOT NULL`, table).Scan(&reachable).Error; err != nil {
		return false, fmt.Errorf("%w: probe table %s: %w", ErrSchemaObjectUnreachable, table, err)
	}
	return reachable, nil
}

// RequireTable returns nil only when model's table is provably reachable.
//
// purpose names the work that cannot be done without it and is quoted back in
// the error, because the operator reading it needs to know what silently did
// not happen, not just which relation is missing.
//
// Deliberately not memoised. Measured at 0.23ms per probe against the local
// PostgreSQL, so the heaviest caller in the tree — `migrate flatten-plugins`
// with four probes on each of 6738 rows — pays about six seconds once. A cache
// would buy that back in exchange for a stale-positive failure mode, which is
// the wrong trade for the one function whose whole job is not to answer from
// memory. (The column probes in capability_item_git_guard.go do memoise, on a
// hot per-write path, and cache only the positive answer for the same reason.)
func RequireTable(db *gorm.DB, model any, purpose string) error {
	reachable, err := TableReachable(db, model)
	if err != nil {
		return err
	}
	if reachable {
		return nil
	}
	table, nameErr := tableNameOf(db, model)
	if nameErr != nil {
		table = fmt.Sprintf("%T", model)
	}
	return fmt.Errorf(
		"%w: %s is not reachable from this connection's search_path, so %s cannot be recorded\n"+
			"  why it matters: the operation that needs it is refused rather than completed silently — a caller that\n"+
			"  saw success here would report work as done that no row records\n"+
			"  fix: run the schema migrations against this database (`DATABASE_URL=... go run ./cmd/migrate`, or your\n"+
			"  deployment's migrate job) and confirm the connection's search_path reaches the schema they created",
		ErrSchemaObjectUnreachable, table, purpose)
}

// ColumnReachable reports whether model's field resolves on the table an
// unqualified statement would reach.
//
// Unlike TableReachable this one has a legitimate "absent means skip" caller:
// a schema predating a column cannot hold the state the guard enforces, so
// there is nothing to enforce. The value here is only that the answer is about
// the right table and that a failed probe is not silently a "no".
func ColumnReachable(db *gorm.DB, model any, column string) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("%w: nil database handle", ErrSchemaObjectUnreachable)
	}
	if !isPostgres(db) {
		return db.Migrator().HasColumn(model, column), nil
	}
	table, err := tableNameOf(db, model)
	if err != nil {
		return false, err
	}
	var count int64
	// attrelid = to_regclass(...) yields no rows when the table itself is
	// unreachable, so a missing table reads as a missing column rather than an
	// error — which is what every caller of this wants.
	if err := db.Raw(`
		SELECT count(*) FROM pg_attribute
		 WHERE attrelid = to_regclass(?)
		   AND attname = ?
		   AND attnum > 0
		   AND NOT attisdropped`, table, column).Scan(&count).Error; err != nil {
		return false, fmt.Errorf("%w: probe column %s.%s: %w", ErrSchemaObjectUnreachable, table, column, err)
	}
	return count > 0, nil
}

// tableNameOf resolves model to the identifier a statement would emit for it,
// honouring TableName()/NamingStrategy exactly as gorm does, so the probe can
// never drift from the statement.
func tableNameOf(db *gorm.DB, model any) (string, error) {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil {
		return "", fmt.Errorf("%w: resolve table name for %T: %w", ErrSchemaObjectUnreachable, model, err)
	}
	if stmt.Table == "" {
		return "", fmt.Errorf("%w: %T has no table name", ErrSchemaObjectUnreachable, model)
	}
	return stmt.Table, nil
}

func isPostgres(db *gorm.DB) bool {
	return db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres"
}
