package clawagent

import (
	"context"
	"strings"
	"testing"
)

func TestMemoryManager_Load_NotFound(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db, true)

	content, err := mgr.Load(context.Background(), "user-nonexistent")
	if err != nil {
		t.Fatalf("Load non-existent: %v", err)
	}
	if content != "" {
		t.Errorf("Load non-existent = %q, want empty", content)
	}
}

func TestMemoryManager_SaveAndLoad(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db, true)
	ctx := context.Background()

	memContent := "用户偏好 Go 语言。常用 workspace 是 ws-001。"
	if err := mgr.Save(ctx, "user-1", memContent); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := mgr.Load(ctx, "user-1")
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if loaded != memContent {
		t.Errorf("Load = %q, want %q", loaded, memContent)
	}
}

func TestMemoryManager_Overwrite(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db, true)
	ctx := context.Background()

	_ = mgr.Save(ctx, "user-1", "旧内容")
	_ = mgr.Save(ctx, "user-1", "新内容")

	loaded, _ := mgr.Load(ctx, "user-1")
	if loaded != "新内容" {
		t.Errorf("Load after overwrite = %q, want %q", loaded, "新内容")
	}
}

func TestMemoryManager_Truncation(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db, true)
	ctx := context.Background()

	// Create content exceeding MaxMemoryBytes
	overLimit := strings.Repeat("a", MaxMemoryBytes+1000)
	if err := mgr.Save(ctx, "user-1", overLimit); err != nil {
		t.Fatalf("Save over limit: %v", err)
	}

	loaded, _ := mgr.Load(ctx, "user-1")
	if len(loaded) > MaxMemoryBytes {
		t.Errorf("loaded content length = %d, want <= %d", len(loaded), MaxMemoryBytes)
	}
}

func TestMemoryManager_MultipleUsers(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db, true)
	ctx := context.Background()

	_ = mgr.Save(ctx, "user-a", "Alice 的记忆")
	_ = mgr.Save(ctx, "user-b", "Bob 的记忆")

	alice, _ := mgr.Load(ctx, "user-a")
	bob, _ := mgr.Load(ctx, "user-b")

	if alice != "Alice 的记忆" {
		t.Errorf("alice = %q", alice)
	}
	if bob != "Bob 的记忆" {
		t.Errorf("bob = %q", bob)
	}
}

func TestMemoryManager_EmptyContent(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db, true)
	ctx := context.Background()

	if err := mgr.Save(ctx, "user-1", ""); err != nil {
		t.Fatalf("Save empty: %v", err)
	}

	loaded, _ := mgr.Load(ctx, "user-1")
	if loaded != "" {
		t.Errorf("Load after empty save = %q, want empty", loaded)
	}
}

// TestMemoryManager_Disabled_AgentLoadHidesStoredContent verifies that with
// enabled=false, LoadForAgent never surfaces rows that exist in the DB —
// guaranteeing the persona builder's # Memory section stays absent when the
// feature is off. The raw Load (REST handler path) must stay live so users
// can still inspect/clear their data with the feature switched off.
func TestMemoryManager_Disabled_AgentLoadHidesStoredContent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed a row via an enabled manager so the DB actually has content.
	seed := NewMemoryManager(db, true)
	if err := seed.Save(ctx, "user-1", "secret-should-not-load"); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// Disabled manager's agent-facing load must not expose the row.
	disabled := NewMemoryManager(db, false)
	agentLoaded, err := disabled.LoadForAgent(ctx, "user-1")
	if err != nil {
		t.Fatalf("disabled LoadForAgent: %v", err)
	}
	if agentLoaded != "" {
		t.Errorf("disabled LoadForAgent = %q, want empty (memory must not surface to agent when feature is off)", agentLoaded)
	}

	// Raw Load (REST handler path) must still return the seeded row.
	raw, err := disabled.Load(ctx, "user-1")
	if err != nil {
		t.Fatalf("disabled raw Load: %v", err)
	}
	if raw != "secret-should-not-load" {
		t.Errorf("disabled raw Load = %q, want seeded content (REST CRUD stays live)", raw)
	}
}

// TestMemoryManager_Disabled_RefreshNoOps verifies that with enabled=false,
// Refresh skips both the LLM call and the DB write — even when a stub LLM
// client would otherwise return content.
func TestMemoryManager_Disabled_RefreshNoOps(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	mgr := NewMemoryManager(db, false)
	cfg := ClawAgentConfig{
		DefaultProvider:  "openai",
		DefaultBaseURL:   "https://example.test",
		DefaultAPIKey:    "k",
		DefaultModelName: "test-model",
	}
	// Stub LLM that would produce a non-empty memory if invoked.
	stub := &stubLLM{content: "should-not-be-written"}

	if err := mgr.Refresh(ctx, "user-1", "hi", "hello", stub, cfg); err != nil {
		t.Fatalf("disabled Refresh: %v", err)
	}

	// Verify no row was written by reading through an enabled manager.
	probe := NewMemoryManager(db, true)
	loaded, _ := probe.Load(ctx, "user-1")
	if loaded != "" {
		t.Errorf("disabled Refresh wrote memory = %q, want no row", loaded)
	}
	if stub.calls != 0 {
		t.Errorf("disabled Refresh invoked LLM %d time(s), want 0", stub.calls)
	}
}

// stubLLM is a minimal llmGenerator for testing — only records whether Generate
// was called and what content it would return. The Stream / WithTools methods
// are unused by Refresh but required to satisfy the interface.
type stubLLM struct {
	content string
	calls   int
}

func (s *stubLLM) Generate(ctx context.Context, cfg ProviderConfig, msgs []ChatMessage) (*ChatCompletionResponse, error) {
	s.calls++
	return &ChatCompletionResponse{
		Choices: []struct {
			Index        int         `json:"index"`
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{
			{Message: ChatMessage{Role: "assistant", Content: s.content}},
		},
	}, nil
}

func (s *stubLLM) GenerateStream(ctx context.Context, cfg ProviderConfig, msgs []ChatMessage) (<-chan StreamEvent, <-chan error) {
	ch := make(chan StreamEvent)
	errCh := make(chan error, 1)
	close(ch)
	return ch, errCh
}

func (s *stubLLM) GenerateWithTools(ctx context.Context, cfg ProviderConfig, msgs []ChatMessage, tools []ToolDefinition) (*ChatCompletionResponse, error) {
	return s.Generate(ctx, cfg, msgs)
}
