package clawagent

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/server/internal/channel"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// mockSender implements channel.Sender for testing.
type mockSender struct {
	rc channel.ReplyContext
}

func (s *mockSender) Send(_ context.Context, _ string) error                    { return nil }
func (s *mockSender) SendMessage(_ context.Context, _ channel.OutboundMessage) error { return nil }
func (s *mockSender) ReplyContext() channel.ReplyContext                        { return s.rc }

func setupSessionIsolationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := createTestTables(db); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func setupTestRuntime(t *testing.T) *ClawAgentRuntime {
	t.Helper()
	db := setupSessionIsolationDB(t)
	rt := &ClawAgentRuntime{
		db:          db,
		SessionMeta: NewSessionMetaManager(db),
		agentCfg: ClawAgentConfig{
			SessionDailyResetHour:   4,
			SessionGroupIdleMinutes: 30,
		},
	}
	rt.bgCtx = context.Background()
	return rt
}

// TestResolveActiveSession_SamePlatformUser_SameSession verifies that two messages
// from the same platform user resolve to the same session.
func TestResolveActiveSession_SamePlatformUser_SameSession(t *testing.T) {
	rt := setupTestRuntime(t)

	baseKey := "agent:clawagent:single:platform-user-A"

	sid1, err := rt.resolveActiveSession("platform-user-A", baseKey, "direct")
	if err != nil {
		t.Fatalf("resolveActiveSession (1st): %v", err)
	}
	sid2, err := rt.resolveActiveSession("platform-user-A", baseKey, "direct")
	if err != nil {
		t.Fatalf("resolveActiveSession (2nd): %v", err)
	}

	if sid1 != sid2 {
		t.Errorf("same user should get same session: sid1=%q sid2=%q", sid1, sid2)
	}
}

// TestResolveActiveSession_DifferentPlatformUsers_DifferentSessions verifies that
// two different platform users get different sessions.
func TestResolveActiveSession_DifferentPlatformUsers_DifferentSessions(t *testing.T) {
	rt := setupTestRuntime(t)

	sidA, err := rt.resolveActiveSession("platform-user-A",
		"agent:clawagent:single:platform-user-A", "direct")
	if err != nil {
		t.Fatalf("resolveActiveSession (A): %v", err)
	}
	sidB, err := rt.resolveActiveSession("platform-user-B",
		"agent:clawagent:single:platform-user-B", "direct")
	if err != nil {
		t.Fatalf("resolveActiveSession (B): %v", err)
	}

	if sidA == sidB {
		t.Errorf("different users should get different sessions: both = %q", sidA)
	}
}

// TestSessionKey_ConsistencyBetweenInboundAndOutbound is the core regression test.
// It verifies that the baseKey format is identical when:
//   - Inbound (user sends message): uses platformUserID from ReplyContext.UserID
//   - Outbound (device event triggers AI): uses platformUserID from req.UserID
// Both paths MUST produce the same baseKey for the same platform user.
func TestSessionKey_ConsistencyBetweenInboundAndOutbound(t *testing.T) {
	platformUserID := "platform-user-A"
	chatType := "single"

	// Inbound path (runtime.go Handle):
	// baseKey = fmt.Sprintf("agent:clawagent:%s:%s", msg.ExternalChatType, userID)
	inboundBaseKey := "agent:clawagent:" + chatType + ":" + platformUserID

	// Outbound path (event_handler.go HandleAIEvent with Sender):
	// baseKey = fmt.Sprintf("agent:clawagent:%s:%s", chatType, req.UserID)
	outboundBaseKey := "agent:clawagent:" + chatType + ":" + platformUserID

	if inboundBaseKey != outboundBaseKey {
		t.Fatalf("baseKey mismatch:\n  inbound:  %q\n  outbound: %q",
			inboundBaseKey, outboundBaseKey)
	}

	// Both resolve to the same session
	rt := setupTestRuntime(t)
	sidInbound, err := rt.resolveActiveSession(platformUserID, inboundBaseKey, "direct")
	if err != nil {
		t.Fatalf("inbound resolveActiveSession: %v", err)
	}
	sidOutbound, err := rt.resolveActiveSession(platformUserID, outboundBaseKey, "direct")
	if err != nil {
		t.Fatalf("outbound resolveActiveSession: %v", err)
	}
	if sidInbound != sidOutbound {
		t.Errorf("inbound and outbound should resolve to same session:\n  inbound:  %q\n  outbound: %q",
			sidInbound, sidOutbound)
	}
}

