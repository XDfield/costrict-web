// @title           Costrict Web API
// @version         1.0
// @description     AI Agent Platform API - Skill marketplace, organization and repository management.
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
	"log"
	"os"

	_ "github.com/costrict/costrict-web/server/docs"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/handlers"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/scheduler"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	err = db.AutoMigrate(
		&models.Organization{},
		&models.OrgMember{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityAsset{},
		&models.CapabilityArtifact{},
		&models.SyncJob{},
		&models.SyncLog{},
	)
	if err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	log.Println("Database migrated successfully")

	handlers.EnsurePublicRegistry()

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

	sched := &scheduler.Scheduler{
		JobService: jobSvc,
		DB:         db,
	}
	if err := sched.Start(); err != nil {
		log.Fatalf("Failed to start scheduler: %v", err)
	}
	defer sched.Stop()
	handlers.SyncScheduler = sched

	r := gin.Default()

	casdoorEndpoint := cfg.Casdoor.Endpoint

	r.Use(middleware.CORS())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.OptionalAuth(casdoorEndpoint))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	api := r.Group("/api")
	{
		auth := api.Group("/auth")
		{
			auth.GET("/login", handlers.Login)
			auth.POST("/logout", handlers.Logout)
			auth.GET("/me", handlers.GetCurrentUser)
		}

		orgs := api.Group("/organizations")
		{
			orgs.GET("", handlers.ListOrganizations)
			orgs.GET("/my", handlers.GetMyOrganizations)
			orgs.POST("", handlers.CreateOrganization)
			orgs.GET("/:id", handlers.GetOrganization)
			orgs.PUT("/:id", handlers.UpdateOrganization)
			orgs.DELETE("/:id", handlers.DeleteOrganization)
			orgs.GET("/:id/members", handlers.ListOrganizationMembers)
			orgs.POST("/:id/members", handlers.AddOrganizationMember)
			orgs.DELETE("/:id/members/:userId", handlers.RemoveOrganizationMember)
			orgs.GET("/:id/registry", handlers.GetOrganizationRegistry)
			orgs.POST("/:id/sync", handlers.TriggerOrgSync)
			orgs.POST("/:id/sync/cancel", handlers.CancelOrgSync)
			orgs.GET("/:id/sync-status", handlers.GetOrgSyncStatus)
			orgs.GET("/:id/sync-logs", handlers.ListOrgSyncLogs)
			orgs.GET("/:id/sync-jobs", handlers.ListOrgSyncJobs)
		}

		api.GET("/registries", handlers.ListRegistries)
		api.GET("/registries/my", handlers.ListMyRegistries)
		api.GET("/registries/public", handlers.GetPublicRegistry)
		api.POST("/registries", handlers.CreateRegistry)
		api.POST("/registries/ensure-personal", handlers.EnsurePersonalRegistry)
		api.GET("/registries/:id", handlers.GetRegistry)
		api.PUT("/registries/:id", handlers.UpdateRegistry)
		api.DELETE("/registries/:id", handlers.DeleteRegistry)
		api.GET("/registries/:id/items", handlers.ListItems)
		api.POST("/registries/:id/items", handlers.CreateItem)
		api.POST("/registries/:id/sync", handlers.TriggerRegistrySync)
		api.POST("/registries/:id/sync/cancel", handlers.CancelRegistrySync)
		api.GET("/registries/:id/sync-status", handlers.GetRegistrySyncStatus)
		api.GET("/registries/:id/sync-logs", handlers.ListRegistrySyncLogs)
		api.GET("/registries/:id/sync-jobs", handlers.ListRegistrySyncJobs)

		api.GET("/items/my", handlers.ListMyItems)
		api.GET("/items", handlers.ListAllItems)
		api.POST("/items", handlers.CreateItemDirect)
		api.GET("/items/:id", handlers.GetItem)
		api.PUT("/items/:id", handlers.UpdateItem)
		api.DELETE("/items/:id", handlers.DeleteItem)
		api.GET("/items/:id/versions", handlers.ListItemVersions)
		api.GET("/items/:id/versions/:version", handlers.GetItemVersion)
		api.GET("/items/:id/artifacts", handlers.ListArtifacts)

		api.POST("/artifacts/upload", handlers.UploadArtifact)
		api.GET("/artifacts/:id/download", handlers.DownloadArtifact)
		api.DELETE("/artifacts/:id", handlers.DeleteArtifact)

		api.GET("/items/:id/download", handlers.DownloadItem)

		api.GET("/registry/:org/access", handlers.RegistryAccess)
		api.GET("/registry/:org/index.json", handlers.RegistryIndex)
		api.GET("/registry/:org/:slug/:file", handlers.DownloadRegistryFile)

		api.GET("/sync-logs/:id", handlers.GetSyncLogDetail)
		api.GET("/sync-jobs/:id", handlers.GetSyncJobDetail)
		api.POST("/webhooks/github", handlers.HandleGitHubWebhook)
	}

	log.Printf("Server starting on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
