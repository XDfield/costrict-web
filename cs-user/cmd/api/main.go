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
	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/migration"
	"github.com/costrict/costrict-web/cs-user/internal/storage"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/costrict/costrict-web/cs-user/internal/user"
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

	// Open the independent Postgres pool (ADR D1). main owns the lifecycle;
	// shutdown closes it after the HTTP server stops accepting traffic.
	pool, err := storage.Open(cfg)
	if err != nil {
		logger.Fatal("open postgres", zap.Error(err))
	}
	defer func() {
		if cerr := pool.Close(); cerr != nil {
			logger.Error("close postgres pool", zap.Error(cerr))
		}
	}()
	logger.Info("postgres pool opened",
		zap.String("host", cfg.Postgres.Host),
		zap.String("db", cfg.Postgres.Database))

	// Construct the user Service bound to the pool. P0-3 wires only the
	// read methods; write methods (bind/unbind/transfer) land in Phase A
	// once JWT-claims plumbing is available.
	userSvc := user.NewService(pool.Gorm)

	// Dev-mode auto-migrate: when CS_USER_AUTO_MIGRATE is truthy ("1"/"true"),
	// apply pending migrations inline at boot so local dev doesn't need a
	// separate migrate invocation. Prod wiring (Helm pre-deploy hook calling
	// the migrate binary) is added in P0-5; this guard keeps prod explicit.
	if isTruthy(os.Getenv("CS_USER_AUTO_MIGRATE")) {
		sqlDB, err := pool.SQLDB()
		if err != nil {
			logger.Fatal("acquire sql.DB for migration", zap.Error(err))
		}
		// gorm postgres driver maps to the "postgres" goose dialect.
		runner, err := migration.NewRunner(sqlDB, "postgres")
		if err != nil {
			logger.Fatal("init migration runner", zap.Error(err))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := runner.Up(ctx); err != nil {
			cancel()
			logger.Fatal("auto-migrate", zap.Error(err))
		}
		cancel()
		logger.Info("auto-migrate applied")
	}

	// Load the JWT signer when configured (Phase A3). When the env var is
	// unset, signer stays nil and /.well-known/jwks returns 503 — this keeps
	// Phase A boot optional so a deployment that hasn't cutover to JWT
	// self-signing can still run. A7 (OAuth callback takeover) will tighten
	// this to required.
	var signer *auth.Signer
	if cfg.JWT.SigningKeyPath != "" {
		signer, err = auth.NewSignerFromPEMPath(cfg.JWT.SigningKeyPath)
		if err != nil {
			logger.Fatal("load JWT signing key", zap.String("path", cfg.JWT.SigningKeyPath), zap.Error(err))
		}
		logger.Info("JWT signer loaded", zap.String("kid", signer.KID()))
	} else {
		logger.Warn("CS_USER_JWT_SIGNING_KEY_PATH unset — /.well-known/jwks returns 503; JWT issuance disabled")
	}

	// Real readiness check (replaces the P0-1 stub): /readyz now reflects
	// actual DB reachability via Ping.
	r := app.NewRouter(cfg, app.Deps{
		ReadyChecker:     pool,
		Users:            userSvc,
		AuthIdentities:   userSvc,
		EmploymentReader: userSvc,
		PermissionReader: userSvc,
		Signer:           signer,
		TenantResolver:   tenant.NewResolver(pool.Gorm),
		TenantAdmin:      tenant.NewAdmin(pool.Gorm),
		TenantConfig:     tenantconfig.New(pool.Gorm),
	})

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

// isTruthy returns true for common affirmative env values ("1", "true", "yes"
// case-insensitive). Empty / unknown values fall back to false so the default
// remains "do not auto-migrate in prod".
func isTruthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes":
		return true
	}
	return false
}
