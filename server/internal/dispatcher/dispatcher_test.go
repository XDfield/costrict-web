package dispatcher

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
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
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", 60, nil)
	if d == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	if d.bufferPeriod != 60*1e9 {
		t.Fatalf("expected bufferPeriod 60s, got %v", d.bufferPeriod)
	}
}

func TestNewDispatcher_ZeroBuffer(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", 0, nil)
	if d.bufferPeriod != 0 {
		t.Fatalf("expected bufferPeriod 0, got %v", d.bufferPeriod)
	}
}

func TestDispatcher_Dispatch_UnsupportedEvent(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, notification.NewStore(db), "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "unknown",
		SessionID: "session-1",
		DeviceID:  "device-1",
	})
}

func TestDispatcher_Dispatch_Permission_Buffered(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-1",
		DeviceID:  "device-1",
		ActionData: map[string]any{
			"toolName": "bash",
		},
	})

	count := 0
	d.pendingMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 1 {
		t.Fatalf("expected 1 pending entry, got %d", count)
	}
}

func TestDispatcher_Dispatch_SameSessionMultipleEvents(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-1",
		DeviceID:  "device-1",
		ActionData: map[string]any{
			"toolName": "bash",
		},
	})

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-1",
		DeviceID:  "device-1",
		ActionData: map[string]any{
			"toolName": "read_file",
		},
	})

	count := 0
	d.pendingMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 2 {
		t.Fatalf("expected 2 pending entries for same session, got %d", count)
	}
}

func TestDispatcher_Dispatch_Question_Buffered(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-2",
		EventType: "question",
		SessionID: "session-2",
		DeviceID:  "device-2",
		ActionData: map[string]any{
			"questions": []any{
				map[string]any{
					"question": "继续吗？",
					"header":   "确认",
					"options": []any{
						map[string]any{"label": "是", "description": "继续执行"},
						map[string]any{"label": "否", "description": "取消执行"},
					},
				},
			},
		},
	})

	count := 0
	d.pendingMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 1 {
		t.Fatalf("expected 1 pending entry, got %d", count)
	}
}

func TestDispatcher_Dispatch_SessionEvent_Immediate(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "session.completed",
		SessionID: "session-3",
		DeviceID:  "device-1",
	})

	count := 0
	d.pendingMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("expected 0 pending entries, got %d", count)
	}
}

func TestDispatcher_OnInterventionResponse(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-4",
		DeviceID:  "device-1",
		ActionData: map[string]any{
			"toolName": "bash",
		},
	})

	var actionToken string
	d.pendingMap.Range(func(key, _ any) bool {
		actionToken = key.(string)
		return false
	})

	if actionToken == "" {
		t.Fatal("expected to find action token in pending map")
	}

	d.OnInterventionResponse(actionToken)

	count := 0
	d.pendingMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatal("expected pending entry to be removed after response")
	}
}

func TestDispatcher_OnInterventionResponse_WrongToken(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)

	store := notification.NewStore(db)
	d := NewDispatcher(db, nil, store, "http://localhost:3000", 60, nil)

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-5",
		DeviceID:  "device-1",
	})

	d.OnInterventionResponse("wrong-token")

	count := 0
	d.pendingMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 1 {
		t.Fatal("expected pending entry to remain with wrong token")
	}
}

func TestResolveWeComUserID(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", 60, nil)

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

func TestDispatcher_DispatchStaleNotification(t *testing.T) {
	db := setupTestDB(t)

	d := NewDispatcher(db, nil, nil, "http://localhost:3000", 60, nil)

	// Should not panic with nil adapter and no IDTrust identity
	d.DispatchStaleNotification(models.SystemNotification{
		UserID:      "user-1",
		Type:        "permission",
		SessionID:   "session-stale",
		DeviceID:    "device-1",
		ActionToken: "stale-token-123",
	})
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
