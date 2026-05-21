package handlers

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// DistributionHandler holds dependencies for distribution endpoints.
type DistributionHandler struct {
	db              *gorm.DB
	distSvc         *services.DistributionService
	systemRoleSvc   *systemrole.SystemRoleService
}

// NewDistributionHandler creates a new distribution handler.
func NewDistributionHandler(db *gorm.DB, distSvc *services.DistributionService) *DistributionHandler {
	return &DistributionHandler{
		db:            db,
		distSvc:       distSvc,
		systemRoleSvc: systemrole.NewSystemRoleService(db),
	}
}

func (h *DistributionHandler) isPlatformAdmin(userID string) bool {
	if userID == "" {
		return false
	}
	hasRole, err := h.systemRoleSvc.HasRole(userID, systemrole.SystemRolePlatformAdmin)
	return err == nil && hasRole
}

func (h *DistributionHandler) loadItem(c *gin.Context, itemID string) (*models.CapabilityItem, bool) {
	var item models.CapabilityItem
	if err := h.db.Preload("Registry").First(&item, "id = ?", itemID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return nil, false
	}
	return &item, true
}

// DistributeItem godoc
// @Summary      Distribute item to targets
// @Description  Distribute an item to users or organizations with specified permission mode
// @Tags         distributions
// @Accept       json
// @Produce      json
// @Param        id     path      string                       true  "Item ID"
// @Param        body   body      services.DistributeItemRequest  true  "Distribution request"
// @Success      201    {object}  object{distributions=[]object}
// @Failure      400    {object}  object{error=string}
// @Failure      403    {object}  object{error=string}
// @Failure      404    {object}  object{error=string}
// @Router       /items/{id}/distribute [post]
func (h *DistributionHandler) DistributeItem(c *gin.Context) {
	itemID := c.Param("id")
	item, ok := h.loadItem(c, itemID)
	if !ok {
		return
	}

	userID := c.GetString(middleware.UserIDKey)
	if !h.distSvc.CanDistribute(item, userID, h.isPlatformAdmin(userID)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You do not have permission to distribute this item"})
		return
	}

	var req services.DistributeItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	results, err := h.distSvc.DistributeItem(c.Request.Context(), item, userID, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"distributions": results})
}

// ListItemDistributions godoc
// @Summary      List item distributions
// @Description  Get all distributions for a specific item
// @Tags         distributions
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{distributions=[]models.ItemDistribution}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/distributions [get]
func (h *DistributionHandler) ListItemDistributions(c *gin.Context) {
	itemID := c.Param("id")
	distributions, err := h.distSvc.ListItemDistributions(c.Request.Context(), itemID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"distributions": distributions})
}

// ListSentDistributions godoc
// @Summary      List my sent distributions
// @Description  Get all distributions sent by the current user
// @Tags         distributions
// @Produce      json
// @Success      200  {object}  object{distributions=[]models.ItemDistribution}
// @Failure      500  {object}  object{error=string}
// @Router       /distributions/my/sent [get]
func (h *DistributionHandler) ListSentDistributions(c *gin.Context) {
	userID := c.GetString(middleware.UserIDKey)
	distributions, err := h.distSvc.ListSentDistributions(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"distributions": distributions})
}

// ListReceivedDistributions godoc
// @Summary      List my received distributions
// @Description  Get all distributions received by the current user with item details
// @Tags         distributions
// @Produce      json
// @Success      200  {object}  object{receipts=[]models.ItemDistributionReceipt}
// @Failure      500  {object}  object{error=string}
// @Router       /distributions/my/received [get]
func (h *DistributionHandler) ListReceivedDistributions(c *gin.Context) {
	userID := c.GetString(middleware.UserIDKey)
	receipts, err := h.distSvc.ListReceivedDistributions(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"receipts": receipts})
}

