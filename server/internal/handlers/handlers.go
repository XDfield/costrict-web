package handlers

import (
	"net/http"
	"github.com/gin-gonic/gin"
	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
)

// Auth handlers
func Login(c *gin.Context) {
	var req struct {
		Code  string `json:"code" binding:"required"`
		State string `json:"state"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	cfg := casdoor.NewClient(nil) // TODO: Get from config
	tokenResp, err := cfg.ExchangeCodeForToken(req.Code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to exchange code for token"})
		return
	}

	// Get user info
	userInfo, err := cfg.GetUserInfo(tokenResp.AccessToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user info"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      tokenResp.AccessToken,
		"tokenType":  tokenResp.TokenType,
		"user":       userInfo.User,
	})
}

func Logout(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "Logout successful"})
}

func GetCurrentUser(c *gin.Context) {
	accessToken := c.GetHeader("Authorization")
	if accessToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	cfg := casdoor.NewClient(nil) // TODO: Get from config
	userInfo, err := cfg.GetUserInfo(accessToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user info"})
		return
	}

	c.JSON(http.StatusOK, userInfo.User)
}

// Organization handlers
func ListOrganizations(c *gin.Context) {
	db := database.GetDB()
	var orgs []models.Organization
	result := db.Find(&orgs)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch organizations"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"organizations": orgs})
}

func CreateOrganization(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		DisplayName string `json:"displayName"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	org := models.Organization{
		ID:          uuid.New().String(),
		Name:        req.Name,
		DisplayName: req.DisplayName,
	}

	db := database.GetDB()
	if result := db.Create(&org); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create organization"})
		return
	}

	c.JSON(http.StatusCreated, org)
}

func GetOrganization(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var org models.Organization
	result := db.First(&org, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
		return
	}

	c.JSON(http.StatusOK, org)
}

func UpdateOrganization(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var org models.Organization
	result := db.First(&org, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
		return
	}

	if req.Name != "" {
		org.Name = req.Name
	}
	if req.DisplayName != "" {
		org.DisplayName = req.DisplayName
	}

	if result := db.Save(&org); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update organization"})
		return
	}

	c.JSON(http.StatusOK, org)
}

func DeleteOrganization(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.Organization{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete organization"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Organization deleted"})
}

func AddOrganizationMember(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		UserID string `json:"userId" binding:"required"`
		Role   string `json:"role"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// TODO: Add member to organization
	c.JSON(http.StatusOK, gin.H{"message": "Member added"})
}

func RemoveOrganizationMember(c *gin.Context) {
	id := c.Param("id")
	userID := c.Param("userId")
	db := database.GetDB()
	// TODO: Remove member from organization
	c.JSON(http.StatusOK, gin.H{"message": "Member removed"})
}

// Repository handlers
func ListRepositories(c *gin.Context) {
	db := database.GetDB()
	var repos []models.SkillRepository
	result := db.Find(&repos)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch repositories"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"repositories": repos})
}

func CreateRepository(c *gin.Context) {
	var req struct {
		Name         string `json:"name" binding:"required"`
		Description  string `json:"description"`
		Visibility   string `json:"visibility"`
		OwnerID      string `json:"ownerId" binding:"required"`
		OrganizationID *string `json:"organizationId"`
		GroupID      *string `json:"groupId"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	repo := models.SkillRepository{
		ID:             uuid.New().String(),
		Name:           req.Name,
		Description:    req.Description,
		Visibility:     req.Visibility,
		OwnerID:        req.OwnerID,
		OrganizationID: req.OrganizationID,
		GroupID:        req.GroupID,
	}

	db := database.GetDB()
	if result := db.Create(&repo); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create repository"})
		return
	}

	c.JSON(http.StatusCreated, repo)
}

func GetRepository(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var repo models.SkillRepository
	result := db.Preload("Skills").Preload("Agents").Preload("Commands").Preload("MCPServers").First(&repo, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return
	}

	c.JSON(http.StatusOK, repo)
}

