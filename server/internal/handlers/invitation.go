package handlers

import (
	"net/http"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const invitationTTL = 7 * 24 * time.Hour

// CreateRepoInvitation godoc
// @Summary      Invite a user to a repository
// @Description  Send an invitation to a user to join the repository (requires owner or admin role)
// @Tags         repositories
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Repository ID"
// @Param        body  body      object{inviteeUsername=string,inviteeId=string,role=string}  true  "Invitation data"
// @Success      201   {object}  models.RepoInvitation
// @Failure      400   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /repositories/{id}/invitations [post]
func CreateRepoInvitation(c *gin.Context) {
	repoID := c.Param("id")

	callerRole := getCallerRepoRole(c, repoID)
	if !isRepoAdmin(callerRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only repo owner or admin can invite members"})
		return
	}

	callerIDVal, _ := c.Get("userId")
	callerID, _ := callerIDVal.(string)
	callerName, _ := c.Get("userName")
	callerUsername, _ := callerName.(string)

	var req struct {
		InviteeUsername string `json:"inviteeUsername" binding:"required"`
		InviteeID       string `json:"inviteeId"`
		Role            string `json:"role"`
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot invite with owner role"})
		return
	}
	if role != "admin" && role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be admin or member"})
		return
	}

	db := database.GetDB()

	if req.InviteeID != "" {
		var existing models.RepoMember
		if db.Where("repo_id = ? AND user_id = ?", repoID, req.InviteeID).First(&existing).Error == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "User is already a member of this repository"})
			return
		}
	}

	var pending models.RepoInvitation
	query := db.Where("repo_id = ? AND invitee_username = ? AND status = ?", repoID, req.InviteeUsername, "pending")
	if query.First(&pending).Error == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "A pending invitation already exists for this user"})
		return
	}

	invitation := models.RepoInvitation{
		ID:              uuid.New().String(),
		RepoID:          repoID,
		InviterID:       callerID,
		InviterUsername: callerUsername,
		InviteeID:       req.InviteeID,
		InviteeUsername: req.InviteeUsername,
		Role:            role,
		Status:          "pending",
		ExpiresAt:       time.Now().Add(invitationTTL),
	}

	if err := db.Create(&invitation).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invitation"})
		return
	}

	db.Preload("Repository").First(&invitation, "id = ?", invitation.ID)

	c.JSON(http.StatusCreated, invitation)
}

// ListRepoInvitations godoc
// @Summary      List repository invitations
// @Description  Get all invitations for a repository (requires owner or admin role)
// @Tags         repositories
// @Produce      json
// @Param        id      path      string  true  "Repository ID"
// @Param        status  query     string  false "Filter by status (pending|accepted|declined|cancelled)"
// @Success      200     {object}  object{invitations=[]models.RepoInvitation}
// @Failure      403     {object}  object{error=string}
// @Router       /repositories/{id}/invitations [get]
func ListRepoInvitations(c *gin.Context) {
	repoID := c.Param("id")

	if !isRepoAdmin(getCallerRepoRole(c, repoID)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only repo owner or admin can view invitations"})
		return
	}

	db := database.GetDB()
	query := db.Where("repo_id = ?", repoID)
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	var invitations []models.RepoInvitation
	query.Order("created_at DESC").Find(&invitations)

	c.JSON(http.StatusOK, gin.H{"invitations": invitations})
}

// CancelRepoInvitation godoc
// @Summary      Cancel a repository invitation
// @Description  Cancel a pending invitation (requires owner or admin role)
// @Tags         repositories
// @Produce      json
// @Param        id     path      string  true  "Repository ID"
// @Param        invId  path      string  true  "Invitation ID"
// @Success      200    {object}  object{message=string}
// @Failure      403    {object}  object{error=string}
// @Failure      404    {object}  object{error=string}
// @Router       /repositories/{id}/invitations/{invId} [delete]
func CancelRepoInvitation(c *gin.Context) {
	repoID := c.Param("id")
	invID := c.Param("invId")

	if !isRepoAdmin(getCallerRepoRole(c, repoID)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only repo owner or admin can cancel invitations"})
		return
	}

	db := database.GetDB()
	var inv models.RepoInvitation
	if db.Where("id = ? AND repo_id = ?", invID, repoID).First(&inv).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invitation not found"})
		return
	}
	if inv.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Only pending invitations can be cancelled"})
		return
	}

	inv.Status = "cancelled"
	if err := db.Save(&inv).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to cancel invitation"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invitation cancelled"})
}

