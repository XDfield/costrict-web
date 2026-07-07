package dispatcher

import (
	"context"
	"fmt"
	"sync"
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

	if err := db.AutoMigrate(&DeferredNotification{}); err != nil {
		t.Fatalf("automigrate DeferredNotification: %v", err)
	}
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

// newTestDispatcher wires a DB-backed Dispatcher with short debounce + fast
// polling for snappy tests, starts the polling goroutine, and returns a
// teardown.
func newTestDispatcher(t *testing.T, db *gorm.DB, window, maxCap time.Duration) (*Dispatcher, func()) {
	t.Helper()
	d := NewDispatcherWithPolling(db, nil, notification.NewStore(db), "http://localhost:3000", nil, nil, window, maxCap, 10*time.Millisecond)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("disp.Start: %v", err)
	}
	return d, func() { d.Close() }
}

func TestNewDispatcher(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil)
	if d == nil {
		t.Fatal("expected non-nil dispatcher")
	}
}

func TestDispatcher_Dispatch_UnsupportedEvent(t *testing.T) {
	db := setupTestDB(t)
	d, td := newTestDispatcher(t, db, 30*time.Second, 60*time.Second)
	defer td()

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
	d, td := newTestDispatcher(t, db, 30*time.Second, 60*time.Second)
	defer td()

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "session.completed",
		SessionID: "session-3",
		DeviceID:  "device-1",
	})
}

func TestResolveWeComUserID(t *testing.T) {
	db := setupTestDB(t)
	d, td := newTestDispatcher(t, db, 30*time.Second, 60*time.Second)
	defer td()

	if got := d.resolveWeComUserID("user-none"); got != "" {
		t.Fatalf("expected empty for non-existent user, got %q", got)
	}

	insertIDTrustIdentity(t, db, "user-1", "zhangsan")

	got := d.resolveWeComUserID("user-1")
	if got != "zhangsan" {
		t.Fatalf("expected 'zhangsan', got %q", got)
	}

	db.Exec(`INSERT INTO user_auth_identities
		(user_subject_id, provider, external_key, is_primary, created_at, updated_at)
		VALUES ('user-1', 'github', 'gh:test', false, datetime('now'), datetime('now'))`)

	got = d.resolveWeComUserID("user-1")
	if got != "zhangsan" {
		t.Fatalf("expected 'zhangsan' with multiple providers, got %q", got)
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
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer d.Close()

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

// TestDispatcher_Debounce_Cancel verifies that calling CancelDeferredNotification
// before the timer fires prevents the AI handler from running.
func TestDispatcher_Debounce_Cancel(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 100*time.Millisecond, 500*time.Millisecond)
	defer td()

	aiCalled := make(chan struct{})
	d.SetAIEventHandler(func(ctx context.Context, inputs []DispatchInput) bool {
		close(aiCalled)
		return true
	})

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "permission",
		SessionID: "session-defer-1",
		DeviceID:  "device-1",
	})

	d.CancelDeferredNotification("user-1")

	select {
	case <-aiCalled:
		t.Fatal("AI handler must NOT be invoked after CancelDeferredNotification")
	case <-time.After(400 * time.Millisecond):
		// Good — no fire.
	}

	// Backlog should be empty.
	var count int64
	db.Model(&DeferredNotification{}).Where("user_id = ?", "user-1").Count(&count)
	if count != 0 {
		t.Errorf("expected backlog drained after cancel, got %d rows", count)
	}
}

// TestDispatcher_Debounce_BatchFire verifies the core coalescing invariant:
// three events dispatched in close succession produce ONE fire (all three
// events drained from the backlog together) rather than three separate
// fires.
func TestDispatcher_Debounce_BatchFire(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 100*time.Millisecond, 500*time.Millisecond)
	defer td()

	var mu sync.Mutex
	seen := make(map[string]bool)
	var firstFireAt time.Time
	d.SetAIEventHandler(func(ctx context.Context, inputs []DispatchInput) bool {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			firstFireAt = time.Now()
		}
		for _, in := range inputs {
			seen[in.SessionID] = true
		}
		return true
	})

	for i := 0; i < 3; i++ {
		d.Dispatch(DispatchInput{
			UserID:    "user-batch",
			EventType: "permission",
			SessionID: fmt.Sprintf("session-%d", i),
			DeviceID:  "device-1",
		})
	}

	// Wait for all three events to drain.
	deadline := time.After(1 * time.Second)
	for {
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("AI handler never completed the batch; seen=%v", seen)
		default:
		}
		mu.Lock()
		if len(seen) == 3 {
			mu.Unlock()
			break
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	// Snapshot fire count at this point. Any further fire would indicate a
	// second batch — which would break the debounce contract.
	mu.Lock()
	afterBatch := len(seen)
	firstAt := firstFireAt
	mu.Unlock()

	// Wait several poll intervals to confirm no second fire.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != afterBatch {
		t.Errorf("expected no further fires after batch; seen grew from %d to %d", afterBatch, len(seen))
	}
	if elapsed := time.Since(firstAt); elapsed > 500*time.Millisecond {
		t.Errorf("fire happened too late: elapsed=%v", elapsed)
	}

	// Backlog must be drained.
	var count int64
	db.Model(&DeferredNotification{}).Where("user_id = ?", "user-batch").Count(&count)
	if count != 0 {
		t.Errorf("expected backlog drained after fire, got %d rows", count)
	}
}

