package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/costrict/costrict-web/server/internal/gateway"
)

// EventManager queries the device to determine whether a deferred event is still
// pending. The dispatcher's deferred-timer callback consults the manager before
// firing the AI notification: if the user already resolved the event through
// another channel (cs-cloud CLI, web UI, prior AI reply), the notification is
// suppressed.
//
// Each event type that participates in the deferred-notification flow has its
// own implementation. New event types add a new manager — no changes to the
// dispatcher core are required.
type EventManager interface {
	// IsStillPending returns true if the event identified by input is still
	// pending on the device. A false return (or a non-nil error treated as
	// "treat as pending" by callers) suppresses the deferred notification.
	IsStillPending(ctx context.Context, input DispatchInput) (bool, error)
}

// deviceSessionFetcher mirrors gateway.ProxyDeviceSessionRequest so tests can
// inject a fake without spinning up an HTTP server.
type deviceSessionFetcher func(client *gateway.Client, registry *gateway.GatewayRegistry, userID, deviceID, directory, method, path string, body []byte, result any) error

// idEntry is the minimal slice of csc's Permission.Request / Question.Request
// schema that PermissionManager / QuestionManager need.
type idEntry struct {
	ID string `json:"id"`
}

// parseIDList extracts the "id" field from each list entry. It accepts both
// forms the device may return:
//
//   - bare array:        [{"id": "..."}, ...]   (opencode main branch)
//   - wrapped object:    {"permissions": [{"id": "..."}]}   (deployed csc)
//
// Wrapper key is configurable ("permissions" vs "questions"). Empty/missing
// arrays resolve to an empty (non-nil) slice.
func parseIDList(rawMsg json.RawMessage, wrapperKey string) ([]string, error) {
	trimmed := json.RawMessage(bytes.TrimSpace(rawMsg))
	if len(trimmed) == 0 {
		return []string{}, nil
	}

	// Wrapper form: object with `wrapperKey` array field.
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &wrapped); err == nil {
		if inner, ok := wrapped[wrapperKey]; ok {
			return decodeIDs(inner)
		}
		// Object but key missing — fall through to error path below; the
		// bare-array branch will fail since trimmed is an object.
	}

	// Bare-array form.
	return decodeIDs(trimmed)
}

