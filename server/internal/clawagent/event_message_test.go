package clawagent

import (
	"context"
	"strings"
	"testing"
)

func TestAppendEventPending_LoadPendingEvent(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()
	const sessionID = "agent:clawagent:direct:user-1:v1"

	ec := &EventContext{
		EventType:   "permission",
		SessionID:   "device-sess-123",
		DeviceID:    "device-1",
		Path:        "/workspace",
		PermissionID: "perm-1",
		ActionData:  map[string]any{"id": "perm-1"},
	}

	if err := mgr.AppendEventPending(ctx, sessionID, ec); err != nil {
		t.Fatalf("AppendEventPending: %v", err)
	}

	got, err := mgr.LoadPendingEvent(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadPendingEvent: %v", err)
	}
	if got == nil {
		t.Fatal("expected pending event, got nil")
	}
	if got.EventType != "permission" {
		t.Errorf("EventType = %q, want permission", got.EventType)
	}
	if got.SessionID != "device-sess-123" {
		t.Errorf("SessionID = %q, want device-sess-123", got.SessionID)
	}
	if got.DeviceID != "device-1" {
		t.Errorf("DeviceID = %q", got.DeviceID)
	}
	if got.PermissionID != "perm-1" {
		t.Errorf("PermissionID = %q", got.PermissionID)
	}
}

func TestLoadPendingEvent_None(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()

	got, err := mgr.LoadPendingEvent(ctx, "no-such-session")
	if err != nil {
		t.Fatalf("LoadPendingEvent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty session, got %+v", got)
	}
}

func TestLoadPendingEvent_OnlyResolvedReturnsNil(t *testing.T) {
	// When the latest event-kind row is EVENT_RESOLVED, the session has no
	// pending event — LoadPendingEvent must return nil.
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()
	const sessionID = "agent:clawagent:direct:user-1:v1"

	ec := &EventContext{
		EventType: "permission",
		SessionID: "device-sess-123",
		DeviceID:  "device-1",
		Path:      "/workspace",
	}
	if err := mgr.AppendEventPending(ctx, sessionID, ec); err != nil {
		t.Fatalf("AppendEventPending: %v", err)
	}
	if err := mgr.MarkEventResolved(ctx, sessionID, "device-sess-123", ResolvedReasonToolSuccess); err != nil {
		t.Fatalf("MarkEventResolved: %v", err)
	}

	got, err := mgr.LoadPendingEvent(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadPendingEvent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after resolve, got %+v", got)
	}
}

func TestMarkEventResolved_TransitionsContentAndKind(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()
	const sessionID = "agent:clawagent:direct:user-1:v1"

	ec := &EventContext{
		EventType: "question",
		SessionID: "device-sess-456",
		DeviceID:  "device-2",
		Path:      "/repo",
	}
	if err := mgr.AppendEventPending(ctx, sessionID, ec); err != nil {
		t.Fatalf("AppendEventPending: %v", err)
	}
	if err := mgr.MarkEventResolved(ctx, sessionID, "device-sess-456", ResolvedReasonDeviceAlreadyDone); err != nil {
		t.Fatalf("MarkEventResolved: %v", err)
	}

	// Inspect the row directly to verify content/kind transition.
	var row SessionMessage
	if err := db.Where("session_id = ? AND kind = ?", sessionID, KindEventResolved).
		First(&row).Error; err != nil {
		t.Fatalf("query resolved row: %v", err)
	}
	if row.Content != ContentEventResolved {
		t.Errorf("Content = %q, want %q", row.Content, ContentEventResolved)
	}
	meta, err := unmarshalEventMetadata(row.Metadata)
	if err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.ResolvedReason != ResolvedReasonDeviceAlreadyDone {
		t.Errorf("ResolvedReason = %q, want %q", meta.ResolvedReason, ResolvedReasonDeviceAlreadyDone)
	}
	if meta.DeviceSessionID != "device-sess-456" {
		t.Errorf("DeviceSessionID roundtrip = %q", meta.DeviceSessionID)
	}

	// No pending rows should remain.
	var pendingCount int64
	db.Model(&SessionMessage{}).Where("session_id = ? AND kind = ?", sessionID, KindEventPending).Count(&pendingCount)
	if pendingCount != 0 {
		t.Errorf("pending rows after resolve = %d, want 0", pendingCount)
	}
}

