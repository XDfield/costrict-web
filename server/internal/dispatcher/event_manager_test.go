package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gateway"
)

// fakeFetcher captures the call and writes rawRespBytes into result (which
// callers always pass as *json.RawMessage in production). If rawRespBytes is
// nil, the fetcher succeeds without writing anything — this exercises the
// empty-body path.
type fakeFetcher struct {
	rawRespBytes []byte
	returnErr    error
	calls        int
	lastPath     string
}

func (f *fakeFetcher) fetch(_ *gateway.Client, _ *gateway.GatewayRegistry, _, _, _, _, path string, _ []byte, result any) error {
	f.calls++
	f.lastPath = path
	if f.returnErr != nil {
		return f.returnErr
	}
	if f.rawRespBytes == nil {
		return nil
	}
	// result is *json.RawMessage — copy bytes in.
	if rm, ok := result.(*json.RawMessage); ok {
		*rm = append((*rm)[:0], f.rawRespBytes...)
		return nil
	}
	// Fallback for tests that pass other result types.
	return json.Unmarshal(f.rawRespBytes, result)
}

// marshalJSON wraps t.Fatal for inline fixture construction.
func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// --- PermissionManager tests ---

// Wrapped format: {"permissions": [...]} — this is what the deployed csc
// returns (see ops log 2026-07-07). Regression guard.
func TestPermissionManager_WrappedFormat_StillPending(t *testing.T) {
	body := marshalJSON(t, map[string]any{
		"permissions": []map[string]any{
			{
				"id":         "d9392ddf-d2cb-481b-9b5c-f2c5b3edbff1",
				"kind":       "tool",
				"permission": "glob",
				"sessionID":  "1cc3da44-bb6d-4059-9f54-c5f8ee6f53fd",
				"patterns":   []string{"D:/DEV/cs-cloud", "*"},
				"metadata":   map[string]any{"input": map[string]any{"path": "D:/DEV/cs-cloud", "pattern": "*"}},
				"options":    []map[string]any{{"kind": "allow", "name": "Allow", "option_id": "allow"}},
				"always":     []string{},
				"tool":       map[string]any{"callID": "call_36da471496ea42779ec490ba", "messageID": ""},
			},
		},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{
		SessionID: "1cc3da44-bb6d-4059-9f54-c5f8ee6f53fd",
		ActionData: map[string]any{
			"id": "d9392ddf-d2cb-481b-9b5c-f2c5b3edbff1",
		},
	}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true (wrapped format must parse); the prior bug suppressed this notification")
	}
	if fetcher.lastPath != "/api/v1/permissions" {
		t.Errorf("fetcher path = %q, want /api/v1/permissions", fetcher.lastPath)
	}
	if fetcher.calls != 1 {
		t.Errorf("expected 1 fetcher call, got %d", fetcher.calls)
	}
}

// Wrapped format with extra deployed-only fields (kind, options) — these
// must not break parsing.
func TestPermissionManager_WrappedFormat_DeployedSchemaWithExtraFields(t *testing.T) {
	// Verbatim fixture from the ops report — confirms the exact body that
	// previously triggered the bug now parses cleanly.
	body := []byte(`{"permissions":[` +
		`{"always":[],"id":"d9392ddf-d2cb-481b-9b5c-f2c5b3edbff1","kind":"tool",` +
		`"metadata":{"input":{"path":"D:/DEV/cs-cloud","pattern":"*"}},` +
		`"options":[{"kind":"allow","name":"Allow","option_id":"allow"},{"kind":"deny","name":"Deny","option_id":"deny"}],` +
		`"patterns":["D:/DEV/cs-cloud","*"],"permission":"glob",` +
		`"sessionID":"1cc3da44-bb6d-4059-9f54-c5f8ee6f53fd",` +
		`"tool":{"callID":"call_36da471496ea42779ec490ba","messageID":""}},` +
		`{"always":[],"id":"c71b4bd5-46cc-4ed6-b254-bab0183875a8","kind":"tool",` +
		`"metadata":{"input":{"filePath":"D:\\DEV\\cs-cloud\\README.md"}},` +
		`"options":[{"kind":"allow","name":"Allow","option_id":"allow"},{"kind":"deny","name":"Deny","option_id":"deny"}],` +
		`"patterns":[],"permission":"read",` +
		`"sessionID":"1cc3da44-bb6d-4059-9f54-c5f8ee6f53fd",` +
		`"tool":{"callID":"call_945436f4b5b1466893be3267","messageID":""}}]}`)

	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	// Second permission should be flagged as still pending.
	input := DispatchInput{
		ActionData: map[string]any{"id": "c71b4bd5-46cc-4ed6-b254-bab0183875a8"},
	}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true for second permission in wrapped fixture")
	}
}

