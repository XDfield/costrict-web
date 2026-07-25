//go:build cgo

package etl

import (
	"context"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// resetTestDB is a superset of newDB that also migrates the secondary
// user-bound tables covered by ResetTarget. The standard newDB() in
// helpers_test.go only migrates users + user_auth_identities, which is enough
// for the ImportUsers / ImportAuthIdentities tests but not for --init scope.
//
// UserEvent and AuditLog carry Postgres-specific column types (uuid, jsonb,
// timestamptz) that SQLite cannot AutoMigrate. We hand-roll those two tables
// with SQLite-compatible equivalents — the row counts and DELETE semantics
// we exercise here are type-agnostic, so this is faithful enough.
func resetTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newDB(t)
	if err := db.AutoMigrate(
		&models.EmploymentIdentity{},
		&models.TenantAdmin{},
		&models.PlatformAdmin{},
		&models.Tenant{},
	); err != nil {
		t.Fatalf("AutoMigrate secondary models: %v", err)
	}
	// Hand-rolled AuditLog + UserEvent with SQLite-compatible types.
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS user_center_audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tenant_id TEXT,
		actor_subject_id TEXT,
		actor_tenant_role VARCHAR(32),
		actor_platform_scope VARCHAR(32),
		action VARCHAR(64) NOT NULL,
		target_type VARCHAR(32),
		target_id TEXT,
		payload BLOB,
		ip VARCHAR(45),
		user_agent TEXT,
		created_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create audit_log table: %v", err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS user_events (
		event_id TEXT PRIMARY KEY,
		event_type VARCHAR(64) NOT NULL,
		subject_id TEXT NOT NULL,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		payload TEXT NOT NULL,
		status VARCHAR(16) NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		available_at DATETIME NOT NULL,
		delivered_at DATETIME,
		created_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create user_events table: %v", err)
	}
	return db
}

func ptrString(s string) *string { return &s }

func TestResetTarget_EmptyScopeRejected(t *testing.T) {
	db := resetTestDB(t)
	if _, err := ResetTarget(context.Background(), db, 0, false); err != ErrNothingToReset {
		t.Fatalf("expected ErrNothingToReset, got %v", err)
	}
}

func TestResetTarget_NilDBRejected(t *testing.T) {
	if _, err := ResetTarget(context.Background(), nil, ResetUserDataset, false); err != ErrNilDB {
		t.Fatalf("expected ErrNilDB, got %v", err)
	}
}

