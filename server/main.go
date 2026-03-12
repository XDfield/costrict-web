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
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize database
	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Auto migrate database schema
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

	// Initialize Gin router
	r := gin.Default()

	casdoorEndpoint := cfg.Casdoor.Endpoint

	// Middleware
	r.Use(middleware.CORS())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.OptionalAuth(casdoorEndpoint))

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// Swagger UI
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// API routes
	api := r.Group("/api")
	{
		// Auth routes
		auth := api.Group("/auth")
		{
			auth.GET("/login", handlers.Login)
			auth.POST("/logout", handlers.Logout)
			auth.GET("/me", handlers.GetCurrentUser)
		}

		// Organization routes
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
		}

		// Skill Registries
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

		// My items
		api.GET("/items/my", handlers.ListMyItems)

		// Global items query (with visibility filtering)
		api.GET("/items", handlers.ListAllItems)

		// Convenient item creation (auto-selects public registry)
		api.POST("/items", handlers.CreateItemDirect)

		// Skill Items
		api.GET("/items/:id", handlers.GetItem)
		api.PUT("/items/:id", handlers.UpdateItem)
		api.DELETE("/items/:id", handlers.DeleteItem)
		api.GET("/items/:id/versions", handlers.ListItemVersions)
		api.GET("/items/:id/versions/:version", handlers.GetItemVersion)
		api.GET("/items/:id/artifacts", handlers.ListArtifacts)

		// Artifacts
		api.POST("/artifacts/upload", handlers.UploadArtifact)
		api.GET("/artifacts/:id/download", handlers.DownloadArtifact)
		api.DELETE("/artifacts/:id", handlers.DeleteArtifact)

		// Item content download
		api.GET("/items/:id/download", handlers.DownloadItem)

		// Plugin Registry
		api.GET("/registry/:org/access", handlers.RegistryAccess)
		api.GET("/registry/:org/index.json", handlers.RegistryIndex)
		api.GET("/registry/:org/:slug/:file", handlers.DownloadRegistryFile)
	}

	// Start server
	log.Printf("Server starting on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
