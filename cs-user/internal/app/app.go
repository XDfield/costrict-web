// Package app wires cs-user's HTTP routes into a gin router.
//
// Extracted from cmd/api/main.go so HTTP handler behaviour can be tested
// without starting a real TCP listener (httptest.NewRecorder + NewRouter).
package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/handlers"
	"github.com/costrict/costrict-web/cs-user/internal/middleware"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/costrict/costrict-web/cs-user/internal/tenantconfig"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// ReadyChecker returns nil when cs-user is ready to serve traffic (DB up,
// migrations applied, etc). Phase 1 P0-2 wires a real Postgres ping here.
type ReadyChecker interface {
	Ready() error
}

// Deps bundles the optional services NewRouter can wire. Each field is
// optional; a nil field yields a stub handler that returns HTTP 503, so the
// swagger spec stays consistent across deployments while letting unit tests
// construct a minimal router without spinning up real services.
type Deps struct {
	ReadyChecker   ReadyChecker
	Users          handlers.UserService
	AuthIdentities handlers.AuthIdentityService
	// EmploymentReader is the read-side subset the A7 reissue-token flow
	// needs. Typically the same *user.Service as Users; declared separately
	// to keep the auth handler's dependency surface explicit + minimal.
	EmploymentReader handlers.EmploymentReader
	// PermissionReader is the Phase C1 read-side subset the reissue-token
	// flow uses to populate platform_admin / tenant_roles JWT claims. When
	// nil, the issued token omits the permission claims (灰度 rollout).
	PermissionReader handlers.PermissionReader
	// Signer is the JWT signing primitive (Phase A3). Optional — when nil,
	// /.well-known/jwks returns 503 and no path issues tokens. A7 (OAuth
	// callback takeover) will require it.
	Signer *auth.Signer
	// TenantResolver is the §5 three-layer resolver (Phase B3b). When nil,
	// ResolveTenant middleware is not mounted and handlers see no tenant in
	// the request context (Phase A still works in implicit-default mode).
	// Also drives the /api/internal/tenants/resolve-by-email RPC endpoint
	// (B3b.2b-step2) — when nil, that endpoint returns 503.
	TenantResolver *tenant.Resolver
	// TenantAdmin is the write/lifecycle surface for the tenants table
	// (Phase C2). Drives /api/internal/platform/tenants* (7 endpoints:
	// list / get / create / patch / suspend / restore / delete). When nil,
	// those endpoints return 503 so the swagger spec stays consistent.
	TenantAdmin *tenant.Admin
	// TenantConfig is the per-tenant YAML config CRUD surface (Phase C3.2).
	// Drives /api/internal/tenant/config (GET + PUT). When nil, those
	// endpoints return 503 so the swagger spec stays consistent.
	TenantConfig *tenantconfig.Service
	// AuditLog is the Phase C4.1 best-effort writer. When nil, the
	// platform-tenant / tenant-config / provider-mapping handlers skip the
	// post-success audit-log write (recordAudit is nil-safe). Tests that
	// need to assert on audit rows inject a real *auditlog.Service bound to
	// the same sqlite/gorm DB; production wires one bound to the Postgres
	// pool.
	AuditLog *auditlog.Service
}

// NewRouter builds the gin engine with all cs-user routes.
//
// Routes:
//   - GET /healthz  — liveness (always 200 once process is up)
//   - GET /readyz   — readiness (delegates to deps.ReadyChecker; 503 on err)
//   - /api/internal/* — gated by X-Internal-Token (ADR D8)
//   - GET /swagger/*any — Swagger UI (serves generated spec)
func NewRouter(cfg *config.Config, deps Deps) *gin.Engine {
	if cfg == nil {
		panic("app.NewRouter: nil config")
	}
	if deps.ReadyChecker == nil {
		deps.ReadyChecker = stubReady{}
	}

	r := gin.New()
	r.Use(gin.Recovery())

	// Tenant resolver runs before any route group so handlers can pull the
	// resolved tenant via middleware.TenantFromGin (B3b.1). When no resolver
	// is wired, the middleware is a no-op and Phase A behavior is unchanged
	// (implicit default tenant).
	if deps.TenantResolver != nil {
		r.Use(middleware.ResolveTenant(deps.TenantResolver, cfg.Tenant.ApexDomains))
	}

	r.GET("/healthz", healthz)
	r.GET("/readyz", readyz(deps.ReadyChecker))

	// JWKS endpoint — public per RFC 7517. Mounted at the root (not under
	// /api/internal) so the well-known path matches the OIDC convention
	// relying parties already expect. cs-user consumer:
	// server/internal/middleware/jwks.go.
	jwksAPI := handlers.JWKSAPI{Signer: deps.Signer}
	r.GET("/.well-known/jwks", jwksAPI.GetJWKS)

	// Internal endpoints — consumed by costrict-web only (shared-secret gated).
	internal := r.Group("/api/internal", middleware.RequireInternalToken(cfg.Internal.Token))
	internal.GET("/ping", PingHandler)
	registerUserRoutes(internal, deps)
	registerAuthIdentityRoutes(internal, deps)
	registerAuthRoutes(internal, cfg, deps)
	registerTenantRoutes(internal, deps)
	registerPlatformTenantRoutes(internal, deps)
	registerTenantConfigRoutes(internal, deps)
	registerTenantProviderMappingRoutes(internal, deps)

	// Swagger UI. The spec is generated by `make swagger` (swag init) and
	// registered globally via the blank import of cs-user/docs in main.go.
	// Without that blank import the UI loads but shows an empty spec.
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	return r
}

