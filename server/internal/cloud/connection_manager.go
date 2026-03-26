package cloud

import (
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/google/uuid"
)

type ConnectionManager struct {
	mu                   sync.RWMutex
	connections          map[string]*SSEConnection
	userConnections      map[string]map[string]struct{}
	sessionSubscriptions map[string]map[string]struct{}
	startTime            time.Time
}

func NewConnectionManager() *ConnectionManager {
	m := &ConnectionManager{
		connections:          make(map[string]*SSEConnection),
		userConnections:      make(map[string]map[string]struct{}),
		sessionSubscriptions: make(map[string]map[string]struct{}),
		startTime:            time.Now(),
	}
	go m.startHeartbeat()
	go m.startCleanup()
	return m
}

func (m *ConnectionManager) newConn(userID, workspaceID string) *SSEConnection {
	return &SSEConnection{
		ID:           uuid.New().String(),
		Type:         ConnTypeUser,
		UserID:       userID,
		WorkspaceID:  workspaceID,
		Send:         make(chan Event, SendChannelCapacity),
		Done:         make(chan struct{}),
		LastActivity: time.Now().UnixMilli(),
	}
}

func (m *ConnectionManager) RegisterUserConnection(userID, workspaceID string) (*SSEConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if conns, ok := m.userConnections[userID]; ok && len(conns) >= MaxConnectionsPerUser {
		return nil, ErrConnectionLimitExceeded
	}

	conn := m.newConn(userID, workspaceID)
	m.connections[conn.ID] = conn

	if m.userConnections[userID] == nil {
		m.userConnections[userID] = make(map[string]struct{})
	}
	m.userConnections[userID][conn.ID] = struct{}{}

	return conn, nil
}

func (m *ConnectionManager) SubscribeToSession(sessionID, connID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	conn, ok := m.connections[connID]
	if !ok {
		return ErrConnectionNotFound
	}

	if subs, ok := m.sessionSubscriptions[sessionID]; ok {
		userSubCount := 0
		for subConnID := range subs {
			if subConn, exists := m.connections[subConnID]; exists && subConn.UserID == conn.UserID {
				userSubCount++
			}
		}
		if userSubCount >= MaxSubscriptionsPerUser {
			return ErrSubscriptionLimitExceeded
		}
	}

	if m.sessionSubscriptions[sessionID] == nil {
		m.sessionSubscriptions[sessionID] = make(map[string]struct{})
	}
	m.sessionSubscriptions[sessionID][connID] = struct{}{}

	return nil
}

func (m *ConnectionManager) UnsubscribeFromSession(sessionID, connID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if subs, ok := m.sessionSubscriptions[sessionID]; ok {
		delete(subs, connID)
		if len(subs) == 0 {
			delete(m.sessionSubscriptions, sessionID)
		}
	}
}

func (m *ConnectionManager) CloseConnection(connID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	conn, ok := m.connections[connID]
	if !ok {
		return
	}
	m.closeConnLocked(conn)
}

func (m *ConnectionManager) closeConnLocked(conn *SSEConnection) {
	select {
	case <-conn.Done:
	default:
		close(conn.Done)
	}

	delete(m.connections, conn.ID)

	if conns, ok := m.userConnections[conn.UserID]; ok {
		delete(conns, conn.ID)
		if len(conns) == 0 {
			delete(m.userConnections, conn.UserID)
		}
	}
	for sessionID, subs := range m.sessionSubscriptions {
		delete(subs, conn.ID)
		if len(subs) == 0 {
			delete(m.sessionSubscriptions, sessionID)
		}
	}
}

func (m *ConnectionManager) RouteEvent(event Event, targetConnIDs []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, connID := range targetConnIDs {
		conn, ok := m.connections[connID]
		if !ok {
			continue
		}
		select {
		case conn.Send <- event:
			conn.LastActivity = time.Now().UnixMilli()
		default:
			logger.Warn("[CloudSSE] connection %s send buffer full, dropping event %s", conn.ID, event.Type)
		}
	}
}

func (m *ConnectionManager) FindUserConnsBySession(sessionID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	subs, ok := m.sessionSubscriptions[sessionID]
	if !ok {
		return nil
	}

	result := make([]string, 0, len(subs))
	for connID := range subs {
		if _, exists := m.connections[connID]; exists {
			result = append(result, connID)
		}
	}
	return result
}

func (m *ConnectionManager) GetConn(connID string) *SSEConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connections[connID]
}

func (m *ConnectionManager) Stats() ManagerStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return ManagerStats{
		TotalConnections:     len(m.connections),
		UserConnections:      len(m.userConnections),
		SessionSubscriptions: len(m.sessionSubscriptions),
		Uptime:               int64(time.Since(m.startTime).Seconds()),
	}
}

func (m *ConnectionManager) startHeartbeat() {
	ticker := time.NewTicker(HeartbeatIntervalMs * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		m.mu.RLock()
		for _, conn := range m.connections {
			event := Event{
				Type:       EventHeartbeat,
				Properties: map[string]any{"timestamp": now},
			}
			select {
			case conn.Send <- event:
				conn.LastActivity = now
			default:
			}
		}
		m.mu.RUnlock()
	}
}

func (m *ConnectionManager) startCleanup() {
	ticker := time.NewTicker(CleanupIntervalMs * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		var stale []string

		m.mu.RLock()
		for connID, conn := range m.connections {
			if now-conn.LastActivity > ConnectionTimeoutMs {
				stale = append(stale, connID)
			}
		}
		m.mu.RUnlock()

		for _, connID := range stale {
			logger.Warn("[CloudSSE] closing timed-out connection %s", connID)
			m.CloseConnection(connID)
		}
	}
}