// TestDispatcher_Debounce_FiresWhenPending verifies that the AI handler is
// invoked when no EventManager is registered (legacy fires-unconditionally).
func TestDispatcher_Debounce_FiresWhenPending(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 50*time.Millisecond, 500*time.Millisecond)
	defer td()

	aiCalled := make(chan struct{})
	d.SetAIEventHandler(func(ctx context.Context, inputs []DispatchInput) bool {
		close(aiCalled)
		return true
	})

	d.Dispatch(DispatchInput{
		UserID:     "user-1",
		EventType:  "question",
		SessionID:  "session-pending",
		DeviceID:   "device-1",
		ActionData: map[string]any{"id": "q-1"},
	})

	select {
	case <-aiCalled:
		// Good: AI handler was invoked.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AI handler not invoked despite no manager registered")
	}
}

// fakeEventManager is a test-only EventManager returning a configured pending
// flag for IsStillPending calls.
type fakeEventManager struct {
	pending bool
	calls   int
}

func (f *fakeEventManager) IsStillPending(ctx context.Context, input DispatchInput) (bool, error) {
	f.calls++
	return f.pending, nil
}

// TestDispatcher_EventManager_SkipsWhenResolved verifies that when the
// registered EventManager reports the event as no longer pending, the AI
// handler is NOT invoked after the timer fires.
func TestDispatcher_EventManager_SkipsWhenResolved(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 50*time.Millisecond, 500*time.Millisecond)
	defer td()

	mgr := &fakeEventManager{pending: false}
	d.SetEventManager("permission", mgr)

	aiCalled := make(chan struct{})
	d.SetAIEventHandler(func(ctx context.Context, inputs []DispatchInput) bool {
		close(aiCalled)
		return true
	})

	d.Dispatch(DispatchInput{
		UserID:     "user-resolved",
		EventType:  "permission",
		SessionID:  "session-resolved",
		DeviceID:   "device-1",
		ActionData: map[string]any{"id": "perm-1"},
	})

	// Wait long enough for the timer to fire and the manager probe to run.
	deadline := time.After(400 * time.Millisecond)
	for {
		select {
		case <-aiCalled:
			t.Fatal("AI handler must NOT be invoked when EventManager reports resolved")
		case <-deadline:
			if mgr.calls == 0 {
				t.Fatal("manager never queried")
			}
			return
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestDispatcher_EventManager_FiresWhenPending verifies the symmetric case:
// manager reports still pending → AI handler is invoked.
func TestDispatcher_EventManager_FiresWhenPending(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 50*time.Millisecond, 500*time.Millisecond)
	defer td()

	mgr := &fakeEventManager{pending: true}
	d.SetEventManager("question", mgr)

	aiCalled := make(chan struct{})
	d.SetAIEventHandler(func(ctx context.Context, inputs []DispatchInput) bool {
		close(aiCalled)
		return true
	})

	d.Dispatch(DispatchInput{
		UserID:     "user-pending",
		EventType:  "question",
		SessionID:  "session-pending",
		DeviceID:   "device-1",
		ActionData: map[string]any{"id": "q-1"},
	})

	select {
	case <-aiCalled:
		// Good.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AI handler not invoked despite manager reporting still pending")
	}

	if mgr.calls == 0 {
		t.Error("manager should have been queried at least once")
	}
}

// TestDispatcher_NoManager_FiresUnconditionally verifies that when no manager
// is registered for the event type, the timer fires unconditionally. This
// protects the upgrade path — events without managers still deliver.
func TestDispatcher_NoManager_FiresUnconditionally(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 50*time.Millisecond, 500*time.Millisecond)
	defer td()

	aiCalled := make(chan struct{})
	d.SetAIEventHandler(func(ctx context.Context, inputs []DispatchInput) bool {
		close(aiCalled)
		return true
	})

	d.Dispatch(DispatchInput{
		UserID:     "user-nomgr",
		EventType:  "permission",
		SessionID:  "session-no-mgr",
		DeviceID:   "device-1",
		ActionData: map[string]any{"id": "perm-1"},
	})

	select {
	case <-aiCalled:
		// Good.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AI handler not invoked when no manager is registered")
	}
}

// TestDispatcher_NonDeferrableEvent_Immediate verifies that non-deferrable
// events don't create backlog rows.
func TestDispatcher_NonDeferrableEvent_Immediate(t *testing.T) {
	db := setupTestDB(t)
	setupNotificationTable(t, db)
	d, td := newTestDispatcher(t, db, 100*time.Millisecond, 500*time.Millisecond)
	defer td()

	d.Dispatch(DispatchInput{
		UserID:    "user-1",
		EventType: "session.completed",
		SessionID: "session-immediate-1",
		DeviceID:  "device-1",
	})

	var count int64
	db.Model(&DeferredNotification{}).Where("user_id = ?", "user-1").Count(&count)
	if count != 0 {
		t.Errorf("expected no backlog for non-deferrable event, got %d rows", count)
	}
}

// TestDispatcher_Debounce_Defaults verifies the debounce defaults.
func TestDispatcher_Debounce_Defaults(t *testing.T) {
	db := setupTestDB(t)
	d := NewDispatcher(db, nil, nil, "http://localhost:3000", nil, nil)
	if d.debounceWindow != defaultDebounceWindow {
		t.Errorf("window = %v, want %v", d.debounceWindow, defaultDebounceWindow)
	}
	if d.debounceMaxCap != defaultDebounceMaxCap {
		t.Errorf("maxCap = %v, want %v", d.debounceMaxCap, defaultDebounceMaxCap)
	}
}