// Bare array format: [...] — what opencode main branch returns.
// Both formats must work because we may deploy either csc build.
func TestPermissionManager_BareArrayFormat_StillPending(t *testing.T) {
	body := marshalJSON(t, []map[string]any{
		{
			"id":         "0f71720e-58e9-4d9f-a5e9-3d6f2ba8b295",
			"sessionID":  "ses-1",
			"permission": "bash",
			"patterns":   []string{"ls -la"},
		},
		{"id": "per_other", "sessionID": "ses-2", "permission": "edit"},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{
		ActionData: map[string]any{"id": "0f71720e-58e9-4d9f-a5e9-3d6f2ba8b295"},
	}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true since permission is in the bare-array list")
	}
}

func TestPermissionManager_Resolved(t *testing.T) {
	body := marshalJSON(t, map[string]any{
		"permissions": []map[string]any{{"id": "per_other"}},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{
		ActionData: map[string]any{"id": "0f71720e-58e9-4d9f-a5e9-3d6f2ba8b295"},
	}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pending {
		t.Errorf("expected pending=false since permission is NOT in the list (regression)")
	}
}

func TestPermissionManager_EmptyPermissionList_Resolved(t *testing.T) {
	body := marshalJSON(t, map[string]any{"permissions": []any{}})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "perm-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pending {
		t.Errorf("expected pending=false on empty wrapped list, got true")
	}
}

func TestPermissionManager_BatchPermission_AnyStillPending(t *testing.T) {
	body := marshalJSON(t, map[string]any{
		"permissions": []map[string]any{{"id": "perm-2"}},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{
		ActionData: map[string]any{
			"permissions": []any{
				map[string]any{"id": "perm-1"},
				map[string]any{"id": "perm-2"},
				map[string]any{"id": "perm-3"},
			},
		},
	}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true since at least one batch item is still pending")
	}
}

func TestPermissionManager_BatchPermission_AllResolved(t *testing.T) {
	body := marshalJSON(t, map[string]any{
		"permissions": []map[string]any{{"id": "perm-9"}},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{
		ActionData: map[string]any{
			"permissions": []any{
				map[string]any{"id": "perm-1"},
				map[string]any{"id": "perm-2"},
			},
		},
	}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pending {
		t.Errorf("expected pending=false since none of batch ids are pending")
	}
}

func TestPermissionManager_MissingID_TreatedAsPending(t *testing.T) {
	fetcher := &fakeFetcher{}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	// actionData has no "id" and no "permissions" — must not call the device
	// (conservative: don't drop a notification we can't identify).
	input := DispatchInput{ActionData: map[string]any{"foo": "bar"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true (conservative) when actionData has no id")
	}
	if fetcher.calls != 0 {
		t.Errorf("expected no device call when no id present, got %d calls", fetcher.calls)
	}
}

func TestPermissionManager_FetcherError_TreatedAsPending(t *testing.T) {
	// Network failure / device unreachable — don't suppress the notification
	// because we genuinely don't know whether the permission is resolved.
	fetcher := &fakeFetcher{returnErr: errors.New("gateway: device not connected")}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "perm-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true (conservative) on fetcher error")
	}
}

func TestPermissionManager_MalformedBody_TreatedAsPending(t *testing.T) {
	// Garbage body — don't crash, don't suppress. Conservative: still notify.
	fetcher := &fakeFetcher{rawRespBytes: []byte(`not json at all`)}
	mgr := NewPermissionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "perm-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true (conservative) on malformed body")
	}
}

func TestPermissionManager_FetcherNil_ReturnsError(t *testing.T) {
	mgr := &PermissionManager{} // no fetcher wired
	input := DispatchInput{ActionData: map[string]any{"id": "perm-1"}}
	_, err := mgr.IsStillPending(context.Background(), input)
	if err == nil {
		t.Fatal("expected error when fetcher is nil (gateway not configured)")
	}
}

// --- parseIDList unit tests (the dual-format core) ---

func TestParseIDList_WrappedForm(t *testing.T) {
	raw := json.RawMessage(`{"permissions":[{"id":"a"},{"id":"b"},{"id":""}]}`)
	ids, err := parseIDList(raw, "permissions")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("got %v, want [a b] (empty id should be skipped)", ids)
	}
}

func TestParseIDList_BareArrayForm(t *testing.T) {
	raw := json.RawMessage(`[{"id":"x"},{"id":"y"}]`)
	ids, err := parseIDList(raw, "permissions")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "y" {
		t.Errorf("got %v, want [x y]", ids)
	}
}

func TestParseIDList_EmptyBody(t *testing.T) {
	raw := json.RawMessage(`   `)
	ids, err := parseIDList(raw, "permissions")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ids == nil || len(ids) != 0 {
		t.Errorf("expected empty non-nil slice, got %v", ids)
	}
}

func TestParseIDList_EmptyArray(t *testing.T) {
	raw := json.RawMessage(`[]`)
	ids, err := parseIDList(raw, "permissions")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty slice, got %v", ids)
	}
}

