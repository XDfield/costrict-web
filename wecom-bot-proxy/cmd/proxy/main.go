package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/api"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/backend"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/config"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/dedup"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/router"
	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/ws"
	"github.com/gin-gonic/gin"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Setup logger
	var logger *slog.Logger
	if cfg.Logging.Format == "json" {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: parseLogLevel(cfg.Logging.Level),
		}))
	} else {
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: parseLogLevel(cfg.Logging.Level),
		}))
	}
	slog.SetDefault(logger)

	logger.Info("starting wecom-bot-proxy",
		"bot_id", cfg.Bot.BotID,
		"listen", cfg.Server.Listen,
		"backends", cfg.BackendNames(),
		"default_backend", cfg.Routing.DefaultBackend,
	)

	// Initialize dedup store
	var dedupStore *dedup.Store
	if cfg.Dedup.Enabled {
		dedupStore, err = dedup.NewStore(cfg.Dedup.MaxEntries, time.Duration(cfg.Dedup.TTLSeconds)*time.Second)
		if err != nil {
			logger.Error("failed to create dedup store", "error", err)
			os.Exit(1)
		}
	}

	// Initialize route table
	routes, err := router.NewTable(
		cfg.Routing.DefaultBackend,
		cfg.Dedup.MaxEntries,
		cfg.Routing.TaskRouteTTL,
	)
	if err != nil {
		logger.Error("failed to create route table", "error", err)
		os.Exit(1)
	}

	// Initialize backend manager
	backendMgr := backend.NewManager(cfg.Backends, logger)

	// Create proxy (core orchestrator)
	proxy := api.NewProxy(cfg, logger, nil, routes, backendMgr, dedupStore)

	// Initialize WS connection
	wsConn := ws.NewConn(cfg.Bot, logger, proxy.HandleWSFrame)
	proxy.SetWSConn(wsConn)

	// Context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Start WS connection
	wsConn.Start(ctx)

	// Setup HTTP server
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	proxy.RegisterRoutes(engine)

	// Run HTTP server
	srvErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.Server.Listen)
		if err := engine.Run(cfg.Server.Listen); err != nil {
			srvErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-srvErr:
		logger.Error("http server error", "error", err)
	}
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
