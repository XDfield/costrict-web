package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/audit"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// DistributionHandler holds dependencies for distribution endpoints.
type DistributionHandler struct {
	db            *gorm.DB
	distSvc       *services.DistributionService
	systemRoleSvc *systemrole.SystemRoleService
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
// @Summary      Push item to targets
// @Description  Push (distribute) an item to users or departments with specified permission mode.
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
	isPlatformAdmin := h.isPlatformAdmin(userID)
	if !h.distSvc.CanDistribute(item, userID, isPlatformAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You do not have permission to push this item"})
		return
	}

	var req services.DistributeItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Per-target boundary: a department manager may only push within the subtree(s)
	// they manage. Platform admins pass unconditionally. Any out-of-scope target
	// rejects the whole request (atomic — no partial distribution).
	if err := h.distSvc.AuthorizeTargets(userID, isPlatformAdmin, req.Targets); err != nil {
		status := http.StatusForbidden
		if errors.Is(err, services.ErrUnsupportedScope) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	results, err := h.distSvc.DistributeItem(c.Request.Context(), item, userID, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	audit.Record(userID, audit.ActionDistributionCreate, audit.TargetDistribution, itemID, gin.H{
		"itemId":         itemID,
		"targets":        req.Targets,
		"permissionMode": req.PermissionMode,
	})

	c.JSON(http.StatusCreated, gin.H{"distributions": results})
}

// MyDistributionAuthority godoc
// @Summary      My distribution authority
// @Description  The current user's distribution reach: unlimited for platform admins, otherwise the department subtrees they lead (manage). Drives the frontend distribute entry + department picker scope. Any authenticated user may query their own.
// @Tags         distributions
// @Produce      json
// @Success      200  {object}  services.DistributionAuthority
// @Router       /distributions/my/authority [get]
func (h *DistributionHandler) MyDistributionAuthority(c *gin.Context) {
	userID := c.GetString(middleware.UserIDKey)
	authority, err := h.distSvc.ResolveDistributionAuthority(userID, h.isPlatformAdmin(userID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve distribution authority"})
		return
	}
	c.JSON(http.StatusOK, authority)
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

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// ListAllDistributions godoc
// @Summary      List all distributions (platform admin)
// @Description  List distributions across all distributors with status/scope/search filters and pagination. Platform admin only.
// @Tags         distributions
// @Produce      json
// @Param        status    query     string  false  "Filter by status (active|paused|revoked)"
// @Param        scope     query     string  false  "Filter by scope type (user|department)"
// @Param        search    query     string  false  "Search by item name / distributor / target"
// @Param        page      query     int     false  "Page number (1-based)"
// @Param        pageSize  query     int     false  "Page size (default 20)"
// @Success      200  {object}  object{distributions=[]models.ItemDistribution,total=int}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/distributions [get]
func (h *DistributionHandler) ListAllDistributions(c *gin.Context) {
	f := services.DistributionListFilter{
		Status:    c.Query("status"),
		ScopeType: c.Query("scope"),
		Search:    c.Query("search"),
		Page:      atoiDefault(c.Query("page"), 1),
		PageSize:  atoiDefault(c.Query("pageSize"), 20),
	}
	list, total, err := h.distSvc.ListAllDistributions(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"distributions": list, "total": total})
}

// ListDistributionReceipts godoc
// @Summary      List distribution receipts (platform admin)
// @Description  List all receipts for a distribution (unread/read/accepted/dismissed). Platform admin only.
// @Tags         distributions
// @Produce      json
// @Param        id   path      string  true  "Distribution ID"
// @Success      200  {object}  object{receipts=[]models.ItemDistributionReceipt}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/distributions/{id}/receipts [get]
func (h *DistributionHandler) ListDistributionReceipts(c *gin.Context) {
	receipts, err := h.distSvc.ListReceipts(c.Request.Context(), c.Param("id"))
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

	audit.Record(userID, audit.ActionDistributionUpdate, audit.TargetDistribution, distID, gin.H{
		"status":         req.Status,
		"permissionMode": req.PermissionMode,
	})

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

	audit.Record(userID, audit.ActionDistributionRevoke, audit.TargetDistribution, distID, nil)

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