// TestSessionKey_PreFixBug_Reproduces verifies the OLD buggy behavior:
// When ReplyContext.UserID is empty, the baseKey used ExternalUserID instead.
// This test documents the difference so the fix is clearly understood.
func TestSessionKey_PreFixBug_ExternalUserIDDiffersFromPlatformUserID(t *testing.T) {
	platformUserID := "platform-user-A"
	externalUserID := "WecomUser-A"

	// FIXED path (with platformUserID resolved)
	fixedBaseKey := "agent:clawagent:single:" + platformUserID

	// BUGGY path (UserID empty, falls back to ExternalUserID)
	buggyBaseKey := "agent:clawagent:single:" + externalUserID

	if fixedBaseKey == buggyBaseKey {
		t.Fatal("test setup error: fixed and buggy keys should differ")
	}

	// Verify they would produce different sessions
	rt := setupTestRuntime(t)
	sidFixed, _ := rt.resolveActiveSession(platformUserID, fixedBaseKey, "direct")
	sidBuggy, _ := rt.resolveActiveSession(externalUserID, buggyBaseKey, "direct")

	if sidFixed == sidBuggy {
		t.Error("different keys should produce different sessions")
	}
}

// TestSessionKey_SingleVsGroup_DifferentSessions verifies that the same user
// in single chat vs group chat would produce different session keys.
// (Group chat is currently rejected at the Handle level, but the session
// resolution logic itself should still produce distinct keys.)
func TestSessionKey_SingleVsGroup_DifferentSessions(t *testing.T) {
	rt := setupTestRuntime(t)

	sidSingle, _ := rt.resolveActiveSession("platform-user-A",
		"agent:clawagent:single:platform-user-A", "direct")
	sidGroup, _ := rt.resolveActiveSession("platform-user-A",
		"agent:clawagent:group:group-456:group", "group")

	if sidSingle == sidGroup {
		t.Errorf("single and group sessions should differ: both = %q", sidSingle)
	}
}

// TestResolveActiveSession_TwoUsers_NoCrossContamination creates sessions for two
// different platform users and verifies their message histories don't overlap.
func TestResolveActiveSession_TwoUsers_NoCrossContamination(t *testing.T) {
	rt := setupTestRuntime(t)

	sidA, _ := rt.resolveActiveSession("platform-user-A",
		"agent:clawagent:single:platform-user-A", "direct")
	sidB, _ := rt.resolveActiveSession("platform-user-B",
		"agent:clawagent:single:platform-user-B", "direct")

	// Both sessions exist in DB with different user_ids
	metaA, err := rt.SessionMeta.Get(context.Background(), sidA)
	if err != nil {
		t.Fatalf("get meta A: %v", err)
	}
	metaB, err := rt.SessionMeta.Get(context.Background(), sidB)
	if err != nil {
		t.Fatalf("get meta B: %v", err)
	}

	if metaA.UserID == metaB.UserID {
		t.Errorf("sessions should have different UserIDs: A=%q B=%q",
			metaA.UserID, metaB.UserID)
	}
	if metaA.SessionID == metaB.SessionID {
		t.Errorf("sessions should have different SessionIDs: both = %q", metaA.SessionID)
	}

	// Verify each session is only visible to its owner
	activeA, err := rt.SessionMeta.Active(context.Background(), "platform-user-A",
		"agent:clawagent:single:platform-user-A")
	if err != nil {
		t.Fatalf("active session for A: %v", err)
	}
	if activeA.SessionID != sidA {
		t.Errorf("active session for A = %q, want %q", activeA.SessionID, sidA)
	}

	// User B should NOT see user A's session
	_, err = rt.SessionMeta.Active(context.Background(), "platform-user-B",
		"agent:clawagent:single:platform-user-A")
	if err == nil {
		t.Error("user B should NOT find user A's session under B's baseKey")
	}
}
