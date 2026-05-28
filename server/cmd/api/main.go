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
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/costrict/costrict-web/server/docs"
	"github.com/costrict/costrict-web/server/internal/authz"
	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wechat"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/cloud"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/dispatcher"
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

// getMapKeys extracts keys from a map for debugging
func getMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// resolveQuestionAnswer parses the action string (e.g. "select:opt_0") and
// actionData to produce the answers field that cs-cloud expects: string[][].
// Each inner array contains the selected option label strings for one question.
func resolveQuestionAnswer(action string, actionData map[string]any) [][]string {
	// Parse option indices from action string like "select:opt_0" or "select:opt_0,opt_2"
	var optIndices []int
	if strings.HasPrefix(action, "select:opt_") {
		idxStr := action[len("select:opt_"):]
		for _, part := range strings.Split(idxStr, ",") {
			if n, err := strconv.Atoi(strings.TrimPrefix(part, "opt_")); err == nil {
				optIndices = append(optIndices, n)
			}
		}
	}
	log.Printf("[action-callback] resolveQuestionAnswer: action=%q optIndices=%v", action, optIndices)

	questionsRaw, _ := actionData["questions"].([]any)
	if len(questionsRaw) == 0 {
		log.Printf("[action-callback] resolveQuestionAnswer: no questions in actionData")
		return [][]string{}
	}

	answers := make([][]string, len(questionsRaw))
	for qi, qRaw := range questionsRaw {
		q, _ := qRaw.(map[string]any)
		optionsRaw, _ := q["options"].([]any)

		var labels []string
		if qi == 0 && len(optIndices) > 0 {
			for _, idx := range optIndices {
				if idx >= 0 && idx < len(optionsRaw) {
					opt, _ := optionsRaw[idx].(map[string]any)
					if label, ok := opt["label"].(string); ok {
						labels = append(labels, label)
					}
				}
			}
		}
		if len(labels) == 0 && len(optionsRaw) > 0 {
			opt, _ := optionsRaw[0].(map[string]any)
			if label, ok := opt["label"].(string); ok {
				labels = []string{label}
			}
		}
		answers[qi] = labels
	}
	log.Printf("[action-callback] resolveQuestionAnswer: resolved answers=%v", answers)
	return answers
}

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
	tagSvc := &services.TagService{DB: db}
	handlers.CategorySvc = categorySvc
	handlers.TagSvc = tagSvc
	itemHandler := handlers.NewItemHandler(db, indexerSvc, parserSvc, categorySvc, tagSvc)

	// Initialize AI-powered handlers
	searchHandler := handlers.NewSearchHandler(searchSvc)
	recommendHandler := handlers.NewRecommendHandler(recommendSvc, behaviorSvc)
	usageSvc := services.NewUsageService(usageProvider, userModule.Service)
	usageHandler := handlers.NewUsageHandler(usageSvc)

	// Initialize Distribution handler
	distSvc := services.NewDistributionService(db, behaviorSvc)
	distHandler := handlers.NewDistributionHandler(db, distSvc)

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
			Provider:          claims.Provider,
			ProviderUserID:    claims.ProviderUserID,
			Phone:             claims.Phone,
		})
	})

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
			authed.PUT("/items/:id", itemHandler.UpdateItem)
			authed.POST("/items/:id/check-consistency", itemHandler.CheckItemConsistency)
			authed.DELETE("/items/:id", handlers.DeleteItem)
			authed.PUT("/items/:id/move", handlers.MoveItem)
			authed.PUT("/items/:id/transfer", handlers.TransferItemToRepo)
			authed.POST("/items/:id/distribute", distHandler.DistributeItem)
			authed.GET("/items/:id/distributions", distHandler.ListItemDistributions)
			authed.POST("/items/:id/scan", handlers.TriggerItemScan)
			authed.POST("/scan-jobs/:id/cancel", handlers.CancelScanJob)
			authed.POST("/items/:id/favorite", recommendHandler.FavoriteItem)
			authed.DELETE("/items/:id/favorite", recommendHandler.UnfavoriteItem)

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

			admin := authed.Group("/admin")
			admin.Use(systemrole.RequirePlatformAdmin(db))
			authzModule.RegisterAdminRoutes(admin)

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

	disp := dispatcher.NewDispatcher(db, notificationSvc, notificationStore, cfg.AppURL, cfg.NotificationBufferSeconds, wecomAdapterForDispatcher, gatewayClient, gatewayRegistry)

	// Create cloud module before action handlers so closures can reference it
	cloudModule = cloud.New(gatewayRegistry, gatewayClient)
	cloudModule.NotificationService = notificationSvc
	cloudModule.NotificationStore = notificationStore
	cloudModule.DB = db
	cloudModule.Dispatcher = disp

	// Wire the action handler into channel service for interactive card callbacks
	channelModule.Service.SetActionHandler(func(ctx context.Context, action, token, responseCode, externalUserID string) error {
		n, err := notificationStore.ExecuteAction(token, map[string]any{"action": action})
		if err != nil {
			logger.Error("[action-callback] token invalid or expired: %v", err)
			return err
		}

		disp.OnInterventionResponse(token)
		// Update card status using stored card data
		if responseCode != "" && n.CardData != nil && len(n.CardData) > 0 {
			go channelModule.Service.UpdateInteractiveCard("wecom", responseCode, "已处理", action, n.CardData, externalUserID)
		}

		// Parse action data for the response
		var actionData map[string]any
		if n.ActionData != nil && len(n.ActionData) > 0 {
			if err := json.Unmarshal(n.ActionData, &actionData); err != nil {
				logger.Error("[action-callback] failed to unmarshal action data: %v", err)
			} else {
				logger.Info("[action-callback] parsed actionData: %+v", actionData)
			}
		}

		// Extract id from action data (unified field for both permission and question)
		var id string
		if actionData != nil {
			if val, ok := actionData["id"].(string); ok {
				id = val
			}
		}
		logger.Info("[action-callback] type=%s, action=%s, id=%s, sessionID=%s", n.Type, action, id, n.SessionID)

		// Route to appropriate cs-cloud endpoint based on type
		if id != "" && n.DeviceID != "" {
			var proxyPath string
			var requestBody map[string]any

			switch n.Type {
			case "permission":
				proxyPath = fmt.Sprintf("/api/v1/permissions/%s/reply", id)
				requestBody = map[string]any{"approved": (action == "approve")}
				logger.Info("[action-callback] proxying to cs-cloud: %s, approved=%v", proxyPath, requestBody["approved"])

			case "question":
				proxyPath = fmt.Sprintf("/api/v1/questions/%s/reply", id)
				// cs-cloud expects answers: string[][] - one string array per question,
				// each containing the selected option labels.
				// The action string is "select:opt_N" where N is the option index.
				answers := resolveQuestionAnswer(action, actionData)
				requestBody = map[string]any{"answers": answers}
				logger.Info("[action-callback] proxying to cs-cloud: %s, answers=%v", proxyPath, answers)
			default:
				logger.Error("[action-callback] unknown type: %s", n.Type)
				return nil
			}

			// Proxy the request to cs-cloud through gateway
			userID := ""
			// Try to extract userID from notification's session context
			if n.UserID != "" {
				userID = n.UserID
			} else {
				// Fallback: extract from context if available (from auth middleware)
				if u, ok := ctx.Value("user_id").(string); ok {
					userID = u
				} else {
					logger.Error("[action-callback] no userID available for proxy request")
					return fmt.Errorf("no userID available")
				}
			}

			// Marshal request body
			bodyBytes, _ := json.Marshal(requestBody)
			logger.Info("[action-callback] proxying with userID=%s, deviceID=%s, sessionID=%s, path=%s", userID, n.DeviceID, n.SessionID, proxyPath)

			// Proxy through gateway to cs-cloud
			var result map[string]any
			if err := gateway.ProxyDeviceSessionRequest(gatewayClient, gatewayRegistry, userID, n.DeviceID, "", "POST", proxyPath, bodyBytes, &result); err != nil {
				logger.Error("[action-callback] proxy request failed: %v", err)
				return err
			}
			logger.Info("[action-callback] proxy response successful: %+v", result)
		} else {
			logger.Error("[action-callback] missing id or deviceID: id=%q, deviceID=%q", id, n.DeviceID)
		}

		return nil
	})

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
