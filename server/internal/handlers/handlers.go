package handlers

import (
	"net/http"
	"github.com/gin-gonic/gin"
	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
)

// Login godoc
// @Summary      OAuth login
// @Description  Exchange OAuth authorization code for access token
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body  object{code=string,state=string}  true  "OAuth code"
// @Success      200   {object}  object{token=string,tokenType=string,user=object}
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /auth/login [get]
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

// Logout godoc
// @Summary      Logout
// @Description  Invalidate current session
// @Tags         auth
// @Produce      json
// @Success      200  {object}  object{message=string}
// @Router       /auth/logout [post]
func Logout(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "Logout successful"})
}

// GetCurrentUser godoc
// @Summary      Get current user
// @Description  Get information of the authenticated user
// @Tags         auth
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  object{}
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /auth/me [get]
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

// ListOrganizations godoc
// @Summary      List organizations
// @Description  Get all organizations
// @Tags         organizations
// @Produce      json
// @Success      200  {object}  object{organizations=[]models.Organization}
// @Failure      500  {object}  object{error=string}
// @Router       /organizations [get]
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

// CreateOrganization godoc
// @Summary      Create organization
// @Description  Create a new organization
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        body  body  object{name=string,displayName=string,description=string,visibility=string,ownerId=string}  true  "Organization data"
// @Success      201  {object}  models.Organization
// @Failure      400  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /organizations [post]
func CreateOrganization(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
		OwnerID     string `json:"ownerId" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	visibility := req.Visibility
	if visibility == "" {
		visibility = "private"
	}

	org := models.Organization{
		ID:          uuid.New().String(),
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Visibility:  visibility,
		OwnerID:     req.OwnerID,
	}

	db := database.GetDB()
	if result := db.Create(&org); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create organization"})
		return
	}

	ownerMember := models.OrgMember{
		ID:     uuid.New().String(),
		OrgID:  org.ID,
		UserID: req.OwnerID,
		Role:   "owner",
	}
	db.Create(&ownerMember)

	orgRegistry := models.SkillRegistry{
		ID:          uuid.New().String(),
		Name:        org.Name,
		Description: "Registry for organization " + org.Name,
		SourceType:  "internal",
		Visibility:  visibility,
		OrgID:       org.ID,
		OwnerID:     req.OwnerID,
	}
	db.Create(&orgRegistry)

	c.JSON(http.StatusCreated, org)
}

// GetOrganization godoc
// @Summary      Get organization
// @Description  Get organization by ID
// @Tags         organizations
// @Produce      json
// @Param        id   path      string  true  "Organization ID"
// @Success      200  {object}  models.Organization
// @Failure      404  {object}  object{error=string}
// @Router       /organizations/{id} [get]
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

// UpdateOrganization godoc
// @Summary      Update organization
// @Description  Update organization by ID
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Organization ID"
// @Param        body  body      object{name=string,displayName=string,description=string,visibility=string}  false  "Organization data"
// @Success      200   {object}  models.Organization
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /organizations/{id} [put]
func UpdateOrganization(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
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
	if req.Description != "" {
		org.Description = req.Description
	}
	if req.Visibility != "" {
		org.Visibility = req.Visibility
	}

	if result := db.Save(&org); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update organization"})
		return
	}

	c.JSON(http.StatusOK, org)
}

// DeleteOrganization godoc
// @Summary      Delete organization
// @Description  Delete organization by ID
// @Tags         organizations
// @Produce      json
// @Param        id   path      string  true  "Organization ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /organizations/{id} [delete]
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

// AddOrganizationMember godoc
// @Summary      Add organization member
// @Description  Add a user to an organization
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Organization ID"
// @Param        body  body      object{userId=string,username=string,role=string}  true  "Member data"
// @Success      201   {object}  models.OrgMember
// @Failure      400   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /organizations/{id}/members [post]
func AddOrganizationMember(c *gin.Context) {
	orgID := c.Param("id")
	var req struct {
		UserID   string `json:"userId" binding:"required"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	role := req.Role
	if role == "" {
		role = "member"
	}

	db := database.GetDB()
	var existing models.OrgMember
	if db.Where("org_id = ? AND user_id = ?", orgID, req.UserID).First(&existing).Error == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
		return
	}

	member := models.OrgMember{
		ID:       uuid.New().String(),
		OrgID:    orgID,
		UserID:   req.UserID,
		Username: req.Username,
		Role:     role,
	}

	if result := db.Create(&member); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	c.JSON(http.StatusCreated, member)
}

