package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

type ConnectionManager struct {
	mu          sync.RWMutex
	connections map[string]*DeviceConnection
}

func NewConnectionManager() *ConnectionManager {
	m := &ConnectionManager{
		connections: make(map[string]*DeviceConnection),
	}
	go m.startHeartbeat()
	return m
}

func (m *ConnectionManager) Register(deviceID string) *DeviceConnection {
	m.mu.Lock()
	defer m.mu.Unlock()

	if old, ok := m.connections[deviceID]; ok {
		select {
		case <-old.Done:
		default:
			close(old.Done)
		}
	}

	conn := &DeviceConnection{
		DeviceID:     deviceID,
		Send:         make(chan []byte, SendChannelCapacity),
		Done:         make(chan struct{}),
		LastActivity: time.Now().UnixMilli(),
	}
	m.connections[deviceID] = conn
	return conn
}

func (m *ConnectionManager) Close(deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	conn, ok := m.connections[deviceID]
	if !ok {
		return
	}
	select {
	case <-conn.Done:
	default:
		close(conn.Done)
	}
	delete(m.connections, deviceID)
}

func (m *ConnectionManager) Send(deviceID string, data []byte) error {
	m.mu.RLock()
	conn, ok := m.connections[deviceID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("device not connected")
	}

	select {
	case conn.Send <- data:
		conn.LastActivity = time.Now().UnixMilli()
	default:
		log.Printf("[Gateway] WARN: send buffer full for device %s, dropping event", deviceID)
	}
	return nil
}

func (m *ConnectionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connections)
}

func (m *ConnectionManager) startHeartbeat() {
	ticker := time.NewTicker(HeartbeatInterval * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		event := Event{
			Type:       "heartbeat",
			Properties: map[string]any{"timestamp": now},
		}
		data, _ := json.Marshal(event)
		payload := fmt.Sprintf("event: message\ndata: %s\n\n", data)

		m.mu.RLock()
		for _, conn := range m.connections {
			select {
			case conn.Send <- []byte(payload):
				conn.LastActivity = now
			default:
			}
		}
		m.mu.RUnlock()
	}
}
