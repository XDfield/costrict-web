package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// RecommendHandler handles recommendation requests
type RecommendHandler struct {
	recommendSvc *services.RecommendService
	behaviorSvc  *services.BehaviorService
}

// NewRecommendHandler creates a new recommend handler
func NewRecommendHandler(recommendSvc *services.RecommendService, behaviorSvc *services.BehaviorService) *RecommendHandler {
	return &RecommendHandler{
		recommendSvc: recommendSvc,
		behaviorSvc:  behaviorSvc,
	}
}

// GetRecommendations godoc
// @Summary      Get personalized recommendations
// @Description  Get personalized skill recommendations using multiple strategies
// @Tags         recommendations
// @Accept       json
// @Produce      json
// @Param        body  body      object{limit=integer,types=[]string,categories=[]string,context=string,sessionItems=[]string}  true  "Recommendation request"
// @Success      200   {object}  services.RecommendResponse
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/recommend [post]
func (h *RecommendHandler) GetRecommendations(c *gin.Context) {
	var req struct {
		Limit        int      `json:"limit"`
		Types        []string `json:"types"`
		Categories   []string `json:"categories"`
		Context      string   `json:"context"`
		SessionItems []string `json:"sessionItems"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Get user ID
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	recReq := services.RecommendRequest{
		UserID:       uid,
		Limit:        req.Limit,
		Types:        req.Types,
		Categories:   req.Categories,
		Context:      req.Context,
		SessionItems: req.SessionItems,
	}

	result, err := h.recommendSvc.GetRecommendations(c.Request.Context(), recReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetTrending godoc
// @Summary      Get trending items
// @Description  Get trending items based on recent activity
// @Tags         recommendations
// @Produce      json
// @Param        limit  query     integer  false  "Number of results (default: 10)"
// @Param        types  query     []string false  "Filter by item types"
// @Success      200    {object}  object{items=[]models.CapabilityItem}
// @Failure      500    {object}  object{error=string}
// @Router       /marketplace/items/trending [get]
func (h *RecommendHandler) GetTrending(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	types := c.QueryArray("types")

	items, err := h.recommendSvc.GetTrendingItems(c.Request.Context(), limit, types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

// GetNewAndNoteworthy godoc
// @Summary      Get new and noteworthy items
// @Description  Get recently added high-quality items
// @Tags         recommendations
// @Produce      json
// @Param        limit  query     integer  false  "Number of results (default: 10)"
// @Success      200    {object}  object{items=[]models.CapabilityItem}
// @Failure      500    {object}  object{error=string}
// @Router       /marketplace/items/new [get]
func (h *RecommendHandler) GetNewAndNoteworthy(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))

	items, err := h.recommendSvc.GetNewAndNoteworthy(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

// LogBehavior godoc
// @Summary      Log user behavior
// @Description  Log a user interaction with a capability item
// @Tags         behavior
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{actionType=string,context=string,searchQuery=string,sessionId=string,durationMs=integer,rating=integer,feedback=string,metadata=object}  true  "Behavior data"
// @Success      201   {object}  models.BehaviorLog
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/behavior [post]
func (h *RecommendHandler) LogBehavior(c *gin.Context) {
	itemID := c.Param("id")

	var req struct {
		ActionType  string                 `json:"actionType" binding:"required"`
		Context     string                 `json:"context"`
		SearchQuery string                 `json:"searchQuery"`
		SessionID   string                 `json:"sessionId"`
		DurationMs  int64                  `json:"durationMs"`
		Rating      int                    `json:"rating"`
		Feedback    string                 `json:"feedback"`
		Metadata    map[string]interface{} `json:"metadata"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Get user ID
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	behaviorReq := services.LogBehaviorRequest{
		UserID:      uid,
		ItemID:      itemID,
		ActionType:  models.ActionType(req.ActionType),
		Context:     models.ContextType(req.Context),
		SearchQuery: req.SearchQuery,
		SessionID:   req.SessionID,
		DurationMs:  req.DurationMs,
		Rating:      req.Rating,
		Feedback:    req.Feedback,
		Metadata:    req.Metadata,
	}

	log, err := h.behaviorSvc.LogBehavior(c.Request.Context(), behaviorReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, log)
}

// GetItemStats godoc
// @Summary      Get item behavior statistics
// @Description  Get behavior statistics for a specific item
// @Tags         behavior
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  services.ItemBehaviorStats
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/stats [get]
func (h *RecommendHandler) GetItemStats(c *gin.Context) {
	itemID := c.Param("id")

	stats, err := h.behaviorSvc.GetItemBehaviorStats(c.Request.Context(), itemID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, stats)
}

// GetUserSummary godoc
// @Summary      Get user behavior summary
// @Description  Get a summary of user's behavior and preferences
// @Tags         behavior
// @Produce      json
// @Success      200  {object}  models.UserBehaviorSummary
// @Failure      500  {object}  object{error=string}
// @Router       /users/me/behavior/summary [get]
func (h *RecommendHandler) GetUserSummary(c *gin.Context) {
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	if uid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "User not authenticated"})
		return
	}

	summary, err := h.behaviorSvc.GetUserBehaviorSummary(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, summary)
}
