package models

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ContentBackendDB / ContentBackendGit are the two values of
// capability_items.content_backend. The column is NOT NULL DEFAULT 'db', so
// every writer that does not know about Git backing silently produces a 'db'
// row — which is exactly why the guard below is default-deny rather than a
// per-entrypoint `if`.
const (
	ContentBackendDB  = "db"
	ContentBackendGit = "git"
)

// GitSyncBypassSetting is the session key the authoritative Git writer sets on
// its own transaction to opt out of the guard:
//
//	tx.Set(models.GitSyncBypassSetting, true)
//
// The marker is deliberately explicit, statement-local and greppable. It must
// never be replaced by implicit trust ("the caller lives in package worker"),
// because that trust cannot be audited.
const GitSyncBypassSetting = "capability:gitsync"

// ErrGitOwnedField is returned when a non-Git writer tries to change a column
// whose truth lives in the repository. Handlers map it to HTTP 409, following
// the named-sentinel pattern already used by DeleteGitServer.
var ErrGitOwnedField = errors.New("capability item field is owned by Git; change it through the repository")

// gitOwnedCapabilityColumns are the capability_items columns projected from a
// repository manifest (or from the binding that locates it). A Git-backed row
// accepts writes to these columns only from the Git sync writer.
//
// Deliberately NOT in the set, because both writers are legitimate:
//   - runtime counters: preview_count / install_count / favorite_count / experience_score
//   - platform state: status / security_status / last_scan_id / is_builtin
//   - placement: registry_id / repo_id (move/transfer get their own 409 later)
//   - descriptions / metadata: fork back-fills descriptions right after
//     creating the row, and metadata is merged rather than owned
var gitOwnedCapabilityColumns = []string{
	"catalog_entry_dir",
	"category",
	"content",
	// content_backend is not in the design's list, but flipping it from 'git'
	// to 'db' would disarm every other entry here in one statement. Guarding it
	// keeps the set closed.
	"content_backend",
	"content_md5",
	"current_revision",
	"description",
	"git_last_synced_at",
	"git_sha",
	"git_sync_error",
	"git_sync_status",
	"item_type",
	"name",
	"slug",
	"source_git_entry_key",
	"source_git_repo_id",
	"source_git_server_id",
	"source_path",
	"source_repo_path",
	"source_repo_ref",
	"source_repo_url",
	"source_sha",
	"version",
}

var gitOwnedCapabilityColumnSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(gitOwnedCapabilityColumns))
	for _, column := range gitOwnedCapabilityColumns {
		set[column] = struct{}{}
	}
	return set
}()

// GitOwnedCapabilityColumns returns the guarded column names (sorted).
func GitOwnedCapabilityColumns() []string {
	out := make([]string, len(gitOwnedCapabilityColumns))
	copy(out, gitOwnedCapabilityColumns)
	return out
}

// IsGitOwnedCapabilityColumn reports whether a capability_items column is
// owned by the repository.
func IsGitOwnedCapabilityColumn(column string) bool {
	_, owned := gitOwnedCapabilityColumnSet[column]
	return owned
}

// BeforeUpdate refuses legacy writes to Git-owned columns.
//
// Only BeforeUpdate is implemented, not BeforeSave: BeforeSave also fires on
// INSERT, and creating rows (including the Git discovery insert) must stay
// open. Every update form still routes through the update callback chain:
//
//   - db.Model(&item).Updates(map[string]any{...}) — Statement.Dest is the map;
//     each key is resolved to its column and matched against the guarded set.
//   - db.Save(&item) — Save appends "*" to Statement.Selects and Dest is the
//     struct, so GORM writes every column including zero values. The guard
//     therefore treats a struct Dest as "may write everything selected", and
//     then narrows it by diffing against the stored row (see below).
//   - db.Model(&x).Select("a","b").Updates(...) — Statement.Selects/Omits are
//     applied through Statement.SelectAndOmitColumns, exactly as GORM's own
//     ConvertToAssignments does, so a column excluded by Select is not
//     considered written.
//
// Blind spots, stated so they are not mistaken for coverage: raw
// tx.Exec("UPDATE capability_items ...") and tx.Table("capability_items")
// bypass model hooks entirely. Those call sites carry their own
// content_backend = 'db' predicate in SQL.
func (item *CapabilityItem) BeforeUpdate(tx *gorm.DB) error {
	return guardGitOwnedCapabilityUpdate(item, tx)
}