// RemoveOrganizationMember godoc
// @Summary      Remove organization member
// @Description  Remove a user from an organization
// @Tags         organizations
// @Produce      json
// @Param        id      path      string  true  "Organization ID"
// @Param        userId  path      string  true  "User ID"
// @Success      200     {object}  object{message=string}
// @Failure      500     {object}  object{error=string}
// @Router       /organizations/{id}/members/{userId} [delete]
func RemoveOrganizationMember(c *gin.Context) {
	orgID := c.Param("id")
	userID := c.Param("userId")
	db := database.GetDB()
	result := db.Where("org_id = ? AND user_id = ?", orgID, userID).Delete(&models.OrgMember{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Member removed"})
}

// ListOrganizationMembers godoc
// @Summary      List organization members
// @Description  Get all members of an organization
// @Tags         organizations
// @Produce      json
// @Param        id   path      string  true  "Organization ID"
// @Success      200  {object}  object{members=[]models.OrgMember}
// @Failure      500  {object}  object{error=string}
// @Router       /organizations/{id}/members [get]
func ListOrganizationMembers(c *gin.Context) {
	orgID := c.Param("id")
	db := database.GetDB()
	var members []models.OrgMember
	result := db.Where("org_id = ?", orgID).Find(&members)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch members"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}

// GetOrganizationRegistry godoc
// @Summary      Get organization registry
// @Description  Get the internal skill registry for an organization
// @Tags         organizations
// @Produce      json
// @Param        id   path      string  true  "Organization ID"
// @Success      200  {object}  models.SkillRegistry
// @Failure      404  {object}  object{error=string}
// @Router       /organizations/{id}/registry [get]
func GetOrganizationRegistry(c *gin.Context) {
	orgID := c.Param("id")
	db := database.GetDB()
	var registry models.SkillRegistry
	result := db.Where("org_id = ? AND source_type = 'internal'", orgID).First(&registry)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found for this organization"})
		return
	}
	c.JSON(http.StatusOK, registry)
}

// GetMyOrganizations godoc
// @Summary      Get my organizations
// @Description  Get all organizations the user belongs to
// @Tags         organizations
// @Produce      json
// @Param        userId  query     string  true  "User ID"
// @Success      200     {object}  object{organizations=[]models.Organization}
// @Failure      400     {object}  object{error=string}
// @Router       /organizations/my [get]
func GetMyOrganizations(c *gin.Context) {
	userID := c.Query("userId")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "userId is required"})
		return
	}
	db := database.GetDB()
	var members []models.OrgMember
	db.Where("user_id = ?", userID).Find(&members)
	orgIDs := make([]string, 0, len(members))
	for _, m := range members {
		orgIDs = append(orgIDs, m.OrgID)
	}
	var orgs []models.Organization
	if len(orgIDs) > 0 {
		db.Where("id IN ?", orgIDs).Find(&orgs)
	}
	c.JSON(http.StatusOK, gin.H{"organizations": orgs})
}

// ListRepositories godoc
// @Summary      List repositories
// @Description  Get all skill repositories
// @Tags         repositories
// @Produce      json
// @Success      200  {object}  object{repositories=[]models.SkillRepository}
// @Failure      500  {object}  object{error=string}
// @Router       /repositories [get]
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

// CreateRepository godoc
// @Summary      Create repository
// @Description  Create a new skill repository
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,visibility=string,ownerId=string,organizationId=string,groupId=string}  true  "Repository data"
// @Success      201   {object}  models.SkillRepository
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /repositories [post]
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

// GetRepository godoc
// @Summary      Get repository
// @Description  Get skill repository by ID
// @Tags         repositories
// @Produce      json
// @Param        id   path      string  true  "Repository ID"
// @Success      200  {object}  models.SkillRepository
// @Failure      404  {object}  object{error=string}
// @Router       /repositories/{id} [get]
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

