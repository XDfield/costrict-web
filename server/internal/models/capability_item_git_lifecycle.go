package models

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"gorm.io/gorm"
)

// This file owns ONE rule, and it is the half of the Git lifecycle contract the
// database cannot express on its own:
//
//	a human may take a Git-backed row DOWN at any time, and doing so revokes
//	Git's standing permission to put it back up; a human may NOT put a row back
//	up while Git still claims it is gone.
//
// `git_lifecycle_reason` is that claim. The archive writer sets it, a successful
// projection clears it, and everything below decides what a NON-Git writer is
// allowed to do while it is present.
//
// Why this lives in a GORM hook rather than in each handler
// --------------------------------------------------------
// The threat is not one endpoint, it is every writer that predates Git backing.
// `git_lifecycle_reason` is deliberately absent from gitOwnedCapabilityColumns
// (a moderation write MUST be able to clear it, so guarding it there would block
// the very path that needs it), and `status` is absent for the same reason. So
// without this hook any legacy `Update("status", "active")` flips a
// Git-archived row live while the reason still sits on it — producing a row that
// is active, has no reachable content, and can never be auto-recovered because
// Git's own recovery predicate requires it to be archived. That state is silent:
// nothing errors, the item simply 404s on every content read.
//
// The hook is therefore default-deny in the same shape as the Git-owned column
// guard next to it, and shares its bypass marker: the authoritative Git writer
// sets GitSyncBypassSetting on its own transaction and is unaffected.

// ErrGitLifecycleArchived is returned when a non-Git writer tries to activate a
// capability row that Git currently claims is gone. Handlers map it to HTTP 409
// alongside ErrGitOwnedField.
var ErrGitLifecycleArchived = errors.New("capability item is archived by Git; it cannot be activated from Cloud while the repository claim stands")

// ErrGitLifecycleClaimUnclearable reports a hidden-status write whose statement
// shape gives the hook no way to clear the Git claim atomically. It is refused
// rather than applied, because applying it would leave a row that a human hid
// while Git still holds permission to raise it back.
var ErrGitLifecycleClaimUnclearable = errors.New("capability item status update cannot clear the Git lifecycle claim; write git_lifecycle_reason explicitly")

// capabilityHiddenStatuses are the statuses a human puts a row into to take it
// off the shelf.
//
// This is the single definition, and the Git sync service reads it from here
// (services.gitCapabilityHiddenStatuses) rather than keeping its own. The two
// lists must agree by construction: this guard decides which statuses CLEAR
// Git's archive claim, while the sync decides which statuses Git must not
// overwrite. A status in one list but not the other is a resurrection hole —
// the moderator hides the row without revoking Git's permission, and the next
// push republishes it.
var capabilityHiddenStatuses = []string{"archived", "banned", "inactive"}

// IsCapabilityHiddenStatus reports whether a status takes an item off the shelf.
func IsCapabilityHiddenStatus(status string) bool {
	for _, hidden := range capabilityHiddenStatuses {
		if status == hidden {
			return true
		}
	}
	return false
}

// CapabilityHiddenStatuses returns the off-the-shelf statuses (sorted).
func CapabilityHiddenStatuses() []string {
	out := make([]string, len(capabilityHiddenStatuses))
	copy(out, capabilityHiddenStatuses)
	return out
}

// guardGitLifecycleStatusWrite applies the manual-status half of the lifecycle
// contract to one UPDATE.
//
// Three outcomes:
//
//   - the statement does not write `status` → nothing to decide.
//   - it writes a hidden status → the Git claim is CLEARED in the same
//     statement, so the human's decision cannot be undone by a returning
//     manifest.
//   - it writes any other status (i.e. activation) → refused while any targeted
//     Git-backed row still carries a reason.
//
// Clearing the reason alone is sufficient, and `git_sync_status` is deliberately
// left untouched. Git's recovery predicate
// (services.gitCapabilityRecoverablePredicate) requires BOTH the orphan marker
// AND a recoverable reason, so removing the reason revokes the permission on its
// own — while the marker keeps saying, truthfully and usefully to an operator,
// that this row was originally taken down by Git rather than by a person.
//
// The "any targeted row" quantifier is deliberate. A batch update whose WHERE
// matches one claimed row and ninety-nine clean ones is refused whole rather
// than partially applied: a partial success here is indistinguishable, to the
// operator, from a complete one.
func guardGitLifecycleStatusWrite(receiver *CapabilityItem, tx *gorm.DB) error {
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

	status, writesStatus := statusWrittenByStatement(stmt)
	if !writesStatus {
		return nil
	}
	// Same reasoning as capabilityItemsHaveContentBackend: a schema that predates
	// the lifecycle columns has no claim to enforce, and referencing the column
	// there would turn a first-run migration into a hard stop.
	if !capabilityItemsHaveContentBackend(tx) || !capabilityItemsHaveGitLifecycle(tx) {
		return nil
	}

	if IsCapabilityHiddenStatus(status) {
		return clearGitLifecycleClaim(stmt)
	}

	var claimed int64
	if err := gitBackedTargetQuery(tx, receiver).
		Where("git_lifecycle_reason IS NOT NULL").
		Count(&claimed).Error; err != nil {
		return err
	}
	if claimed == 0 {
		return nil
	}
	return fmt.Errorf("%w (rows: %d, requested status: %q)", ErrGitLifecycleArchived, claimed, status)
}