func UpdateRepository(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var repo models.SkillRepository
	result := db.First(&repo, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return
	}

	if req.Name != "" {
		repo.Name = req.Name
	}
	if req.Description != "" {
		repo.Description = req.Description
	}
	if req.Visibility != "" {
		repo.Visibility = req.Visibility
	}

	if result := db.Save(&repo); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update repository"})
		return
	}

	c.JSON(http.StatusOK, repo)
}

func DeleteRepository(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.SkillRepository{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete repository"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Repository deleted"})
}

func AddRepositoryMember(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		UserID string `json:"userId" binding:"required"`
		Role   string `json:"role"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// TODO: Add member to repository
	c.JSON(http.StatusOK, gin.H{"message": "Member added"})
}

func RemoveRepositoryMember(c *gin.Context) {
	id := c.Param("id")
	userID := c.Param("userId")
	db := database.GetDB()
	// TODO: Remove member from repository
	c.JSON(http.StatusOK, gin.H{"message": "Member removed"})
}

// Skill handlers
func ListSkills(c *gin.Context) {
	db := database.GetDB()
	var skills []models.Skill
	result := db.Find(&skills)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch skills"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

func CreateSkill(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		Version     string `json:"version"`
		RepoID      string `json:"repoId" binding:"required"`
		IsPublic    bool   `json:"isPublic"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	skill := models.Skill{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Description: req.Description,
		Version:   req.Version,
		RepoID:    req.RepoID,
		IsPublic:  req.IsPublic,
	}

	db := database.GetDB()
	if result := db.Create(&skill); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create skill"})
		return
	}

	c.JSON(http.StatusCreated, skill)
}

func GetSkill(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var skill models.Skill
	result := db.Preload("Repository").First(&skill, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Skill not found"})
		return
	}

	c.JSON(http.StatusOK, skill)
}

func UpdateSkill(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
		IsPublic    bool   `json:"isPublic"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var skill models.Skill
	result := db.First(&skill, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Skill not found"})
		return
	}

	if req.Name != "" {
		skill.Name = req.Name
	}
	if req.Description != "" {
		skill.Description = req.Description
	}
	if req.Version != "" {
		skill.Version = req.Version
	}
	skill.IsPublic = req.IsPublic

	if result := db.Save(&skill); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update skill"})
		return
	}

	c.JSON(http.StatusOK, skill)
}

func DeleteSkill(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.Skill{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete skill"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Skill deleted"})
}

func InstallSkill(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var skill models.Skill
	result := db.First(&skill, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Skill not found"})
		return
	}

	skill.InstallCount++
	if result := db.Save(&skill); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update skill"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Skill installed", "installCount": skill.InstallCount})
}

func RateSkill(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Rating int    `json:"rating" binding:"required,min=1,max=5"`
		Comment string `json:"comment"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var skill models.Skill
	result := db.First(&skill, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Skill not found"})
		return
	}

	// Calculate new rating
	// TODO: Implement proper rating calculation
	skill.Rating = float64(req.Rating)

	if result := db.Save(&skill); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update skill"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Skill rated", "rating": skill.Rating})
}

// Agent handlers
func ListAgents(c *gin.Context) {
	db := database.GetDB()
	var agents []models.Agent
	result := db.Find(&agents)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agents"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

func CreateAgent(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		Version     string `json:"version"`
		RepoID      string `json:"repoId" binding:"required"`
		IsPublic    bool   `json:"isPublic"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	agent := models.Agent{
		ID:       uuid.New().String(),
		Name:     req.Name,
		Description: req.Description,
		Version:  req.Version,
		RepoID:   req.RepoID,
		IsPublic: req.IsPublic,
	}

	db := database.GetDB()
	if result := db.Create(&agent); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create agent"})
		return
	}

	c.JSON(http.StatusCreated, agent)
}

func GetAgent(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var agent models.Agent
	result := db.Preload("Repository").First(&agent, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	c.JSON(http.StatusOK, agent)
}

func UpdateAgent(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
		IsPublic    bool   `json:"isPublic"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var agent models.Agent
	result := db.First(&agent, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	if req.Name != "" {
		agent.Name = req.Name
	}
	if req.Description != "" {
		agent.Description = req.Description
	}
	if req.Version != "" {
		agent.Version = req.Version
	}
	agent.IsPublic = req.IsPublic

	if result := db.Save(&agent); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update agent"})
		return
	}

	c.JSON(http.StatusOK, agent)
}

func DeleteAgent(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.Agent{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete agent"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Agent deleted"})
}

