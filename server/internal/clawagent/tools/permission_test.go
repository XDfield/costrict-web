package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubDeviceProxy captures ReplyPermission calls for assertion.
type stubDeviceProxy struct {
	lastPermissionID string
	lastApproved     bool
	lastDirectory    string
	err              error
}

func (s *stubDeviceProxy) ReplyPermission(ctx context.Context, deviceID, permissionID string, approved bool, directory string) error {
	s.lastPermissionID = permissionID
	s.lastApproved = approved
	s.lastDirectory = directory
	return s.err
}
func (s *stubDeviceProxy) ReplyQuestion(ctx context.Context, deviceID, questionID string, answers [][]string, directory string) error {
	return nil
}
func (s *stubDeviceProxy) GetSessionInfo(ctx context.Context, deviceID, sessionID, directory string) (map[string]any, error) {
	return nil, nil
}
func (s *stubDeviceProxy) GetRecentMessages(ctx context.Context, deviceID, sessionID, directory string, limit int) ([]map[string]any, error) {
	return nil, nil
}

// --- buildDrainSuffix contract tests ---
//
// These pin the drain contract independently of the Execute path. The
// workspace-resolution branch in Execute uses Postgres-specific SQL
// (chr() + jsonb) that sqlite test DBs can't run, which would otherwise
// block drain testing. buildDrainSuffix is extracted precisely so the drain
// behavior can be unit-tested in isolation.

func TestBuildDrainSuffix_Nil(t *testing.T) {
	// DrainSessionPermissions nil → no-op, no panic.
	got := buildDrainSuffix(context.Background(), &Context{}, "perm-1")
	if got != "" {
		t.Errorf("got %q, want empty when drain not wired", got)
	}
}

func TestBuildDrainSuffix_ZeroDrained(t *testing.T) {
	// 0 siblings drained → no suffix (don't pollute with "0 条").
	tc := &Context{
		DrainSessionPermissions: func(ctx context.Context, _ string) ([]string, error) {
			return nil, nil
		},
	}
	got := buildDrainSuffix(context.Background(), tc, "perm-1")
	if got != "" {
		t.Errorf("got %q, want empty when 0 drained", got)
	}
}

func TestBuildDrainSuffix_Counted(t *testing.T) {
	tc := &Context{
		DrainSessionPermissions: func(ctx context.Context, _ string) ([]string, error) {
			return []string{"perm-a", "perm-b", "perm-c"}, nil
		},
	}
	got := buildDrainSuffix(context.Background(), tc, "perm-99")
	if !strings.Contains(got, "批量批准 3 条同类待审权限") {
		t.Errorf("got %q, want suffix mentioning count 3", got)
	}
}

func TestBuildDrainSuffix_DrainError(t *testing.T) {
	tc := &Context{
		DrainSessionPermissions: func(ctx context.Context, _ string) ([]string, error) {
			return nil, errors.New("gateway timeout")
		},
	}
	got := buildDrainSuffix(context.Background(), tc, "perm-1")
	if !strings.Contains(got, "批量处理其它待审权限时部分失败") {
		t.Errorf("got %q, want failure suffix", got)
	}
	if !strings.Contains(got, "gateway timeout") {
		t.Errorf("got %q, want underlying error text", got)
	}
}

func TestBuildDrainSuffix_PassesExcludePermissionID(t *testing.T) {
	// The just-replied permission MUST be excluded — replying it twice would
	// be a device-side error. This pins the excludePermissionID contract.
	var got string
	tc := &Context{
		DrainSessionPermissions: func(ctx context.Context, exclude string) ([]string, error) {
			got = exclude
			return []string{"perm-other"}, nil
		},
	}
	buildDrainSuffix(context.Background(), tc, "perm-abc-123")
	if got != "perm-abc-123" {
		t.Errorf("excludePermissionID = %q, want perm-abc-123", got)
	}
}

// --- Execute path tests (drain-agnostic) ---
//
// These exercise paths that don't depend on the Postgres-specific workspace
// resolution SQL. The full enableAutoAccept → drain chain is verified via
// the auto_accept_test.go in the notification package + buildDrainSuffix
// contract tests above.

func TestPermissionTool_Execute_NoEnableAutoAccept_DrainNotCalled(t *testing.T) {
	// When enableAutoAccept is omitted/false, drain must NOT be invoked —
	// it's specific to the "user said remember this choice" path.
	proxy := &stubDeviceProxy{}
	drainCalled := false
	toolCtx := &Context{
		DeviceID:    "D1",
		Directory:   "D:/proj",
		SessionID:   "dev-ses-1",
		UserID:      "user-1",
		DeviceProxy: proxy,
		DrainSessionPermissions: func(ctx context.Context, _ string) ([]string, error) {
			drainCalled = true
			return nil, nil
		},
	}

	if _, err := NewPermissionTool().Execute(context.Background(),
		`{"permissionID":"perm-1","approved":true}`, toolCtx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if drainCalled {
		t.Errorf("drain must not be called when enableAutoAccept omitted")
	}
}

func TestPermissionTool_Execute_RepliesViaProxy(t *testing.T) {
	// Sanity: the tool calls DeviceProxy.ReplyPermission with the right args.
	proxy := &stubDeviceProxy{}
	toolCtx := &Context{
		DeviceID:    "D1",
		Directory:   "D:/proj",
		SessionID:   "dev-ses-1",
		UserID:      "user-1",
		DeviceProxy: proxy,
	}

	if _, err := NewPermissionTool().Execute(context.Background(),
		`{"permissionID":"perm-7","approved":false}`, toolCtx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if proxy.lastPermissionID != "perm-7" {
		t.Errorf("proxy got permissionID = %q, want perm-7", proxy.lastPermissionID)
	}
	if proxy.lastApproved {
		t.Errorf("proxy got approved=true, want false")
	}
	if proxy.lastDirectory != "D:/proj" {
		t.Errorf("proxy got directory = %q, want D:/proj", proxy.lastDirectory)
	}
}

func TestPermissionTool_Execute_NoDeviceID_ReturnsError(t *testing.T) {
	toolCtx := &Context{
		// DeviceID intentionally empty
		DeviceProxy: &stubDeviceProxy{},
	}
	_, err := NewPermissionTool().Execute(context.Background(),
		`{"permissionID":"perm-1","approved":true}`, toolCtx)
	if err == nil {
		t.Errorf("expected error when DeviceID is empty")
	}
}
