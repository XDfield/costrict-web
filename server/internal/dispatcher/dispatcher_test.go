package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/notification"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	db.Exec(`CREATE TABLE channel_configs (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		channel_type TEXT NOT NULL,
		name TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT TRUE,
		config TEXT DEFAULT '{}',
		webhook_verified BOOLEAN NOT NULL DEFAULT FALSE,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`)

	db.Exec(`CREATE TABLE user_auth_identities (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_subject_id TEXT NOT NULL,
		provider TEXT NOT NULL,
		issuer TEXT DEFAULT '',
		external_key TEXT NOT NULL UNIQUE,
		external_subject TEXT DEFAULT '',
		external_user_id TEXT DEFAULT '',
		provider_user_id TEXT DEFAULT '',
		display_name TEXT DEFAULT '',
		email TEXT DEFAULT '',
		phone TEXT DEFAULT '',
		avatar_url TEXT DEFAULT '',
		organization TEXT DEFAULT '',
		is_primary BOOLEAN NOT NULL DEFAULT FALSE,
		explicitly_unbound BOOLEAN NOT NULL DEFAULT FALSE,
		last_login_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`)

	return db
}

func setupNotificationTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec(`CREATE TABLE system_notifications (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		title TEXT NOT NULL DEFAULT '',
		content TEXT DEFAULT '',
		session_id TEXT DEFAULT '',
		device_id TEXT DEFAULT '',
		workspace_id TEXT DEFAULT '',
		action_type TEXT DEFAULT '',
		action_data TEXT DEFAULT '{}',
		action_token TEXT DEFAULT '' UNIQUE,
		action_result TEXT DEFAULT '{}',
		card_data TEXT DEFAULT '{}',
		acted_at DATETIME,
		expires_at DATETIME,
		created_at DATETIME,
		read_at DATETIME,
		deleted_at DATETIME
	)`)
}

func insertIDTrustIdentity(t *testing.T, db *gorm.DB, userSubjectID, providerUserID string) {
	t.Helper()
	db.Exec(`INSERT INTO user_auth_identities
		(user_subject_id, provider, external_key, provider_user_id, is_primary, created_at, updated_at)
		VALUES (?, 'idtrust', ?, ?, true, datetime('now'), datetime('now'))`,
		userSubjectID, "idtrust:"+providerUserID, providerUserID)
}

func TestNewDispatcher(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil, false, false, nil, nil, 30*time.Second)
	if d == nil {
		t.Fatal("expected non-nil dispatcher")
	}
}

func TestDispatcher_Dispatch_UnsupportedEvent(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, notification.NewStore(db), "http://localhost:3000", nil, nil, false, false, nil, nil, 30*time.Second)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "unknown",
		SessionID: "session-1",
		DeviceID:  "device-1",
	})
}

func TestDispatcher_Dispatch_SessionEvent_Immediate(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", nil, nil, false, false, nil, nil, 30*time.Second)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "session.completed",
		SessionID: "session-3",
		DeviceID:  "device-1",
	})
}

func TestResolveWeComUserID(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil, false, false, nil, nil, 30*time.Second)

	// No identity → empty
	if got := d.resolveWeComUserID("user-none"); got != "" {
		t.Fatalf("expected empty for non-existent user, got %q", got)
	}

	// Insert IDTrust identity
	insertIDTrustIdentity(t, db, "user-1", "zhangsan")

	got := d.resolveWeComUserID("user-1")
	if got != "zhangsan" {
		t.Fatalf("expected 'zhangsan', got %q", got)
	}

	// Non-idtrust provider should not match
	db.Exec(`INSERT INTO user_auth_identities
		(user_subject_id, provider, external_key, is_primary, created_at, updated_at)
		VALUES ('user-1', 'github', 'gh:test', false, datetime('now'), datetime('now'))`)

	// Should still return the idtrust one
	got = d.resolveWeComUserID("user-1")
	if got != "zhangsan" {
		t.Fatalf("expected 'zhangsan' with multiple providers, got %q", got)
	}
}

func TestExtractQuestionInfos(t *testing.T) {
	tests := []struct {
		name       string
		actionData map[string]any
		wantCount  int
		wantFirst  questionInfo
	}{
		{
			name:       "nil data",
			actionData: nil,
			wantCount:  0,
		},
		{
			name:       "no questions key",
			actionData: map[string]any{"other": 1},
			wantCount:  0,
		},
		{
			name: "single question",
			actionData: map[string]any{
				"questions": []any{
					map[string]any{
						"question": "继续？",
						"header":   "确认",
						"options": []any{
							map[string]any{"label": "是"},
							map[string]any{"label": "否"},
						},
					},
				},
			},
			wantCount: 1,
			wantFirst: questionInfo{
				Question: "继续？",
				Header:   "确认",
				Options:  []questionOption{{Label: "是"}, {Label: "否"}},
			},
		},
		{
			name: "multiple questions",
			actionData: map[string]any{
				"questions": []any{
					map[string]any{
						"question": "Q1?",
						"options":  []any{map[string]any{"label": "A"}},
					},
					map[string]any{
						"question": "Q2?",
						"options":  []any{map[string]any{"label": "B"}},
						"multiple": true,
					},
				},
			},
			wantCount: 2,
			wantFirst: questionInfo{
				Question: "Q1?",
				Options:  []questionOption{{Label: "A"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractQuestionInfos(tt.actionData)
			if len(got) != tt.wantCount {
				t.Fatalf("expected %d questions, got %d", tt.wantCount, len(got))
			}
			if tt.wantCount > 0 {
				if got[0].Question != tt.wantFirst.Question {
					t.Errorf("question = %q, want %q", got[0].Question, tt.wantFirst.Question)
				}
				if got[0].Header != tt.wantFirst.Header {
					t.Errorf("header = %q, want %q", got[0].Header, tt.wantFirst.Header)
				}
				if len(got[0].Options) != len(tt.wantFirst.Options) {
					t.Errorf("options count = %d, want %d", len(got[0].Options), len(tt.wantFirst.Options))
				}
			}
		})
	}
}

func TestMapEventTypeToTitle(t *testing.T) {
	tests := []struct {
		eventType string
		want      string
	}{
		{"session.completed", "会话已完成"},
		{"session.failed", "会话失败"},
		{"session.aborted", "会话已中断"},
		{"permission", "权限请求"},
		{"question", "问题"},
		{"idle", "空闲超时"},
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			if got := mapEventTypeToTitle(tt.eventType); got != tt.want {
				t.Errorf("mapEventTypeToTitle(%q) = %q, want %q", tt.eventType, got, tt.want)
			}
		})
	}
}

func TestNeedsInteraction(t *testing.T) {
	if !needsInteraction("permission") {
		t.Error("expected permission to need interaction")
	}
	if !needsInteraction("question") {
		t.Error("expected question to need interaction")
	}
	if needsInteraction("session.completed") {
		t.Error("expected session.completed to not need interaction")
	}
}

func TestDispatcher_NilStore(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil, false, false, nil, nil, 30*time.Second)

	// Should not panic with nil store
	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-1",
		DeviceID:  "device-1",
	})
}

func TestIsDeferrable(t *testing.T) {
	if !isDeferrable("permission") {
		t.Error("expected permission to be deferrable")
	}
	if !isDeferrable("permission_batch") {
		t.Error("expected permission_batch to be deferrable")
	}
	if !isDeferrable("question") {
		t.Error("expected question to be deferrable")
	}
	if isDeferrable("session.completed") {
		t.Error("expected session.completed to not be deferrable")
	}
	if isDeferrable("idle") {
		t.Error("expected idle to not be deferrable")
	}
}

func TestDispatcher_DeferredTimer_Cancel(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	store := notification.NewStore(db)
	// Use a short delay so the test runs fast
	d := NewDispatcher(db, nil, store, "http://localhost:3000", nil, nil, false, false, nil, nil, 5*time.Second)

	// AI handler returns true — event is "handled" by AI, so deferred timer starts
	d.SetAIEventHandler(func(ctx context.Context, userID, eventType, sessionID, deviceID, path string, actionData map[string]any) bool {
		return true
	})

	// Dispatch a permission event — should start a deferred timer
	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-defer-1",
		DeviceID:  "device-1",
	})

	// Verify the timer exists
	d.deferredMu.Lock()
	_, hasTimer := d.deferredTimers["session-defer-1"]
	d.deferredMu.Unlock()
	if !hasTimer {
		t.Fatal("expected deferred timer to be set for session-defer-1")
	}

	// Cancel the deferred notification (simulates user replying before timer fires)
	d.CancelDeferredNotification("session-defer-1")

	// Verify the timer was cancelled
	d.deferredMu.Lock()
	_, hasTimer = d.deferredTimers["session-defer-1"]
	d.deferredMu.Unlock()
	if hasTimer {
		t.Fatal("expected deferred timer to be cancelled for session-defer-1")
	}
}

func TestDispatcher_DeferredTimer_ZeroDelay(t *testing.T) {
	db := setupTestDB(t)
	// Zero delay should default to 30s
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil, false, false, nil, nil, 0)
	if d.notificationDelay != 30*time.Second {
		t.Fatalf("expected 30s default, got %v", d.notificationDelay)
	}
}

func TestDispatcher_NonDeferrableEvent_Immediate(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", nil, nil, false, false, nil, nil, 30*time.Second)

	// Dispatch a non-deferrable event — should NOT start a deferred timer
	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "session.completed",
		SessionID: "session-immediate-1",
		DeviceID:  "device-1",
	})

	// No deferred timer should exist for session.completed
	d.deferredMu.Lock()
	_, hasTimer := d.deferredTimers["session-immediate-1"]
	d.deferredMu.Unlock()
	if hasTimer {
		t.Fatal("expected no deferred timer for non-deferrable event")
	}
}