func guardGitOwnedCapabilityUpdate(receiver *CapabilityItem, tx *gorm.DB) error {
	if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil {
		return nil
	}
	stmt := tx.Statement
	if stmt.SkipHooks {
		return nil
	}
	if bypass, ok := tx.Get(GitSyncBypassSetting); ok {
		if enabled, _ := bypass.(bool); enabled {
			return nil
		}
	}

	// Short circuit, on purpose: work out whether this statement can touch a
	// Git-owned column *before* looking at the database. Runtime writes
	// (counters, scan status, favourites) never reach the query below, so the
	// guard costs them nothing.
	columns, destStruct := gitOwnedColumnsInStatement(stmt)
	if len(columns) == 0 {
		return nil
	}

	// A schema that predates the content_backend column has no Git-backed rows
	// to protect, and referencing the column there would turn a first-run
	// deployment migration into a hard stop (backfillCapabilityContentVersioning
	// runs in one transaction and reports failure through log.Fatalf). Mirrors
	// capabilityItemsDBOnlyPredicate in cmd/migrate.
	if !capabilityItemsHaveContentBackend(tx) {
		return nil
	}

	// Re-read the target row on the same handle the hook was given, so an
	// uncommitted content_backend written earlier in this transaction is
	// visible.
	target := gitBackedTargetQuery(tx, receiver)

	// A struct Dest carries proposed values for every column, so the write can
	// be narrowed to the columns that actually differ from the stored row.
	// Without this, a PUT that only flips `status` would be rejected purely
	// because Save() rewrites all columns with their unchanged values.
	if destStruct.IsValid() && receiver != nil && receiver.ID != "" {
		var current CapabilityItem
		if err := target.Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // not a Git-backed row
			}
			return err
		}
		changed := changedGitOwnedColumns(stmt, destStruct, current, columns)
		if len(changed) == 0 {
			return nil
		}
		return gitOwnedFieldError(changed)
	}

	var gitBacked int64
	if err := target.Count(&gitBacked).Error; err != nil {
		return err
	}
	if gitBacked == 0 {
		return nil
	}
	return gitOwnedFieldError(sortedColumns(columns))
}

func gitOwnedFieldError(columns []string) error {
	return fmt.Errorf("%w (fields: %v)", ErrGitOwnedField, columns)
}

// capabilityContentBackendColumn memoises, per *gorm.Config, that
// capability_items.content_backend exists. Only the positive answer is cached:
// a column can be added mid-process (cmd/migrate runs prepareSchema before its
// backfills), and caching "absent" there would disable the guard for the rest
// of that run.
var capabilityContentBackendColumn sync.Map // *gorm.Config -> struct{}

func capabilityItemsHaveContentBackend(tx *gorm.DB) bool {
	if tx == nil || tx.Config == nil {
		return false
	}
	if _, cached := capabilityContentBackendColumn.Load(tx.Config); cached {
		return true
	}
	if !tx.Migrator().HasColumn(&CapabilityItem{}, "content_backend") {
		return false
	}
	capabilityContentBackendColumn.Store(tx.Config, struct{}{})
	return true
}

// gitBackedTargetQuery rebuilds the statement's target set restricted to
// Git-backed rows.
//
// The WHERE clause and the primary key are combined rather than chosen
// between, because GORM sources them differently and both may be absent:
// db.Model(&item).Updates(...) has no WHERE clause yet at BeforeUpdate time
// (ConvertToAssignments adds the primary-key predicate afterwards), while
// db.Model(&Item{}).Where(...).Updates(...) has a WHERE clause and an empty
// receiver. A statement with neither is a global update, which correctly
// resolves to "does any Git-backed row exist".
func gitBackedTargetQuery(tx *gorm.DB, receiver *CapabilityItem) *gorm.DB {
	query := tx.Model(&CapabilityItem{}).Where("content_backend = ?", ContentBackendGit)
	if whereClause, ok := tx.Statement.Clauses["WHERE"]; ok {
		if where, ok := whereClause.Expression.(clause.Where); ok && len(where.Exprs) > 0 {
			query = query.Clauses(clause.Where{Exprs: where.Exprs})
		}
	}
	if receiver != nil && receiver.ID != "" {
		query = query.Where("id = ?", receiver.ID)
	}
	return query
}