// registerUserRoutes wires GET + POST /users/* endpoints. When deps.Users
// is nil (e.g. unit tests that only exercise health/ping), routes resolve
// to a 503 stub so the path always exists in the swagger spec.
//
// Phase 1: read endpoints (GET).
// Phase 2: write endpoints (POST/DELETE) — see handlers/users.go.
func registerUserRoutes(rg *gin.RouterGroup, deps Deps) {
	usersAPI := handlers.UsersAPI{Svc: deps.Users}
	if deps.Users == nil {
		usersAPI.Svc = unavailableUserService{}
	}

	users := rg.Group("/users")
	users.GET("/:subject_id", usersAPI.GetUser)
	users.POST("/by-ids", usersAPI.GetUsersByIDs)
	users.GET("/search", usersAPI.SearchUsers)

	// Phase 2 write endpoints.
	users.POST("/get-or-create", usersAPI.GetOrCreate)
	users.POST("/transfer-identity", usersAPI.TransferIdentity)
	// These two share the :subject_id path param with GetUser; gin's path
	// tree accepts distinct method+suffix combinations without conflict.
	users.POST("/:subject_id/bind-identity", usersAPI.BindIdentity)
	users.DELETE("/:subject_id/identities/:provider", usersAPI.UnbindIdentity)

	// Phase E3a.1: Gitea binding status (read-only). Returns 404 when the
	// user has no binding (provisioning not yet run). The route lives under
	// /users/:subject_id/ to mirror the auth-identities pattern.
	users.GET("/:subject_id/gitea-binding", usersAPI.GetGiteaBinding)

	// Phase A4b: enterprise-mapping refresh hook fired by the server's OAuth
	// callback after GetOrCreateUser. Lives outside the :subject_id path
	// subtree so it doesn't collide with the routes above.
	users.POST("/apply-enterprise-mapping", usersAPI.ApplyEnterpriseMapping)
}

// registerAuthIdentityRoutes wires GET /users/:subject_id/auth-identities.
func registerAuthIdentityRoutes(rg *gin.RouterGroup, deps Deps) {
	api := handlers.AuthIdentitiesAPI{Svc: deps.AuthIdentities}
	if deps.AuthIdentities == nil {
		api.Svc = unavailableAuthIdentityService{}
	}

	// Register on the same /users group so the path is /users/:subject_id/auth-identities.
	// gin doesn't allow redeclaring the group name, so mount via inline group.
	rg.GET("/users/:subject_id/auth-identities", api.ListIdentities)
}

// registerAuthRoutes wires POST /users/reissue-token. Mounted inside the
// /users subtree to match the other users-group endpoints but lives on the
// AuthAPI handler because it spans user-data + signer orchestration (Phase A7).
// When deps.EmploymentReader is nil (unit tests), an unavailableAuthReader
// stub returns 503 so the path exists in the swagger spec without requiring
// a real service.
func registerAuthRoutes(rg *gin.RouterGroup, cfg *config.Config, deps Deps) {
	reader := deps.EmploymentReader
	if reader == nil {
		reader = unavailableAuthReader{}
	}
	authAPI := handlers.AuthAPI{
		Svc:         reader,
		Signer:      deps.Signer,
		JWT:         cfg.JWT,
		Permissions: deps.PermissionReader,
	}
	rg.POST("/users/reissue-token", authAPI.ReissueToken)
}

// registerTenantRoutes wires POST /tenants/resolve-by-email (Phase B3b.2b-step2).
// When deps.TenantResolver is nil (unit tests), an unavailableTenantResolver
// stub returns 503 so the path always exists in the swagger spec.
func registerTenantRoutes(rg *gin.RouterGroup, deps Deps) {
	resolver := handlers.TenantResolverService(deps.TenantResolver)
	if deps.TenantResolver == nil {
		resolver = unavailableTenantResolver{}
	}
	tenantsAPI := handlers.TenantsAPI{Resolver: resolver}
	rg.POST("/tenants/resolve-by-email", tenantsAPI.ResolveByEmail)
}

