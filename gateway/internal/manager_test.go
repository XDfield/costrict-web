package internal

import (
	"net"
	"sync"
	"testing"

	"github.com/hashicorp/yamux"
)

func newTestSession(t *testing.T) *yamux.Session {
	t.Helper()
	conn1, conn2 := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	server, err := yamux.Server(conn1, cfg)
	if err != nil {
		t.Fatalf("yamux.Server: %v", err)
	}
	client, err := yamux.Client(conn2, cfg)
	if err != nil {
		t.Fatalf("yamux.Client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return server
}

// V001: Cross-gateway takeover — Register replaces old session, returns new connID
func TestRegister_ReplacesOldSession(t *testing.T) {
	m := NewTunnelManager()
	sessA := newTestSession(t)

	connID1 := m.Register("deviceX", sessA)
	if connID1 == "" {
		t.Fatal("Register returned empty connID")
	}

	sessB := newTestSession(t)
	connID2 := m.Register("deviceX", sessB)
	if connID2 == connID1 {
		t.Fatalf("second Register should return different connID: got %s twice", connID1)
	}

	if !sessA.IsClosed() {
		t.Fatal("old session should be closed after re-Register")
	}

	got, ok := m.Get("deviceX")
	if !ok || got != sessB {
		t.Fatal("Get should return the new session")
	}
}

// V002: Same-gateway takeover — Register within same gateway replaces session
func TestRegister_SameGatewayReplacesSession(t *testing.T) {
	m := NewTunnelManager()
	sessA := newTestSession(t)
	connID1 := m.Register("deviceX", sessA)

	sessB := newTestSession(t)
	connID2 := m.Register("deviceX", sessB)

	if !sessA.IsClosed() {
		t.Fatal("Register should close the old session in same gateway")
	}
	if connID1 == connID2 {
		t.Fatal("connID should differ between registrations")
	}
	if m.Count() != 1 {
		t.Fatalf("expected 1 session, got %d", m.Count())
	}
}

// V003/V010: UnregisterIf returns false when session has been replaced
func TestUnregisterIf_ReturnsFalseWhenReplaced(t *testing.T) {
	m := NewTunnelManager()
	sessA := newTestSession(t)
	m.Register("deviceX", sessA)

	sessB := newTestSession(t)
	m.Register("deviceX", sessB)

	if m.UnregisterIf("deviceX", sessA) {
		t.Fatal("UnregisterIf should return false when session was already replaced")
	}

	got, ok := m.Get("deviceX")
	if !ok || got != sessB {
		t.Fatal("sessB should still be active after UnregisterIf(sessA) returned false")
	}
}

func TestUnregisterIf_ReturnsTrueWhenMatch(t *testing.T) {
	m := NewTunnelManager()
	sessA := newTestSession(t)
	m.Register("deviceX", sessA)

	if !m.UnregisterIf("deviceX", sessA) {
		t.Fatal("UnregisterIf should return true when session matches")
	}
	if _, ok := m.Get("deviceX"); ok {
		t.Fatal("device should be removed after successful UnregisterIf")
	}
	if !sessA.IsClosed() {
		t.Fatal("session should be closed after UnregisterIf")
	}
}

// V012: Takeover chain A→B→C→A — each Register closes the previous
func TestRegister_TakeoverChain(t *testing.T) {
	m := NewTunnelManager()
	sessions := make([]*yamux.Session, 3)
	connIDs := make([]string, 3)

	for i := 0; i < 3; i++ {
		sessions[i] = newTestSession(t)
		connIDs[i] = m.Register("deviceX", sessions[i])
	}

	if !sessions[0].IsClosed() {
		t.Fatal("session 0 should be closed")
	}
	if !sessions[1].IsClosed() {
		t.Fatal("session 1 should be closed")
	}
	if sessions[2].IsClosed() {
		t.Fatal("session 2 should be alive (current)")
	}
	if m.Count() != 1 {
		t.Fatalf("expected 1 session, got %d", m.Count())
	}

	// A reconnects
	sessA2 := newTestSession(t)
	connID4 := m.Register("deviceX", sessA2)
	if !sessions[2].IsClosed() {
		t.Fatal("session 2 should be closed after A reconnects")
	}
	if sessA2.IsClosed() {
		t.Fatal("new session should be alive")
	}
	for _, id := range connIDs {
		if id == connID4 {
			t.Fatal("connID should be unique across all registrations")
		}
	}
}

// V018: Gateway restart — all sessions lost, new Register works
func TestRegister_AfterRestart(t *testing.T) {
	m1 := NewTunnelManager()
	sessA := newTestSession(t)
	m1.Register("deviceX", sessA)

	// Simulate restart: create a fresh manager
	m2 := NewTunnelManager()
	sessB := newTestSession(t)
	connID := m2.Register("deviceX", sessB)

	if connID == "" {
		t.Fatal("Register on fresh manager should work")
	}
	got, ok := m2.Get("deviceX")
	if !ok || got != sessB {
		t.Fatal("new manager should have the session")
	}
}

// V024: CloseIfConnID with stale connID does not close current session
func TestCloseIfConnID_StaleConnIDDoesNotClose(t *testing.T) {
	m := NewTunnelManager()
	sessA := newTestSession(t)
	connID1 := m.Register("deviceX", sessA)

	// Simulate reconnect: new session replaces old
	sessB := newTestSession(t)
	connID2 := m.Register("deviceX", sessB)

	// Close request with old connID should NOT close
	if m.CloseIfConnID("deviceX", connID1) {
		t.Fatal("CloseIfConnID with stale connID should return false")
	}
	got, ok := m.Get("deviceX")
	if !ok || got != sessB {
		t.Fatal("current session should survive stale close request")
	}
	if sessB.IsClosed() {
		t.Fatal("current session should not be closed")
	}

	// Close with current connID should succeed
	if !m.CloseIfConnID("deviceX", connID2) {
		t.Fatal("CloseIfConnID with matching connID should return true")
	}
	if !sessB.IsClosed() {
		t.Fatal("session should be closed after CloseIfConnID with matching connID")
	}
}

// V029: CloseIfConnID with empty connID unconditionally closes (backward compat)
func TestCloseIfConnID_EmptyConnIDClosesUnconditionally(t *testing.T) {
	m := NewTunnelManager()
	sessA := newTestSession(t)
	m.Register("deviceX", sessA)

	if !m.CloseIfConnID("deviceX", "") {
		t.Fatal("CloseIfConnID with empty connID should close unconditionally")
	}
	if !sessA.IsClosed() {
		t.Fatal("session should be closed")
	}
	if _, ok := m.Get("deviceX"); ok {
		t.Fatal("device should be removed")
	}
}

func TestCloseIfConnID_DeviceNotFound(t *testing.T) {
	m := NewTunnelManager()
	if m.CloseIfConnID("nonexistent", "") {
		t.Fatal("CloseIfConnID should return false for nonexistent device")
	}
}

// V042: Multi-level reconnect race — close(connID=1) when current is connID=4
func TestCloseIfConnID_MultiLevelReconnectRace(t *testing.T) {
	m := NewTunnelManager()
	sess1 := newTestSession(t)
	connID1 := m.Register("deviceX", sess1)

	// Multiple reconnects
	sess2 := newTestSession(t)
	m.Register("deviceX", sess2)
	sess3 := newTestSession(t)
	connID3 := m.Register("deviceX", sess3)

	// Another takeover (simulating C)
	sess4 := newTestSession(t)
	connID4 := m.Register("deviceX", sess4)

	// Stale close request from the first takeover
	if m.CloseIfConnID("deviceX", connID1) {
		t.Fatal("close(connID=1) should not close when current is connID=4")
	}
	if !sess4.IsClosed() {
		// Good, session 4 survives
	} else {
		t.Fatal("connID=4 session should not be closed by stale connID=1 close request")
	}

	// Close from the second takeover (connID=3) should also not affect connID=4
	if m.CloseIfConnID("deviceX", connID3) {
		t.Fatal("close(connID=3) should not close when current is connID=4")
	}

	_ = connID4 // just to reference
}

// V022: Concurrent Register() and UnregisterIf() are mutually exclusive
func TestConcurrent_RegisterAndUnregisterIf(t *testing.T) {
	m := NewTunnelManager()
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n * 2)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			sess := newTestSession(t)
			m.Register("deviceX", sess)
		}()
		go func() {
			defer wg.Done()
			sess, ok := m.Get("deviceX")
			if ok {
				m.UnregisterIf("deviceX", sess)
			}
		}()
	}
	wg.Wait()

	// After all goroutines, exactly 0 or 1 sessions exist
	if m.Count() > 1 {
		t.Fatalf("expected at most 1 session after concurrent ops, got %d", m.Count())
	}
}

// V009 (gateway-side): Concurrent Register from two different "clones"
func TestConcurrent_RegisterTwoDevices(t *testing.T) {
	m := NewTunnelManager()
	var wg sync.WaitGroup
	wg.Add(2)

	var connID1, connID2 string
	go func() {
		defer wg.Done()
		sess := newTestSession(t)
		connID1 = m.Register("deviceA", sess)
	}()
	go func() {
		defer wg.Done()
		sess := newTestSession(t)
		connID2 = m.Register("deviceB", sess)
	}()
	wg.Wait()

	if connID1 == "" || connID2 == "" {
		t.Fatal("both connIDs should be non-empty")
	}
	if connID1 == connID2 {
		t.Fatal("connIDs for different devices should differ")
	}
	if m.Count() != 2 {
		t.Fatalf("expected 2 sessions, got %d", m.Count())
	}
}
