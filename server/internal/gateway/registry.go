package gateway

import (
	"fmt"
	"log"
	"time"
)

type GatewayRegistry struct {
	store Store
	epoch int64
}

func NewGatewayRegistry(store Store) *GatewayRegistry {
	initVal := time.Now().UnixMilli()
	epoch, err := store.GetOrInitEpoch(initVal)
	if err != nil {
		log.Printf("[GatewayRegistry] failed to get epoch from store, using local: %v", err)
		epoch = initVal
	}
	r := &GatewayRegistry{store: store, epoch: epoch}
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
		log.Printf("[GatewayRegistry] BindDevice failed: %v", err)
	}
}

func (r *GatewayRegistry) UnbindDevice(deviceID string) {
	if err := r.store.UnbindDevice(deviceID); err != nil {
		log.Printf("[GatewayRegistry] UnbindDevice failed: %v", err)
	}
}

func (r *GatewayRegistry) Deregister(gatewayID string) error {
	return r.store.RemoveGateway(gatewayID)
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
			log.Printf("[GatewayRegistry] removing timed-out gateway %s", gw.ID)
			_ = r.store.RemoveGateway(gw.ID)
		}
	}
}
