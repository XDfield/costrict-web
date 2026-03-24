package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var CasdoorClient *casdoor.CasdoorClient
var cookieSecure bool // whether to set Secure flag on auth cookies

func InitCasdoor(cfg *config.CasdoorConfig) {
	CasdoorClient = casdoor.NewClient(cfg)
}

// InitCookieConfig sets cookie-related configuration from the global config.
func InitCookieConfig(cfg *config.Config) {
	cookieSecure = cfg.CookieSecure
}

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

// AuthCallback godoc
// @Summary      OAuth callback
// @Description  Exchange OAuth authorization code for access token and set cookie
// @Tags         auth
// @Produce      json
// @Param        code          query  string  true  "OAuth code"
// @Param        redirect_uri  query  string  false "Redirect URI"
// @Success      200  {object}  object{token=string,user=object}
// @Failure      400  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /auth/callback [get]
func AuthCallback(c *gin.Context) {
	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code is required"})
		return
	}

	tokenResp, err := CasdoorClient.ExchangeCodeForToken(code)
	if err != nil || tokenResp.AccessToken == "" {
		fmt.Printf("[ERROR] ExchangeCodeForToken failed: err=%v, tokenResp=%+v\n", err, tokenResp)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to exchange code for token: %v", err)})
		return
	}

	userInfo, err := CasdoorClient.GetUserInfo(tokenResp.AccessToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user info"})
		return
	}

	c.SetCookie("auth_token", tokenResp.AccessToken, int(7*24*time.Hour/time.Second), "/", "", cookieSecure, true)

	// If state contains a redirect URL, redirect the user back
	redirectTo := c.Query("state")
	if redirectTo != "" {
		c.Redirect(http.StatusFound, redirectTo)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token": tokenResp.AccessToken,
		"user":  userInfo.User,
	})
}

// AuthLogin godoc
// @Summary      OAuth login redirect
// @Description  Redirect user to Casdoor login page for OAuth authorization
// @Tags         auth
// @Produce      html
// @Param        redirect_to   query  string  false "URL to redirect after login"
// @Param        callback_url  query  string  false "OAuth callback URL"
// @Success      302
// @Failure      400  {object}  object{error=string}
// @Router       /auth/login [get]
func AuthLogin(c *gin.Context) {
	redirectTo := c.Query("redirect_to")
	callbackURL := c.Query("callback_url")

	// Use redirect_to as state so AuthCallback can redirect back after login
	state := redirectTo

	var loginURL string
	if callbackURL != "" {
		loginURL = CasdoorClient.GetLoginURLWithCallback(state, callbackURL)
	} else {
		loginURL = CasdoorClient.GetLoginURL(state)
	}

	c.Redirect(http.StatusFound, loginURL)
}