// UpdateRepository godoc
// @Summary      Update repository
// @Description  Update skill repository by ID
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Repository ID"
// @Param        body  body      object{name=string,description=string,visibility=string}  false  "Repository data"
// @Success      200   {object}  models.SkillRepository
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /repositories/{id} [put]
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

// DeleteRepository godoc
// @Summary      Delete repository
// @Description  Delete skill repository by ID
// @Tags         repositories
// @Produce      json
// @Param        id   path      string  true  "Repository ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /repositories/{id} [delete]
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

// AddRepositoryMember godoc
// @Summary      Add repository member
// @Description  Add a user to a repository
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Repository ID"
// @Param        body  body      object{userId=string,role=string}  true  "Member data"
// @Success      200   {object}  object{message=string}
// @Failure      400   {object}  object{error=string}
// @Router       /repositories/{id}/members [post]
func AddRepositoryMember(c *gin.Context) {
	_ = c.Param("id")
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

// RemoveRepositoryMember godoc
// @Summary      Remove repository member
// @Description  Remove a user from a repository
// @Tags         repositories
// @Produce      json
// @Param        id      path      string  true  "Repository ID"
// @Param        userId  path      string  true  "User ID"
// @Success      200     {object}  object{message=string}
// @Router       /repositories/{id}/members/{userId} [delete]
func RemoveRepositoryMember(c *gin.Context) {
	_ = c.Param("id")
	_ = c.Param("userId")
	_ = database.GetDB()
	// TODO: Remove member from repository
	c.JSON(http.StatusOK, gin.H{"message": "Member removed"})
}

// ListSkills godoc
// @Summary      List skills
// @Description  Get all skills
// @Tags         skills
// @Produce      json
// @Success      200  {object}  object{skills=[]models.Skill}
// @Failure      500  {object}  object{error=string}
// @Router       /skills [get]
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

// CreateSkill godoc
// @Summary      Create skill
// @Description  Create a new skill
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,version=string,repoId=string,isPublic=boolean}  true  "Skill data"
// @Success      201   {object}  models.Skill
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /skills [post]
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

// GetSkill godoc
// @Summary      Get skill
// @Description  Get skill by ID
// @Tags         skills
// @Produce      json
// @Param        id   path      string  true  "Skill ID"
// @Success      200  {object}  models.Skill
// @Failure      404  {object}  object{error=string}
// @Router       /skills/{id} [get]
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

// UpdateSkill godoc
// @Summary      Update skill
// @Description  Update skill by ID
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Skill ID"
// @Param        body  body      object{name=string,description=string,version=string,isPublic=boolean}  false  "Skill data"
// @Success      200   {object}  models.Skill
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /skills/{id} [put]
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

// DeleteSkill godoc
// @Summary      Delete skill
// @Description  Delete skill by ID
// @Tags         skills
// @Produce      json
// @Param        id   path      string  true  "Skill ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /skills/{id} [delete]
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

// InstallSkill godoc
// @Summary      Install skill
// @Description  Increment install count for a skill
// @Tags         skills
// @Produce      json
// @Param        id   path      string  true  "Skill ID"
// @Success      200  {object}  object{message=string,installCount=integer}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /skills/{id}/install [post]
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

// RateSkill godoc
// @Summary      Rate skill
// @Description  Submit a rating for a skill
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Skill ID"
// @Param        body  body      object{rating=integer,comment=string}  true  "Rating data"
// @Success      200   {object}  object{message=string,rating=number}
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /skills/{id}/rating [post]
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

// ListAgents godoc
// @Summary      List agents
// @Description  Get all agents
// @Tags         agents
// @Produce      json
// @Success      200  {object}  object{agents=[]models.Agent}
// @Failure      500  {object}  object{error=string}
// @Router       /agents [get]
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

// CreateAgent godoc
// @Summary      Create agent
// @Description  Create a new agent
// @Tags         agents
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,version=string,repoId=string,isPublic=boolean}  true  "Agent data"
// @Success      201   {object}  models.Agent
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /agents [post]
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

// GetAgent godoc
// @Summary      Get agent
// @Description  Get agent by ID
// @Tags         agents
// @Produce      json
// @Param        id   path      string  true  "Agent ID"
// @Success      200  {object}  models.Agent
// @Failure      404  {object}  object{error=string}
// @Router       /agents/{id} [get]
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