// Command handlers
func ListCommands(c *gin.Context) {
	db := database.GetDB()
	var commands []models.Command
	result := db.Find(&commands)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch commands"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"commands": commands})
}

func CreateCommand(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		RepoID      string `json:"repoId" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	command := models.Command{
		ID:   uuid.New().String(),
		Name: req.Name,
		Description: req.Description,
		RepoID: req.RepoID,
	}

	db := database.GetDB()
	if result := db.Create(&command); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create command"})
		return
	}

	c.JSON(http.StatusCreated, command)
}

func GetCommand(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var command models.Command
	result := db.Preload("Repository").First(&command, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Command not found"})
		return
	}

	c.JSON(http.StatusOK, command)
}

func UpdateCommand(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var command models.Command
	result := db.First(&command, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Command not found"})
		return
	}

	if req.Name != "" {
		command.Name = req.Name
	}
	if req.Description != "" {
		command.Description = req.Description
	}

	if result := db.Save(&command); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update command"})
		return
	}

	c.JSON(http.StatusOK, command)
}

func DeleteCommand(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.Command{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete command"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Command deleted"})
}

// MCP Server handlers
func ListMCPServers(c *gin.Context) {
	db := database.GetDB()
	var mcpServers []models.MCPServer
	result := db.Find(&mcpServers)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch MCP servers"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"mcpServers": mcpServers})
}

func CreateMCPServer(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		RepoID      string `json:"repoId" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	mcpServer := models.MCPServer{
		ID:   uuid.New().String(),
		Name: req.Name,
		Description: req.Description,
		RepoID: req.RepoID,
	}

	db := database.GetDB()
	if result := db.Create(&mcpServer); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create MCP server"})
		return
	}

	c.JSON(http.StatusCreated, mcpServer)
}

func GetMCPServer(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var mcpServer models.MCPServer
	result := db.Preload("Repository").First(&mcpServer, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
		return
	}

	c.JSON(http.StatusOK, mcpServer)
}

func UpdateMCPServer(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var mcpServer models.MCPServer
	result := db.First(&mcpServer, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
		return
	}

	if req.Name != "" {
		mcpServer.Name = req.Name
	}
	if req.Description != "" {
		mcpServer.Description = req.Description
	}

	if result := db.Save(&mcpServer); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update MCP server"})
		return
	}

	c.JSON(http.StatusOK, mcpServer)
}

func DeleteMCPServer(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.MCPServer{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete MCP server"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "MCP server deleted"})
}

// Marketplace handlers
func ListMarketplaceSkills(c *gin.Context) {
	db := database.GetDB()
	var skills []models.Skill
	result := db.Where("is_public = ?", true).Find(&skills)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch marketplace skills"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

func ListCategories(c *gin.Context) {
	categories := []string{
		"Development",
		"AI & ML",
		"DevOps",
		"Security",
		"Data",
		"Testing",
		"Documentation",
		"Utilities",
	}

	c.JSON(http.StatusOK, gin.H{"categories": categories})
}

func GetTrendingSkills(c *gin.Context) {
	db := database.GetDB()
	var skills []models.Skill
	result := db.Where("is_public = ?", true).Order("install_count DESC").Limit(10).Find(&skills)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch trending skills"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"skills": skills})
}
