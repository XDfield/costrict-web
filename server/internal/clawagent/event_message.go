package clawagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Message kinds for special system rows in agent_session_messages.
// Empty string (default) means a normal user/assistant/tool message.
const (
	KindEventPending  = "event_pending"
	KindEventResolved = "event_resolved"
)

// Content markers for event rows. The AI sees these in conversation history
// and uses them to understand event lifecycle: a pending event is one whose
// latest event-kind row is EVENT_PENDING (no matching EVENT_RESOLVED).
const (
	ContentEventPending  = "[EVENT_PENDING]"
	ContentEventResolved = "[EVENT_RESOLVED]"
)

// ResolvedReason values written into eventMetadata when an event row is
// transitioned from pending to resolved.
const (
	ResolvedReasonToolSuccess          = "tool_success"
	ResolvedReasonDeviceAlreadyDone    = "device_already_resolved"
	ResolvedReasonDispatcherSuppressed = "dispatcher_suppressed"
	// ResolvedReasonAutoAcceptDrain marks rows resolved as a side effect of the
	// user choosing auto-accept: the just-replied permission triggers a drain
	// of sibling pending permissions in the same session, each of which must
	// also be flipped to resolved in chat_messages (the device side and
	// system_notifications table are updated separately by the drain function).
	ResolvedReasonAutoAcceptDrain = "auto_accept_drain"
)

// eventMetadata is the JSON shape stored in SessionMessage.Metadata for event
// rows. Carries all EventContext fields plus resolvedReason on resolution.
type eventMetadata struct {
	EventType       string         `json:"eventType"`
	DeviceSessionID string         `json:"deviceSessionId"`
	DeviceID        string         `json:"deviceId"`
	Path            string         `json:"path"`
	ActionData      map[string]any `json:"actionData,omitempty"`
	PermissionID    string         `json:"permissionId,omitempty"`
	Questions       []QuestionItem `json:"questions,omitempty"`
	ResolvedReason  string         `json:"resolvedReason,omitempty"`
}

// metadataFromEventContext converts an EventContext into the serializable form.
// IsProcessed is intentionally omitted — pending state is derived from the
// latest event-kind row, not from a stored flag.
func metadataFromEventContext(ec *EventContext) eventMetadata {
	return eventMetadata{
		EventType:       ec.EventType,
		DeviceSessionID: ec.SessionID,
		DeviceID:        ec.DeviceID,
		Path:            ec.Path,
		ActionData:      ec.ActionData,
		PermissionID:    ec.PermissionID,
		Questions:       ec.Questions,
	}
}

// toEventContext converts metadata back to EventContext.
func (m eventMetadata) toEventContext() *EventContext {
	return &EventContext{
		EventType:    m.EventType,
		SessionID:    m.DeviceSessionID,
		DeviceID:     m.DeviceID,
		Path:         m.Path,
		ActionData:   m.ActionData,
		PermissionID: m.PermissionID,
		Questions:    m.Questions,
	}
}

// marshalEventMetadata serializes eventMetadata to a JSON string for storage.
func marshalEventMetadata(m eventMetadata) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal event metadata: %w", err)
	}
	return string(data), nil
}

// unmarshalEventMetadata parses a JSON string back to eventMetadata.
// Returns an error if the input is empty or malformed.
func unmarshalEventMetadata(s string) (eventMetadata, error) {
	if s == "" {
		return eventMetadata{}, errors.New("empty metadata")
	}
	var m eventMetadata
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return eventMetadata{}, fmt.Errorf("unmarshal event metadata: %w", err)
	}
	return m, nil
}

// AppendEventPending writes a new EVENT_PENDING system row for the given session,
// carrying the EventContext as JSON metadata. The row participates in LoadMessages
// like any other message, giving the AI full lifecycle visibility.
func (m *MessageManager) AppendEventPending(ctx context.Context, sessionID string, ec *EventContext) error {
	if ec == nil {
		return errors.New("AppendEventPending: nil EventContext")
	}
	meta := metadataFromEventContext(ec)
	metaJSON, err := marshalEventMetadata(meta)
	if err != nil {
		return err
	}
	record := &SessionMessage{
		SessionID: sessionID,
		Role:      "system",
		Content:   ContentEventPending,
		Kind:      KindEventPending,
		Metadata:  metaJSON,
	}
	return m.db.WithContext(ctx).Create(record).Error
}

// MarkEventResolved transitions the EVENT_PENDING row matching deviceSessionID
// into EVENT_RESOLVED by updating its Content, Kind, and Metadata (resolvedReason).
// If no matching pending row exists, this is a no-op (idempotent).
//
// The row is preserved (not deleted) so the AI conversation history retains the
// full lifecycle: pending → resolved. Future compaction can fold resolved rows
// into summaries, but pending rows must survive compaction.
func (m *MessageManager) MarkEventResolved(ctx context.Context, sessionID, deviceSessionID, reason string) error {
	// Load all EVENT_PENDING rows for this session (bounded N — typically 1, at
	// most a handful within a debounce window). We over-fetch and filter in Go
	// to keep the query portable across SQLite (tests) and MySQL/Postgres (prod),
	// which differ in JSON-path syntax.
	var rows []SessionMessage
	if err := m.db.WithContext(ctx).
		Where("session_id = ? AND kind = ?", sessionID, KindEventPending).
		Order("created_at DESC, id DESC").
		Find(&rows).Error; err != nil {
		return fmt.Errorf("load pending rows: %w", err)
	}

	for _, row := range rows {
		meta, err := unmarshalEventMetadata(row.Metadata)
		if err != nil {
			continue
		}
		if meta.DeviceSessionID != deviceSessionID {
			continue
		}
		meta.ResolvedReason = reason
		metaJSON, mErr := marshalEventMetadata(meta)
		if mErr != nil {
			return mErr
		}
		return m.db.WithContext(ctx).Model(&SessionMessage{}).
			Where("id = ?", row.ID).
			Updates(map[string]any{
				"content":  ContentEventResolved,
				"kind":     KindEventResolved,
				"metadata": metaJSON,
			}).Error
	}
	return nil
}

