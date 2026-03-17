package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	gw "github.com/costrict/costrict-web/gateway/internal"
)

func main() {
	cfg := gw.LoadConfig()
	manager := gw.NewTunnelManager()

	// Start HTTP server first (for health checks)
	r := gw.SetupRouter(manager, cfg)
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("[Gateway] starting on port %s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Gateway] failed to start: %v", err)
		}
	}()

	// Register with server in background
	registerWithRetry(cfg)

	stopHeartbeat := make(chan struct{})
	go heartbeatLoop(cfg, manager, stopHeartbeat)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Printf("[Gateway] shutting down gracefully...")
	close(stopHeartbeat)
	manager.NotifyAllOffline(cfg.ServerURL, cfg.GatewayID)
	if err := gw.Deregister(cfg.ServerURL, cfg.GatewayID); err != nil {
		log.Printf("[Gateway] deregister failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	log.Printf("[Gateway] shutdown complete")
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

func heartbeatLoop(cfg *gw.Config, manager *gw.TunnelManager, stop <-chan struct{}) {
	ticker := time.NewTicker(gw.HeartbeatInterval * time.Second)
	defer ticker.Stop()
	var lastEpoch int64
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
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
