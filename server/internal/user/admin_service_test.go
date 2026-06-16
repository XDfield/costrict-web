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
// ListUsers: pagination, search, status filter
// ---------------------------------------------------------------------------

func TestListUsers_PaginationAndTotal(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	for i, name := range []string{"alice", "bob", "carol", "dave", "erin"} {
		seedUser(t, db, "usr_"+name, name, "org_a", UserStatusActive)
		_ = i
	}

	users, total, err := svc.ListUsers(ListUsersParams{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(users) != 2 {
		t.Fatalf("page size = %d, want 2", len(users))
	}

	page3, _, err := svc.ListUsers(ListUsersParams{Page: 3, PageSize: 2})
	if err != nil {
		t.Fatalf("ListUsers page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("last page size = %d, want 1", len(page3))
	}
}

func TestListUsers_SearchKeyword(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	seedUser(t, db, "usr_alice", "alice", "org_a", UserStatusActive)
	seedUser(t, db, "usr_bob", "bob", "org_a", UserStatusActive)

	users, total, err := svc.ListUsers(ListUsersParams{Keyword: "ali"})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if total != 1 || len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("keyword filter wrong: total=%d users=%v", total, users)
	}
}

func TestListUsers_StatusFilterIncludesBanned(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	seedUser(t, db, "usr_active", "active_user", "org_a", UserStatusActive)
	seedUser(t, db, "usr_banned", "banned_user", "org_a", UserStatusBanned)

	// Admin list (no status filter) must surface banned users too (unlike SearchUsers).
	all, total, err := svc.ListUsers(ListUsersParams{})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if total != 2 || len(all) != 2 {
		t.Fatalf("unfiltered list should include banned: total=%d len=%d", total, len(all))
	}

	banned, total, err := svc.ListUsers(ListUsersParams{Status: UserStatusBanned})
	if err != nil {
		t.Fatalf("ListUsers banned: %v", err)
	}
	if total != 1 || len(banned) != 1 || banned[0].Username != "banned_user" {
		t.Fatalf("status filter wrong: total=%d users=%v", total, banned)
	}
}

// ---------------------------------------------------------------------------
// SetUserStatus: success, invalid value, self-lock rejection, not found
// ---------------------------------------------------------------------------

func TestSetUserStatus_Success(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)
	seedUser(t, db, "usr_target", "target", "org_a", UserStatusActive)

	if err := svc.SetUserStatus("usr_target", UserStatusBanned, "usr_admin"); err != nil {
		t.Fatalf("SetUserStatus: %v", err)
	}

	got, err := svc.GetUserStatus("usr_target")
	if err != nil {
		t.Fatalf("GetUserStatus: %v", err)
	}
	if got != UserStatusBanned {
		t.Fatalf("status = %q, want banned", got)
	}
}

func TestSetUserStatus_InvalidValue(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)
	seedUser(t, db, "usr_target", "target", "org_a", UserStatusActive)

	if err := svc.SetUserStatus("usr_target", "nonsense", "usr_admin"); err != ErrInvalidUserStatus {
		t.Fatalf("expected ErrInvalidUserStatus, got %v", err)
	}
}

func TestSetUserStatus_RejectsSelfLock(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)
	seedUser(t, db, "usr_admin", "admin", "org_a", UserStatusActive)

	if err := svc.SetUserStatus("usr_admin", UserStatusBanned, "usr_admin"); err != ErrCannotChangeOwnStatus {
		t.Fatalf("expected ErrCannotChangeOwnStatus, got %v", err)
	}

	// status must remain unchanged after a rejected self-lock.
	got, _ := svc.GetUserStatus("usr_admin")
	if got != UserStatusActive {
		t.Fatalf("self-lock should not change status, got %q", got)
	}
}

func TestSetUserStatus_NotFound(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	if err := svc.SetUserStatus("usr_ghost", UserStatusDisabled, "usr_admin"); err != ErrAdminUserNotFound {
		t.Fatalf("expected ErrAdminUserNotFound, got %v", err)
	}
}

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
// ListOrganizations: GROUP BY + count, skips empty org
// ---------------------------------------------------------------------------

func TestListOrganizations_RollUp(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	seedUser(t, db, "usr_1", "u1", "org_big", UserStatusActive)
	seedUser(t, db, "usr_2", "u2", "org_big", UserStatusActive)
	seedUser(t, db, "usr_3", "u3", "org_big", UserStatusActive)
	seedUser(t, db, "usr_4", "u4", "org_small", UserStatusActive)
	seedUser(t, db, "usr_5", "u5", "", UserStatusActive) // empty org — skipped

	orgs, err := svc.ListOrganizations()
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("org count = %d, want 2 (empty org skipped)", len(orgs))
	}
	// busiest first
	if orgs[0].Organization != "org_big" || orgs[0].MemberCount != 3 {
		t.Fatalf("first org wrong: %+v", orgs[0])
	}
	if orgs[1].Organization != "org_small" || orgs[1].MemberCount != 1 {
		t.Fatalf("second org wrong: %+v", orgs[1])
	}
}

// ---------------------------------------------------------------------------
// GetUserProfile + RolesForUsers: aggregation across tables
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