// Login godoc
// @Summary      OAuth login (legacy)
// @Description  Exchange OAuth authorization code for access token via JSON body
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body  object{code=string,state=string}  true  "OAuth code"
// @Success      200   {object}  object{token=string,tokenType=string,user=object}
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /auth/login [post]
func Login(c *gin.Context) {
	var req struct {
		Code  string `json:"code" binding:"required"`
		State string `json:"state"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	tokenResp, err := CasdoorClient.ExchangeCodeForToken(req.Code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to exchange code for token"})
		return
	}

	userInfo, err := CasdoorClient.GetUserInfo(tokenResp.AccessToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user info"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":     tokenResp.AccessToken,
		"tokenType": tokenResp.TokenType,
		"user":      userInfo.User,
	})
}

// Logout godoc
// @Summary      Logout
// @Description  Invalidate current session, revoke token at Casdoor, and clear auth cookie
// @Tags         auth
// @Produce      json
// @Success      200  {object}  object{message=string}
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /auth/logout [post]
func Logout(c *gin.Context) {
	// Extract token from Authorization header or cookie
	token := middleware.ExtractToken(c)

	// Always clear the auth cookie, regardless of whether token revocation succeeds
	c.SetCookie("auth_token", "", -1, "/", "", cookieSecure, true)

	if token == "" {
		// No token present — cookie cleared, nothing else to do
		c.JSON(http.StatusOK, gin.H{"message": "Logout successful"})
		return
	}

	// Revoke token at Casdoor (logout all sessions for this user)
	if err := CasdoorClient.Logout(token, true); err != nil {
		// Log the error but don't fail the logout — cookie is already cleared
		fmt.Printf("[Logout] Casdoor token revocation failed: %v\n", err)
	}

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
	token, exists := c.Get("accessToken")
	if !exists || token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	userInfo, err := CasdoorClient.GetUserInfo(token.(string))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"user": userInfo.User})
}

// ListRepositories godoc
// @Summary      List repositories
// @Description  Get all repositories
// @Tags         repositories
// @Produce      json
// @Success      200  {object}  object{repositories=[]models.Repository}
// @Failure      500  {object}  object{error=string}
// @Router       /repositories [get]
func ListRepositories(c *gin.Context) {
	db := database.GetDB()
	var repos []models.Repository
	result := db.Find(&repos)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch repositories"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"repositories": repos})
}

// CreateSyncRegistryInput holds sync configuration for creating a sync-type repository
type CreateSyncRegistryInput struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	ExternalURL      string   `json:"externalUrl"`
	ExternalBranch   string   `json:"externalBranch"`
	SyncInterval     int      `json:"syncInterval"`
	SyncEnabled      bool     `json:"syncEnabled"`
	IncludePatterns  []string `json:"includePatterns"`
	ExcludePatterns  []string `json:"excludePatterns"`
	ConflictStrategy string   `json:"conflictStrategy"`
	WebhookSecret    string   `json:"webhookSecret"`
}

// CreateRepository godoc
// @Summary      Create repository
// @Description  Create a new repository. Set repoType=sync to create a Git-synced repository.
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        body  body  object{name=string,displayName=string,description=string,visibility=string,ownerId=string,repoType=string,syncRegistry=object,syncRegistries=[]object}  true  "Repository data"
// @Success      201  {object}  models.Repository
// @Failure      400  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /repositories [post]
func CreateRepository(c *gin.Context) {
	var req struct {
		Name             string                    `json:"name" binding:"required"`
		DisplayName      string                    `json:"displayName"`
		Description      string                    `json:"description"`
		Visibility       string                    `json:"visibility"`
		OwnerID          string                    `json:"ownerId" binding:"required"`
		RepoType         string                    `json:"repoType"`
		SyncRegistry     *CreateSyncRegistryInput  `json:"syncRegistry"`
		SyncRegistries   []CreateSyncRegistryInput `json:"syncRegistries"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	repoType := req.RepoType
	if repoType == "" {
		repoType = "normal"
	}

	if req.SyncRegistry != nil && req.SyncRegistry.ExternalURL != "" {
		req.SyncRegistries = append([]CreateSyncRegistryInput{*req.SyncRegistry}, req.SyncRegistries...)
	}

	if repoType == "sync" && len(req.SyncRegistries) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one syncRegistry is required for sync repositories"})
		return
	}

	visibility := req.Visibility
	if visibility == "" {
		visibility = "private"
	}

	repo := models.Repository{
		ID:          uuid.New().String(),
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Visibility:  visibility,
		RepoType:    repoType,
		OwnerID:     req.OwnerID,
	}

	db := database.GetDB()
	if result := db.Create(&repo); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create repository"})
		return
	}

	ownerMember := models.RepoMember{
		ID:     uuid.New().String(),
		RepoID: repo.ID,
		UserID: req.OwnerID,
		Role:   "owner",
	}
	db.Create(&ownerMember)

	if repoType == "sync" {
		var createdRegistries []models.CapabilityRegistry
		for _, sr := range req.SyncRegistries {
			reg := buildExternalRegistry(sr, repo.ID, req.OwnerID, visibility)
			db.Create(&reg)
			if SyncScheduler != nil && sr.SyncEnabled {
				_ = SyncScheduler.RegisterRegistry(&reg)
			}
			createdRegistries = append(createdRegistries, reg)
		}
		c.JSON(http.StatusCreated, gin.H{"repository": repo, "registries": createdRegistries})
		return
	}

	repoRegistry := models.CapabilityRegistry{
		ID:          uuid.New().String(),
		Name:        repo.Name,
		Description: "Registry for repository " + repo.Name,
		SourceType:  "internal",
		Visibility:  visibility,
		RepoID:      repo.ID,
		OwnerID:     req.OwnerID,
	}
	db.Create(&repoRegistry)

	c.JSON(http.StatusCreated, repo)
}

