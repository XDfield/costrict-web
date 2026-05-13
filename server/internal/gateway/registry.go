package gateway

import (
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
)

type GatewayRegistry struct {
	store           Store
	epoch           int64
	onDevicesOffline func(deviceIDs []string)
}

func NewGatewayRegistry(store Store, onDevicesOffline func(deviceIDs []string)) *GatewayRegistry {
	initVal := time.Now().UnixMilli()
	epoch, err := store.GetOrInitEpoch(initVal)
	if err != nil {
		logger.Warn("[GatewayRegistry] failed to get epoch from store, using local: %v", err)
		epoch = initVal
	}
	r := &GatewayRegistry{store: store, epoch: epoch, onDevicesOffline: onDevicesOffline}
	go r.startCleanup()
	return r
}

func (r *GatewayRegistry) Epoch() int64 {
	return r.epoch
}

func (r *GatewayRegistry) Register(info *GatewayInfo) error {
	return r.store.RegisterGateway(info)
}

func (r *GatewayRegistry) Heartbeat(gatewayID string, currentConns int) error {
	return r.store.HeartbeatGateway(gatewayID, currentConns)
}

func (r *GatewayRegistry) Allocate(region string) (*GatewayInfo, error) {
	gateways, err := r.store.ListGateways()
	if err != nil {
		return nil, err
	}

	now := time.Now().UnixMilli()
	var candidates []*GatewayInfo
	for _, gw := range gateways {
		if now-gw.LastHeartbeat > GatewayHeartbeatTimeoutMs {
			continue
		}
		if gw.Capacity > 0 && gw.CurrentConns >= gw.Capacity {
			continue
		}
		candidates = append(candidates, gw)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no gateway available")
	}

	var sameRegion []*GatewayInfo
	for _, gw := range candidates {
		if gw.Region == region {
			sameRegion = append(sameRegion, gw)
		}
	}
	if len(sameRegion) > 0 {
		candidates = sameRegion
	}

	best := candidates[0]
	for _, gw := range candidates[1:] {
		if gw.CurrentConns < best.CurrentConns {
			best = gw
		}
	}
	return best, nil
}

func (r *GatewayRegistry) BindDevice(deviceID, gatewayID string) {
	if err := r.store.BindDevice(deviceID, gatewayID); err != nil {
		logger.Error("[GatewayRegistry] BindDevice failed: %v", err)
	}
}

func (r *GatewayRegistry) UnbindDevice(deviceID string) {
	if err := r.store.UnbindDevice(deviceID); err != nil {
		logger.Error("[GatewayRegistry] UnbindDevice failed: %v", err)
	}
}

func (r *GatewayRegistry) Deregister(gatewayID string) error {
	deviceIDs, err := r.store.RemoveGatewayWithDevices(gatewayID)
	if err != nil {
		return err
	}
	r.notifyOffline(deviceIDs)
	return nil
}

func (r *GatewayRegistry) notifyOffline(deviceIDs []string) {
	if len(deviceIDs) == 0 || r.onDevicesOffline == nil {
		return
	}
	logger.Info("[GatewayRegistry] marking %d device(s) offline", len(deviceIDs))
	r.onDevicesOffline(deviceIDs)
}

func (r *GatewayRegistry) GetDeviceGateway(deviceID string) (*GatewayInfo, error) {
	gwID, err := r.store.GetDeviceGateway(deviceID)
	if err != nil {
		return nil, fmt.Errorf("device not connected")
	}

	gateways, err := r.store.ListGateways()
	if err != nil {
		return nil, err
	}
	for _, gw := range gateways {
		if gw.ID == gwID {
			return gw, nil
		}
	}
	return nil, fmt.Errorf("gateway %s not found", gwID)
}

func (r *GatewayRegistry) IsDeviceBound(deviceID string) bool {
	gwID, err := r.store.GetDeviceGateway(deviceID)
	if err != nil {
		return false
	}
	gateways, err := r.store.ListGateways()
	if err != nil {
		return false
	}
	now := time.Now().UnixMilli()
	for _, gw := range gateways {
		if gw.ID == gwID && now-gw.LastHeartbeat <= GatewayHeartbeatTimeoutMs {
			return true
		}
	}
	return false
}

func (r *GatewayRegistry) startCleanup() {
	ticker := time.NewTicker(GatewayCleanupIntervalMs * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		ok, _ := r.store.TryLock("gateway:cleanup:lock", 15*time.Second)
		if !ok {
			continue
		}
		r.doCleanup()
	}
}

func (r *GatewayRegistry) doCleanup() {
	gateways, err := r.store.ListGateways()
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, gw := range gateways {
		if now-gw.LastHeartbeat > GatewayHeartbeatTimeoutMs {
			logger.Warn("[GatewayRegistry] removing timed-out gateway %s", gw.ID)
			deviceIDs, err := r.store.RemoveGatewayWithDevices(gw.ID)
			if err != nil {
				logger.Error("[GatewayRegistry] failed to remove gateway %s: %v", gw.ID, err)
				continue
			}
			r.notifyOffline(deviceIDs)
		}
	}
}