// registerPlatformTenantRoutes wires the 7 Phase C2 platform-admin tenant
// CRUD endpoints (list / get / create / patch / suspend / restore / delete).
// When deps.TenantAdmin is nil (unit tests), an
// unavailablePlatformTenantService stub returns 503 so the paths always exist
// in the swagger spec while refusing traffic.
func registerPlatformTenantRoutes(rg *gin.RouterGroup, deps Deps) {
	svc := handlers.PlatformTenantService(deps.TenantAdmin)
	if deps.TenantAdmin == nil {
		svc = unavailablePlatformTenantService{}
	}
	api := handlers.PlatformTenantsAPI{Svc: svc, Audit: deps.AuditLog}
	g := rg.Group("/platform/tenants")
	g.GET("", api.ListTenants)
	g.POST("", api.CreateTenant)
	g.GET("/:id", api.GetTenant)
	g.PATCH("/:id", api.UpdateTenant)
	g.POST("/:id/suspend", api.SuspendTenant)
	g.POST("/:id/restore", api.RestoreTenant)
	g.POST("/:id/delete", api.DeleteTenant)
}

// healthz godoc
//
//	@Summary		Liveness probe
//	@Description	Always returns 200 once the process is up. Unauthenticated — safe for K8s livenessProbe.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	object{status=string}
//	@Router			/healthz [get]
func healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz godoc
//
//	@Summary		Readiness probe
//	@Description	Returns 200 when the readiness checker (Postgres ping in P0-2) passes; 503 otherwise. Unauthenticated — safe for K8s readinessProbe. The handler is built by a factory bound to the supplied checker; swag uses the @Router path to register this endpoint.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	object{status=string}
//	@Failure		503	{object}	object{status=string,error=string}
//	@Router			/readyz [get]
func readyz(check ReadyChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := check.Ready(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not-ready", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// PingHandler godoc
//
//	@Summary		Internal handshake
//	@Description	Returns pong. Used by costrict-web to verify the X-Internal-Token handshake at startup. Requires the shared secret.
//	@Tags			internal
//	@Produce		json
//	@Security		InternalToken
//	@Success		200	{object}	object{pong=boolean}
//	@Failure		401	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/api/internal/ping [get]
func PingHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"pong": true})
}

// stubReady is the default when no DB checker is wired. It returns nil
// (HTTP layer is ready). Phase 1 P0-2 replaces it with a real Postgres ping.
type stubReady struct{}

func (stubReady) Ready() error { return nil }

// unavailableUserService is the fallback when Deps.Users is nil — keeps the
// swagger spec stable across configurations by making the route exist but
// refuse traffic.
type unavailableUserService struct{}

func (unavailableUserService) GetUserByID(_ context.Context, _ string) (*models.User, error) {
	return nil, errServiceUnavailable
}
func (unavailableUserService) GetUsersByIDs(_ context.Context, _ []string) (map[string]*models.User, error) {
	return nil, errServiceUnavailable
}
func (unavailableUserService) SearchUsers(_ context.Context, _ string, _ int) ([]*models.User, error) {
	return nil, errServiceUnavailable
}
func (unavailableUserService) GetOrCreateUser(_ context.Context, _ *models.JWTClaims) (*models.User, error) {
	return nil, errServiceUnavailable
}
func (unavailableUserService) BindIdentityToUser(_ context.Context, _ string, _ *models.JWTClaims, _ ...models.BindIdentityOptions) error {
	return errServiceUnavailable
}
func (unavailableUserService) TransferIdentityToUser(_ context.Context, _, _, _ string) error {
	return errServiceUnavailable
}
func (unavailableUserService) UnbindIdentityByProvider(_ context.Context, _, _ string) error {
	return errServiceUnavailable
}
func (unavailableUserService) ApplyEnterpriseMapping(_ context.Context, _ user.EmploymentMappingParams) error {
	return errServiceUnavailable
}
func (unavailableUserService) GetGiteaBinding(_ context.Context, _ string) (*models.UserGiteaBinding, error) {
	return nil, errServiceUnavailable
}

type unavailableAuthIdentityService struct{}

func (unavailableAuthIdentityService) ListIdentities(_ context.Context, _ string) ([]*models.UserAuthIdentity, error) {
	return nil, errServiceUnavailable
}

// unavailableAuthReader is the fallback when Deps.EmploymentReader is nil —
// keeps the reissue-token route resolvable (so swagger spec stays stable)
// while refusing traffic with 503.
type unavailableAuthReader struct{}

func (unavailableAuthReader) GetEmploymentIdentity(_ context.Context, _ string) (*models.EmploymentIdentity, error) {
	return nil, errServiceUnavailable
}