func buildExternalRegistry(sr CreateSyncRegistryInput, repoID, ownerID, visibility string) models.CapabilityRegistry {
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
	name := sr.Name
	if name == "" {
		name = sr.ExternalURL
	}
	return models.CapabilityRegistry{
		ID:             uuid.New().String(),
		Name:           name,
		Description:    sr.Description,
		SourceType:     "external",
		ExternalURL:    sr.ExternalURL,
		ExternalBranch: branch,
		SyncEnabled:    sr.SyncEnabled,
		SyncInterval:   interval,
		SyncStatus:     "idle",
		SyncConfig:     buildSyncConfigJSON(sr.IncludePatterns, sr.ExcludePatterns, conflictStrategy, sr.WebhookSecret),
		Visibility:     visibility,
		RepoID:         repoID,
		OwnerID:        ownerID,
	}
}

// GetRepository godoc
// @Summary      Get repository
// @Description  Get repository by ID
// @Tags         repositories
// @Produce      json
// @Param        id   path      string  true  "Repository ID"
// @Success      200  {object}  models.Repository
// @Failure      404  {object}  object{error=string}
// @Router       /repositories/{id} [get]
func GetRepository(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var repo models.Repository
	result := db.First(&repo, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return
	}

	c.JSON(http.StatusOK, repo)
}

