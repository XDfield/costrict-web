package user

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// ptr is a small helper for nullable string columns in seed rows.
func sptr(s string) *string { return &s }

func seedUser(t *testing.T, db *gorm.DB, subjectID, username, org, status string) {
	t.Helper()
	u := models.User{
		SubjectID:    subjectID,
		Username:     username,
		Organization: sptr(org),
		IsActive:     true,
		Status:       status,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed user %s: %v", subjectID, err)
	}
}

// ---------------------------------------------------------------------------
// GetUserStatus: middleware-facing, fail-open on blank/missing
// ---------------------------------------------------------------------------

func TestGetUserStatus_DefaultsActiveForBlank(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)
	// Create a user row with an explicitly blank status to mimic a pre-migration
	// / legacy row.
	u := models.User{SubjectID: "usr_blank", Username: "blank", IsActive: true, Status: ""}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := svc.GetUserStatus("usr_blank")
	if err != nil {
		t.Fatalf("GetUserStatus: %v", err)
	}
	if got != UserStatusActive {
		t.Fatalf("blank status should resolve to active, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// GetUserProfile + RolesForUsers: aggregation across tables
// (identity + status themselves are proxied to cs-user via RPC; these local
// helpers compute only the activity counts + system roles that live in
// costrict_db.)
// ---------------------------------------------------------------------------

// setupProfileTables hand-creates the activity tables (their postgres uuid
// defaults don't AutoMigrate cleanly on sqlite). Mirrors enterprise_test setup.
func setupProfileTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
			created_by TEXT
		)`,
		`CREATE TABLE item_distributions (
			id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
			distributor_id TEXT
		)`,
		`CREATE TABLE item_distribution_receipts (
			id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
			user_id TEXT
		)`,
		`CREATE TABLE user_system_roles (
			id TEXT PRIMARY KEY,
			user_id TEXT,
			role TEXT,
			created_at DATETIME,
			deleted_at DATETIME
		)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create profile table: %v", err)
		}
	}
}

func TestGetUserProfile_Aggregates(t *testing.T) {
	db := setupUserTestDB(t)
	setupProfileTables(t, db)
	svc := NewUserService(db)

	subject := "usr_profile"
	// 2 created items, 1 distributed, 3 received; plus noise from another user.
	db.Exec(`INSERT INTO capability_items (created_by) VALUES (?), (?), (?)`, subject, subject, "usr_other")
	db.Exec(`INSERT INTO item_distributions (distributor_id) VALUES (?), (?)`, subject, "usr_other")
	db.Exec(`INSERT INTO item_distribution_receipts (user_id) VALUES (?), (?), (?), (?)`, subject, subject, subject, "usr_other")

	profile, err := svc.GetUserProfile(subject)
	if err != nil {
		t.Fatalf("GetUserProfile: %v", err)
	}
	if profile.CreatedItemCount != 2 {
		t.Errorf("created = %d, want 2", profile.CreatedItemCount)
	}
	if profile.DistributedCount != 1 {
		t.Errorf("distributed = %d, want 1", profile.DistributedCount)
	}
	if profile.ReceivedCount != 3 {
		t.Errorf("received = %d, want 3", profile.ReceivedCount)
	}
}

func TestRolesForUsers_BatchAndSkipsDeleted(t *testing.T) {
	db := setupUserTestDB(t)
	setupProfileTables(t, db)
	svc := NewUserService(db)

	now := time.Now()
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES (?,?,?,?,NULL)`,
		"r1", "usr_a", "platform_admin", now)
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES (?,?,?,?,NULL)`,
		"r2", "usr_a", "business_admin", now.Add(time.Second))
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES (?,?,?,?,?)`,
		"r3", "usr_b", "platform_admin", now, now) // soft-deleted

	got := svc.RolesForUsers([]string{"usr_a", "usr_b", "usr_c"})
	if len(got["usr_a"]) != 2 {
		t.Fatalf("usr_a roles = %v, want 2", got["usr_a"])
	}
	if len(got["usr_b"]) != 0 {
		t.Fatalf("usr_b soft-deleted role should be skipped, got %v", got["usr_b"])
	}
	if _, ok := got["usr_c"]; ok {
		t.Fatalf("usr_c has no roles, should be absent")
	}
}
