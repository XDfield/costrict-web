package clawagent

import (
	"testing"
)

func TestNewSessionID_FormatsCorrectly(t *testing.T) {
	tests := []struct {
		baseKey string
		version int
		want    string
	}{
		{"agent:clawagent:wecom-bot:chat123:user456", 1, "agent:clawagent:wecom-bot:chat123:user456:v1"},
		{"agent:clawagent:wecom-bot:chat123:group", 2, "agent:clawagent:wecom-bot:chat123:group:v2"},
		{"base", 5, "base:v5"},
		{"", 1, ":v1"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := NewSessionID(tt.baseKey, tt.version)
			if got != tt.want {
				t.Errorf("NewSessionID(%q, %d) = %q, want %q", tt.baseKey, tt.version, got, tt.want)
			}
		})
	}
}

func TestResetTypeOf_ClassifiesCorrectly(t *testing.T) {
	tests := []struct {
		baseKey string
		want    string
	}{
		{"agent:clawagent:wecom-bot:chat123:user456", "direct"},
		{"agent:clawagent:wecom-bot:chat123:group", "group"},
		{"agent:clawagent:wecom-bot:chat123:group:thread:t1", "thread"},
		{"agent:clawagent:event:permission:evt1", "direct"},
		{"agent:clawagent:task:task-001", "direct"},
	}

	for _, tt := range tests {
		t.Run(tt.baseKey, func(t *testing.T) {
			got := resetTypeOf(tt.baseKey)
			if got != tt.want {
				t.Errorf("resetTypeOf(%q) = %q, want %q", tt.baseKey, got, tt.want)
			}
		})
	}
}

func TestEstimateTokens_Basic(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty", "", 0},
		{"short", "a", 1},
		{"4 chars", "aaaa", 1},
		{"5 chars", "aaaaa", 2},
		{"100 chars", string(make([]byte, 100)), 25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.input)
			if got != tt.want {
				t.Errorf("estimateTokens(len=%d) = %d, want %d", len(tt.input), got, tt.want)
			}
		})
	}
}

func TestConversationSession_Creation(t *testing.T) {
	runner := NewAgentRunner(nil, nil)

	sess := runner.getOrCreateSession("test-sid:v1", "user-1")
	if sess == nil {
		t.Fatal("getOrCreateSession returned nil")
	}
	if sess.SessionID != "test-sid:v1" {
		t.Errorf("SessionID = %q", sess.SessionID)
	}
	if sess.UserID != "user-1" {
		t.Errorf("UserID = %q", sess.UserID)
	}
	if len(sess.Messages) == 0 {
		t.Error("new session should have at least the system message")
	}
	if sess.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want %q", sess.Messages[0].Role, "system")
	}

	// Same session should be reused
	sess2 := runner.getOrCreateSession("test-sid:v1", "user-1")
	if sess != sess2 {
		t.Error("getOrCreateSession should return the same session for same ID")
	}
}

func TestConversationSession_MultipleSessions(t *testing.T) {
	runner := NewAgentRunner(nil, nil)

	s1 := runner.getOrCreateSession("sid1", "user-1")
	s2 := runner.getOrCreateSession("sid2", "user-1")

	if s1 == s2 {
		t.Error("different session IDs should return different sessions")
	}
}

func TestAddAssistantMessage(t *testing.T) {
	runner := NewAgentRunner(nil, nil)
	runner.getOrCreateSession("test-sid", "user-1")

	runner.addAssistantMessage("test-sid", "Hello, how can I help?")

	sess := runner.GetSession("test-sid")
	if sess == nil {
		t.Fatal("GetSession returned nil")
	}

	found := false
	for _, msg := range sess.Messages {
		if msg.Role == "assistant" && msg.Content == "Hello, how can I help?" {
			found = true
			break
		}
	}
	if !found {
		t.Error("assistant message not found in session")
	}
}

func TestGetSession_NonExistent(t *testing.T) {
	runner := NewAgentRunner(nil, nil)
	sess := runner.GetSession("non-existent")
	if sess != nil {
		t.Error("GetSession for non-existent should return nil")
	}
}