// clearGitLifecycleClaim adds the claim-clearing assignments to a statement that
// is already writing a hidden status.
//
// It refuses rather than silently skipping when the statement shape cannot carry
// them. GORM only writes a struct field's zero value when the column was
// explicitly selected, so a `Updates(CapabilityItem{Status: "archived"})` would
// accept a NULL assignment here and then drop it on the floor — the row would be
// hidden with Git's permission intact, which is exactly the state this guard
// exists to prevent. Map destinations and Save() (which selects "*") both carry
// it, and those are the shapes every writer in this repository uses.
func clearGitLifecycleClaim(stmt *gorm.Statement) error {
	if !statementWritesZeroValues(stmt) {
		return fmt.Errorf("%w (status write via %s)", ErrGitLifecycleClaimUnclearable, describeStatementDest(stmt))
	}
	stmt.SetColumn("git_lifecycle_reason", nil)
	return stmt.Error
}

// statusWrittenByStatement reports the `status` value an UPDATE assigns, if it
// assigns one at all.
//
// It mirrors gitOwnedColumnsInStatement's rules for Select/Omit and for struct
// zero values, because the two guards must agree on what "this statement writes
// column X" means.
func statusWrittenByStatement(stmt *gorm.Statement) (string, bool) {
	selectColumns, restricted := stmt.SelectAndOmitColumns(false, true)
	selected, listed := selectColumns["status"]
	if (listed && !selected) || (!listed && restricted) {
		return "", false
	}

	dest := reflect.ValueOf(stmt.Dest)
	for dest.Kind() == reflect.Ptr || dest.Kind() == reflect.Interface {
		if dest.IsNil() {
			return "", false
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
			if column != "status" {
				continue
			}
			value, ok := iter.Value().Interface().(string)
			if !ok {
				// A non-string assignment (an expression, a typed alias) cannot be
				// classified, and guessing would either block a legitimate write or
				// wave through an activation. Reported as "writes an unknown status",
				// which the caller treats as an activation attempt — fail closed.
				return "", true
			}
			return value, true
		}
		return "", false
	case reflect.Struct:
		if dest.Type() != reflect.TypeOf(CapabilityItem{}) {
			return "", false
		}
		field := stmt.Schema.LookUpField("status")
		if field == nil {
			return "", false
		}
		value, isZero := field.ValueOf(stmt.Context, dest)
		if isZero && !listed {
			return "", false
		}
		status, _ := value.(string)
		return status, true
	}
	return "", false
}

// statementWritesZeroValues reports whether an added NULL assignment will
// actually reach the UPDATE. Map destinations always carry it; a struct
// destination only does when the statement selects "*" (which is what Save
// does).
func statementWritesZeroValues(stmt *gorm.Statement) bool {
	dest := reflect.ValueOf(stmt.Dest)
	for dest.Kind() == reflect.Ptr || dest.Kind() == reflect.Interface {
		if dest.IsNil() {
			return false
		}
		dest = dest.Elem()
	}
	if dest.Kind() == reflect.Map {
		return true
	}
	for _, selected := range stmt.Selects {
		if selected == "*" {
			return true
		}
		if strings.EqualFold(selected, "git_lifecycle_reason") {
			return true
		}
	}
	return false
}

func describeStatementDest(stmt *gorm.Statement) string {
	if stmt == nil || stmt.Dest == nil {
		return "unknown destination"
	}
	return fmt.Sprintf("%T", stmt.Dest)
}

// capabilityItemsGitLifecycleColumn memoises, per connection, that
// capability_items.git_lifecycle_reason exists. Positive answers only — see
// capabilityContentBackendColumn for why caching the negative would disable the
// guard for the rest of a migration run.
var capabilityItemsGitLifecycleColumn sync.Map // gorm.Dialector -> struct{}

func capabilityItemsHaveGitLifecycle(tx *gorm.DB) bool {
	if tx == nil || tx.Config == nil || tx.Dialector == nil {
		return false
	}
	if _, cached := capabilityItemsGitLifecycleColumn.Load(tx.Dialector); cached {
		return true
	}
	// ColumnReachable for the same reason capabilityItemsHaveContentBackend
	// uses it: the driver's HasColumn resolves against CURRENT_SCHEMA() while
	// the guarded statement resolves through search_path, so on a divergent
	// connection it would report "pre-lifecycle schema" for a table that has the
	// column and quietly stop enforcing the lifecycle claim. An unanswerable
	// probe enforces, for the same reason it does there.
	present, err := ColumnReachable(tx, &CapabilityItem{}, "git_lifecycle_reason")
	if err != nil {
		return true
	}
	if !present {
		return false
	}
	capabilityItemsGitLifecycleColumn.Store(tx.Dialector, struct{}{})
	return true
}