// unavailableTenantResolver is the fallback when Deps.TenantResolver is nil —
// keeps /tenants/resolve-by-email resolvable for swagger while refusing
// traffic with 503 (production wires a real *tenant.Resolver via main.go).
type unavailableTenantResolver struct{}

func (unavailableTenantResolver) ResolveByEmail(_ context.Context, _ string) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}

func (unavailableTenantResolver) ListByEmailDomain(_ context.Context, _ string) ([]*models.Tenant, error) {
	return nil, errServiceUnavailable
}

// registerTenantConfigRoutes wires GET + PUT /tenant/config (Phase C3.2).
// When deps.TenantConfig is nil (unit tests), an
// unavailableTenantConfigService stub returns 503 so the paths always exist
// in the swagger spec while refusing traffic.
func registerTenantConfigRoutes(rg *gin.RouterGroup, deps Deps) {
	svc := handlers.TenantConfigService(deps.TenantConfig)
	if deps.TenantConfig == nil {
		svc = unavailableTenantConfigService{}
	}
	api := handlers.TenantConfigAPI{Svc: svc, Audit: deps.AuditLog}
	g := rg.Group("/tenant/config")
	g.GET("", api.GetTenantConfig)
	g.PUT("", api.UpdateTenantConfig)
}

// unavailableTenantConfigService is the fallback when Deps.TenantConfig is
// nil — keeps /tenant/config resolvable for swagger while refusing traffic
// with 503 (production wires a real *tenantconfig.Service via main.go).
type unavailableTenantConfigService struct{}

func (unavailableTenantConfigService) Get(_ context.Context, _ string) (*models.TenantConfig, error) {
	return nil, errServiceUnavailable
}
func (unavailableTenantConfigService) Update(_ context.Context, _ tenantconfig.UpdateParams) (*models.TenantConfig, error) {
	return nil, errServiceUnavailable
}

// registerTenantProviderMappingRoutes wires GET + PUT /tenant/provider-mapping
// (Phase C3.3). Shares the same *tenantconfig.Service as the raw-blob route;
// declared separately because the typed surface is a distinct handler /
// interface. When deps.TenantConfig is nil, an
// unavailableTenantProviderMappingService stub returns 503 so the paths
// always exist in the swagger spec while refusing traffic.
func registerTenantProviderMappingRoutes(rg *gin.RouterGroup, deps Deps) {
	var svc handlers.TenantProviderMappingService
	if deps.TenantConfig == nil {
		svc = unavailableTenantProviderMappingService{}
	} else {
		svc = deps.TenantConfig
	}
	api := handlers.TenantProviderMappingAPI{Svc: svc, Audit: deps.AuditLog}
	g := rg.Group("/tenant/provider-mapping")
	g.GET("", api.GetProviderMapping)
	g.PUT("", api.UpdateProviderMapping)
}

// unavailableTenantProviderMappingService is the typed-edit fallback. Pairs
// with unavailableTenantConfigService — both 503 when *tenantconfig.Service
// is unset.
type unavailableTenantProviderMappingService struct{}

func (unavailableTenantProviderMappingService) GetProviderMapping(_ context.Context, _ string) (*tenantconfig.ProviderMapping, error) {
	return nil, errServiceUnavailable
}
func (unavailableTenantProviderMappingService) UpdateProviderMapping(_ context.Context, _ tenantconfig.UpdateProviderMappingParams) (*tenantconfig.ProviderMapping, error) {
	return nil, errServiceUnavailable
}

// unavailablePlatformTenantService is the fallback when Deps.TenantAdmin is
// nil — keeps the 7 platform-tenant routes resolvable for swagger while
// refusing traffic with 503 (production wires a real *tenant.Admin via
// main.go).
type unavailablePlatformTenantService struct{}

func (unavailablePlatformTenantService) CreateTenant(_ context.Context, _ tenant.CreateParams) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}
func (unavailablePlatformTenantService) ListTenants(_ context.Context, _ tenant.ListParams) (*tenant.ListResult, error) {
	return nil, errServiceUnavailable
}
func (unavailablePlatformTenantService) GetTenant(_ context.Context, _ string) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}
func (unavailablePlatformTenantService) UpdateTenant(_ context.Context, _ string, _ tenant.UpdateParams) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}
func (unavailablePlatformTenantService) SuspendTenant(_ context.Context, _ string) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}
func (unavailablePlatformTenantService) RestoreTenant(_ context.Context, _ string) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}
func (unavailablePlatformTenantService) RequestDeletion(_ context.Context, _ string) (*models.Tenant, error) {
	return nil, errServiceUnavailable
}

var errServiceUnavailable = errors.New("user service not configured")