// UpdateRepository godoc
// @Summary      Update repository
// @Description  Update repository by ID
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Repository ID"
// @Param        body  body      object{name=string,displayName=string,description=string,visibility=string}  false  "Repository data"
// @Success      200   {object}  models.Repository
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /repositories/{id} [put]
func UpdateRepository(c *gin.Context) {
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
	var repo models.Repository
	result := db.First(&repo, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return
	}

	if req.Name != "" {
		repo.Name = req.Name
	}
	if req.DisplayName != "" {
		repo.DisplayName = req.DisplayName
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
// @Description  Delete repository by ID and all associated resources (registries, items, versions, assets, artifacts)
// @Tags         repositories
// @Produce      json
// @Param        id   path      string  true  "Repository ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /repositories/{id} [delete]
func DeleteRepository(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()

	err := db.Transaction(func(tx *gorm.DB) error {
		// 1. 获取该仓库下的所有 registry IDs
		var registryIDs []string
		if err := tx.Model(&models.CapabilityRegistry{}).Where("repo_id = ?", id).Pluck("id", &registryIDs).Error; err != nil {
			return fmt.Errorf("failed to get registry IDs: %w", err)
		}

		// 2. 如果有 registry，获取所有 item IDs
		var itemIDs []string
		if len(registryIDs) > 0 {
			if err := tx.Model(&models.CapabilityItem{}).Where("registry_id IN ?", registryIDs).Pluck("id", &itemIDs).Error; err != nil {
				return fmt.Errorf("failed to get item IDs: %w", err)
			}
		}

		// 3. 删除依赖 Item 的记录（按照依赖关系顺序）
		if len(itemIDs) > 0 {
			// 删除扫描任务
			if err := tx.Where("item_id IN ?", itemIDs).Delete(&models.ScanJob{}).Error; err != nil {
				return fmt.Errorf("failed to delete scan jobs: %w", err)
			}

			// 删除安全扫描结果
			if err := tx.Where("item_id IN ?", itemIDs).Delete(&models.SecurityScan{}).Error; err != nil {
				return fmt.Errorf("failed to delete security scans: %w", err)
			}

			// 删除资源文件 (CapabilityAsset)
			if err := tx.Where("item_id IN ?", itemIDs).Delete(&models.CapabilityAsset{}).Error; err != nil {
				return fmt.Errorf("failed to delete capability assets: %w", err)
			}

			// 删除构建产物 (CapabilityArtifact)
			if err := tx.Where("item_id IN ?", itemIDs).Delete(&models.CapabilityArtifact{}).Error; err != nil {
				return fmt.Errorf("failed to delete capability artifacts: %w", err)
			}

			// 删除版本记录 (CapabilityVersion)
			// 虽然有级联删除，但显式删除更保险
			if err := tx.Where("item_id IN ?", itemIDs).Delete(&models.CapabilityVersion{}).Error; err != nil {
				return fmt.Errorf("failed to delete capability versions: %w", err)
			}

			// 删除 Capability Items
			if err := tx.Where("id IN ?", itemIDs).Delete(&models.CapabilityItem{}).Error; err != nil {
				return fmt.Errorf("failed to delete capability items: %w", err)
			}
		}

		// 4. 删除依赖 Registry 的记录
		if len(registryIDs) > 0 {
			// 删除同步任务 (SyncJob)
			if err := tx.Where("registry_id IN ?", registryIDs).Delete(&models.SyncJob{}).Error; err != nil {
				return fmt.Errorf("failed to delete sync jobs: %w", err)
			}

			// 删除同步日志 (SyncLog)
			if err := tx.Where("registry_id IN ?", registryIDs).Delete(&models.SyncLog{}).Error; err != nil {
				return fmt.Errorf("failed to delete sync logs: %w", err)
			}

			// 删除 Capability Registries
			if err := tx.Where("id IN ?", registryIDs).Delete(&models.CapabilityRegistry{}).Error; err != nil {
				return fmt.Errorf("failed to delete capability registries: %w", err)
			}
		}

		// 5. 删除仓库成员
		if err := tx.Where("repo_id = ?", id).Delete(&models.RepoMember{}).Error; err != nil {
			return fmt.Errorf("failed to delete repository members: %w", err)
		}

		// 6. 最后删除仓库本身
		if err := tx.Delete(&models.Repository{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("failed to delete repository: %w", err)
		}

		return nil
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Repository and all associated resources deleted"})
}

func getCallerRepoRole(c *gin.Context, repoID string) string {
	callerID, _ := c.Get("userId")
	if callerID == nil {
		return ""
	}
	db := database.GetDB()
	var m models.RepoMember
	if db.Where("repo_id = ? AND user_id = ?", repoID, callerID.(string)).First(&m).Error != nil {
		return ""
	}
	return m.Role
}

func isRepoAdmin(role string) bool {
	return role == "owner" || role == "admin"
}

// AddRepositoryMember godoc
// @Summary      Add repository member
// @Description  Add a user to a repository (requires owner or admin role)
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Repository ID"
// @Param        body  body      object{userId=string,username=string,role=string}  true  "Member data"
// @Success      201   {object}  models.RepoMember
// @Failure      400   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /repositories/{id}/members [post]
func AddRepositoryMember(c *gin.Context) {
	repoID := c.Param("id")

	if !isRepoAdmin(getCallerRepoRole(c, repoID)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only repo owner or admin can add members"})
		return
	}

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
	if role == "owner" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot assign owner role directly"})
		return
	}

	db := database.GetDB()
	var existing models.RepoMember
	if db.Where("repo_id = ? AND user_id = ?", repoID, req.UserID).First(&existing).Error == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a member"})
		return
	}

	member := models.RepoMember{
		ID:     uuid.New().String(),
		RepoID: repoID,
		UserID: req.UserID,
		Username: req.Username,
		Role:   role,
	}

	if result := db.Create(&member); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	c.JSON(http.StatusCreated, member)
}

// UpdateRepositoryMember godoc
// @Summary      Update repository member role
// @Description  Update a member's role in a repository (requires owner or admin role)
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id      path      string  true  "Repository ID"
// @Param        userId  path      string  true  "User ID"
// @Param        body    body      object{role=string}  true  "Role data"
// @Success      200     {object}  models.RepoMember
// @Failure      400     {object}  object{error=string}
// @Failure      403     {object}  object{error=string}
// @Failure      404     {object}  object{error=string}
// @Failure      500     {object}  object{error=string}
// @Router       /repositories/{id}/members/{userId} [put]
func UpdateRepositoryMember(c *gin.Context) {
	repoID := c.Param("id")
	targetUserID := c.Param("userId")

	if !isRepoAdmin(getCallerRepoRole(c, repoID)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only repo owner or admin can update member roles"})
		return
	}

	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if req.Role == "owner" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot assign owner role"})
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be admin or member"})
		return
	}

	db := database.GetDB()
	var member models.RepoMember
	if db.Where("repo_id = ? AND user_id = ?", repoID, targetUserID).First(&member).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Member not found"})
		return
	}
	if member.Role == "owner" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot change owner's role"})
		return
	}

	member.Role = req.Role
	if err := db.Save(&member).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update member role"})
		return
	}

	c.JSON(http.StatusOK, member)
}

// RemoveRepositoryMember godoc
// @Summary      Remove repository member
// @Description  Remove a user from a repository (requires owner or admin role)
// @Tags         repositories
// @Produce      json
// @Param        id      path      string  true  "Repository ID"
// @Param        userId  path      string  true  "User ID"
// @Success      200     {object}  object{message=string}
// @Failure      403     {object}  object{error=string}
// @Failure      500     {object}  object{error=string}
// @Router       /repositories/{id}/members/{userId} [delete]
func RemoveRepositoryMember(c *gin.Context) {
	repoID := c.Param("id")
	targetUserID := c.Param("userId")

	callerID, _ := c.Get("userId")
	callerRole := getCallerRepoRole(c, repoID)

	if callerID != nil && callerID.(string) == targetUserID {
		db := database.GetDB()
		var m models.RepoMember
		if db.Where("repo_id = ? AND user_id = ?", repoID, targetUserID).First(&m).Error == nil && m.Role == "owner" {
			c.JSON(http.StatusForbidden, gin.H{"error": "Owner cannot leave the repository"})
			return
		}
	} else if !isRepoAdmin(callerRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only repo owner or admin can remove members"})
		return
	}

	db := database.GetDB()
	result := db.Where("repo_id = ? AND user_id = ?", repoID, targetUserID).Delete(&models.RepoMember{})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Member removed"})
}

