package clawagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestGetSession_NonExistent(t *testing.T) {
	runner := NewAgentRunner(nil, nil)
	sess := runner.GetSession("non-existent")
	if sess != nil {
		t.Error("GetSession for non-existent should return nil")
	}
}

// TestStartRun_CancelsPreviousRun verifies the core Option C invariant:
// a second startRun on the same sessionID cancels the first Run's ctx,
// causing the first Run's goroutine to exit. The first Run observes
// runCtx.Done() and exits cleanly.
func TestStartRun_CancelsPreviousRun(t *testing.T) {
	runner := NewAgentRunner(nil, nil)
	const sid = "test-sid-cancel:v1"

	var firstRunSawCtxDone atomic.Bool

	// First Run: park on a channel so it stays in-flight until we start the second.
	firstStarted := make(chan struct{})
	firstRun := func(runCtx context.Context, eventCh chan<- AgentEvent) {
		close(firstStarted)
		// Wait until cancelled. We expect cancellation, not timeout.
		select {
		case <-runCtx.Done():
			firstRunSawCtxDone.Store(true)
		case <-time.After(2 * time.Second):
			t.Error("first Run was not cancelled within 2s")
		}
	}

	eventCh1 := runner.startRun(context.Background(), sid, firstRun)

	// Wait for first Run to be in-flight.
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first Run never started")
	}

	// Second Run on the same sessionID — must cancel the first.
	done := make(chan struct{})
	secondRun := func(runCtx context.Context, eventCh chan<- AgentEvent) {
		defer close(done)
		sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
	}
	_ = runner.startRun(context.Background(), sid, secondRun)

	// Wait for second Run to complete.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second Run never completed")
	}

	// Wait for eventCh1 to close (first Run's goroutine should exit).
	select {
	case <-eventCh1:
	case <-time.After(time.Second):
		t.Fatal("first Run's eventCh never closed after cancel")
	}

	if !firstRunSawCtxDone.Load() {
		t.Error("first Run never observed runCtx.Done()")
	}
}

// TestSendEvent_NoBlockOnFullBuffer verifies the key anti-leak property:
// when the eventCh buffer is full and ctx is cancelled, sendEvent returns
// promptly instead of blocking forever. Without this, a cancelled Run whose
// consumer has exited would block forever once the 128-event buffer fills.
func TestSendEvent_NoBlockOnFullBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan AgentEvent, 2) // small buffer for easy filling

	// Fill the buffer so the next send would block on a bare `ch <- ev`.
	ch <- AgentEvent{Type: "token", Content: "1"}
	ch <- AgentEvent{Type: "token", Content: "2"}

	cancel() // cancel AFTER buffer is full

	returned := make(chan bool, 1)
	go func() {
		sendEvent(ctx, ch, AgentEvent{Type: "token", Content: "3"})
		returned <- true
	}()

	select {
	case <-returned:
		// Good: sendEvent returned even though buffer was full and ctx cancelled.
	case <-time.After(time.Second):
		t.Fatal("sendEvent blocked for >1s on full buffer with cancelled ctx — goroutine leak")
	}
}

// TestStartRun_UnregisterIsSafe verifies that a deferred unregister inside
// the goroutine does NOT remove a newer entry that was swapped in by a
// subsequent startRun. Without the identity check, we'd evict the new Run,
// breaking the coalesce invariant.
func TestStartRun_UnregisterIsSafe(t *testing.T) {
	runner := NewAgentRunner(nil, nil)
	const sid = "test-sid-unreg:v1"

	firstReleased := make(chan struct{})
	firstRun := func(runCtx context.Context, eventCh chan<- AgentEvent) {
		// Park until the test signals release — keeps the goroutine alive
		// so its deferred unregister is delayed past the second Run's start.
		<-firstReleased
		<-runCtx.Done() // also wait for cancellation
	}
	eventCh1 := runner.startRun(context.Background(), sid, firstRun)

	// Second Run — should swap in and cancel first.
	secondDone := make(chan struct{})
	secondRun := func(runCtx context.Context, eventCh chan<- AgentEvent) {
		defer close(secondDone)
		// Hold the goroutine open briefly so first's deferred unregister
		// races against our entry's continued presence.
		time.Sleep(50 * time.Millisecond)
	}
	_ = runner.startRun(context.Background(), sid, secondRun)

	// Release the first Run NOW — its deferred unregister runs concurrently
	// with the second Run still being active.
	close(firstReleased)

	<-secondDone
	<-eventCh1

	// At this point, second Run has completed and cleaned itself up.
	// Verify the map is empty (no leak).
	runner.inflightMu.Lock()
	_, stillThere := runner.inflightCancels[sid]
	runner.inflightMu.Unlock()
	if stillThere {
		t.Error("inflightCancels should be empty after both Runs completed")
	}
}

// TestStartRun_ConcurrentStartNoDeadlock stresses the swap-and-cancel path
// with N goroutines all trying to start Runs on the same sessionID. We don't
// assert specific completion counts (timing-dependent) — just verify no
// deadlock, no panic, all goroutines exit, and the map is empty at the end.
func TestStartRun_ConcurrentStartNoDeadlock(t *testing.T) {
	runner := NewAgentRunner(nil, nil)
	const sid = "test-sid-stress:v1"
	const N = 20

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ch := runner.startRun(context.Background(), sid, func(runCtx context.Context, eventCh chan<- AgentEvent) {
				// Tiny sleep so concurrent Runs overlap; many will be cancelled.
				select {
				case <-time.After(5 * time.Millisecond):
				case <-runCtx.Done():
				}
				sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
			})
			// Drain to allow the goroutine to exit cleanly.
			for range ch {
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock or excessive runtime in concurrent startRun")
	}

	// After all Runs complete, the map should be empty.
	runner.inflightMu.Lock()
	_, stillThere := runner.inflightCancels[sid]
	runner.inflightMu.Unlock()
	if stillThere {
		t.Error("inflightCancels should be empty after all Runs completed")
	}
}

// TestStartRun_DifferentSessionIDsIndependent verifies that Runs on
// different sessionIDs don't interfere with each other — the inflight
// map is keyed by sessionID.
func TestStartRun_DifferentSessionIDsIndependent(t *testing.T) {
	runner := NewAgentRunner(nil, nil)

	const sidA = "test-sid-A:v1"
	const sidB = "test-sid-B:v1"

	aCompleted := make(chan struct{})
	bCompleted := make(chan struct{})

	aRun := func(runCtx context.Context, eventCh chan<- AgentEvent) {
		defer close(aCompleted)
		// Sleep to verify B starts and runs concurrently without cancel.
		time.Sleep(30 * time.Millisecond)
		sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
	}
	bRun := func(runCtx context.Context, eventCh chan<- AgentEvent) {
		defer close(bCompleted)
		sendEvent(runCtx, eventCh, AgentEvent{Type: "done", IsFinal: true})
	}

	chA := runner.startRun(context.Background(), sidA, aRun)
	chB := runner.startRun(context.Background(), sidB, bRun)

	for range chA {
	}
	for range chB {
	}

	select {
	case <-aCompleted:
	case <-time.After(time.Second):
		t.Error("Run A was cancelled by Run B on different sessionID")
	}
	select {
	case <-bCompleted:
	case <-time.After(time.Second):
		t.Error("Run B did not complete")
	}
}
