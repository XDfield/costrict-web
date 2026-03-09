package main

import (
	"log"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/handlers"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/costrict/costrict-web/server/internal/models"
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
		&models.SkillRepository{},
		&models.Skill{},
		&models.Agent{},
		&models.Command{},
		&models.MCPServer{},
		&models.SkillRating{},
		&models.UserPreference{},
	)
	if err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	log.Println("Database migrated successfully")

	// Initialize Gin router
	r := gin.Default()

	// Middleware
	r.Use(middleware.CORS())
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

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
			orgs.POST("", handlers.CreateOrganization)
			orgs.GET("/:id", handlers.GetOrganization)
			orgs.PUT("/:id", handlers.UpdateOrganization)
			orgs.DELETE("/:id", handlers.DeleteOrganization)
			orgs.POST("/:id/members", handlers.AddOrganizationMember)
			orgs.DELETE("/:id/members/:userId", handlers.RemoveOrganizationMember)
		}

		// Repository routes
		repos := api.Group("/repositories")
		{
			repos.GET("", handlers.ListRepositories)
			repos.POST("", handlers.CreateRepository)
			repos.GET("/:id", handlers.GetRepository)
			repos.PUT("/:id", handlers.UpdateRepository)
			repos.DELETE("/:id", handlers.DeleteRepository)
			repos.POST("/:id/members", handlers.AddRepositoryMember)
			repos.DELETE("/:id/members/:userId", handlers.RemoveRepositoryMember)
		}

		// Skill routes
		skills := api.Group("/skills")
		{
			skills.GET("", handlers.ListSkills)
			skills.POST("", handlers.CreateSkill)
			skills.GET("/:id", handlers.GetSkill)
			skills.PUT("/:id", handlers.UpdateSkill)
			skills.DELETE("/:id", handlers.DeleteSkill)
			skills.POST("/:id/install", handlers.InstallSkill)
			skills.POST("/:id/rating", handlers.RateSkill)
		}

		// Agent routes
		agents := api.Group("/agents")
		{
			agents.GET("", handlers.ListAgents)
			agents.POST("", handlers.CreateAgent)
			agents.GET("/:id", handlers.GetAgent)
			agents.PUT("/:id", handlers.UpdateAgent)
			agents.DELETE("/:id", handlers.DeleteAgent)
		}

		// Command routes
		commands := api.Group("/commands")
		{
			commands.GET("", handlers.ListCommands)
			commands.POST("", handlers.CreateCommand)
			commands.GET("/:id", handlers.GetCommand)
			commands.PUT("/:id", handlers.UpdateCommand)
			commands.DELETE("/:id", handlers.DeleteCommand)
		}

		// MCP Server routes
		mcpServers := api.Group("/mcp-servers")
		{
			mcpServers.GET("", handlers.ListMCPServers)
			mcpServers.POST("", handlers.CreateMCPServer)
			mcpServers.GET("/:id", handlers.GetMCPServer)
			mcpServers.PUT("/:id", handlers.UpdateMCPServer)
			mcpServers.DELETE("/:id", handlers.DeleteMCPServer)
		}

		// Marketplace routes
		marketplace := api.Group("/marketplace")
		{
			marketplace.GET("/skills", handlers.ListMarketplaceSkills)
			marketplace.GET("/categories", handlers.ListCategories)
			marketplace.GET("/skills/trending", handlers.GetTrendingSkills)
		}
	}

	// Start server
	log.Printf("Server starting on port %s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