// ListRepositoryMembers godoc
// @Summary      List repository members
// @Description  Get all members of a repository
// @Tags         repositories
// @Produce      json
// @Param        id   path      string  true  "Repository ID"
// @Success      200  {object}  object{members=[]models.RepoMember}
// @Failure      500  {object}  object{error=string}
// @Router       /repositories/{id}/members [get]
func ListRepositoryMembers(c *gin.Context) {
	repoID := c.Param("id")
	db := database.GetDB()
	var members []models.RepoMember
	result := db.Where("repo_id = ?", repoID).Find(&members)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch members"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}

// GetRepositoryRegistry godoc
// @Summary      Get repository registry
// @Description  Get the internal capability registry for a repository
// @Tags         repositories
// @Produce      json
// @Param        id   path      string  true  "Repository ID"
// @Success      200  {object}  models.CapabilityRegistry
// @Failure      404  {object}  object{error=string}
// @Router       /repositories/{id}/registry [get]
func GetRepositoryRegistry(c *gin.Context) {
	repoID := c.Param("id")
	db := database.GetDB()
	var registry models.CapabilityRegistry
	result := db.Where("repo_id = ?", repoID).Order("CASE source_type WHEN 'external' THEN 0 ELSE 1 END").First(&registry)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found for this repository"})
		return
	}
	c.JSON(http.StatusOK, registry)
}

// ListRepoRegistries godoc
// @Summary      List repository registries
// @Description  List all capability registries belonging to a repository
// @Tags         repositories
// @Produce      json
// @Param        id  path  string  true  "Repository ID"
// @Success      200  {object}  object{registries=[]models.CapabilityRegistry}
// @Router       /repositories/{id}/registries [get]
func ListRepoRegistries(c *gin.Context) {
	repoID := c.Param("id")
	db := database.GetDB()
	var registries []models.CapabilityRegistry
	db.Where("repo_id = ?", repoID).Order("created_at ASC").Find(&registries)
	c.JSON(http.StatusOK, gin.H{"registries": registries})
}

// AddRepoRegistry godoc
// @Summary      Add registry to repository
// @Description  Bind a new Git sync registry to a repository
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id    path  string                  true  "Repository ID"
// @Param        body  body  CreateSyncRegistryInput true  "Registry config"
// @Success      201  {object}  models.CapabilityRegistry
// @Failure      400  {object}  object{error=string}
// @Router       /repositories/{id}/registries [post]
func AddRepoRegistry(c *gin.Context) {
	repoID := c.Param("id")
	var req CreateSyncRegistryInput
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if req.ExternalURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "externalUrl is required"})
		return
	}

	db := database.GetDB()
	var repo models.Repository
	if db.First(&repo, "id = ?", repoID).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Repository not found"})
		return
	}

	userIDVal, _ := c.Get("userID")
	ownerID, _ := userIDVal.(string)
	if ownerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	reg := buildExternalRegistry(req, repoID, ownerID, repo.Visibility)
	if err := db.Create(&reg).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create registry"})
		return
	}
	if SyncScheduler != nil && req.SyncEnabled {
		_ = SyncScheduler.RegisterRegistry(&reg)
	}
	c.JSON(http.StatusCreated, reg)
}

