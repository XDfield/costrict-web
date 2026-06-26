// @title           Costrict Web API
// @version         1.0
// @description     AI Agent Platform API - Skill marketplace, repository and registry management.
// @termsOfService  http://swagger.io/terms/

// @contact.name   CoStrict Team
// @contact.url    https://costrict.ai

// @license.name  Apache 2.0
// @license.url   http://www.apache.org/licenses/LICENSE-2.0.html

// @host      localhost:8080
// @BasePath  /api

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Type "Bearer" followed by a space and JWT token.

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/costrict/costrict-web/server/docs"
	"github.com/costrict/costrict-web/server/internal/adminitem"
	"github.com/costrict/costrict-web/server/internal/adminuser"
	"github.com/costrict/costrict-web/server/internal/audit"
	"github.com/costrict/costrict-web/server/internal/authz"
	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wechat"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/cloud"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/deptsync"
	"github.com/costrict/costrict-web/server/internal/dispatcher"
	"github.com/costrict/costrict-web/server/internal/enterprise"
	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/handlers"
	"github.com/costrict/costrict-web/server/internal/kanban"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/memory"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/project"
	"github.com/costrict/costrict-web/server/internal/scheduler"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/settings"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	teampkg "github.com/costrict/costrict-web/server/internal/team"
	usagepkg "github.com/costrict/costrict-web/server/internal/usage"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

