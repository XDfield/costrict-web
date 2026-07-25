package etl

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ResetScope controls which tables ResetTarget wipes. Zero value = wipe none.
// Callers compose with bit-or (ResetUsers | ResetAuthIdentities | ...).
type ResetScope uint

const (
	ResetUsers          ResetScope = 1 << iota // users + downstream FK tables
	ResetAuthIdentities                        // user_auth_identities
	ResetEmploymentIdentities                  // employment_identities
	ResetTenantAdmins                          // tenant_admins
	ResetPlatformAdmins                        // platform_admins
	ResetAuditLogs                             // audit_logs
	ResetUserEvents                            // user_events

	// ResetUserDataset wipes everything that hangs off a user_subject_id.
	// Use this for --init: cs-user enters a pristine state, then the
	// migration repopulates users + auth_identities from server. Downstream
	// tables (employment/roles/admins/audit/events) get rebuilt by the
	// respective services (ApplyEnterpriseMapping, role sync, etc.) as
	// users log back in or as background reconcilers fire.
	ResetUserDataset ResetScope = ResetUsers | ResetAuthIdentities |
		ResetEmploymentIdentities | ResetTenantAdmins |
		ResetPlatformAdmins | ResetAuditLogs | ResetUserEvents
)

// ResetStats records row counts removed per table during ResetTarget.
// Tables not in scope are omitted. Each count includes soft-deleted rows
// (we use Unscoped / TRUNCATE to nuke them too).
type ResetStats struct {
	Users                 int64 `json:"users"`
	AuthIdentities        int64 `json:"auth_identities"`
	EmploymentIdentities  int64 `json:"employment_identities"`
	TenantAdmins          int64 `json:"tenant_admins"`
	PlatformAdmins        int64 `json:"platform_admins"`
	AuditLogs             int64 `json:"audit_logs"`
	UserEvents            int64 `json:"user_events"`
}

// Total returns the sum of every wiped row count.
func (s ResetStats) Total() int64 {
	return s.Users + s.AuthIdentities + s.EmploymentIdentities +
		s.TenantAdmins + s.PlatformAdmins + s.AuditLogs + s.UserEvents
}

// ErrNothingToReset is returned when scope == 0. Surfaces the most common
// misuse of ResetTarget as an explicit error rather than a silent no-op.
var ErrNothingToReset = errors.New("etl.ResetTarget: scope is empty")

// ResetTarget clears the requested tables in the cs-user target DB. Order
// matters: child tables (auth_identities, employment_identities, role/admin
// junctions, audit_logs, user_events) are wiped first, then users last —
// this keeps referential integrity clean even when foreign keys are enforced.
// Identity sequences are restarted so post-reset INSERTs start from 1.
//
// When dryRun is true, no writes are issued; ResetStats still reflects what
// *would* have been deleted (computed via COUNT).
//
// Tables NOT covered (deliberate):
//   - tenants / tenant_configs — operational reference data, not user-bound
//   - outbox — operational queue, replayable post-reset
//
// Caller decides whether to add finer-grained scopes; the canonical --init
// path uses ResetUserDataset.
func ResetTarget(ctx context.Context, db *gorm.DB, scope ResetScope, dryRun bool) (ResetStats, error) {
	if db == nil {
		return ResetStats{}, ErrNilDB
	}
	if scope == 0 {
		return ResetStats{}, ErrNothingToReset
	}

	var stats ResetStats

	// Stage 1: count what's about to go. Cheap, and gives dryRun a real number.
	counts := map[string]*int64{
		"users":                  &stats.Users,
		"user_auth_identities":   &stats.AuthIdentities,
		"employment_identities":  &stats.EmploymentIdentities,
		"tenant_admins":          &stats.TenantAdmins,
		"platform_admins":        &stats.PlatformAdmins,
		"user_center_audit_log":  &stats.AuditLogs,
		"user_events":            &stats.UserEvents,
	}
	inScope := map[string]bool{
		"users":                  scope&ResetUsers != 0,
		"user_auth_identities":   scope&ResetAuthIdentities != 0,
		"employment_identities":  scope&ResetEmploymentIdentities != 0,
		"tenant_admins":          scope&ResetTenantAdmins != 0,
		"platform_admins":        scope&ResetPlatformAdmins != 0,
		"user_center_audit_log":  scope&ResetAuditLogs != 0,
		"user_events":            scope&ResetUserEvents != 0,
	}

	for table, ptr := range counts {
		if !inScope[table] {
			continue
		}
		var n int64
		if err := db.WithContext(ctx).Unscoped().
			Table(table).
			Count(&n).Error; err != nil {
			return stats, fmt.Errorf("etl.ResetTarget: count %s: %w", table, err)
		}
		*ptr = n
	}

	if dryRun {
		return stats, nil
	}

	// Stage 2: wipe. Wipe order is child-first; users last. Each table gets
	// its own TRUNCATE-with-RESTART-IDENTITY on Postgres (fast, resets
	// sequences in one shot). On SQLite there's no TRUNCATE — fall back to
	// DELETE + clear sqlite_sequence.
	//
	// We avoid a single multi-table TRUNCATE because (a) we need
	// per-table error attribution in logs, (b) SQLite doesn't support it,
	// (c) the table list is small (7) so per-table is fine.
	wipeOrder := []string{
		"user_auth_identities",
		"employment_identities",
		"tenant_admins",
		"platform_admins",
		"user_center_audit_log",
		"user_events",
		"users",
	}
	for _, table := range wipeOrder {
		if !inScope[table] {
			continue
		}
		if err := wipeTable(ctx, db, table); err != nil {
			return stats, fmt.Errorf("etl.ResetTarget: wipe %s: %w", table, err)
		}
	}

	return stats, nil
}

// wipeTable nukes every row of one table and restarts its identity sequence.
// Strategy is dialect-sensitive: Postgres uses TRUNCATE (atomic + resets
// sequences); SQLite uses DELETE + clears sqlite_sequence (TRUNCATE is a
// non-existent statement in SQLite, despite the parser accepting it as a
// synonym for DELETE in some builds — explicit DELETE is portable).
func wipeTable(ctx context.Context, db *gorm.DB, table string) error {
	// Sniff dialect once via the gorm Dialector name. Cheap.
	dialect := db.Dialector.Name()
	switch dialect {
	case "postgres":
		// RESTART IDENTITY resets the serial sequence; CASCADE handles any
		// lingering FK references (defensive — we already wipe child-first).
		if err := db.WithContext(ctx).
			Exec(fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", table)).Error; err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
	case "sqlite":
		if err := db.WithContext(ctx).
			Exec(fmt.Sprintf("DELETE FROM %s", table)).Error; err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		// Clear the auto-increment counter so post-reset INSERTs start at 1
		// (matches the Postgres RESTART IDENTITY behavior). Missing
		// sqlite_sequence row = table never had an AUTOINCREMENT — safe to
		// ignore the no-rows case.
		if err := db.WithContext(ctx).
			Exec("DELETE FROM sqlite_sequence WHERE name = ?", table).Error; err != nil {
			return fmt.Errorf("clear sqlite_sequence: %w", err)
		}
	default:
		// Unknown dialect — fall back to DELETE and hope the driver
		// doesn't enforce a sequence we care about. Operator can manually
		// reset sequences if needed.
		if err := db.WithContext(ctx).
			Exec(fmt.Sprintf("DELETE FROM %s", table)).Error; err != nil {
			return fmt.Errorf("delete: %w", err)
		}
	}
	return nil
}