// LoadPendingEvent returns the most recent EVENT_PENDING row's EventContext,
// or nil if no pending event exists for the session. A pending event is one
// whose latest event-kind row is EVENT_PENDING (no later EVENT_RESOLVED).
func (m *MessageManager) LoadPendingEvent(ctx context.Context, sessionID string) (*EventContext, error) {
	var row SessionMessage
	err := m.db.WithContext(ctx).
		Where("session_id = ? AND kind IN ?", sessionID, []string{KindEventPending, KindEventResolved}).
		Order("created_at DESC, id DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load latest event row: %w", err)
	}
	if row.Kind != KindEventPending {
		return nil, nil
	}
	meta, err := unmarshalEventMetadata(row.Metadata)
	if err != nil {
		return nil, err
	}
	return meta.toEventContext(), nil
}

// LoadAllPendingEvents returns every EVENT_PENDING row for the session as
// EventContexts, newest first. Used by the batch tool path to look up the
// right deviceSessionID / deviceID / directory when AI calls reply_permission
// or reply_question with a specific ID emitted from a batch notification.
func (m *MessageManager) LoadAllPendingEvents(ctx context.Context, sessionID string) ([]*EventContext, error) {
	var rows []SessionMessage
	if err := m.db.WithContext(ctx).
		Where("session_id = ? AND kind = ?", sessionID, KindEventPending).
		Order("created_at DESC, id DESC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load pending rows: %w", err)
	}
	ecs := make([]*EventContext, 0, len(rows))
	for _, row := range rows {
		meta, err := unmarshalEventMetadata(row.Metadata)
		if err != nil {
			continue
		}
		ecs = append(ecs, meta.toEventContext())
	}
	return ecs, nil
}

// EventMatchesID reports whether the given ID (a permission_id or question_id
// extracted from a tool call's args) belongs to this event. Handles single
// permission, permission_batch (checks every permission in ActionData), and
// question (top-level + per-question).
func EventMatchesID(ec *EventContext, id string) bool {
	if ec == nil || id == "" {
		return false
	}
	if ec.PermissionID == id {
		return true
	}
	for _, q := range ec.Questions {
		if q.ID == id {
			return true
		}
	}
	if ec.ActionData != nil {
		if perms, ok := ec.ActionData["permissions"].([]any); ok {
			for _, p := range perms {
				if pMap, ok := p.(map[string]any); ok {
					if pid, _ := pMap["id"].(string); pid == id {
						return true
					}
				}
			}
		}
	}
	return false
}

// FindEventByID returns the first EventContext in ecs whose metadata contains
// the given ID (permission_id or question_id), or nil if none match.
func FindEventByID(ecs []*EventContext, id string) *EventContext {
	for _, ec := range ecs {
		if EventMatchesID(ec, id) {
			return ec
		}
	}
	return nil
}

// PermissionIDsFromEvent returns every permission ID carried by an event —
// top-level PermissionID plus any IDs nested in ActionData["permissions"].
// Dedupes and preserves first-seen order. Used by the auto-accept drainer to
// enumerate every sibling permission in a session, including those inside a
// permission_batch event (where PermissionID only captures the first one).
func PermissionIDsFromEvent(ec *EventContext) []string {
	if ec == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(ec.PermissionID)
	if ec.ActionData != nil {
		if perms, ok := ec.ActionData["permissions"].([]any); ok {
			for _, p := range perms {
				if pMap, ok := p.(map[string]any); ok {
					add(strValFromAny(pMap["id"]))
				}
			}
		}
	}
	return ids
}

// strValFromAny coerces an arbitrary value to a string when it's already a
// string. Returns "" otherwise.
func strValFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// MarkEventResolvedByID finds the EVENT_PENDING row whose metadata contains
// the given permission_id / question_id and transitions it to EVENT_RESOLVED.
// Used by the batch tool path where the runner doesn't know which deviceSessionID
// corresponds to the ID AI passed in its tool_call args. No-op (idempotent) if
// no matching row exists.
func (m *MessageManager) MarkEventResolvedByID(ctx context.Context, sessionID, id, reason string) error {
	ecs, err := m.LoadAllPendingEvents(ctx, sessionID)
	if err != nil {
		return err
	}
	match := FindEventByID(ecs, id)
	if match == nil {
		return nil
	}
	return m.MarkEventResolved(ctx, sessionID, match.SessionID, reason)
}
