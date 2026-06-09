package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	_ "github.com/joho/godotenv/autoload"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/costrict/costrict-web/proxy/internal"
	"github.com/costrict/costrict-web/proxy/internal/audit"
	"github.com/costrict/costrict-web/proxy/internal/filter"
	"github.com/costrict/costrict-web/proxy/internal/logger"
	"github.com/costrict/costrict-web/proxy/internal/middleware"
)

func main() {
	logger.Init(logger.Config{
		Dir:        "./logs",
		MaxAgeDays: 7,
		Console:    true,
	})

	gin.DefaultWriter = logger.GinWriter()
	gin.DefaultErrorWriter = logger.GinErrorWriter()

	cfg := internal.LoadConfig()

	if cfg.DatabaseURL == "" {
		logger.Fatal("DATABASE_URL is required")
	}
	if _, err := url.Parse(cfg.DatabaseURL); err != nil {
		logger.Fatal("DATABASE_URL format invalid: %v", err)
	}

	auditStore, err := audit.NewStore(cfg.DatabaseURL, cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLifetimeSec)
	if err != nil {
		logger.Fatal("failed to connect database: %v", err)
	}
	if err := auditStore.Ping(); err != nil {
		logger.Fatal("database ping failed: %v", err)
	}
	if err := auditStore.AutoMigrate(); err != nil {
		logger.Fatal("database automigrate failed: %v", err)
	}
	logger.Info("database connected and migrated")

	auditWorker := audit.NewWorker(auditStore, cfg.AuditChannelSize, cfg.AuditBatchSize, cfg.AuditFlushIntervalMs, cfg.AuditRetentionDays)
	go auditWorker.Start()
	logger.Info("audit worker started")

	rules, err := filter.LoadRules(cfg.FilterRulesPath)
	if err != nil {
		logger.Warn("failed to load filter rules, using defaults: %v", err)
		rules = filter.DefaultRules()
	}
	logger.Info("filter rules loaded")

	if cfg.ServerURL != "" {
		if err := checkUpstream(cfg.ServerURL); err != nil {
			logger.Warn("upstream server not reachable: %v", err)
		} else {
			logger.Info("upstream server reachable at %s", cfg.ServerURL)
		}
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.JWTDecode())

	router := internal.NewRouter(engine, cfg, rules, auditWorker)
	router.Setup()

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: engine,
	}

	go func() {
		logger.Info("session proxy starting on %s → %s", cfg.ListenAddr, cfg.ServerURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("failed to start: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	logger.Info("shutting down gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout)*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("server shutdown error: %v", err)
	}

	auditWorker.Stop()
	logger.Info("audit worker stopped")

	if err := auditStore.Close(); err != nil {
		logger.Error("database close error: %v", err)
	}

	logger.Sync()
	logger.Info("shutdown complete")
}

func checkUpstream(serverURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/health")
	if err != nil {
		return fmt.Errorf("upstream unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return nil
}
