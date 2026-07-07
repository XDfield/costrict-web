package clawagent

import (
	"testing"
)

// TestResolveToolDeviceContext pins the contract documented on
// resolveToolDeviceContext. Failure here would regress the batch tool path
// — query_session_info / query_recent_messages silently losing device
// context, or reply_permission/reply_question losing exact-match semantics.
func TestResolveToolDeviceContext(t *testing.T) {
	perm1 := &EventContext{
		EventType: "permission", SessionID: "dev-ses-1", DeviceID: "dev-1", Path: "D:/a",
		PermissionID: "perm-111",
		ActionData:   map[string]any{"id": "perm-111"},
	}
	perm2 := &EventContext{
		EventType: "permission", SessionID: "dev-ses-2", DeviceID: "dev-2", Path: "D:/b",
		PermissionID: "perm-222",
		ActionData:   map[string]any{"id": "perm-222"},
	}

	cases := []struct {
		name              string
		toolName          string
		argsJSON          string
		ecs               []*EventContext
		wantDeviceID      string
		wantDirectory     string
		wantSessionID     string
	}{
		// Reply-class tools — exact match via args.permissionID.
		{
			name: "reply_permission / args has matching ID / single event",
			toolName: "reply_permission",
			argsJSON: `{"permissionID":"perm-111","approved":true}`,
			ecs:       []*EventContext{perm1},
			wantDeviceID: "dev-1", wantDirectory: "D:/a", wantSessionID: "dev-ses-1",
		},
		{
			name: "reply_permission / args has matching ID / picks correct from batch",
			toolName: "reply_permission",
			argsJSON: `{"permissionID":"perm-222","approved":true}`,
			ecs:       []*EventContext{perm1, perm2},
			wantDeviceID: "dev-2", wantDirectory: "D:/b", wantSessionID: "dev-ses-2",
		},
		{
			name: "reply_question / args has matching questionID",
			toolName: "reply_question",
			argsJSON: `{"questionID":"perm-111"}`,
			ecs:       []*EventContext{perm1},
			wantDeviceID: "dev-1", wantDirectory: "D:/a", wantSessionID: "dev-ses-1",
		},
		{
			name: "reply_permission / ID in args doesn't match any event",
			toolName: "reply_permission",
			argsJSON: `{"permissionID":"perm-not-present","approved":true}`,
			ecs:       []*EventContext{perm1},
			wantDeviceID: "", wantDirectory: "", wantSessionID: "",
		},

		// Query-class tools — args don't carry an ID; fall back to first event.
		// Regression: previously returned ("","","") and made query_session_info
		// always fail with "missing deviceID or sessionID".
		{
			name: "query_session_info / single event / falls back to first",
			toolName: "query_session_info",
			argsJSON: `{}`,
			ecs:       []*EventContext{perm1},
			wantDeviceID: "dev-1", wantDirectory: "D:/a", wantSessionID: "dev-ses-1",
		},
		{
			name: "query_recent_messages / single event / falls back to first",
			toolName: "query_recent_messages",
			argsJSON: `{"limit":5}`,
			ecs:       []*EventContext{perm1},
			wantDeviceID: "dev-1", wantDirectory: "D:/a", wantSessionID: "dev-ses-1",
		},
		{
			name: "query_session_info / empty args / falls back to first",
			toolName: "query_session_info",
			argsJSON: ``,
			ecs:       []*EventContext{perm1},
			wantDeviceID: "dev-1", wantDirectory: "D:/a", wantSessionID: "dev-ses-1",
		},
		{
			name: "query_session_info / multi-event batch / picks first",
			toolName: "query_session_info",
			argsJSON: `{}`,
			ecs:       []*EventContext{perm1, perm2},
			wantDeviceID: "dev-1", wantDirectory: "D:/a", wantSessionID: "dev-ses-1",
		},

		// Defensive — no events at all and no ID.
		{
			name: "query_session_info / no pending events / returns empty",
			toolName: "query_session_info",
			argsJSON: `{}`,
			ecs:       []*EventContext{},
			wantDeviceID: "", wantDirectory: "", wantSessionID: "",
		},
		{
			name: "query_session_info / nil ecs / returns empty",
			toolName: "query_session_info",
			argsJSON: `{}`,
			ecs:       nil,
			wantDeviceID: "", wantDirectory: "", wantSessionID: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDev, gotDir, gotSes := resolveToolDeviceContext(tc.toolName, tc.argsJSON, tc.ecs)
			if gotDev != tc.wantDeviceID || gotDir != tc.wantDirectory || gotSes != tc.wantSessionID {
				t.Errorf("resolveToolDeviceContext(%q, %q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.toolName, tc.argsJSON,
					gotDev, gotDir, gotSes,
					tc.wantDeviceID, tc.wantDirectory, tc.wantSessionID)
			}
		})
	}
}