func TestMarkEventResolved_NoMatchingRow_IsNoOp(t *testing.T) {
	// Marking resolved for a deviceSessionID that doesn't match any pending
	// row must be idempotent (no error, no row mutation).
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()
	const sessionID = "agent:clawagent:direct:user-1:v1"

	ec := &EventContext{EventType: "permission", SessionID: "device-real", DeviceID: "d"}
	if err := mgr.AppendEventPending(ctx, sessionID, ec); err != nil {
		t.Fatalf("AppendEventPending: %v", err)
	}
	if err := mgr.MarkEventResolved(ctx, sessionID, "device-nonexistent", ResolvedReasonToolSuccess); err != nil {
		t.Fatalf("MarkEventResolved on non-existent ID: %v", err)
	}

	// Original pending row should still be intact.
	got, err := mgr.LoadPendingEvent(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadPendingEvent: %v", err)
	}
	if got == nil || got.SessionID != "device-real" {
		t.Errorf("pending event mutated by no-op MarkEventResolved: %+v", got)
	}
}

func TestLoadPendingEvent_MultipleEvents_ReturnsLatest(t *testing.T) {
	// Within a debounce window multiple events may be appended. LoadPendingEvent
	// must return the most recent EVENT_PENDING (no later EVENT_RESOLVED for it).
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()
	const sessionID = "agent:clawagent:direct:user-1:v1"

	_ = mgr.AppendEventPending(ctx, sessionID, &EventContext{
		EventType: "permission", SessionID: "old-event", DeviceID: "d",
	})
	_ = mgr.AppendEventPending(ctx, sessionID, &EventContext{
		EventType: "question", SessionID: "new-event", DeviceID: "d",
	})

	got, err := mgr.LoadPendingEvent(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadPendingEvent: %v", err)
	}
	if got == nil || got.SessionID != "new-event" {
		t.Errorf("expected latest 'new-event', got %+v", got)
	}
}

func TestLoadMessages_IncludesEventRows(t *testing.T) {
	// LoadMessages is used to build LLM context. EVENT_PENDING/EVENT_RESOLVED
	// rows must appear in the message stream so the AI can see the lifecycle.
	db := setupTestDB(t)
	mgr := NewMessageManager(db)
	ctx := context.Background()
	const sessionID = "agent:clawagent:direct:user-1:v1"

	_ = mgr.AppendMessage(ctx, sessionID, ChatMessage{Role: "user", Content: "hello"})
	_ = mgr.AppendEventPending(ctx, sessionID, &EventContext{
		EventType: "permission", SessionID: "ds-1", DeviceID: "d",
	})
	_ = mgr.AppendMessage(ctx, sessionID, ChatMessage{Role: "assistant", Content: "ack"})

	msgs, err := mgr.LoadMessages(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[1].Role != "system" || msgs[1].Content != ContentEventPending {
		t.Errorf("middle msg = %+v, want system/[EVENT_PENDING]", msgs[1])
	}
}

func TestMarshalUnmarshalMetadata_RoundTrip(t *testing.T) {
	original := metadataFromEventContext(&EventContext{
		EventType:    "permission",
		SessionID:    "ds-1",
		DeviceID:     "dev-1",
		Path:         "/repo",
		PermissionID: "perm-1",
		ActionData:   map[string]any{"id": "perm-1", "permissions": []any{map[string]any{"id": "perm-1"}}},
	})
	s, err := marshalEventMetadata(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(s, "deviceSessionId") {
		t.Errorf("marshaled JSON missing deviceSessionId: %s", s)
	}
	got, err := unmarshalEventMetadata(s)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EventType != original.EventType || got.DeviceSessionID != original.DeviceSessionID {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, original)
	}
}

func TestUnmarshalEventMetadata_EmptyString(t *testing.T) {
	if _, err := unmarshalEventMetadata(""); err == nil {
		t.Error("expected error on empty metadata")
	}
}
