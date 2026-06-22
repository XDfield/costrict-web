package notification

import (
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupBroadcastTestDB creates users + system_notifications tables (the
// announcement broadcast path writes per-user in-app notifications). Hand-rolled
// schema mirrors the postgres columns the code touches.
func setupBroadcastTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	stmts := []string{
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			subject_id TEXT,
			username TEXT NOT NULL,
			organization TEXT,
			is_active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE system_notifications (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			type TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			title TEXT NOT NULL,
			content TEXT,
			session_id TEXT,
			device_id TEXT,
			workspace_id TEXT,
			action_type TEXT,
			action_data TEXT DEFAULT '{}',
			action_token TEXT,
			action_result TEXT,
			card_data TEXT,
			acted_at DATETIME,
			expires_at DATETIME,
			created_at DATETIME,
			read_at DATETIME,
			deleted_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("migrate test db: %v", err)
		}
	}
	seed := []struct{ id, subject, org string }{
		{"1", "u1", "acme"},
		{"2", "u2", "acme"},
		{"3", "u3", "globex"},
	}
	for _, s := range seed {
		if err := db.Exec(`INSERT INTO users (id, subject_id, username, organization, is_active) VALUES (?, ?, ?, ?, ?)`,
			s.id, s.subject, s.subject, s.org, true).Error; err != nil {
			t.Fatalf("seed user %s: %v", s.subject, err)
		}
	}
	return db
}

func countNotifications(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.SystemNotification{}).Count(&n).Error; err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

func TestBroadcast_AllScope(t *testing.T) {
	db := setupBroadcastTestDB(t)
	svc := NewNotificationService(db, "")

	sent, err := svc.Broadcast(BroadcastScope{Type: "all"}, "Hi", "Body", false, "operator")
	if err != nil {
		t.Fatalf("Broadcast error: %v", err)
	}
	if sent != 3 {
		t.Fatalf("sentCount = %d, want 3", sent)
	}
	if got := countNotifications(t, db); got != 3 {
		t.Fatalf("in-app notifications = %d, want 3", got)
	}
}

func TestBroadcast_OrganizationScope(t *testing.T) {
	db := setupBroadcastTestDB(t)
	svc := NewNotificationService(db, "")

	sent, err := svc.Broadcast(BroadcastScope{Type: "organization", TargetID: "acme"}, "Hi", "Body", false, "operator")
	if err != nil {
		t.Fatalf("Broadcast error: %v", err)
	}
	if sent != 2 {
		t.Fatalf("sentCount = %d, want 2 (acme members)", sent)
	}
	if got := countNotifications(t, db); got != 2 {
		t.Fatalf("in-app notifications = %d, want 2", got)
	}
}

func TestBroadcast_UserScope(t *testing.T) {
	db := setupBroadcastTestDB(t)
	svc := NewNotificationService(db, "")

	sent, err := svc.Broadcast(BroadcastScope{Type: "user", TargetID: "u3"}, "Hi", "Body", false, "operator")
	if err != nil {
		t.Fatalf("Broadcast error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sentCount = %d, want 1", sent)
	}
	if got := countNotifications(t, db); got != 1 {
		t.Fatalf("in-app notifications = %d, want 1", got)
	}
}

func TestBroadcast_InvalidScope(t *testing.T) {
	db := setupBroadcastTestDB(t)
	svc := NewNotificationService(db, "")

	if _, err := svc.Broadcast(BroadcastScope{Type: "bogus"}, "Hi", "Body", false, "operator"); err == nil {
		t.Fatalf("expected error for invalid scope")
	}
	if _, err := svc.Broadcast(BroadcastScope{Type: "organization"}, "Hi", "Body", false, "operator"); err == nil {
		t.Fatalf("expected error for organization scope without targetId")
	}
}

func TestBroadcast_EmptyOrganizationReturnsZero(t *testing.T) {
	db := setupBroadcastTestDB(t)
	svc := NewNotificationService(db, "")

	sent, err := svc.Broadcast(BroadcastScope{Type: "organization", TargetID: "nonexistent"}, "Hi", "Body", false, "operator")
	if err != nil {
		t.Fatalf("Broadcast error: %v", err)
	}
	if sent != 0 {
		t.Fatalf("sentCount = %d, want 0 for empty org", sent)
	}
}

// TestAdminBroadcastAnnouncementHandler_RequiresAdmin verifies the HTTP route is
// guarded and that a platform admin can broadcast.
func TestAdminBroadcastAnnouncementHandler_RequiresAdmin(t *testing.T) {
	r, db := newNotificationTestRouter(t)
	// newNotificationTestRouter seeds users u1/u2 but not system_notifications;
	// create the table so the broadcast write path has somewhere to land.
	if err := db.Exec(`CREATE TABLE system_notifications (
		id TEXT PRIMARY KEY, user_id TEXT NOT NULL, type TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending', title TEXT NOT NULL, content TEXT,
		session_id TEXT, device_id TEXT, workspace_id TEXT, action_type TEXT,
		action_data TEXT DEFAULT '{}', action_token TEXT, action_result TEXT,
		card_data TEXT, acted_at DATETIME, expires_at DATETIME, created_at DATETIME,
		read_at DATETIME, deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create system_notifications: %v", err)
	}

	body := map[string]any{
		"scope":   map[string]any{"type": "user", "targetId": "u2"},
		"title":   "Maintenance",
		"content": "Down at midnight",
	}

	// Non-admin u2 → 403.
	w := performNotificationJSON(r, http.MethodPost, "/api/admin/announcements", "u2", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin: expected 403, got %d, body=%s", w.Code, w.Body.String())
	}

	svc := systemrole.NewSystemRoleService(db)
	if err := svc.GrantRole("u1", systemrole.SystemRolePlatformAdmin, "u1"); err != nil {
		t.Fatalf("grant platform admin: %v", err)
	}

	w = performNotificationJSON(r, http.MethodPost, "/api/admin/announcements", "u1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("admin broadcast: expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
}
