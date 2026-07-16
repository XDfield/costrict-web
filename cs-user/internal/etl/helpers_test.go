//go:build cgo

package etl

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// now returns a UTC timestamp truncated to microsecond precision, matching
// sqlite's storage resolution. Tests that compare timestamps loaded back
// from the DB need this to avoid spurious diff failures.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// newDB opens an in-memory sqlite DB and AutoMigrates the cs-user schema.
// Used by ETL integration tests as a stand-in for both source and target
// Postgres — sqlite's ON CONFLICT / soft-delete semantics are close enough
// to PG's for the field-level write logic this package tests; the postgres-
// only paths (advisory lock, ON CONFLICT WHERE) are exercised separately
// by the standalone migrate binary against a real PG.
func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return db
}

// seedUser inserts a user via Unscoped so the soft-delete column is honored
// when DeletedAt.Valid is set. gorm's default Create drops the row into the
// soft-delete index correctly, but Unscoped + explicit set on DeletedAt.Time
// ensures the row is visible to subsequent Unscoped reads in the same way
// it would be in PG.
func seedUser(t *testing.T, db *gorm.DB, u *models.User) {
	t.Helper()
	if err := db.Unscoped().Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func seedAuthIdentity(t *testing.T, db *gorm.DB, ai *models.UserAuthIdentity) {
	t.Helper()
	if err := db.Unscoped().Create(ai).Error; err != nil {
		t.Fatalf("seed auth identity: %v", err)
	}
}

// freshUser builds a minimal valid User with the given subject_id.
// Pointer fields default to nil; override in caller.
func freshUser(subjectID, username string) *models.User {
	return &models.User{
		SubjectID: subjectID,
		Username:  username,
		IsActive:  true,
		Status:    "active",
	}
}

func freshAuthIdentity(externalKey, subjectID, provider string) *models.UserAuthIdentity {
	return &models.UserAuthIdentity{
		ExternalKey:   externalKey,
		UserSubjectID: subjectID,
		Provider:      provider,
	}
}