func TestParseIDList_Malformed(t *testing.T) {
	raw := json.RawMessage(`not json`)
	_, err := parseIDList(raw, "permissions")
	if err == nil {
		t.Errorf("expected error on malformed JSON")
	}
}

// --- QuestionManager tests ---

func TestQuestionManager_WrappedFormat_StillPending(t *testing.T) {
	body := marshalJSON(t, map[string]any{
		"questions": []map[string]any{
			{"id": "que-1", "sessionID": "ses-1"},
			{"id": "que-2", "sessionID": "ses-2"},
		},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewQuestionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "que-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true since question is in wrapped list")
	}
	if fetcher.lastPath != "/api/v1/questions" {
		t.Errorf("fetcher path = %q, want /api/v1/questions", fetcher.lastPath)
	}
}

func TestQuestionManager_BareArrayFormat_StillPending(t *testing.T) {
	body := marshalJSON(t, []map[string]any{
		{"id": "que-1"},
		{"id": "que-2"},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewQuestionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "que-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true since question is in bare-array list")
	}
}

func TestQuestionManager_Resolved(t *testing.T) {
	body := marshalJSON(t, map[string]any{
		"questions": []map[string]any{{"id": "que-other"}},
	})
	fetcher := &fakeFetcher{rawRespBytes: body}
	mgr := NewQuestionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "que-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pending {
		t.Errorf("expected pending=false since question not in list")
	}
}

func TestQuestionManager_MissingID_TreatedAsPending(t *testing.T) {
	fetcher := &fakeFetcher{}
	mgr := NewQuestionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"foo": "bar"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true (conservative) when no id")
	}
	if fetcher.calls != 0 {
		t.Errorf("expected no device call, got %d", fetcher.calls)
	}
}

func TestQuestionManager_FetcherError_TreatedAsPending(t *testing.T) {
	fetcher := &fakeFetcher{returnErr: errors.New("timeout")}
	mgr := NewQuestionManager(nil, nil)
	mgr.fetcher = fetcher.fetch

	input := DispatchInput{ActionData: map[string]any{"id": "que-1"}}
	pending, err := mgr.IsStillPending(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pending {
		t.Errorf("expected pending=true (conservative) on fetcher error")
	}
}