func main() {
	// Initialise structured logging with daily rotation and 7-day retention.
	// After this call every log.Printf in the codebase writes to logs/app.log,
	// and logger.Error() additionally writes to logs/error.log with a stack trace.
	logger.Init(logger.Config{
		Dir:        "./logs",
		MaxAgeDays: 7,
		Console:    true,
	})

	// Redirect Gin's built-in output to our log files.
	gin.DefaultWriter = logger.GinWriter()
	gin.DefaultErrorWriter = logger.GinErrorWriter()

	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	db.Model(&models.CapabilityRegistry{}).
		Where("sync_status = ?", "syncing").
		Update("sync_status", "error")

	// Install the process-wide admin audit logger (fire-and-forget writes from
	// management handlers). Must run before any handler can call audit.Record.
	audit.Init(db)

	handlers.EnsurePublicRegistry()
	handlers.InitCasdoor(&cfg.Casdoor)
	handlers.InitCookieConfig(cfg)
	userModule := userpkg.NewWithConfig(db, cfg.UserSyncIntervalMinutes)
	handlers.InitUserModule(userModule)

	var usageProvider services.UsageProvider
	switch strings.ToLower(strings.TrimSpace(cfg.UsageProvider)) {
	case "", "sqlite":
		usageDB, err := usagepkg.OpenSQLite(cfg.UsageSQLitePath)
		if err != nil {
			log.Fatalf("Failed to initialize usage sqlite: %v", err)
		}
		usageProvider = services.NewSQLiteUsageProvider(usageDB)
	case "es":
		if strings.TrimSpace(cfg.UsageESReportBaseURL) == "" {
			log.Fatalf("Failed to initialize usage provider: USAGE_ES_REPORT_BASE_URL is required when USAGE_PROVIDER=es")
		}
		if strings.TrimSpace(cfg.UsageESQueryBaseURL) == "" {
			log.Fatalf("Failed to initialize usage provider: USAGE_ES_QUERY_BASE_URL is required when USAGE_PROVIDER=es")
		}
		usageProvider = services.NewESUsageProvider(services.ESUsageProviderConfig{
			ReportBaseURL:      cfg.UsageESReportBaseURL,
			QueryBaseURL:       cfg.UsageESQueryBaseURL,
			ReportPath:         cfg.UsageESReportPath,
			QueryPath:          cfg.UsageESQueryPath,
			Timeout:            time.Duration(cfg.UsageESTimeoutSec) * time.Second,
			BasicUser:          cfg.UsageESBasicUser,
			BasicPass:          cfg.UsageESBasicPass,
			InsecureSkipVerify: cfg.UsageESInsecureSkipVerify,
		})
	default:
		log.Fatalf("Failed to initialize usage provider: unsupported USAGE_PROVIDER=%q", cfg.UsageProvider)
	}

	storagePath := os.Getenv("ARTIFACT_STORAGE_PATH")
	if storagePath == "" {
		storagePath = "./data/artifacts"
	}
	storageBackend, err := storage.NewLocalBackend(storagePath)
	if err != nil {
		log.Fatalf("Failed to initialize storage backend: %v", err)
	}
	handlers.StorageBackend = storageBackend

	jobSvc := &services.JobService{DB: db}
	handlers.JobService = jobSvc

	// DB+HTTP plugin bundle distribution channel. In the API process the pack
	// service serves only the synchronous upload-plugin path (asset reconstruction,
	// no git); catalog plugins are packed by the worker process. BundleJobService
	// enqueues lazy clone-and-pack jobs for catalog plugins on a bundle cache miss.
	bundleTmpDir := os.Getenv("SYNC_TMP_DIR")
	if bundleTmpDir == "" {
		bundleTmpDir = os.TempDir() + "/costrict-sync"
	}
	bundlePackSvc := services.NewBundlePackService(
		db,
		&services.GitService{TempBaseDir: bundleTmpDir},
		storageBackend,
		cfg.GitMirrorBase,
	)
	// OOM guard for the synchronous upload-pack path (same cap the worker applies to
	// clone-pack). BUNDLE_MAX_BYTES (default 100MB); non-positive disables the cap.
	bundlePackSvc.MaxBundleBytes = bundleMaxBytes()
	handlers.BundlePackSvc = bundlePackSvc
	handlers.BundleJobSvc = &services.BundleJobService{DB: db}

	// Search Service
	searchSvc := services.NewSearchService(db, &cfg.Search)

	// Behavior Service
	behaviorSvc := services.NewBehaviorService(db)

	// Recommend Service
	recommendSvc := services.NewRecommendService(db, behaviorSvc, searchSvc)
	scanJobSvc := &services.ScanJobService{DB: db}
	handlers.ScanJobService = scanJobSvc

	sched := &scheduler.Scheduler{
		JobService: jobSvc,
		DB:         db,
	}
	if err := sched.Start(); err != nil {
		log.Fatalf("Failed to start scheduler: %v", err)
	}
	defer sched.Stop()
	handlers.SyncScheduler = sched

	// Initialize ItemHandler with parser
	parserSvc := &services.ParserService{}
	categorySvc := &services.CategoryService{DB: db}
	tagSvc := &services.TagService{DB: db}
	handlers.CategorySvc = categorySvc
	handlers.TagSvc = tagSvc
	itemHandler := handlers.NewItemHandler(db, parserSvc, categorySvc, tagSvc)

	// Initialize search and recommend handlers
	searchHandler := handlers.NewSearchHandler(searchSvc)
	recommendHandler := handlers.NewRecommendHandler(recommendSvc, behaviorSvc)
	usageSvc := services.NewUsageService(usageProvider, userModule.Service)
	usageHandler := handlers.NewUsageHandler(usageSvc)

	// Initialize Distribution handler
	distSvc := services.NewDistributionService(db, behaviorSvc)
	distHandler := handlers.NewDistributionHandler(db, distSvc)

	r := gin.New()

	casdoorEndpoint := cfg.Casdoor.InternalEndpoint
	if casdoorEndpoint == "" {
		casdoorEndpoint = cfg.Casdoor.Endpoint
	}

	// Initialize JWKS provider for JWT signature verification
	jwksProvider := middleware.NewJWKSProvider(casdoorEndpoint)
	jwksProvider.Preload()
	middleware.SetSubjectResolver(func(claims middleware.AuthClaims) (string, string, error) {
		return userModule.Service.ResolveSubjectID(&userpkg.JWTClaims{
			ID:                claims.ID,
			Sub:               claims.Sub,
			UniversalID:       claims.UniversalID,
			Name:              claims.Name,
			PreferredUsername: claims.PreferredUsername,
			Email:             claims.Email,
			Provider:          claims.Provider,
			ProviderUserID:    claims.ProviderUserID,
			Phone:             claims.Phone,
		})
	})

	// Account-status gate (M1 · 成员管理): RequireAuth consults this hook for the
	// resolved subject and rejects banned/disabled members. Conservative by
	// design — the checker errors fail open (handled in middleware), and the gate
	// is entirely inert until installed here. A lightweight single-column lookup
	// per authenticated request; can be cached later if the hot path needs it.
	middleware.SetStatusChecker(func(subjectID string) (string, error) {
		return userModule.Service.GetUserStatus(subjectID)
	})

	// Bootstrap platform-admin granting (initial admin without manual SQL):
	// installed as a post-login hook on GetOrCreateUser, which fires only on a
	// genuine login by the user themselves (the OAuth callback and the JWKS
	// auth-middleware path). Read-only background sync (user-search backfill) goes
	// through SyncUser and does NOT trigger this hook, so a user is never granted
	// platform_admin merely because someone else searched for them.
	// Users whose Casdoor universal_id is in BOOTSTRAP_PLATFORM_ADMIN_UNIVERSAL_IDS are granted platform_admin
	// on login (idempotent, best-effort, granted_by='bootstrap'). The user package
	// stays free of a systemrole import (cycle avoidance) via this injected hook.
	// Empty allowlist = complete no-op.
	bootstrapGranter := userpkg.NewBootstrapAdminGranter(
		systemrole.NewSystemRoleService(db),
		cfg.BootstrapPlatformAdmins,
	)
	userModule.Service.SetPostLoginHook(bootstrapGranter.ApplyOnLogin)

	r.Use(middleware.CORS(middleware.CORSConfig{AllowedOrigins: cfg.CORSAllowedOrigins}))
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.ErrorLogger())

	// Device heartbeat uses its own device token authentication.
	// Register before OptionalAuth to avoid unnecessary Casdoor validation
	// attempts (device tokens are not Casdoor tokens).
	deviceSvc := &services.DeviceService{DB: db}
	r.POST("/api/devices/:deviceID/heartbeat", handlers.DeviceHeartbeatHandler(deviceSvc))

	r.Use(middleware.OptionalAuth(casdoorEndpoint, jwksProvider))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	updateSvc := &services.UpdateService{DB: db, ReleaseDownloadURL: cfg.ReleaseDownloadBaseURL}
	workspaceSvc := &services.WorkspaceService{DB: db, DeviceService: deviceSvc}

	authzModule, err := authz.New(db, systemrole.NewSystemRoleService(db), systemrole.CapabilityProvider{}, casdoorEndpoint, jwksProvider)
	if err != nil {
		log.Fatalf("failed to initialize authz module: %v", err)
	}

	// Shared dept-sync client: backs both the admin department-tree proxy (M1 org
	// view) and the authz fine-grained grant engine (mentor RBAC department
	// inheritance). Optional dependency — when unconfigured, department grants
	// simply never match (CheckGrant fails closed) while user grants and the role
	// path keep working.
	deptSyncClient := deptsync.New(cfg.DeptSync)
	authzModule.Service.SetDepartmentProvider(deptSyncDepartmentProvider{client: deptSyncClient})

	var channelModule *channel.Module
	var cloudModule *cloud.Module
	api := r.Group("/api")
	{
		auth := api.Group("/auth")
		{
			auth.GET("/callback", handlers.AuthCallback)
			auth.GET("/login", handlers.Login)
			auth.POST("/logout", handlers.Logout)
			auth.GET("/bind/callback", handlers.AuthCallback)
		}

		// === Public read-only endpoints (OptionalAuth, no login required) ===
		api.GET("/updates/check", handlers.UpdateCheckHandler(updateSvc))
		api.GET("/multica/updates/check", handlers.MulticaUpdateCheckHandler(&services.MulticaUpdateService{DB: db}))
		api.GET("/registries/public", handlers.GetPublicRegistry)
		api.GET("/registries/:id", handlers.GetRegistry)
		api.GET("/registries/:id/items", handlers.ListItems)
		api.GET("/registry/:repo/access", handlers.RegistryAccess)
		api.GET("/registry/:repo/index.json", handlers.RegistryIndex)
		api.GET("/registry/:repo/:itemType/:slug/*file", handlers.DownloadRegistryFile)
		api.GET("/plugins/:slug/download", handlers.DownloadPluginZip)
		api.GET("/plugins/:slug/bundle", handlers.DownloadPluginBundle)
		api.GET("/marketplace/:repo/marketplace.json", handlers.MarketplaceJSON)
		api.POST("/webhooks/github", handlers.HandleGitHubWebhook)

		api.POST("/releases", middleware.SystemTokenAuth(cfg.SystemToken), handlers.CreateReleaseHandler(updateSvc))

		// Items read (anonymous can preview public items)
		api.GET("/items", handlers.ListAllItems)
		api.GET("/items/:id", handlers.GetItem)
		api.GET("/items/:id/assets", handlers.ListItemAssets)
		api.GET("/items/:id/versions", handlers.ListItemVersions)
		api.GET("/items/:id/versions/:version", handlers.GetItemVersion)
		api.GET("/items/:id/artifacts", handlers.ListArtifacts)
		api.GET("/items/:id/download", handlers.DownloadItem)
		api.GET("/items/:id/scan-status", handlers.GetItemScanStatus)
		api.GET("/items/:id/scan-results", handlers.ListItemScanResults)
		api.GET("/scan-results/:id", handlers.GetScanResult)
		api.GET("/artifacts/:id/download", handlers.DownloadArtifact)

		// User name resolution (public, results cached in memory)
		api.GET("/users/names", handlers.GetUserNames)
		api.GET("/users/info", handlers.GetUserBasicInfo)

		// Category dictionary (public read)
		api.GET("/categories", handlers.ListCategoriesHandler(categorySvc))
		api.GET("/categories/:id", handlers.GetCategoryHandler(categorySvc))

		// Tag dictionary (public read)
		api.GET("/tags", handlers.ListTagsHandler(tagSvc))
		api.GET("/tags/:id", handlers.GetTagHandler(tagSvc))

		// Marketplace browse (public)
		marketplace := api.Group("/marketplace")
		{
			marketplace.POST("/items/search", searchHandler.SemanticSearch)
			marketplace.POST("/items/hybrid-search", searchHandler.HybridSearch)
			marketplace.POST("/items/recommend", recommendHandler.GetRecommendations)
			marketplace.GET("/items/trending", recommendHandler.GetTrending)
			marketplace.GET("/items/new", recommendHandler.GetNewAndNoteworthy)
		}
		api.GET("/items/:id/similar", searchHandler.FindSimilar)
		api.GET("/items/filter-options", handlers.ListItemFilterOptions)
		api.POST("/items/:id/behavior", recommendHandler.LogBehavior)
		api.GET("/items/:id/stats", recommendHandler.GetItemStats)

		// All routes below require authentication
		authed := api.Group("")
		authed.Use(middleware.RequireAuth(casdoorEndpoint, jwksProvider))
		{
			authed.GET("/auth/me", handlers.GetCurrentUser)
			authed.GET("/auth/identities", handlers.ListBoundIdentities)
			authed.POST("/auth/bind/start", handlers.StartBindAuth)
			authed.POST("/auth/identities/:provider/unbind", handlers.UnbindIdentity)

			usage := authed.Group("/usage")
			{
				usage.POST("/report", usageHandler.Report)
				usage.GET("/activity", usageHandler.Activity)
			}

			repos := authed.Group("/repositories")
			{
				repos.GET("", handlers.ListRepositories)
				repos.GET("/my", handlers.GetMyRepositories)
				repos.POST("", handlers.CreateRepository)
				repos.GET("/:id", handlers.GetRepository)
				repos.PUT("/:id", handlers.UpdateRepository)
				repos.DELETE("/:id", handlers.DeleteRepository)
				repos.GET("/:id/members", handlers.ListRepositoryMembers)
				repos.POST("/:id/members", handlers.AddRepositoryMember)
				repos.PUT("/:id/members/:userId", handlers.UpdateRepositoryMember)
				repos.DELETE("/:id/members/:userId", handlers.RemoveRepositoryMember)
				repos.GET("/:id/invitations", handlers.ListRepoInvitations)
				repos.POST("/:id/invitations", handlers.CreateRepoInvitation)
				repos.DELETE("/:id/invitations/:invId", handlers.CancelRepoInvitation)
				repos.GET("/:id/registry", handlers.GetRepositoryRegistry)
				repos.GET("/:id/registries", handlers.ListRepoRegistries)
				repos.POST("/:id/registries", handlers.AddRepoRegistry)
				repos.PUT("/:id/registries/:regId", handlers.UpdateRepoRegistry)
				repos.DELETE("/:id/registries/:regId", handlers.RemoveRepoRegistry)
				repos.POST("/:id/sync", handlers.TriggerRepoSync)
				repos.POST("/:id/sync/cancel", handlers.CancelRepoSync)
				repos.GET("/:id/sync-status", handlers.GetRepoSyncStatus)
				repos.GET("/:id/sync-logs", handlers.ListRepoSyncLogs)
				repos.GET("/:id/sync-jobs", handlers.ListRepoSyncJobs)
			}

			authed.GET("/registries", handlers.ListRegistries)
			authed.GET("/registries/my", handlers.ListMyRegistries)
			authed.POST("/registries", handlers.CreateRegistry)
			authed.PUT("/registries/:id", handlers.UpdateRegistry)
			authed.PUT("/registries/:id/transfer", handlers.TransferRegistry)
			authed.DELETE("/registries/:id", handlers.DeleteRegistry)
			authed.POST("/registries/:id/items", handlers.CreateItem)
			authed.POST("/registries/:id/sync", handlers.TriggerRegistrySync)
			authed.POST("/registries/:id/sync/cancel", handlers.CancelRegistrySync)
			authed.GET("/registries/:id/sync-status", handlers.GetRegistrySyncStatus)
			authed.GET("/registries/:id/sync-logs", handlers.ListRegistrySyncLogs)
			authed.GET("/registries/:id/sync-jobs", handlers.ListRegistrySyncJobs)

			authed.GET("/items/my", handlers.ListMyItems)
			authed.POST("/items", itemHandler.CreateItemDirect)
			authed.POST("/plugins/upload", itemHandler.UploadPlugin)
			api.GET("/plugins/builtin", handlers.ListBuiltinPlugins)
			authed.PUT("/items/:id", itemHandler.UpdateItem)
			authed.POST("/items/:id/check-consistency", itemHandler.CheckItemConsistency)
			authed.POST("/items/:id/fork", itemHandler.ForkItem)
			authed.DELETE("/items/:id", handlers.DeleteItem)
			authed.PUT("/items/:id/move", handlers.MoveItem)
			authed.PUT("/items/:id/transfer", handlers.TransferItemToRepo)
			authed.POST("/items/:id/distribute", distHandler.DistributeItem)
			authed.GET("/items/:id/distributions", distHandler.ListItemDistributions)
			authed.POST("/items/:id/scan", handlers.TriggerItemScan)
			authed.POST("/scan-jobs/:id/cancel", handlers.CancelScanJob)
			authed.POST("/items/:id/favorite", recommendHandler.FavoriteItem)
			authed.DELETE("/items/:id/favorite", recommendHandler.UnfavoriteItem)
			authed.PUT("/items/:id/mcp-config", handlers.UpsertMCPUserConfig)

			// Distribution routes
			authed.GET("/distributions/my/sent", distHandler.ListSentDistributions)
			authed.GET("/distributions/my/received", distHandler.ListReceivedDistributions)
			authed.PUT("/distributions/:id", distHandler.UpdateDistribution)
			authed.DELETE("/distributions/:id", distHandler.RevokeDistribution)
			authed.POST("/distributions/:id/dismiss", distHandler.DismissReceipt)
			authed.POST("/distributions/:id/read", distHandler.MarkReceiptRead)

			authed.POST("/artifacts/upload", handlers.UploadArtifact)
			authed.DELETE("/artifacts/:id", handlers.DeleteArtifact)

			authed.POST("/categories", handlers.CreateCategoryHandler(categorySvc))
			authed.PUT("/categories/:id", handlers.UpdateCategoryHandler(categorySvc))
			authed.DELETE("/categories/:id", handlers.DeleteCategoryHandler(categorySvc))
			authed.POST("/items/:id/tags", handlers.SetItemTagsHandler(tagSvc))

			platformAdmin := authed.Group("")
			platformAdmin.Use(systemrole.RequirePlatformAdmin(db))
			{
				platformAdmin.POST("/tags", handlers.CreateTagHandler(tagSvc))
				platformAdmin.PUT("/tags/:id", handlers.UpdateTagHandler(tagSvc))
				platformAdmin.DELETE("/tags/:id", handlers.DeleteTagHandler(tagSvc))
			}

			authed.GET("/users/search", handlers.SearchUsers)
			authed.GET("/users/me/behavior/summary", recommendHandler.GetUserSummary)

			authed.GET("/invitations/my", handlers.GetMyInvitations)
			authed.POST("/invitations/:id/accept", handlers.AcceptInvitation)
			authed.POST("/invitations/:id/decline", handlers.DeclineInvitation)

			authed.GET("/sync-logs/:id", handlers.GetSyncLogDetail)
			authed.GET("/sync-jobs/:id", handlers.GetSyncJobDetail)

			// Admin Management
			authed.Group("/admin")

			devices := authed.Group("/devices")
			{
				devices.POST("/register", handlers.RegisterDeviceHandler(deviceSvc))
				devices.GET("", handlers.ListDevicesHandler(deviceSvc, updateSvc))
				devices.GET("/:deviceID", handlers.GetDeviceHandler(deviceSvc))
				devices.PUT("/:deviceID", handlers.UpdateDeviceHandler(deviceSvc))
				devices.DELETE("/:deviceID", handlers.DeleteDeviceHandler(deviceSvc))
				devices.POST("/:deviceID/token/rotate", handlers.RotateDeviceTokenHandler(deviceSvc))
			}

			authed.GET("/workspaces/:workspaceID/devices", handlers.ListWorkspaceDevicesHandler(deviceSvc))

			// Workspace routes
			workspaces := authed.Group("/workspaces")
			{
				workspaces.POST("", handlers.CreateWorkspaceHandler(workspaceSvc))
				workspaces.GET("", handlers.ListWorkspacesHandler(workspaceSvc))
				workspaces.GET("/default", handlers.GetDefaultWorkspaceHandler(workspaceSvc))
				workspaces.GET("/:workspaceID", handlers.GetWorkspaceHandler(workspaceSvc))
				workspaces.PUT("/:workspaceID", handlers.UpdateWorkspaceHandler(workspaceSvc))
				workspaces.DELETE("/:workspaceID", handlers.DeleteWorkspaceHandler(workspaceSvc))
				workspaces.POST("/:workspaceID/set-default", handlers.SetDefaultWorkspaceHandler(workspaceSvc))
				workspaces.POST("/:workspaceID/directories", handlers.AddWorkspaceDirectoryHandler(workspaceSvc))
				workspaces.POST("/:workspaceID/directories/reorder", handlers.ReorderWorkspaceDirectoriesHandler(workspaceSvc))
				workspaces.PUT("/:workspaceID/directories/:directoryID", handlers.UpdateWorkspaceDirectoryHandler(workspaceSvc))
				workspaces.DELETE("/:workspaceID/directories/:directoryID", handlers.DeleteWorkspaceDirectoryHandler(workspaceSvc))
			}

			authzModule.RegisterAPIRoutes(authed)

			notificationModule := notification.New(db, cfg.AppURL)
			systemRoleModule := systemrole.New(db)
			systemRoleModule.RegisterRoutes(authed)
			notificationModule.RegisterRoutes(authed)

			enterprise.New(db).RegisterRoutes(authed)
			settings.New(db).RegisterRoutes(authed)

			admin := authed.Group("/admin")
			admin.Use(systemrole.RequirePlatformAdmin(db))
			authzModule.RegisterAdminRoutes(admin)

			// Admin distribution management (platform admin only)
			admin.GET("/distributions", distHandler.ListAllDistributions)
			admin.GET("/distributions/:id/receipts", distHandler.ListDistributionReceipts)

			// Admin audit-log query (platform admin only). The write path is the
			// package-level audit.Logger initialized above.
			audit.NewModule(db).RegisterRoutes(admin)

			// Admin member management (M1, platform admin only): user list,
			// profile, status switch, organization roll-up.
			adminuser.New(userModule.Service).RegisterRoutes(admin)

			// Admin department tree (M1 org view, platform admin only): proxies the
			// external dept-sync service for the real org tree and correlates its
			// members back to local users via universal id. Optional dependency —
			// when dept-sync is not configured these endpoints return 503 and the
			// frontend shows a "department service unavailable" notice.
			deptsync.NewModule(deptSyncClient, db).RegisterRoutes(admin)

			// Admin content management (M6, platform admin only): cross-registry
			// item list, across-author status switch (上下架), and delete.
			adminitem.New(db).RegisterRoutes(admin)

			kanbanModule := kanban.New()
			kanbanModule.RegisterRoutes(authed, authzModule.Service)

			channel.RegisterAdapter(wechat.NewWeChatAdapter())
			channel.RegisterAdapter(wecom.NewWeComAdapter(cfg.Channels.WeCom))
			channelModule = channel.New(db, &channel.EchoMessageHandler{}, cfg.WebhookBaseURL, cfg.Channels.EnabledTypes, cfg.Channels.WeComEnabled, cfg.Channels.WeComWebhookEnabled, cfg.Channels.WeChatEnabled)
			channelModule.RegisterRoutes(r.Group("/api"), authed)

			projectModule := project.NewWithDependencies(db, usageSvc, userModule.Service, notificationModule.Service)
			projectModule.RegisterRoutes(authed)

			memoryModule := memory.New(db, storageBackend)
			memoryModule.RegisterRoutes(authed)

			_ = notificationModule
			_ = systemRoleModule
			_ = projectModule
			_ = memoryModule
			_ = kanbanModule
		}
	}

	var redisClient *redis.Client
	var store gateway.Store
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			log.Fatalf("Invalid REDIS_URL: %v", err)
		}
		redisClient = redis.NewClient(opt)
		store = gateway.NewRedisStore(redisClient)
		log.Printf("Gateway store: Redis (%s)", cfg.RedisURL)
	} else {
		store = gateway.NewMemoryStore()
		log.Printf("Gateway store: Memory (set REDIS_URL to enable Redis)")
	}
	gatewayRegistry := gateway.NewGatewayRegistry(store, func(deviceIDs []string) {
		for _, id := range deviceIDs {
			_ = deviceSvc.SetOffline(id)
		}
	})
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			n, err := deviceSvc.MarkStaleDevicesOffline(gatewayRegistry.IsDeviceBound)
			if err != nil {
				log.Printf("[stale-check] error: %v", err)
			} else if n > 0 {
				log.Printf("[stale-check] marked %d stale device(s) offline", n)
			}
		}
	}()
	gatewayClient := gateway.NewClient(cfg.InternalSecret)

	internalGroup := r.Group("/internal")
	internalGroup.Use(middleware.InternalAuth(cfg.InternalSecret))
	gateway.RegisterInternalRoutes(internalGroup, gatewayRegistry, deviceSvc)
	authzModule.RegisterInternalRoutes(internalGroup)

	r.POST("/cloud/device/gateway-assign", gateway.GatewayAssignHandler(gatewayRegistry, deviceSvc))

	notificationSvc := notification.NewNotificationService(db, cfg.CloudBaseURL)
	distSvc.SetNotificationService(notificationSvc)

	notificationStore := notification.NewStore(db)

	// Use the same WeComAdapter instance that was registered earlier for channel module
	var wecomAdapterForDispatcher *wecom.WeComAdapter
	if a, ok := channel.GetAdapter("wecom"); ok {
		wecomAdapterForDispatcher, _ = a.(*wecom.WeComAdapter)
	}

	disp := dispatcher.NewDispatcher(db, notificationSvc, notificationStore, cfg.AppURL, wecomAdapterForDispatcher, gatewayClient, gatewayRegistry)

	// Create cloud module before action handlers so closures can reference it
	cloudModule = cloud.New(gatewayRegistry, gatewayClient)
	cloudModule.NotificationService = notificationSvc
	cloudModule.NotificationStore = notificationStore
	cloudModule.DB = db
	cloudModule.Dispatcher = disp

	// Wire the action handler into channel service for interactive card callbacks
	actionHandler := notification.NewActionHandler(notificationStore, db, gatewayClient, gatewayRegistry, channelModule.Service)
	channelModule.Service.SetActionHandler(actionHandler.Callback())

	cloudGroup := r.Group("/cloud")
	cloudGroup.Use(middleware.RequireAuth(casdoorEndpoint, jwksProvider))
	cloudModule.RegisterRoutes(cloudGroup, deviceSvc, casdoorEndpoint)

	deviceCloudGroup := r.Group("/cloud")
	cloudModule.RegisterDeviceRoutes(deviceCloudGroup, deviceSvc)

	// Device proxy: require user auth + device ownership check
	r.Any("/cloud/device/:deviceID/proxy/*path", middleware.RequireAuth(casdoorEndpoint, jwksProvider), gateway.DeviceProxyHandler(gatewayRegistry, gatewayClient, deviceSvc))

	// Cloud Team module
	teamModule := teampkg.New(db, redisClient)
	teamModule.Handler.SetAssignedTaskPusher(func(ctx context.Context, sessionID string, machineID string, userID string, task teampkg.TeamTask) error {
		_ = ctx
		dispatchEvent := cloud.Event{
			Type: cloud.EventTeamTaskDispatch,
			Properties: map[string]any{
				"sessionID": sessionID,
				"task":      task,
			},
		}
		err := cloudModule.Router.RouteUserCommand(machineID, dispatchEvent)
		if err == nil || strings.TrimSpace(userID) == "" {
			return err
		}

		// Fallback: some team members are browser-only machine IDs and cannot be
		// routed by gateway (gateway routes by cloud deviceID). In that case, try
		// the user's currently connected cloud devices.
		devices, listErr := deviceSvc.ListDevices(userID)
		if listErr != nil {
			return err
		}
		for _, dev := range devices {
			if dev.DeviceID == "" || dev.DeviceID == machineID {
				continue
			}
			if _, gwErr := gatewayRegistry.GetDeviceGateway(dev.DeviceID); gwErr != nil {
				continue
			}
			if routeErr := cloudModule.Router.RouteUserCommand(dev.DeviceID, dispatchEvent); routeErr == nil {
				logger.Warn("[team] fallback routed task=%s session=%s from machine=%s to device=%s", task.ID, sessionID, machineID, dev.DeviceID)
				return nil
			}
		}
		return err
	})
	teamAPIGroup := r.Group("/api")
	teamAPIGroup.Use(requireUserOrDeviceAuth(deviceSvc))
	teamWSGroup := r.Group("/ws")
	teamWSGroup.Use(middleware.OptionalAuth(casdoorEndpoint, jwksProvider))
	teamModule.RegisterRoutes(teamAPIGroup, teamWSGroup)

	log.Printf("Server starting on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func requireUserOrDeviceAuth(deviceSvc *services.DeviceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Casdoor user already resolved upstream by OptionalAuth (UserIDKey set).
		// OptionalAuth does NOT run the account-status gate, so enforce it here to
		// close the bypass where a banned user keeps a live Casdoor session.
		if c.GetString(middleware.UserIDKey) != "" {
			if middleware.EnforceAccountStatus(c) {
				return
			}
			c.Next()
			return
		}

		token := middleware.ExtractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		dev, err := deviceSvc.VerifyDeviceToken(token)
		if err != nil || dev == nil || strings.TrimSpace(dev.UserID) == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		c.Set(middleware.UserIDKey, dev.UserID)
		if c.GetString(middleware.UserNameKey) == "" {
			c.Set(middleware.UserNameKey, dev.DisplayName)
		}
		c.Set("deviceId", dev.DeviceID)
		c.Set("authSource", "device-token")

		// Device-token path: previously this bypassed the banned/disabled gate
		// entirely (RequireAuth was never in the chain). Apply it now that the
		// subject is resolved, so a banned member can't keep using a device token.
		// Fails open on lookup error / no checker (see middleware.EnforceAccountStatus).
		// NOTE: the team WebSocket group (/ws) still uses bare OptionalAuth and is
		// intentionally NOT gated here — a separate path with its own handlers.
		if middleware.EnforceAccountStatus(c) {
			return
		}
		c.Next()
	}
}

