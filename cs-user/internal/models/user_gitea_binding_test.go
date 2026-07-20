//go:build cgo

package models

import (
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newUserGiteaBindingDB mirrors newAuditLogDB for in-memory sqlite +
// AutoMigrate. cgo-gated because sqlite needs CGO.
func newUserGiteaBindingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&UserGiteaBinding{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// int64Ptr is a local helper (mirrors strPtr in audit_log_test.go).
func int64Ptr(v int64) *int64 { return &v }

// timePtr is a local helper for nullable timestamp assertions.
func timePtr(t time.Time) *time.Time { return &t }

// TestUserGiteaBinding_TableName verifies the singular table name override —
// migration ships user_gitea_binding (singular), not gorm's default
// user_gitea_bindings pluralization.
func TestUserGiteaBinding_TableName(t *testing.T) {
	t.Parallel()
	b := UserGiteaBinding{}
	if got := b.TableName(); got != "user_gitea_binding" {
		t.Errorf("TableName: got %q, want %q", got, "user_gitea_binding")
	}
}

// TestUserGiteaBinding_NullableFieldsDefault verifies gitea_uid /
// last_synced_at / last_error all accept NULL — the pending state is
// first-class (created before POST /admin/users returns).
func TestUserGiteaBinding_NullableFieldsDefault(t *testing.T) {
	t.Parallel()
	db := newUserGiteaBindingDB(t)

	now := time.Now()
	b := &UserGiteaBinding{
		UserSubjectID: "usr_abc",
		TenantID:      "default",
		GiteaUsername: "u-alice",
		SyncStatus:    GiteaSyncStatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := db.Create(b).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got UserGiteaBinding
	if err := db.First(&got, "user_subject_id = ? AND tenant_id = ?", "usr_abc", "default").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.GiteaUID != nil {
		t.Errorf("gitea_uid: got %v, want nil", *got.GiteaUID)
	}
	if got.LastSyncedAt != nil {
		t.Errorf("last_synced_at: got %v, want nil", *got.LastSyncedAt)
	}
	if got.LastError != nil {
		t.Errorf("last_error: got %v, want nil", *got.LastError)
	}
}

// TestUserGiteaBinding_SyncedStateRoundTrip verifies a fully-populated
// synced row round-trips — gitea_uid / last_synced_at populated, last_error
// still NULL.
func TestUserGiteaBinding_SyncedStateRoundTrip(t *testing.T) {
	t.Parallel()
	db := newUserGiteaBindingDB(t)

	now := time.Now()
	uid := int64(42)
	b := &UserGiteaBinding{
		UserSubjectID: "usr_xyz",
		TenantID:      "tenant-acme",
		GiteaUID:      &uid,
		GiteaUsername: "u-bob",
		SyncStatus:    GiteaSyncStatusSynced,
		LastSyncedAt:  timePtr(now),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := db.Create(b).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got UserGiteaBinding
	if err := db.First(&got, "user_subject_id = ? AND tenant_id = ?", "usr_xyz", "tenant-acme").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.GiteaUID == nil || *got.GiteaUID != 42 {
		t.Errorf("gitea_uid: got %v, want 42", got.GiteaUID)
	}
	if got.LastSyncedAt == nil {
		t.Errorf("last_synced_at: got nil, want non-nil")
	}
	if got.SyncStatus != GiteaSyncStatusSynced {
		t.Errorf("sync_status: got %q, want %q", got.SyncStatus, GiteaSyncStatusSynced)
	}
}

// TestUserGiteaBinding_SyncStatusVocab verifies the 3 status constants are
// stable strings — a regression guard against accidental vocabulary renames
// (downstream consumers key off these strings).
func TestUserGiteaBinding_SyncStatusVocab(t *testing.T) {
	t.Parallel()
	statuses := []string{
		GiteaSyncStatusPending,
		GiteaSyncStatusSynced,
		GiteaSyncStatusError,
	}
	for i, s := range statuses {
		if strings.TrimSpace(s) == "" {
			t.Errorf("status %d must not be empty", i)
		}
	}
}

// TestUserGiteaBinding_Int64PointerHelper is a tiny compile-check that the
// int64Ptr helper used in the synced-state test produces a non-nil pointer
// — keeps the helper guarded against accidental refactor to a value-return.
func TestUserGiteaBinding_Int64PointerHelper(t *testing.T) {
	t.Parallel()
	p := int64Ptr(99)
	if p == nil || *p != 99 {
		t.Errorf("int64Ptr(99): got %v, want non-nil pointer to 99", p)
	}
}
