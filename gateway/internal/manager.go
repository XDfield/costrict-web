package internal

import (
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/yamux"
)

var connIDCounter uint64

type managedSession struct {
	session *yamux.Session
	connID  string
}

type TunnelManager struct {
	mu       sync.RWMutex
	sessions map[string]*managedSession
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{sessions: make(map[string]*managedSession)}
}

func nextConnID() string {
	return strconv.FormatUint(atomic.AddUint64(&connIDCounter, 1), 36)
}

// Register creates a new tunnel session for deviceID with a unique connID.
// If a previous session exists, it is closed (same-gateway replacement).
// Returns the connID so callers can pass it to NotifyOnline.
func (m *TunnelManager) Register(deviceID string, session *yamux.Session) string {
	connID := nextConnID()
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.sessions[deviceID]; ok {
		old.session.Close()
	}
	m.sessions[deviceID] = &managedSession{session: session, connID: connID}
	return connID
}

func (m *TunnelManager) Get(deviceID string) (*yamux.Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ms, ok := m.sessions[deviceID]
	if !ok {
		return nil, false
	}
	return ms.session, true
}

func (m *TunnelManager) Close(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ms, ok := m.sessions[deviceID]; ok {
		ms.session.Close()
		delete(m.sessions, deviceID)
	}
}

// CloseIfConnID closes the session for deviceID only if the stored connID matches.
// Returns true if the session was found and closed.
// If connID is empty, falls back to unconditional close for backward compatibility.
func (m *TunnelManager) CloseIfConnID(deviceID, connID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.sessions[deviceID]
	if !ok {
		return false
	}
	if connID != "" && ms.connID != connID {
		return false
	}
	ms.session.Close()
	delete(m.sessions, deviceID)
	return true
}

func (m *TunnelManager) UnregisterIf(deviceID string, session *yamux.Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ms, ok := m.sessions[deviceID]; ok && ms.session == session {
		ms.session.Close()
		delete(m.sessions, deviceID)
		return true
	}
	return false
}

func (m *TunnelManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}