// deptSyncDepartmentProvider adapts the deptsync HTTP client to authz's narrow
// DepartmentProvider interface, translating deptsync.Dept into authz.DepartmentInfo
// (only dept id + materialized path are needed for prefix-based inheritance). It
// keeps authz free of any deptsync import (avoids an import cycle) while letting
// the grant engine reuse the same cached client as the admin department view.
type deptSyncDepartmentProvider struct {
	client *deptsync.Client
}

func (p deptSyncDepartmentProvider) GetUserDepartments(deptSyncUserID string) ([]authz.DepartmentInfo, error) {
	depts, err := p.client.GetUserDepartments(deptSyncUserID)
	if err != nil {
		return nil, err
	}
	out := make([]authz.DepartmentInfo, 0, len(depts))
	for _, d := range depts {
		out = append(out, authz.DepartmentInfo{DeptID: d.DeptID, DeptPath: d.DeptPath})
	}
	return out, nil
}

func (p deptSyncDepartmentProvider) GetDepartmentPath(deptID string) (string, error) {
	return p.client.GetDepartmentPath(deptID)
}

// bundleMaxBytes returns the maximum packed-bundle size in bytes for the
// synchronous upload-pack path. Configurable via BUNDLE_MAX_BYTES (default 100MB);
// non-positive disables the cap. Mirrors the worker process's setting.
func bundleMaxBytes() int64 {
	const defaultMax = 100 * 1024 * 1024 // 100MB
	if v := os.Getenv("BUNDLE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.Printf("Warning: invalid BUNDLE_MAX_BYTES=%q, using default %d", v, defaultMax)
	}
	return defaultMax
}