// UpdateRepoRegistry godoc
// @Summary      Update repository registry
// @Description  Update sync configuration of a registry belonging to a repository
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id     path  string  true  "Repository ID"
// @Param        regId  path  string  true  "Registry ID"
// @Param        body   body  object{name=string,description=string,externalUrl=string,externalBranch=string,syncEnabled=boolean,syncInterval=integer,includePatterns=[]string,excludePatterns=[]string,conflictStrategy=string,webhookSecret=string}  false  "Registry update data"
// @Success      200  {object}  models.CapabilityRegistry
// @Failure      404  {object}  object{error=string}
// @Router       /repositories/{id}/registries/{regId} [put]
func UpdateRepoRegistry(c *gin.Context) {
	repoID := c.Param("id")
	regID := c.Param("regId")
	db := database.GetDB()

	var reg models.CapabilityRegistry
	if db.Where("id = ? AND repo_id = ?", regID, repoID).First(&reg).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}

	var req struct {
		Name             string   `json:"name"`
		Description      string   `json:"description"`
		ExternalURL      string   `json:"externalUrl"`
		ExternalBranch   string   `json:"externalBranch"`
		SyncEnabled      *bool    `json:"syncEnabled"`
		SyncInterval     int      `json:"syncInterval"`
		IncludePatterns  []string `json:"includePatterns"`
		ExcludePatterns  []string `json:"excludePatterns"`
		ConflictStrategy string   `json:"conflictStrategy"`
		WebhookSecret    string   `json:"webhookSecret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	if req.Name != "" {
		reg.Name = req.Name
	}
	if req.Description != "" {
		reg.Description = req.Description
	}
	if req.ExternalURL != "" {
		reg.ExternalURL = req.ExternalURL
	}
	if req.ExternalBranch != "" {
		reg.ExternalBranch = req.ExternalBranch
	}
	if req.SyncEnabled != nil {
		reg.SyncEnabled = *req.SyncEnabled
	}
	if req.SyncInterval > 0 {
		reg.SyncInterval = req.SyncInterval
	}
	if req.IncludePatterns != nil || req.ExcludePatterns != nil || req.ConflictStrategy != "" {
		cs := req.ConflictStrategy
		if cs == "" {
			cs = "keep_remote"
		}
		reg.SyncConfig = buildSyncConfigJSON(req.IncludePatterns, req.ExcludePatterns, cs, req.WebhookSecret)
	}

	if err := db.Save(&reg).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update registry"})
		return
	}
	if SyncScheduler != nil {
		if reg.SyncEnabled {
			_ = SyncScheduler.RegisterRegistry(&reg)
		} else {
			SyncScheduler.UnregisterRegistry(reg.ID)
		}
	}
	c.JSON(http.StatusOK, reg)
}

// RemoveRepoRegistry godoc
// @Summary      Remove registry from repository
// @Description  Delete a sync registry from a repository
// @Tags         repositories
// @Produce      json
// @Param        id     path  string  true  "Repository ID"
// @Param        regId  path  string  true  "Registry ID"
// @Success      200  {object}  object{message=string}
// @Failure      404  {object}  object{error=string}
// @Router       /repositories/{id}/registries/{regId} [delete]
func RemoveRepoRegistry(c *gin.Context) {
	repoID := c.Param("id")
	regID := c.Param("regId")
	db := database.GetDB()

	var reg models.CapabilityRegistry
	if db.Where("id = ? AND repo_id = ?", regID, repoID).First(&reg).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Registry not found"})
		return
	}

	if SyncScheduler != nil {
		SyncScheduler.UnregisterRegistry(reg.ID)
	}

	if err := db.Model(&models.CapabilityRegistry{}).Where("id = ?", reg.ID).Update("last_sync_log_id", nil).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear last sync log reference"})
		return
	}

	if err := db.Where("registry_id = ?", reg.ID).Delete(&models.SyncLog{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete sync logs"})
		return
	}

	if err := db.Where("registry_id = ?", reg.ID).Delete(&models.SyncJob{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete sync jobs"})
		return
	}

	db.Delete(&reg)
	c.JSON(http.StatusOK, gin.H{"message": "Registry removed"})
}

// GetMyRepositories godoc
// @Summary      Get my repositories
// @Description  Get all repositories the current authenticated user belongs to
// @Tags         repositories
// @Produce      json
// @Success      200     {object}  object{repositories=[]models.Repository}
// @Failure      401     {object}  object{error=string}
// @Router       /repositories/my [get]
func GetMyRepositories(c *gin.Context) {
	userIDVal, exists := c.Get("userId")
	if !exists || userIDVal == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	userID, ok := userIDVal.(string)
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	db := database.GetDB()
	var members []models.RepoMember
	db.Where("user_id = ?", userID).Find(&members)
	repoIDs := make([]string, 0, len(members))
	for _, m := range members {
		repoIDs = append(repoIDs, m.RepoID)
	}
	var repos []models.Repository
	if len(repoIDs) > 0 {
		db.Where("id IN ?", repoIDs).Find(&repos)
	}
	c.JSON(http.StatusOK, gin.H{"repositories": repos})
}
