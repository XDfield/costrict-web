//go:build cgo

package models

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newEmploymentIdentityDB opens an in-memory sqlite DB, AutoMigrates the
// EmploymentIdentity struct, and recreates the partial unique index from the
// migration (sqlite supports partial indexes since 3.8.0 — well below what
// modern Go ships with). This lets us test the soft-delete-then-reinsert
// contract the production Postgres migration enforces.
//
// We can't rely on gorm AutoMigrate for the partial unique because the gorm
// tag form doesn't carry a WHERE clause; the production migration owns it.
func newEmploymentIdentityDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&EmploymentIdentity{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_employment_identities_user_subject_id
		ON employment_identities(user_subject_id) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("create partial unique index: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func ptr[T any](v T) *T { return &v }

// TestEmploymentIdentity_CRUDRoundTrip exercises the full lifecycle: create,
// read back, update a nullable field, delete. Verifies every column type
// round-trips through gorm — especially the nullable fields that callers will
// conditionally populate (manager refs, dates, attributes blob).
func TestEmploymentIdentity_CRUDRoundTrip(t *testing.T) {
	t.Parallel()
	db := newEmploymentIdentityDB(t)

	original := &EmploymentIdentity{
		UserSubjectID:            "usr_alice",
		Provider:                 "idtrust",
		EmployeeNumber:           ptr("E001"),
		CostCenter:               ptr("CC-1001"),
		OrgPath:                  ptr("/总部/研发/平台组"),
		DirectManagerSubjectID:   ptr("usr_bob"),
		DirectManagerExternalRef: nil,
		JobTitle:                 ptr("Senior Engineer"),
		JobLevel:                 ptr("P7"),
		EmploymentType:           ptr("full_time"),
		HireDate:                 ptr(time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)),
		RegularDate:              ptr(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)),
		WorkLocation:             ptr("上海-张江"),
		Attributes:               `{"department":"platform"}`,
	}
	if err := db.Create(original).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if original.ID == 0 {
		t.Fatal("ID not populated after Create")
	}

	// Read back by primary key.
	var got EmploymentIdentity
	if err := db.First(&got, original.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.UserSubjectID != original.UserSubjectID {
		t.Errorf("UserSubjectID: got %q, want %q", got.UserSubjectID, original.UserSubjectID)
	}
	if got.Provider != original.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, original.Provider)
	}
	if got.EmployeeNumber == nil || *got.EmployeeNumber != "E001" {
		t.Errorf("EmployeeNumber: got %+v, want E001", got.EmployeeNumber)
	}
	if got.DirectManagerExternalRef != nil {
		t.Errorf("DirectManagerExternalRef: got %+v, want nil", got.DirectManagerExternalRef)
	}
	if got.HireDate == nil || !got.HireDate.Equal(*original.HireDate) {
		t.Errorf("HireDate: got %+v, want %+v", got.HireDate, original.HireDate)
	}
	if got.Attributes != `{"department":"platform"}` {
		t.Errorf("Attributes: got %q, want JSON blob verbatim", got.Attributes)
	}

	// Update nullable field from nil → value, and a value → new value.
	if err := db.Model(&got).Updates(map[string]any{
		"direct_manager_external_ref": "cn=bob,dc=example,dc=com",
		"cost_center":                 "CC-2002",
	}).Error; err != nil {
		t.Fatalf("Updates: %v", err)
	}
	var updated EmploymentIdentity
	if err := db.First(&updated, original.ID).Error; err != nil {
		t.Fatalf("First after update: %v", err)
	}
	if updated.DirectManagerExternalRef == nil || *updated.DirectManagerExternalRef != "cn=bob,dc=example,dc=com" {
		t.Errorf("DirectManagerExternalRef after update: got %+v", updated.DirectManagerExternalRef)
	}
	if updated.CostCenter == nil || *updated.CostCenter != "CC-2002" {
		t.Errorf("CostCenter after update: got %+v, want CC-2002", updated.CostCenter)
	}

	// Soft-delete: row stays in the table but First through default scope
	// must return ErrRecordNotFound.
	if err := db.Delete(&updated).Error; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var gone EmploymentIdentity
	if err := db.First(&gone, original.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("First after soft-delete: got err=%v, want ErrRecordNotFound", err)
	}
	// Unscoped query still finds the row.
	var stillThere EmploymentIdentity
	if err := db.Unscoped().First(&stillThere, original.ID).Error; err != nil {
		t.Fatalf("Unscoped First after soft-delete: %v", err)
	}
	if !stillThere.DeletedAt.Valid {
		t.Error("DeletedAt should be set after soft-delete")
	}
}

// TestEmploymentIdentity_Defaults asserts the NOT NULL columns with explicit
// defaults (sync_status='fresh', attributes='{}', timestamps) populate
// correctly when the caller omits them.
func TestEmploymentIdentity_Defaults(t *testing.T) {
	t.Parallel()
	db := newEmploymentIdentityDB(t)

	zero := &EmploymentIdentity{
		UserSubjectID: "usr_defaults",
		Provider:      "idtrust",
	}
	if err := db.Create(zero).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got EmploymentIdentity
	if err := db.First(&got, zero.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.SyncStatus != "fresh" {
		t.Errorf("SyncStatus default: got %q, want fresh", got.SyncStatus)
	}
	if got.Attributes != "{}" {
		t.Errorf("Attributes default: got %q, want {}", got.Attributes)
	}
	if got.LastSyncedAt.IsZero() {
		t.Error("LastSyncedAt should default to a non-zero timestamp")
	}
	if got.NextSyncDueAt.IsZero() {
		t.Error("NextSyncDueAt should default to a non-zero timestamp")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should default to a non-zero timestamp")
	}
}

// TestEmploymentIdentity_UserSubjectIDUniqueAfterDelete pins the soft-delete
// contract the migration's partial unique index protects: at most one
// non-deleted snapshot row per user. A second insert with the same
// user_subject_id must fail; soft-deleting the first must allow the second to
// succeed so a re-onboarded user gets a fresh row.
func TestEmploymentIdentity_UserSubjectIDUniqueAfterDelete(t *testing.T) {
	t.Parallel()
	db := newEmploymentIdentityDB(t)

	first := &EmploymentIdentity{UserSubjectID: "usr_rehire", Provider: "idtrust"}
	if err := db.Create(first).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}

	// Second active row for the same user — must fail.
	second := &EmploymentIdentity{UserSubjectID: "usr_rehire", Provider: "idtrust"}
	if err := db.Create(second).Error; err == nil {
		t.Fatal("expected unique-constraint failure on duplicate user_subject_id, got nil")
	} else if !isUniqueViolation(err) {
		t.Fatalf("expected unique-constraint failure, got: %v", err)
	}

	// Soft-delete the first, then the second insert must succeed.
	if err := db.Delete(first).Error; err != nil {
		t.Fatalf("soft-delete first: %v", err)
	}
	third := &EmploymentIdentity{UserSubjectID: "usr_rehire", Provider: "aad"}
	if err := db.Create(third).Error; err != nil {
		t.Fatalf("create after soft-delete should succeed, got: %v", err)
	}
	if third.ID == 0 {
		t.Fatal("re-onboard row ID not populated")
	}
}

// isUniqueViolation is a small cross-driver check: sqlite returns the literal
// "UNIQUE constraint failed" substring; Postgres would surface a pgconn error
// we can unwrap via errors.Is(errors.As). For the in-memory test boundary the
// substring match is sufficient.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
