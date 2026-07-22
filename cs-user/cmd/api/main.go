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
	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/giteasync"
	"github.com/costrict/costrict-web/cs-user/internal/gitserver"
	"github.com/costrict/costrict-web/cs-user/internal/idp"
	"github.com/costrict/costrict-web/cs-user/internal/logger"
	"github.com/costrict/costrict-web/cs-user/internal/migration"
	"github.com/costrict/costrict-web/cs-user/internal/storage"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func main() {
	// Dev convenience: load .env from CWD if present. In production the
	// container runtime injects env vars directly and .env is absent —
	// godotenv.Load returns nil for missing files and is a safe no-op.
	// Existing process env wins (Load does not override set vars).
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("load .env: %v (continuing with process env)", err)
	}

	logger.Init(logger.Config{FilePrefix: "cs-user", Console: true})
	defer logger.Sync()

	cfg, err := config.Load()
	if err != nil {
		logger.L().Fatal("load config", zap.Error(err))
	}

	// Open the independent Postgres pool (ADR D1). main owns the lifecycle;
	// shutdown closes it after the HTTP server stops accepting traffic.
	pool, err := storage.Open(cfg)
	if err != nil {
		logger.L().Fatal("open postgres", zap.Error(err))
	}
	defer func() {
		if cerr := pool.Close(); cerr != nil {
			logger.L().Error("close postgres pool", zap.Error(cerr))
		}
	}()
	logger.L().Info("postgres pool opened",
		zap.String("host", cfg.Postgres.Host),
		zap.String("db", cfg.Postgres.Database))

	// Construct the user Service bound to the pool. P0-3 wires only the
	// read methods; write methods (bind/unbind/transfer) land in Phase A
	// once JWT-claims plumbing is available.
	userSvc := user.NewService(pool.Gorm)

	// Phase E3a.1: wire Gitea auto-provisioning. Per Phase E3b.1.1, the
	// service resolves the tenant's git_server endpoint on each call via
	// the per-tenant DBResolver (replacing the broken global env-var
	// singleton that all tenants were funnelled through). The template
	// row must already exist (bootstrap ran above) before we construct
	// the service.
	auditSvc := auditlog.NewService(pool.Gorm, nil)
	if cfg.Gitea.Enabled() {
		giteaResolver := gitserver.NewDBResolver(pool.Gorm)
		giteaSvc := giteasync.NewService(pool.Gorm, giteaResolver, auditSvc, nil)
		userSvc.SetGiteaSync(giteaSvc)
		logger.L().Info("Gitea auto-provisioning enabled (per-tenant resolver)")
	} else {
		logger.L().Warn("Gitea auto-provisioning disabled — CS_USER_GITEA_BASE_URL / CS_USER_GITEA_ADMIN_TOKEN unset")
	}

	// Dev-mode auto-migrate: when CS_USER_AUTO_MIGRATE is truthy ("1"/"true"),
	// apply pending migrations inline at boot so local dev doesn't need a
	// separate migrate invocation. Prod wiring (Helm pre-deploy hook calling
	// the migrate binary) is added in P0-5; this guard keeps prod explicit.
	if isTruthy(os.Getenv("CS_USER_AUTO_MIGRATE")) {
		sqlDB, err := pool.SQLDB()
		if err != nil {
			logger.L().Fatal("acquire sql.DB for migration", zap.Error(err))
		}
		// gorm postgres driver maps to the "postgres" goose dialect.
		runner, err := migration.NewRunner(sqlDB, "postgres")
		if err != nil {
			logger.L().Fatal("init migration runner", zap.Error(err))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := runner.Up(ctx); err != nil {
			cancel()
			logger.L().Fatal("auto-migrate", zap.Error(err))
		}
		cancel()
		logger.L().Info("auto-migrate applied")
	}

	// Phase E3b.1.1: bootstrap the git_servers template row from env vars.
	// When CS_USER_GITEA_BASE_URL + CS_USER_GITEA_ADMIN_TOKEN are set and no
	// template row exists yet, materialize one + bind any unbound tenants.
	// Idempotent — safe on every boot. ErrNoTemplateInput means the operator
	// hasn't configured Gitea yet; that's a soft skip (feature disabled),
	// not a fatal condition.
	if cfg.Gitea.Enabled() {
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
		templateID, err := gitserver.BootstrapTemplate(bootCtx, pool.Gorm, gitserver.TemplateInput{
			Endpoint:      cfg.Gitea.BaseURL,
			AdminToken:    cfg.Gitea.AdminToken,
			DisplayName:   "Gitea (env bootstrap)",
			AdminUser:     cfg.Gitea.AdminUser,
			AdminPassword: cfg.Gitea.AdminPassword,
		})
		bootCancel()
		if err != nil {
			logger.L().Fatal("bootstrap git_servers template", zap.Error(err))
		}
		logger.L().Info("git_servers template ready", zap.String("server_id", templateID))
	} else {
		logger.L().Warn("git_servers template bootstrap skipped — CS_USER_GITEA_BASE_URL / CS_USER_GITEA_ADMIN_TOKEN unset")
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
			logger.L().Fatal("load JWT signing key", zap.String("path", cfg.JWT.SigningKeyPath), zap.Error(err))
		}
		logger.L().Info("JWT signer loaded", zap.String("kid", signer.KID()))
	} else {
		logger.L().Warn("CS_USER_JWT_SIGNING_KEY_PATH unset — /.well-known/jwks returns 503; JWT issuance disabled")
	}

	// Real readiness check (replaces the P0-1 stub): /readyz now reflects
	// actual DB reachability via Ping.
	r := app.NewRouter(cfg, app.Deps{
		ReadyChecker:      pool,
		Users:             userSvc,
		AuthIdentities:    userSvc,
		EmploymentReader:  userSvc,
		PermissionReader:  userSvc,
		Signer:            signer,
		TenantResolver:    tenant.NewResolver(pool.Gorm),
		TenantAdmin:       tenant.NewAdmin(pool.Gorm),
		TenantConfig:      tenantconfig.New(pool.Gorm),
		AuditLog:          auditSvc,
		GitServerResolver: gitserver.NewDBResolver(pool.Gorm),
		IdPSources:        newIdPService(pool.Gorm, cfg.IDP.AllowInsecure),
	})

	srv := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.L().Info("cs-user listening", zap.String("port", cfg.HTTP.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.L().Fatal("listen", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.L().Info("shutdown signal received")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.L().Error("forced shutdown", zap.Error(err))
	}
	logger.L().Info("bye")
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

// newIdPService constructs the IdP service with the validator configured
// per env. When CS_USER_IDP_ALLOW_INSECURE=true, the validator permits
// http:// IdP endpoint URLs (required for local dev where Casdoor / mock
// IdPs run on plain HTTP). Production must leave this false.
func newIdPService(db *gorm.DB, allowInsecure bool) *idp.Service {
	svc := idp.New(db)
	if allowInsecure {
		v := idp.NewValidator()
		v.AllowInsecure = true
		svc.SetValidator(v)
	}
	return svc
}

