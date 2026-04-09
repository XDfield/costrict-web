package notification

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupNotificationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			subject_id TEXT,
			username TEXT NOT NULL,
			display_name TEXT,
			email TEXT,
			avatar_url TEXT,
			casdoor_id TEXT,
			casdoor_universal_id TEXT,
			casdoor_sub TEXT,
			organization TEXT,
			is_active BOOLEAN NOT NULL DEFAULT TRUE,
			last_login_at DATETIME,
			last_sync_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE user_system_roles (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			role TEXT NOT NULL,
			granted_by TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE UNIQUE INDEX uk_user_system_role ON user_system_roles(user_id, role)`,
		`CREATE TABLE system_notification_channels (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			name TEXT NOT NULL,
			workspace_id TEXT,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			system_config JSON,
			created_by TEXT NOT NULL,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE user_notification_channels (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			system_channel_id TEXT,
			channel_type TEXT NOT NULL,
			name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			user_config JSON,
			trigger_events TEXT,
			last_used_at DATETIME,
			last_error TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE notification_logs (
			id TEXT PRIMARY KEY,
			user_channel_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			channel_type TEXT NOT NULL,
			event_type TEXT NOT NULL,
			session_id TEXT,
			device_id TEXT,
			status TEXT NOT NULL,
			error TEXT,
			sent_at DATETIME,
			created_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("migrate test db: %v", err)
		}
	}
	if err := db.Exec(`INSERT INTO users (id, subject_id, username, is_active) VALUES (?, ?, ?, ?)`, 1, "u1", "u1", true).Error; err != nil {
		t.Fatalf("seed user u1: %v", err)
	}
	if err := db.Exec(`INSERT INTO users (id, subject_id, username, is_active) VALUES (?, ?, ?, ?)`, 2, "u2", "u2", true).Error; err != nil {
		t.Fatalf("seed user u2: %v", err)
	}
	return db
}

func newNotificationTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	db := setupNotificationTestDB(t)
	module := New(db, "")
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if userID := c.GetHeader("X-User-ID"); userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	})
	module.RegisterRoutes(api)
	return r, db
}

func performNotificationJSON(r *gin.Engine, method, path, userID string, body any) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListAvailableTypesIncludesSupportedEvents(t *testing.T) {
	r, _ := newNotificationTestRouter(t)
	w := performNotificationJSON(r, http.MethodGet, "/api/notification-channels/available", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		SupportedEvents []string `json:"supportedEvents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.SupportedEvents) == 0 {
		t.Fatalf("expected supported events, got empty response: %s", w.Body.String())
	}
	found := false
	for _, event := range resp.SupportedEvents {
		if event == EventProjectInvitationCreated {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected %s in supported events: %+v", EventProjectInvitationCreated, resp.SupportedEvents)
	}
}

func TestCreateMyChannelRejectsUnsupportedTriggerEvent(t *testing.T) {
	r, _ := newNotificationTestRouter(t)
	w := performNotificationJSON(r, http.MethodPost, "/api/notification-channels", "u1", map[string]any{
		"channelType":   "webhook",
		"name":          "my webhook",
		"userConfig":    map[string]any{"webhookUrl": "https://example.com/hook"},
		"triggerEvents": []string{"unsupported.event"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestAdminNotificationRoutesRequirePlatformAdmin(t *testing.T) {
	r, db := newNotificationTestRouter(t)

	w := performNotificationJSON(r, http.MethodGet, "/api/admin/notification-channels", "u2", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d, body=%s", w.Code, w.Body.String())
	}

	svc := systemrole.NewSystemRoleService(db)
	if err := svc.GrantRole("u1", systemrole.SystemRolePlatformAdmin, "u1"); err != nil {
		t.Fatalf("grant platform admin: %v", err)
	}

	w = performNotificationJSON(r, http.MethodGet, "/api/admin/notification-channels", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestGetWorkspaceIDNormalizesWindowsPath(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	stmts := []string{
		`CREATE TABLE devices (
			id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE workspaces (
			id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE workspace_directories (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			path TEXT NOT NULL,
			deleted_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("migrate test db: %v", err)
		}
	}

	if err := db.Exec(`INSERT INTO devices (id, device_id) VALUES (?, ?)`, "dev-uuid-1", "device-1").Error; err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if err := db.Exec(`INSERT INTO workspaces (id, device_id) VALUES (?, ?)`, "ws-1", "dev-uuid-1").Error; err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := db.Exec(`INSERT INTO workspace_directories (id, workspace_id, path) VALUES (?, ?, ?)`, "wd-1", "ws-1", "D:/DEV/myclaw").Error; err != nil {
		t.Fatalf("seed workspace directory: %v", err)
	}

	svc := NewNotificationService(db, "")
	workspaceID, err := svc.getWorkspaceID("device-1", `D:\DEV\myclaw`)
	if err != nil {
		t.Fatalf("get workspace id: %v", err)
	}
	if workspaceID != "ws-1" {
		t.Fatalf("expected workspace id ws-1, got %s", workspaceID)
	}
}
