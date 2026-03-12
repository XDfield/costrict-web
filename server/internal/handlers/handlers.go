package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

func buildSyncConfigJSON(includes, excludes []string, conflictStrategy, webhookSecret string) datatypes.JSON {
	cfg := map[string]any{
		"includePatterns":  includes,
		"excludePatterns":  excludes,
		"conflictStrategy": conflictStrategy,
		"webhookSecret":    webhookSecret,
	}
	b, _ := json.Marshal(cfg)
	return datatypes.JSON(b)
}

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

// CreateSyncRegistryInput holds sync configuration for creating a sync-type organization
type CreateSyncRegistryInput struct {
	ExternalURL      string   `json:"externalUrl" binding:"required"`
	ExternalBranch   string   `json:"externalBranch"`
	SyncInterval     int      `json:"syncInterval"`
	SyncEnabled      bool     `json:"syncEnabled"`
	IncludePatterns  []string `json:"includePatterns"`
	ExcludePatterns  []string `json:"excludePatterns"`
	ConflictStrategy string   `json:"conflictStrategy"`
	WebhookSecret    string   `json:"webhookSecret"`
}

// CreateOrganization godoc
// @Summary      Create organization
// @Description  Create a new organization. Set orgType=sync to create a Git-synced organization.
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        body  body  object{name=string,displayName=string,description=string,visibility=string,ownerId=string,orgType=string}  true  "Organization data"
// @Success      201  {object}  models.Organization
// @Failure      400  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /organizations [post]
func CreateOrganization(c *gin.Context) {
	var req struct {
		Name         string                   `json:"name" binding:"required"`
		DisplayName  string                   `json:"displayName"`
		Description  string                   `json:"description"`
		Visibility   string                   `json:"visibility"`
		OwnerID      string                   `json:"ownerId" binding:"required"`
		OrgType      string                   `json:"orgType"`
		SyncRegistry *CreateSyncRegistryInput `json:"syncRegistry"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	orgType := req.OrgType
	if orgType == "" {
		orgType = "normal"
	}

	if orgType == "sync" {
		if req.SyncRegistry == nil || req.SyncRegistry.ExternalURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "syncRegistry.externalUrl is required for sync organizations"})
			return
		}
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
		OrgType:     orgType,
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

	if orgType == "sync" && req.SyncRegistry != nil {
		sr := req.SyncRegistry
		branch := sr.ExternalBranch
		if branch == "" {
			branch = "main"
		}
		interval := sr.SyncInterval
		if interval <= 0 {
			interval = 3600
		}
		conflictStrategy := sr.ConflictStrategy
		if conflictStrategy == "" {
			conflictStrategy = "keep_remote"
		}

		syncConfigJSON := buildSyncConfigJSON(sr.IncludePatterns, sr.ExcludePatterns, conflictStrategy, sr.WebhookSecret)

	orgRegistry := models.CapabilityRegistry{
		ID:             uuid.New().String(),
		Name:           org.Name,
		Description:    "Sync registry for organization " + org.Name,
		SourceType:     "external",
		ExternalURL:    sr.ExternalURL,
		ExternalBranch: branch,
		SyncEnabled:    sr.SyncEnabled,
		SyncInterval:   interval,
		SyncStatus:     "idle",
		SyncConfig:     syncConfigJSON,
		Visibility:     visibility,
		OrgID:          org.ID,
		OwnerID:        req.OwnerID,
	}
		db.Create(&orgRegistry)

		if SyncScheduler != nil && sr.SyncEnabled {
			_ = SyncScheduler.RegisterRegistry(&orgRegistry)
		}

		c.JSON(http.StatusCreated, gin.H{"organization": org, "registry": orgRegistry})
		return
	}

	orgRegistry := models.CapabilityRegistry{
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
// @Description  Get the internal capability registry for an organization
// @Tags         organizations
// @Produce      json
// @Param        id   path      string  true  "Organization ID"
// @Success      200  {object}  models.CapabilityRegistry
// @Failure      404  {object}  object{error=string}
// @Router       /organizations/{id}/registry [get]
func GetOrganizationRegistry(c *gin.Context) {
	orgID := c.Param("id")
	db := database.GetDB()
	var registry models.CapabilityRegistry
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