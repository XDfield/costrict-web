package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	gw "github.com/costrict/costrict-web/gateway/internal"
	"github.com/costrict/costrict-web/gateway/internal/logger"
	"github.com/gin-gonic/gin"
)

func main() {
	// Initialise structured logging with daily rotation and 7-day retention.
	logger.Init(logger.Config{
		Dir:        "./logs",
		MaxAgeDays: 7,
		Console:    true,
	})

	// Redirect Gin's built-in output to our log files.
	gin.DefaultWriter = logger.GinWriter()
	gin.DefaultErrorWriter = logger.GinErrorWriter()

	cfg := gw.LoadConfig()
	manager := gw.NewTunnelManager()

	endpoint, err := gw.NewEndpointResolver().Resolve(cfg)

	source := "env"
	if gw.NacosEnabled(cfg.Nacos) && err == nil {
		source = "nacos"
	}

	if err != nil {
		if errors.Is(err, gw.ErrNacosConfigNotFound) {
			log.Printf("[Gateway] Nacos config not found (dataId=%s, group=%s), falling back to env endpoint", cfg.Nacos.DataID, cfg.Nacos.Group)
		} else {
			log.Printf("[Gateway] failed to resolve endpoint from Nacos, falling back to env endpoint: %v", err)
		}
		endpoint = cfg.Endpoint
		source = "env"
	}
	log.Printf("[Gateway] endpoint resolved: source=%s value=%s", source, endpoint)

	apiBaseURL, err := gw.NewEndpointResolver().ResolveAPIBaseURL(cfg)
	apiSource := "env"
	if gw.NacosAPIBaseURLEnabled(cfg.Nacos) && err == nil {
		apiSource = "nacos"
	}
	if err != nil {
		if errors.Is(err, gw.ErrNacosConfigNotFound) {
			log.Printf("[Gateway] Nacos apiBaseURL config not found (dataId=%s, group=%s), falling back to env", cfg.Nacos.APIBaseURLDataID, cfg.Nacos.Group)
		} else {
			log.Printf("[Gateway] failed to resolve apiBaseURL from Nacos, falling back to env: %v", err)
		}
		apiBaseURL = cfg.APIBaseURL
		apiSource = "env"
	}
	log.Printf("[Gateway] apiBaseURL resolved: source=%s value=%q", apiSource, apiBaseURL)

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
	registerWithRetry(cfg, endpoint, apiBaseURL)

	stopHeartbeat := make(chan struct{})
	go heartbeatLoop(cfg, manager, endpoint, apiBaseURL, stopHeartbeat)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Printf("[Gateway] shutting down gracefully...")
	close(stopHeartbeat)
	manager.NotifyAllOffline(cfg.ServerURL, cfg.GatewayID, cfg.InternalSecret)
	if err := gw.Deregister(cfg.ServerURL, cfg.GatewayID, cfg.InternalSecret); err != nil {
		log.Printf("[Gateway] deregister failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	log.Printf("[Gateway] shutdown complete")
}

func registerWithRetry(cfg *gw.Config, endpoint, apiBaseURL string) {
	for {
		if err := gw.Register(cfg.ServerURL, cfg.GatewayID, endpoint, cfg.InternalURL, cfg.Region, cfg.InternalSecret, cfg.Capacity, apiBaseURL); err != nil {
			log.Printf("[Gateway] register failed, retrying in 5s: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("[Gateway] registered with server %s as %s (endpoint=%s)", cfg.ServerURL, cfg.GatewayID, endpoint)
		return
	}
}

func heartbeatLoop(cfg *gw.Config, manager *gw.TunnelManager, endpoint, apiBaseURL string, stop <-chan struct{}) {
	ticker := time.NewTicker(gw.HeartbeatInterval * time.Second)
	defer ticker.Stop()
	var lastEpoch int64
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		epoch, err := gw.Heartbeat(cfg.ServerURL, cfg.GatewayID, cfg.InternalSecret, manager.Count())
		if err != nil {
			log.Printf("[Gateway] heartbeat failed: %v, re-registering...", err)
			if regErr := gw.Register(cfg.ServerURL, cfg.GatewayID, endpoint, cfg.InternalURL, cfg.Region, cfg.InternalSecret, cfg.Capacity, apiBaseURL); regErr != nil {
				log.Printf("[Gateway] re-register failed: %v", regErr)
				continue
			}
			log.Printf("[Gateway] re-registered successfully")
			manager.NotifyAllOnline(cfg.ServerURL, cfg.GatewayID, cfg.InternalSecret)
			lastEpoch = 0
			continue
		}
		if lastEpoch != 0 && epoch != lastEpoch {
			log.Printf("[Gateway] server epoch changed (%d -> %d), re-notifying devices", lastEpoch, epoch)
			manager.NotifyAllOnline(cfg.ServerURL, cfg.GatewayID, cfg.InternalSecret)
		}
		lastEpoch = epoch
	}
}