// UpdateDistribution godoc
// @Summary      Update distribution
// @Description  Update a distribution's status, permission mode, or message
// @Tags         distributions
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Distribution ID"
// @Param        body  body      object{status=string,permissionMode=string,message=string}  false  "Update fields"
// @Success      200   {object}  models.ItemDistribution
// @Failure      400   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Router       /distributions/{id} [put]
func (h *DistributionHandler) UpdateDistribution(c *gin.Context) {
	distID := c.Param("id")
	userID := c.GetString(middleware.UserIDKey)

	var req struct {
		Status         *string `json:"status,omitempty"`
		PermissionMode *string `json:"permissionMode,omitempty"`
		Message        *string `json:"message,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	dist, err := h.distSvc.UpdateDistribution(c.Request.Context(), distID, userID, h.isPlatformAdmin(userID), req.Status, req.PermissionMode, req.Message)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrDistributionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "Distribution not found"})
		case errors.Is(err, services.ErrNotDistributor):
			c.JSON(http.StatusForbidden, gin.H{"error": "Only the distributor or platform admin can modify this distribution"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, dist)
}

// RevokeDistribution godoc
// @Summary      Revoke distribution
// @Description  Revoke a distribution (soft delete)
// @Tags         distributions
// @Produce      json
// @Param        id   path      string  true  "Distribution ID"
// @Success      200  {object}  object{message=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Router       /distributions/{id} [delete]
func (h *DistributionHandler) RevokeDistribution(c *gin.Context) {
	distID := c.Param("id")
	userID := c.GetString(middleware.UserIDKey)

	if err := h.distSvc.RevokeDistribution(c.Request.Context(), distID, userID, h.isPlatformAdmin(userID)); err != nil {
		switch {
		case errors.Is(err, services.ErrDistributionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "Distribution not found"})
		case errors.Is(err, services.ErrNotDistributor):
			c.JSON(http.StatusForbidden, gin.H{"error": "Only the distributor or platform admin can revoke this distribution"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Distribution revoked"})
}

// DismissReceipt godoc
// @Summary      Dismiss distribution receipt
// @Description  Dismiss a received distribution from the user's view
// @Tags         distributions
// @Produce      json
// @Param        id   path      string  true  "Distribution ID"
// @Success      200  {object}  object{message=string}
// @Failure      400  {object}  object{error=string}
// @Router       /distributions/{id}/dismiss [post]
func (h *DistributionHandler) DismissReceipt(c *gin.Context) {
	distID := c.Param("id")
	userID := c.GetString(middleware.UserIDKey)

	if err := h.distSvc.DismissReceipt(c.Request.Context(), distID, userID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Distribution dismissed"})
}

// MarkReceiptRead godoc
// @Summary      Mark distribution as read
// @Description  Mark a received distribution as read
// @Tags         distributions
// @Produce      json
// @Param        id   path      string  true  "Distribution ID"
// @Success      200  {object}  object{message=string}
// @Failure      400  {object}  object{error=string}
// @Router       /distributions/{id}/read [post]
func (h *DistributionHandler) MarkReceiptRead(c *gin.Context) {
	distID := c.Param("id")
	userID := c.GetString(middleware.UserIDKey)

	if err := h.distSvc.MarkReceiptRead(c.Request.Context(), distID, userID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Marked as read"})
}

// ForkItem godoc
// @Summary      Fork distributed item
// @Description  Fork an item from a distribution into the user's personal registry
// @Tags         distributions
// @Produce      json
// @Param        id   path      string  true  "Distribution ID"
// @Success      201  {object}  models.CapabilityItem
// @Failure      400  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Router       /distributions/{id}/fork [post]
func (h *DistributionHandler) ForkItem(c *gin.Context) {
	distID := c.Param("id")
	userID := c.GetString(middleware.UserIDKey)

	forkedItem, err := h.distSvc.ForkItem(c.Request.Context(), distID, userID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrForkNotAllowed):
			c.JSON(http.StatusForbidden, gin.H{"error": "Fork not allowed for this distribution"})
		case errors.Is(err, services.ErrAlreadyForked):
			c.JSON(http.StatusConflict, gin.H{"error": "Item already forked"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusCreated, forkedItem)
}

// HasEditableDistribution is a helper to check if a user has editable access via distribution.
func HasEditableDistribution(db *gorm.DB, itemID, userID string) bool {
	if db == nil || itemID == "" || userID == "" {
		return false
	}
	distSvc := services.NewDistributionService(db, nil)
	return distSvc.HasEditableDistribution(nil, itemID, userID)
}
