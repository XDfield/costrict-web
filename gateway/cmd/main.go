package main

import (
	"log"
	"time"

	gw "github.com/costrict/costrict-web/gateway/internal"
)

func main() {
	cfg := gw.LoadConfig()
	manager := gw.NewTunnelManager()

	registerWithRetry(cfg)

	go heartbeatLoop(cfg, manager)

	r := gw.SetupRouter(manager, cfg)

	log.Printf("[Gateway] starting on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("[Gateway] failed to start: %v", err)
	}
}

func registerWithRetry(cfg *gw.Config) {
	for {
		if err := gw.Register(cfg.ServerURL, cfg.GatewayID, cfg.Endpoint, cfg.InternalURL, cfg.Region, cfg.Capacity); err != nil {
			log.Printf("[Gateway] register failed, retrying in 5s: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("[Gateway] registered with server %s as %s", cfg.ServerURL, cfg.GatewayID)
		return
	}
}

func heartbeatLoop(cfg *gw.Config, manager *gw.TunnelManager) {
	ticker := time.NewTicker(gw.HeartbeatInterval * time.Second)
	defer ticker.Stop()
	var lastEpoch int64
	for range ticker.C {
		epoch, err := gw.Heartbeat(cfg.ServerURL, cfg.GatewayID, manager.Count())
		if err != nil {
			log.Printf("[Gateway] heartbeat failed: %v, re-registering...", err)
			if regErr := gw.Register(cfg.ServerURL, cfg.GatewayID, cfg.Endpoint, cfg.InternalURL, cfg.Region, cfg.Capacity); regErr != nil {
				log.Printf("[Gateway] re-register failed: %v", regErr)
				continue
			}
			log.Printf("[Gateway] re-registered successfully")
			manager.NotifyAllOnline(cfg.ServerURL, cfg.GatewayID)
			lastEpoch = 0
			continue
		}
		if lastEpoch != 0 && epoch != lastEpoch {
			log.Printf("[Gateway] server epoch changed (%d -> %d), re-notifying devices", lastEpoch, epoch)
			manager.NotifyAllOnline(cfg.ServerURL, cfg.GatewayID)
		}
		lastEpoch = epoch
	}
}
