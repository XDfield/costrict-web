//go:build cgo

package models

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newTenantScopedDB mirrors the per-table fixtures but migrates all three
// B2-affected user tables together (plus Tenant so the migration's FK target
// exists). Used to exercise tenant_id column defaults, indexes, and cross-
// table queries.
func newTenantScopedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&Tenant{},
		&User{},
		&UserAuthIdentity{},
		&EmploymentIdentity{},
	); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	// Bootstrap the default tenant explicitly — the production migration does
	// this via INSERT ... ON CONFLICT; sqlite tests need to seed by hand.
	if err := db.Create(&Tenant{TenantID: "default", Slug: "default", DisplayName: "Default Tenant"}).Error; err != nil {
		t.Fatalf("seed default tenant: %v", err)
	}
	if err := db.Create(&Tenant{TenantID: "t-acme", Slug: "acme", DisplayName: "Acme Inc."}).Error; err != nil {
		t.Fatalf("seed acme tenant: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestUser_TenantIDDefaultsToDefault verifies that a User created without an
// explicit TenantID picks up the column default 'default' from the GORM tag
// (which mirrors the migration's NOT NULL DEFAULT 'default'). This is the
// backfill path for Phase B2 — all existing single-tenant rows land in the
// 'default' tenant without code changes.
func TestUser_TenantIDDefaultsToDefault(t *testing.T) {
	t.Parallel()
	db := newTenantScopedDB(t)

	u := &User{SubjectID: "u-defaults", Username: "alice"}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got User
	if err := db.First(&got, u.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID != "default" {
		t.Errorf("TenantID default: got %q, want 'default'", got.TenantID)
	}
}

// TestUser_ExplicitTenantID verifies that an explicitly-set TenantID round-
// trips — i.e. setting TenantID="t-acme" at Create survives read-back. This
// is the path B3+ will use once tenant resolution lands.
func TestUser_ExplicitTenantID(t *testing.T) {
	t.Parallel()
	db := newTenantScopedDB(t)

	u := &User{TenantID: "t-acme", SubjectID: "u-acme", Username: "bob"}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got User
	if err := db.First(&got, u.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID != "t-acme" {
		t.Errorf("TenantID explicit: got %q, want 't-acme'", got.TenantID)
	}
}

// TestUser_QueryByTenantID exercises the idx_users_tenant_id hot path: insert
// users in two tenants, count by tenant, verify the index-backed filter
// returns only that tenant's users.
func TestUser_QueryByTenantID(t *testing.T) {
	t.Parallel()
	db := newTenantScopedDB(t)

	for _, c := range []struct {
		subject, username, tenant string
	}{
		{"u-a1", "alice", "default"},
		{"u-a2", "alan", "default"},
		{"u-a3", "amy", "default"},
		{"u-b1", "bob", "t-acme"},
		{"u-b2", "bill", "t-acme"},
	} {
		if err := db.Create(&User{TenantID: c.tenant, SubjectID: c.subject, Username: c.username}).Error; err != nil {
			t.Fatalf("create %s: %v", c.subject, err)
		}
	}

	var defaultCount, acmeCount int64
	if err := db.Model(&User{}).Where("tenant_id = ?", "default").Count(&defaultCount).Error; err != nil {
		t.Fatalf("count default: %v", err)
	}
	if err := db.Model(&User{}).Where("tenant_id = ?", "t-acme").Count(&acmeCount).Error; err != nil {
		t.Fatalf("count acme: %v", err)
	}
	if defaultCount != 3 {
		t.Errorf("default tenant user count: got %d, want 3", defaultCount)
	}
	if acmeCount != 2 {
		t.Errorf("t-acme tenant user count: got %d, want 2", acmeCount)
	}
}

// TestUserAuthIdentity_TenantIDDefaultsToDefault mirrors the User test on the
// user_auth_identities table — same NOT NULL DEFAULT 'default' contract.
func TestUserAuthIdentity_TenantIDDefaultsToDefault(t *testing.T) {
	t.Parallel()
	db := newTenantScopedDB(t)

	// Need a backing user to satisfy the application-layer reference (no SQL
	// FK in the migration, but tests should still respect the data model).
	if err := db.Create(&User{SubjectID: "u-x", Username: "x"}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	id := &UserAuthIdentity{
		UserSubjectID: "u-x",
		Provider:      "idtrust",
		ExternalKey:   "idtrust:u-x",
	}
	if err := db.Create(id).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got UserAuthIdentity
	if err := db.First(&got, id.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID != "default" {
		t.Errorf("TenantID default: got %q, want 'default'", got.TenantID)
	}
}

// TestEmploymentIdentity_TenantIDDefaultsToDefault verifies the third B2
// table picks up the default tenant_id.
func TestEmploymentIdentity_TenantIDDefaultsToDefault(t *testing.T) {
	t.Parallel()
	db := newTenantScopedDB(t)
	// Recreate the partial unique index the migration owns (mirror the
	// existing newEmploymentIdentityDB helper) — without it the AutoMigrate
	// path leaves the contract unenforced in sqlite.
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_employment_identities_user_subject_id
		ON employment_identities(user_subject_id) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("create partial unique index: %v", err)
	}

	ei := &EmploymentIdentity{UserSubjectID: "u-ei", Provider: "idtrust"}
	if err := db.Create(ei).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got EmploymentIdentity
	if err := db.First(&got, ei.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.TenantID != "default" {
		t.Errorf("TenantID default: got %q, want 'default'", got.TenantID)
	}
}

// TestEmploymentIdentity_QueryByTenantAndSubject exercises the composite index
// idx_employment_identities_tenant_user created in B2. Insert identities
// across two tenants (some with the same user_subject_id, simulating the
// multi-tenant membership case where the same subject_id exists in different
// tenants) and verify the (tenant_id, user_subject_id) lookup returns exactly
// the right row.
func TestEmploymentIdentity_QueryByTenantAndSubject(t *testing.T) {
	t.Parallel()
	db := newTenantScopedDB(t)

	rows := []EmploymentIdentity{
		{TenantID: "default", UserSubjectID: "u-shared", Provider: "idtrust"},
		{TenantID: "t-acme", UserSubjectID: "u-shared", Provider: "idtrust"},
		{TenantID: "default", UserSubjectID: "u-only-here", Provider: "idtrust"},
	}
	for i := range rows {
		// Override the default 'default' on rows where TenantID is set explicitly.
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// Look up the t-acme row for u-shared.
	var got EmploymentIdentity
	if err := db.Where("tenant_id = ? AND user_subject_id = ?", "t-acme", "u-shared").First(&got).Error; err != nil {
		t.Fatalf("First by (tenant,subject): %v", err)
	}
	if got.TenantID != "t-acme" || got.UserSubjectID != "u-shared" {
		t.Errorf("composite lookup returned wrong row: tenant=%q subject=%q", got.TenantID, got.UserSubjectID)
	}

	// Count rows per (tenant, subject): u-shared appears in 2 tenants.
	var sharedCount int64
	if err := db.Model(&EmploymentIdentity{}).Where("user_subject_id = ?", "u-shared").Count(&sharedCount).Error; err != nil {
		t.Fatalf("count u-shared: %v", err)
	}
	if sharedCount != 2 {
		t.Errorf("u-shared across tenants: got %d, want 2", sharedCount)
	}

	// Sanity: filter by (tenant_id='default', user_subject_id='u-shared') = 1.
	var defaultSharedCount int64
	if err := db.Model(&EmploymentIdentity{}).
		Where("tenant_id = ? AND user_subject_id = ?", "default", "u-shared").
		Count(&defaultSharedCount).Error; err != nil {
		t.Fatalf("count default+u-shared: %v", err)
	}
	if defaultSharedCount != 1 {
		t.Errorf("(default, u-shared) count: got %d, want 1", defaultSharedCount)
	}
}