// gitOwnedColumnsInStatement returns the Git-owned columns this UPDATE may
// write, and — when the destination is a CapabilityItem struct — that struct,
// so the caller can diff it against the stored row.
func gitOwnedColumnsInStatement(stmt *gorm.Statement) (map[string]struct{}, reflect.Value) {
	selectColumns, restricted := stmt.SelectAndOmitColumns(false, true)
	touched := make(map[string]struct{})
	writable := func(column string) bool {
		selected, listed := selectColumns[column]
		return (listed && selected) || (!listed && !restricted)
	}

	dest := reflect.ValueOf(stmt.Dest)
	for dest.Kind() == reflect.Ptr || dest.Kind() == reflect.Interface {
		if dest.IsNil() {
			return touched, reflect.Value{}
		}
		dest = dest.Elem()
	}

	switch dest.Kind() {
	case reflect.Map:
		iter := dest.MapRange()
		for iter.Next() {
			key, ok := iter.Key().Interface().(string)
			if !ok {
				continue
			}
			column := key
			if field := stmt.Schema.LookUpField(key); field != nil && field.DBName != "" {
				column = field.DBName
			}
			if IsGitOwnedCapabilityColumn(column) && writable(column) {
				touched[column] = struct{}{}
			}
		}
		return touched, reflect.Value{}
	case reflect.Struct:
		if dest.Type() != reflect.TypeOf(CapabilityItem{}) {
			// An unknown struct schema cannot be diffed field by field; assume
			// it may write everything the statement allows.
			for _, column := range gitOwnedCapabilityColumns {
				if writable(column) {
					touched[column] = struct{}{}
				}
			}
			return touched, reflect.Value{}
		}
		for _, column := range gitOwnedCapabilityColumns {
			field := stmt.Schema.LookUpField(column)
			if field == nil || !writable(column) {
				continue
			}
			// Mirrors ConvertToAssignments: a struct update only writes zero
			// values when the column was explicitly selected (which is what
			// Save's "*" does).
			_, listed := selectColumns[column]
			if _, isZero := field.ValueOf(stmt.Context, dest); isZero && !listed {
				continue
			}
			touched[column] = struct{}{}
		}
		return touched, dest
	}
	return touched, reflect.Value{}
}

func changedGitOwnedColumns(stmt *gorm.Statement, dest reflect.Value, current CapabilityItem, columns map[string]struct{}) []string {
	currentValue := reflect.ValueOf(current)
	changed := make([]string, 0, len(columns))
	for _, column := range gitOwnedCapabilityColumns {
		if _, ok := columns[column]; !ok {
			continue
		}
		field := stmt.Schema.LookUpField(column)
		if field == nil {
			continue
		}
		proposed, _ := field.ValueOf(stmt.Context, dest)
		stored, _ := field.ValueOf(stmt.Context, currentValue)
		if !columnValuesEqual(proposed, stored) {
			changed = append(changed, column)
		}
	}
	return changed
}

func columnValuesEqual(a, b any) bool {
	switch left := a.(type) {
	case time.Time:
		right, ok := b.(time.Time)
		return ok && left.Equal(right)
	case *time.Time:
		right, ok := b.(*time.Time)
		if !ok {
			return false
		}
		if left == nil || right == nil {
			return left == right
		}
		return left.Equal(*right)
	}
	return reflect.DeepEqual(a, b)
}

func sortedColumns(columns map[string]struct{}) []string {
	out := make([]string, 0, len(columns))
	for column := range columns {
		out = append(out, column)
	}
	sort.Strings(out)
	return out
}