// GetMyInvitations godoc
// @Summary      Get my invitations
// @Description  Get all pending invitations for the current user
// @Tags         invitations
// @Produce      json
// @Success      200  {object}  object{invitations=[]models.RepoInvitation}
// @Failure      401  {object}  object{error=string}
// @Router       /invitations/my [get]
func GetMyInvitations(c *gin.Context) {
	callerIDVal, exists := c.Get("userId")
	if !exists || callerIDVal == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	callerID := callerIDVal.(string)
	callerName, _ := c.Get("userName")
	callerUsername, _ := callerName.(string)

	db := database.GetDB()

	now := time.Now()
	db.Model(&models.RepoInvitation{}).
		Where("status = ? AND expires_at < ?", "pending", now).
		Update("status", "cancelled")

	var invitations []models.RepoInvitation
	query := db.Where("status = ?", "pending")
	if callerID != "" && callerUsername != "" {
		query = query.Where("invitee_id = ? OR invitee_username = ?", callerID, callerUsername)
	} else if callerID != "" {
		query = query.Where("invitee_id = ?", callerID)
	} else {
		query = query.Where("invitee_username = ?", callerUsername)
	}

	query.Preload("Repository").Order("created_at DESC").Find(&invitations)

	c.JSON(http.StatusOK, gin.H{"invitations": invitations})
}

// AcceptInvitation godoc
// @Summary      Accept an invitation
// @Description  Accept a pending invitation to join a repository
// @Tags         invitations
// @Produce      json
// @Param        id  path      string  true  "Invitation ID"
// @Success      200 {object}  models.RepoMember
// @Failure      401 {object}  object{error=string}
// @Failure      404 {object}  object{error=string}
// @Failure      409 {object}  object{error=string}
// @Router       /invitations/{id}/accept [post]
func AcceptInvitation(c *gin.Context) {
	invID := c.Param("id")

	callerIDVal, exists := c.Get("userId")
	if !exists || callerIDVal == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	callerID := callerIDVal.(string)
	callerNameVal, _ := c.Get("userName")
	callerUsername, _ := callerNameVal.(string)

	db := database.GetDB()
	var inv models.RepoInvitation
	if db.First(&inv, "id = ?", invID).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invitation not found"})
		return
	}

	if inv.InviteeID != "" && inv.InviteeID != callerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "This invitation is not for you"})
		return
	}
	if inv.InviteeID == "" && inv.InviteeUsername != callerUsername {
		c.JSON(http.StatusForbidden, gin.H{"error": "This invitation is not for you"})
		return
	}

	if inv.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invitation is no longer pending"})
		return
	}
	if time.Now().After(inv.ExpiresAt) {
		inv.Status = "cancelled"
		db.Save(&inv)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invitation has expired"})
		return
	}

	var existing models.RepoMember
	if db.Where("repo_id = ? AND user_id = ?", inv.RepoID, callerID).First(&existing).Error == nil {
		inv.Status = "accepted"
		db.Save(&inv)
		c.JSON(http.StatusConflict, gin.H{"error": "You are already a member of this repository"})
		return
	}

	member := models.RepoMember{
		ID:       uuid.New().String(),
		RepoID:   inv.RepoID,
		UserID:   callerID,
		Username: callerUsername,
		Role:     inv.Role,
	}
	if err := db.Create(&member).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member"})
		return
	}

	inv.Status = "accepted"
	if inv.InviteeID == "" {
		inv.InviteeID = callerID
	}
	db.Save(&inv)

	c.JSON(http.StatusOK, member)
}

// DeclineInvitation godoc
// @Summary      Decline an invitation
// @Description  Decline a pending invitation to join a repository
// @Tags         invitations
// @Produce      json
// @Param        id  path      string  true  "Invitation ID"
// @Success      200 {object}  object{message=string}
// @Failure      401 {object}  object{error=string}
// @Failure      404 {object}  object{error=string}
// @Router       /invitations/{id}/decline [post]
func DeclineInvitation(c *gin.Context) {
	invID := c.Param("id")

	callerIDVal, exists := c.Get("userId")
	if !exists || callerIDVal == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	callerID := callerIDVal.(string)
	callerNameVal, _ := c.Get("userName")
	callerUsername, _ := callerNameVal.(string)

	db := database.GetDB()
	var inv models.RepoInvitation
	if db.First(&inv, "id = ?", invID).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invitation not found"})
		return
	}

	if inv.InviteeID != "" && inv.InviteeID != callerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "This invitation is not for you"})
		return
	}
	if inv.InviteeID == "" && inv.InviteeUsername != callerUsername {
		c.JSON(http.StatusForbidden, gin.H{"error": "This invitation is not for you"})
		return
	}

	if inv.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invitation is no longer pending"})
		return
	}

	inv.Status = "declined"
	if err := db.Save(&inv).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decline invitation"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invitation declined"})
}
