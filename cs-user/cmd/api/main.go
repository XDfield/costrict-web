//	@title			cs-user API
//	@version		1.0
//	@description	User identity service for the costrict-cloud platform (Phase 1: user CRUD + read-through RPC).

//	@host		localhost:8081
//	@BasePath	/

//	@securityDefinitions.apikey	InternalToken
//	@in							header
//	@name						X-Internal-Token
//	@description				Shared secret for service-to-service calls from costrict-web (ADR D8).

// cs-user is the user identity service for the costrict-cloud platform.
//
// Phase 1 scope (ADR_CS_USER_PHASE1_DECISIONS.md):
//   - user data ownership (users / user_auth_identities CRUD)
//   - read-through RPC consumed by costrict-web
//   - REST only (no gRPC)
//   - X-Internal-Token auth for /api/internal/* routes
//
// Out of scope for Phase 1: JWT self-signing, OAuth callback takeover,
// employment_identities, tenant_configs, webhook.
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

	_ "github.com/costrict/costrict-web/cs-user/docs"
	"github.com/costrict/costrict-web/cs-user/internal/app"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"go.uber.org/zap"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	defer logger.Sync()
	zap.ReplaceGlobals(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("load config", zap.Error(err))
	}

	// Phase 1 P0-1: stub readiness checker (HTTP-only). P0-2 will pass a real
	// Postgres ping here.
	r := app.NewRouter(cfg, nil)

	srv := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("cs-user listening", zap.String("port", cfg.HTTP.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("listen", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutdown signal received")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("forced shutdown", zap.Error(err))
	}
	logger.Info("bye")
}
