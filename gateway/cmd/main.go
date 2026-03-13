package main

import (
	"log"

	gw "github.com/costrict/costrict-web/gateway/internal"
)

func main() {
	cfg := gw.LoadConfig()
	manager := gw.NewConnectionManager()

	if err := gw.Register(cfg.ServerURL, cfg.GatewayID, cfg.Endpoint, cfg.InternalURL, cfg.Region, cfg.Capacity); err != nil {
		log.Printf("[Gateway] WARNING: failed to register with server: %v", err)
	} else {
		log.Printf("[Gateway] registered with server %s as %s", cfg.ServerURL, cfg.GatewayID)
	}

	go gw.StartHeartbeat(cfg.ServerURL, cfg.GatewayID, manager)

	r := gw.SetupRouter(manager, cfg)

	log.Printf("[Gateway] starting on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("[Gateway] failed to start: %v", err)
	}
}