// TestResetTarget_FullUserDataset asserts the canonical --init scope wipes
// every user-bound table including soft-deleted rows, and that downstream
// reference tables (tenants) are NOT touched.
func TestResetTarget_FullUserDataset(t *testing.T) {
	db := resetTestDB(t)

	// Seed one row per table in scope.
	if err := db.Create(&models.User{
		SubjectID: "usr_a", Username: "a", IsActive: true,
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Create(&models.UserAuthIdentity{
		UserSubjectID: "usr_a", Provider: "idtrust", ExternalKey: "casdoor:idtrust:1",
	}).Error; err != nil {
		t.Fatalf("seed auth_identity: %v", err)
	}
	if err := db.Create(&models.EmploymentIdentity{
		UserSubjectID: "usr_a", Provider: "idtrust", SyncStatus: "fresh",
	}).Error; err != nil {
		t.Fatalf("seed employment: %v", err)
	}
	if err := db.Create(&models.TenantAdmin{
		TenantID: "default", UserID: "usr_a", Role: models.TenantRoleAdmin,
	}).Error; err != nil {
		t.Fatalf("seed tenant_admin: %v", err)
	}
	if err := db.Create(&models.PlatformAdmin{
		UserID: "usr_a", GrantedBy: "system", GrantedAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed platform_admin: %v", err)
	}
	if err := db.Exec(`INSERT INTO user_center_audit_log (action, created_at) VALUES (?, ?)`,
		"test", time.Now()).Error; err != nil {
		t.Fatalf("seed audit_log: %v", err)
	}
	if err := db.Create(&models.UserEvent{
		EventID: "evt-1", EventType: "test", SubjectID: "usr_a",
		Payload: "{}", AvailableAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed user_event: %v", err)
	}

	// Also seed a Tenant — ResetTarget must leave this intact.
	if err := db.Create(&models.Tenant{
		TenantID: "default", Slug: "default", DisplayName: "Default", Status: "active",
	}).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	stats, err := ResetTarget(context.Background(), db, ResetUserDataset, false)
	if err != nil {
		t.Fatalf("ResetTarget: %v", err)
	}

	if stats.Users != 1 || stats.AuthIdentities != 1 || stats.EmploymentIdentities != 1 ||
		stats.TenantAdmins != 1 || stats.PlatformAdmins != 1 ||
		stats.AuditLogs != 1 || stats.UserEvents != 1 {
		t.Fatalf("unexpected per-table counts: %+v", stats)
	}
	if stats.Total() != 7 {
		t.Fatalf("Total: got %d, want 7", stats.Total())
	}

	// Every in-scope table must now be empty.
	for _, table := range []string{
		"users", "user_auth_identities", "employment_identities",
		"tenant_admins", "platform_admins", "user_center_audit_log", "user_events",
	} {
		var n int64
		if err := db.Unscoped().Table(table).Count(&n).Error; err != nil {
			t.Fatalf("count %s post-reset: %v", table, err)
		}
		if n != 0 {
			t.Errorf("table %s still has %d row(s) after reset", table, n)
		}
	}

	// Tenants preserved.
	var tenantCount int64
	db.Model(&models.Tenant{}).Count(&tenantCount)
	if tenantCount != 1 {
		t.Errorf("tenants table should be untouched, got %d row(s)", tenantCount)
	}
}

// TestResetTarget_DryRunLeavesDataIntact verifies --dry-run reports the
// would-be wipe but issues no DELETE/TRUNCATE.
func TestResetTarget_DryRunLeavesDataIntact(t *testing.T) {
	db := resetTestDB(t)
	if err := db.Create(&models.User{
		SubjectID: "usr_keep", Username: "keep", IsActive: true,
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	stats, err := ResetTarget(context.Background(), db, ResetUsers, true)
	if err != nil {
		t.Fatalf("ResetTarget dry-run: %v", err)
	}
	if stats.Users != 1 {
		t.Fatalf("dry-run users count: got %d, want 1", stats.Users)
	}

	// Row still present.
	var n int64
	db.Model(&models.User{}).Count(&n)
	if n != 1 {
		t.Errorf("dry-run should not delete, found %d row(s)", n)
	}
}

// TestResetTarget_PartialScope verifies a single-table scope only wipes that
// one table and leaves the others intact.
func TestResetTarget_PartialScope(t *testing.T) {
	db := resetTestDB(t)
	if err := db.Create(&models.User{
		SubjectID: "usr_keep", Username: "keep", IsActive: true,
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Create(&models.UserAuthIdentity{
		UserSubjectID: "usr_keep", Provider: "idtrust", ExternalKey: "casdoor:idtrust:keep",
	}).Error; err != nil {
		t.Fatalf("seed auth_identity: %v", err)
	}

	stats, err := ResetTarget(context.Background(), db, ResetAuthIdentities, false)
	if err != nil {
		t.Fatalf("ResetTarget partial: %v", err)
	}
	if stats.AuthIdentities != 1 {
		t.Errorf("AuthIdentities: got %d, want 1", stats.AuthIdentities)
	}
	if stats.Users != 0 {
		t.Errorf("Users should be 0 (out of scope), got %d", stats.Users)
	}

	// User preserved.
	var userCount int64
	db.Model(&models.User{}).Count(&userCount)
	if userCount != 1 {
		t.Errorf("users table should be intact, got %d", userCount)
	}

	// Auth identities gone.
	var idCount int64
	db.Unscoped().Model(&models.UserAuthIdentity{}).Count(&idCount)
	if idCount != 0 {
		t.Errorf("auth_identities should be wiped, got %d", idCount)
	}
}

// TestResetTarget_RestartsAutoIncrement verifies post-reset INSERTs start
// from id=1, not from the pre-reset max+1. Without this, post-reset cs-user
// rows would carry arbitrarily large IDs from the dual-write-canary era.
func TestResetTarget_RestartsAutoIncrement(t *testing.T) {
	db := resetTestDB(t)
	if err := db.Create(&models.User{
		SubjectID: "usr_old1", Username: "old1", IsActive: true,
	}).Error; err != nil {
		t.Fatalf("seed old1: %v", err)
	}
	if err := db.Create(&models.User{
		SubjectID: "usr_old2", Username: "old2", IsActive: true,
	}).Error; err != nil {
		t.Fatalf("seed old2: %v", err)
	}

	// Confirm auto-increment advanced.
	var preMaxID int64
	db.Model(&models.User{}).Select("MAX(id)").Scan(&preMaxID)
	if preMaxID < 2 {
		t.Fatalf("expected at least 2 seeded rows, max id = %d", preMaxID)
	}

	if _, err := ResetTarget(context.Background(), db, ResetUsers, false); err != nil {
		t.Fatalf("ResetTarget: %v", err)
	}

	// Insert one fresh row — should get id=1.
	fresh := &models.User{SubjectID: "usr_new", Username: "new", IsActive: true}
	if err := db.Create(fresh).Error; err != nil {
		t.Fatalf("create post-reset: %v", err)
	}
	if fresh.ID != 1 {
		t.Errorf("post-reset ID: got %d, want 1 (sequence should have restarted)", fresh.ID)
	}
}

// TestResetTarget_IncludesSoftDeletedRows confirms Unscoped semantics: a
// soft-deleted row in users still counts toward the wipe (otherwise
// re-migration would collide on the lingering soft-deleted external_key).
func TestResetTarget_IncludesSoftDeletedRows(t *testing.T) {
	db := resetTestDB(t)

	// Create + soft-delete one user.
	u := &models.User{SubjectID: "usr_ghost", Username: "ghost", IsActive: true}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Delete(u).Error; err != nil {
		t.Fatalf("soft-delete user: %v", err)
	}

	stats, err := ResetTarget(context.Background(), db, ResetUsers, false)
	if err != nil {
		t.Fatalf("ResetTarget: %v", err)
	}
	// Default GORM Count() filters soft-deleted; Unscoped count includes them.
	// Stats should report at least 1 (the live row before delete) but the
	// post-reset table must be fully empty including the soft-deleted one.
	if stats.Users < 1 {
		t.Errorf("expected Users >= 1, got %d", stats.Users)
	}

	var n int64
	db.Unscoped().Table("users").Count(&n)
	if n != 0 {
		t.Errorf("expected 0 rows including soft-deleted, got %d", n)
	}
}

// ptrString is exercised above; this line prevents Go from complaining if
// future test edits stop using the helper.
var _ = ptrString