func decodeIDs(rawMsg json.RawMessage) ([]string, error) {
	var entries []idEntry
	if err := json.Unmarshal(rawMsg, &entries); err != nil {
		return nil, fmt.Errorf("decode id list: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	return ids, nil
}

// PermissionManager checks permission and permission_batch events.
//
// Device API (csc):
//
//	GET /api/v1/permissions → [ { "id": "...", "sessionID": "...", ... } ]
//
// The response is a bare JSON array of Permission.Request objects (see
// opencode's Permission.Request schema). There is NO outer "permissions"
// wrapper, and the ID field is named "id" — not "request_id". The cs-cloud
// forwarder only rewrites the URL path; it does not reshape the body.
//
// A permission is pending iff its id appears in the list. Batch events
// are considered pending if ANY of the included permissions is still pending —
// the user gets one notification covering whichever remain.
type PermissionManager struct {
	gwClient   *gateway.Client
	gwRegistry *gateway.GatewayRegistry
	fetcher    deviceSessionFetcher
}

func NewPermissionManager(gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry) *PermissionManager {
	return &PermissionManager{
		gwClient:   gwClient,
		gwRegistry: gwRegistry,
		fetcher:    gateway.ProxyDeviceSessionRequest,
	}
}

func (m *PermissionManager) IsStillPending(ctx context.Context, input DispatchInput) (bool, error) {
	if m.fetcher == nil {
		return false, fmt.Errorf("permission manager: gateway not configured")
	}

	ids := extractPermissionIDs(input)
	if len(ids) == 0 {
		// No IDs to check (malformed actionData) — conservatively treat as
		// pending so we don't silently drop a notification the user should see.
		slog.Warn("[permission_manager] no permission IDs in actionData, treating as pending",
			"sessionID", input.SessionID)
		return true, nil
	}

	// csc may return either a bare array or a {"permissions": [...]} wrapper.
	// parseIDList handles both. We capture into json.RawMessage so the gateway
	// layer doesn't try to enforce a single shape.
	var rawMsg json.RawMessage
	proxyPath := "/api/v1/permissions"
	if err := m.fetcher(m.gwClient, m.gwRegistry,
		input.UserID, input.DeviceID, input.Path, "GET", proxyPath, nil, &rawMsg); err != nil {
		slog.Warn("[permission_manager] failed to query pending permissions, treating as pending",
			"sessionID", input.SessionID, "deviceID", input.DeviceID, "error", err)
		return true, nil
	}

	pendingIDs, err := parseIDList(rawMsg, "permissions")
	if err != nil {
		slog.Warn("[permission_manager] failed to parse pending list, treating as pending",
			"sessionID", input.SessionID, "error", err)
		return true, nil
	}

	pendingSet := make(map[string]struct{}, len(pendingIDs))
	for _, id := range pendingIDs {
		pendingSet[id] = struct{}{}
	}

	for _, id := range ids {
		if _, ok := pendingSet[id]; ok {
			return true, nil
		}
	}
	slog.Info("[permission_manager] all permissions resolved, suppressing notification",
		"sessionID", input.SessionID, "checkedIDs", ids)
	return false, nil
}

// extractPermissionIDs pulls request_ids from actionData for both single and
// batch permission events. Single permission uses actionData["id"]; batch uses
// actionData["permissions"] (each entry's "id" field — the cs-cloud forwarder
// keys batch entries by id, not request_id, at the cloud ingestion point).
func extractPermissionIDs(input DispatchInput) []string {
	if input.ActionData == nil {
		return nil
	}
	if id, ok := input.ActionData["id"].(string); ok && id != "" {
		return []string{id}
	}
	perms, ok := input.ActionData["permissions"].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, p := range perms {
		if m, ok := p.(map[string]any); ok {
			if id, ok := m["id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// QuestionManager checks question events.
//
// Device API (csc):
//
//	GET /api/v1/questions → [ { "id": "...", "sessionID": "...", "questions": [...] } ]
//
// A question is pending iff its id appears in the list.
type QuestionManager struct {
	gwClient   *gateway.Client
	gwRegistry *gateway.GatewayRegistry
	fetcher    deviceSessionFetcher
}

func NewQuestionManager(gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry) *QuestionManager {
	return &QuestionManager{
		gwClient:   gwClient,
		gwRegistry: gwRegistry,
		fetcher:    gateway.ProxyDeviceSessionRequest,
	}
}

func (m *QuestionManager) IsStillPending(ctx context.Context, input DispatchInput) (bool, error) {
	if m.fetcher == nil {
		return false, fmt.Errorf("question manager: gateway not configured")
	}

	id := ""
	if input.ActionData != nil {
		id, _ = input.ActionData["id"].(string)
	}
	if id == "" {
		slog.Warn("[question_manager] no question ID in actionData, treating as pending",
			"sessionID", input.SessionID)
		return true, nil
	}

	// Like permissions, the question list may be wrapped or bare.
	var rawMsg json.RawMessage
	proxyPath := "/api/v1/questions"
	if err := m.fetcher(m.gwClient, m.gwRegistry,
		input.UserID, input.DeviceID, input.Path, "GET", proxyPath, nil, &rawMsg); err != nil {
		slog.Warn("[question_manager] failed to query pending questions, treating as pending",
			"sessionID", input.SessionID, "deviceID", input.DeviceID, "error", err)
		return true, nil
	}

	pendingIDs, err := parseIDList(rawMsg, "questions")
	if err != nil {
		slog.Warn("[question_manager] failed to parse pending list, treating as pending",
			"sessionID", input.SessionID, "error", err)
		return true, nil
	}

	for _, qid := range pendingIDs {
		if qid == id {
			return true, nil
		}
	}
	slog.Info("[question_manager] question resolved, suppressing notification",
		"sessionID", input.SessionID, "questionID", id)
	return false, nil
}
