package internal

import (
	"sync"

	"github.com/hashicorp/yamux"
)

type TunnelManager struct {
	mu       sync.RWMutex
	sessions map[string]*yamux.Session
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{sessions: make(map[string]*yamux.Session)}
}

func (m *TunnelManager) Register(deviceID string, session *yamux.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.sessions[deviceID]; ok {
		old.Close()
	}
	m.sessions[deviceID] = session
}

func (m *TunnelManager) Get(deviceID string) (*yamux.Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[deviceID]
	return s, ok
}

func (m *TunnelManager) Close(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[deviceID]; ok {
		s.Close()
		delete(m.sessions, deviceID)
	}
}

func (m *TunnelManager) UnregisterIf(deviceID string, session *yamux.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[deviceID]; ok && s == session {
		s.Close()
		delete(m.sessions, deviceID)
	}
}

func (m *TunnelManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}
