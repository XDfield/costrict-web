package gateway

import (
	"fmt"
	"sync"
	"time"
)

type Store interface {
	RegisterGateway(info *GatewayInfo) error
	HeartbeatGateway(gatewayID string, currentConns int) error
	ListGateways() ([]*GatewayInfo, error)
	RemoveGateway(gatewayID string) error
	RemoveGatewayWithDevices(gatewayID string) ([]string, error)

	BindDevice(deviceID, gatewayID string) error
	UnbindDevice(deviceID string) error
	GetDeviceGateway(deviceID string) (string, error)
	ListDevicesByGateway(gatewayID string) ([]string, error)

	TryLock(key string, ttl time.Duration) (bool, error)

	// GetOrInitEpoch 返回集群共享 epoch：若不存在则以 initVal 初始化并返回 initVal，否则返回已有值。
	GetOrInitEpoch(initVal int64) (int64, error)
}

type MemoryStore struct {
	mu            sync.RWMutex
	gateways      map[string]*GatewayInfo
	heartbeats    map[string]int64
	deviceGateway map[string]string
	epoch         int64
}

func NewMemoryStore() Store {
	return &MemoryStore{
		gateways:      make(map[string]*GatewayInfo),
		heartbeats:    make(map[string]int64),
		deviceGateway: make(map[string]string),
	}
}

func (s *MemoryStore) RegisterGateway(info *GatewayInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateways[info.ID] = info
	s.heartbeats[info.ID] = time.Now().UnixMilli()
	return nil
}

func (s *MemoryStore) HeartbeatGateway(gatewayID string, currentConns int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	gw, ok := s.gateways[gatewayID]
	if !ok {
		return fmt.Errorf("gateway %s not found", gatewayID)
	}
	gw.CurrentConns = currentConns
	gw.LastHeartbeat = time.Now().UnixMilli()
	s.heartbeats[gatewayID] = gw.LastHeartbeat
	return nil
}

func (s *MemoryStore) ListGateways() ([]*GatewayInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UnixMilli()
	result := make([]*GatewayInfo, 0, len(s.gateways))
	for id, gw := range s.gateways {
		copy := *gw
		if hb, ok := s.heartbeats[id]; ok {
			copy.LastHeartbeat = hb
		}
		_ = now
		result = append(result, &copy)
	}
	return result, nil
}

func (s *MemoryStore) RemoveGateway(gatewayID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.gateways, gatewayID)
	delete(s.heartbeats, gatewayID)
	return nil
}

func (s *MemoryStore) RemoveGatewayWithDevices(gatewayID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.gateways, gatewayID)
	delete(s.heartbeats, gatewayID)
	var deviceIDs []string
	for devID, gwID := range s.deviceGateway {
		if gwID == gatewayID {
			deviceIDs = append(deviceIDs, devID)
			delete(s.deviceGateway, devID)
		}
	}
	return deviceIDs, nil
}

func (s *MemoryStore) BindDevice(deviceID, gatewayID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceGateway[deviceID] = gatewayID
	return nil
}

func (s *MemoryStore) UnbindDevice(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deviceGateway, deviceID)
	return nil
}

func (s *MemoryStore) GetDeviceGateway(deviceID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	gwID, ok := s.deviceGateway[deviceID]
	if !ok {
		return "", fmt.Errorf("device %s not found", deviceID)
	}
	return gwID, nil
}

func (s *MemoryStore) ListDevicesByGateway(gatewayID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var deviceIDs []string
	for devID, gwID := range s.deviceGateway {
		if gwID == gatewayID {
			deviceIDs = append(deviceIDs, devID)
		}
	}
	return deviceIDs, nil
}

func (s *MemoryStore) TryLock(key string, ttl time.Duration) (bool, error) {
	return true, nil
}

func (s *MemoryStore) GetOrInitEpoch(initVal int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch == 0 {
		s.epoch = initVal
	}
	return s.epoch, nil
}