// UpdateAgent godoc
// @Summary      Update agent
// @Description  Update agent by ID
// @Tags         agents
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Agent ID"
// @Param        body  body      object{name=string,description=string,version=string,isPublic=boolean}  false  "Agent data"
// @Success      200   {object}  models.Agent
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /agents/{id} [put]
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

// DeleteAgent godoc
// @Summary      Delete agent
// @Description  Delete agent by ID
// @Tags         agents
// @Produce      json
// @Param        id   path      string  true  "Agent ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /agents/{id} [delete]
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

// ListCommands godoc
// @Summary      List commands
// @Description  Get all commands
// @Tags         commands
// @Produce      json
// @Success      200  {object}  object{commands=[]models.Command}
// @Failure      500  {object}  object{error=string}
// @Router       /commands [get]
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

// CreateCommand godoc
// @Summary      Create command
// @Description  Create a new command
// @Tags         commands
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,repoId=string}  true  "Command data"
// @Success      201   {object}  models.Command
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /commands [post]
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

// GetCommand godoc
// @Summary      Get command
// @Description  Get command by ID
// @Tags         commands
// @Produce      json
// @Param        id   path      string  true  "Command ID"
// @Success      200  {object}  models.Command
// @Failure      404  {object}  object{error=string}
// @Router       /commands/{id} [get]
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

// UpdateCommand godoc
// @Summary      Update command
// @Description  Update command by ID
// @Tags         commands
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Command ID"
// @Param        body  body      object{name=string,description=string}  false  "Command data"
// @Success      200   {object}  models.Command
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /commands/{id} [put]
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

// DeleteCommand godoc
// @Summary      Delete command
// @Description  Delete command by ID
// @Tags         commands
// @Produce      json
// @Param        id   path      string  true  "Command ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /commands/{id} [delete]
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

// ListMCPServers godoc
// @Summary      List MCP servers
// @Description  Get all MCP servers
// @Tags         mcp-servers
// @Produce      json
// @Success      200  {object}  object{mcpServers=[]models.MCPServer}
// @Failure      500  {object}  object{error=string}
// @Router       /mcp-servers [get]
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

// CreateMCPServer godoc
// @Summary      Create MCP server
// @Description  Create a new MCP server
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        body  body      object{name=string,description=string,repoId=string}  true  "MCP server data"
// @Success      201   {object}  models.MCPServer
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /mcp-servers [post]
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

// GetMCPServer godoc
// @Summary      Get MCP server
// @Description  Get MCP server by ID
// @Tags         mcp-servers
// @Produce      json
// @Param        id   path      string  true  "MCP server ID"
// @Success      200  {object}  models.MCPServer
// @Failure      404  {object}  object{error=string}
// @Router       /mcp-servers/{id} [get]
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

// UpdateMCPServer godoc
// @Summary      Update MCP server
// @Description  Update MCP server by ID
// @Tags         mcp-servers
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "MCP server ID"
// @Param        body  body      object{name=string,description=string}  false  "MCP server data"
// @Success      200   {object}  models.MCPServer
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /mcp-servers/{id} [put]
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

// DeleteMCPServer godoc
// @Summary      Delete MCP server
// @Description  Delete MCP server by ID
// @Tags         mcp-servers
// @Produce      json
// @Param        id   path      string  true  "MCP server ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /mcp-servers/{id} [delete]
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

// ListMarketplaceSkills godoc
// @Summary      List marketplace skills
// @Description  Get all public skills available in the marketplace
// @Tags         marketplace
// @Produce      json
// @Success      200  {object}  object{skills=[]models.Skill}
// @Failure      500  {object}  object{error=string}
// @Router       /marketplace/skills [get]
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

// ListCategories godoc
// @Summary      List categories
// @Description  Get all skill categories
// @Tags         marketplace
// @Produce      json
// @Success      200  {object}  object{categories=[]string}
// @Router       /marketplace/categories [get]
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

// GetTrendingSkills godoc
// @Summary      Get trending skills
// @Description  Get top 10 most installed public skills
// @Tags         marketplace
// @Produce      json
// @Success      200  {object}  object{skills=[]models.Skill}
// @Failure      500  {object}  object{error=string}
// @Router       /marketplace/skills/trending [get]
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
