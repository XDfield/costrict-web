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
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/costrict/costrict-web/server/docs"
	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wechat"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/cloud"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/handlers"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/project"
	"github.com/costrict/costrict-web/server/internal/scheduler"
	"github.com/costrict/costrict-web/server/internal/services"
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
		log.Fatalf("Failed to initialize usage provider: unsupported USAGE_PROVIDER=%s", fmt.Sprintf("%q", cfg.UsageProvider))
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

	// Embedding and Indexer Services
	embeddingSvc := services.NewEmbeddingService(&cfg.Embedding)
	indexerSvc := services.NewIndexerService(db, embeddingSvc)

	// Search Service
	searchSvc := services.NewSearchService(db, embeddingSvc, &cfg.Search)

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

	// Initialize ItemHandler with indexer and parser
	parserSvc := &services.ParserService{}
	categorySvc := &services.CategoryService{DB: db}
	handlers.CategorySvc = categorySvc
	itemHandler := handlers.NewItemHandler(db, indexerSvc, parserSvc, categorySvc)

	// Initialize AI-powered handlers
	searchHandler := handlers.NewSearchHandler(searchSvc)
	recommendHandler := handlers.NewRecommendHandler(recommendSvc, behaviorSvc)
	usageSvc := services.NewUsageService(usageProvider, userModule.Service)
	usageHandler := handlers.NewUsageHandler(usageSvc)

	// Start background indexing (every hour)
	indexerSvc.StartBackgroundIndexing(context.Background(), time.Hour)

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
		})
	})

	r.Use(middleware.CORS(middleware.CORSConfig{AllowedOrigins: cfg.CORSAllowedOrigins}))
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.ErrorLogger())
	r.Use(middleware.OptionalAuth(casdoorEndpoint, jwksProvider))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	deviceSvc := &services.DeviceService{DB: db}
	updateSvc := &services.UpdateService{DB: db, ReleaseDownloadURL: cfg.ReleaseDownloadBaseURL}
	workspaceSvc := &services.WorkspaceService{DB: db, DeviceService: deviceSvc}

	api := r.Group("/api")
	{
		auth := api.Group("/auth")
		{
			auth.GET("/callback", handlers.AuthCallback)
			auth.GET("/login", handlers.Login)
			auth.POST("/logout", handlers.Logout)
		}

		// === Public read-only endpoints (OptionalAuth, no login required) ===
		api.GET("/updates/check", handlers.UpdateCheckHandler(updateSvc))
		api.POST("/devices/:deviceID/heartbeat", handlers.DeviceHeartbeatHandler(deviceSvc))
		api.GET("/registries/public", handlers.GetPublicRegistry)
		api.GET("/registries/:id", handlers.GetRegistry)
		api.GET("/registries/:id/items", handlers.ListItems)
		api.GET("/registry/:repo/access", handlers.RegistryAccess)
		api.GET("/registry/:repo/index.json", handlers.RegistryIndex)
		api.GET("/registry/:repo/:itemType/:slug/*file", handlers.DownloadRegistryFile)
		api.POST("/webhooks/github", handlers.HandleGitHubWebhook)

		api.POST("/releases", middleware.SystemTokenAuth(cfg.SystemToken), handlers.CreateReleaseHandler(updateSvc))

		// Items read (anonymous can preview public items)
		api.GET("/items", handlers.ListAllItems)
		api.GET("/items/:id", handlers.GetItem)
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
		api.POST("/items/:id/behavior", recommendHandler.LogBehavior)
		api.GET("/items/:id/stats", recommendHandler.GetItemStats)

		// All routes below require authentication
		authed := api.Group("")
		authed.Use(middleware.RequireAuth(casdoorEndpoint, jwksProvider))
		{
			authed.GET("/auth/me", handlers.GetCurrentUser)

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
			authed.PUT("/items/:id", itemHandler.UpdateItem)
			authed.POST("/items/:id/check-consistency", itemHandler.CheckItemConsistency)
			authed.DELETE("/items/:id", handlers.DeleteItem)
			authed.PUT("/items/:id/move", handlers.MoveItem)
			authed.PUT("/items/:id/transfer", handlers.TransferItemToRepo)
			authed.POST("/items/:id/scan", handlers.TriggerItemScan)
			authed.POST("/scan-jobs/:id/cancel", handlers.CancelScanJob)
			authed.POST("/items/:id/favorite", recommendHandler.FavoriteItem)
			authed.DELETE("/items/:id/favorite", recommendHandler.UnfavoriteItem)

			authed.POST("/artifacts/upload", handlers.UploadArtifact)
			authed.DELETE("/artifacts/:id", handlers.DeleteArtifact)

			authed.POST("/categories", handlers.CreateCategoryHandler(categorySvc))
			authed.PUT("/categories/:id", handlers.UpdateCategoryHandler(categorySvc))
			authed.DELETE("/categories/:id", handlers.DeleteCategoryHandler(categorySvc))

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
				devices.GET("", handlers.ListDevicesHandler(deviceSvc))
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

			notificationModule := notification.New(db, cfg.CloudBaseURL)
			systemRoleModule := systemrole.New(db)
			systemRoleModule.RegisterRoutes(authed)
			notificationModule.RegisterRoutes(authed)

			channel.RegisterAdapter(wechat.NewWeChatAdapter())
			channel.RegisterAdapter(wecom.NewWeComAdapter(cfg.Channels.WeCom))
			channelModule := channel.New(db, &channel.EchoMessageHandler{}, cfg.CloudBaseURL, cfg.Channels.EnabledTypes)
			channelModule.RegisterRoutes(r.Group("/api"), authed)

			projectModule := project.NewWithDependencies(db, usageSvc, userModule.Service, notificationModule.Service)
			projectModule.RegisterRoutes(authed)
			_ = notificationModule
			_ = systemRoleModule
			_ = projectModule
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
	gatewayRegistry := gateway.NewGatewayRegistry(store)
	gatewayClient := gateway.NewClient(cfg.InternalSecret)

	internalGroup := r.Group("/internal")
	internalGroup.Use(middleware.InternalAuth(cfg.InternalSecret))
	gateway.RegisterInternalRoutes(internalGroup, gatewayRegistry, deviceSvc)

	r.POST("/cloud/device/gateway-assign", gateway.GatewayAssignHandler(gatewayRegistry, deviceSvc))

	notificationSvc := notification.NewNotificationService(db, cfg.CloudBaseURL)

	cloudModule := cloud.New(gatewayRegistry, gatewayClient)
	cloudModule.NotificationService = notificationSvc
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
		if c.GetString(middleware.UserIDKey) != "" {
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
		c.Next()
	}
}
