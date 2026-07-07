package handlers

import (
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// maxFeedbackLen bounds the free-text feedback stored per behavior log.
const maxFeedbackLen = 1000

// RecommendHandler handles recommendation requests
type RecommendHandler struct {
	recommendSvc *services.RecommendService
	behaviorSvc  *services.BehaviorService

	// Rate limiter for trust behavior writes (SRC-2026-4791 P1-1). Wired after the
	// Redis client is built; a nil client makes AllowBehavior a no-op (fail-open).
	rateLimitRedis  *redis.Client
	rateLimitPerMin int
}

// NewRecommendHandler creates a new recommend handler
func NewRecommendHandler(recommendSvc *services.RecommendService, behaviorSvc *services.BehaviorService) *RecommendHandler {
	return &RecommendHandler{
		recommendSvc: recommendSvc,
		behaviorSvc:  behaviorSvc,
	}
}

// SetBehaviorRateLimiter wires the Redis-backed limiter for POST /items/:id/behavior.
// Called once after the Redis client exists; perMinute <= 0 or a nil client disables it.
func (h *RecommendHandler) SetBehaviorRateLimiter(rdb *redis.Client, perMinute int) {
	h.rateLimitRedis = rdb
	h.rateLimitPerMin = perMinute
}

// GetRecommendations godoc
// @Summary      Get personalized recommendations
// @Description  Get personalized skill recommendations using multiple strategies
// @Tags         recommendations
// @Accept       json
// @Produce      json
// @Param        body  body      object{page=integer,pageSize=integer,types=[]string,categories=[]string,context=string,sessionItems=[]string}  true  "Recommendation request"
// @Success      200   {object}  services.RecommendResponse
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/recommend [post]
func (h *RecommendHandler) GetRecommendations(c *gin.Context) {
	var req struct {
		Page         int      `json:"page"`
		PageSize     int      `json:"pageSize"`
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
		Page:         req.Page,
		PageSize:     req.PageSize,
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
// @Param        page      query     integer  false  "Page number (default: 1)"
// @Param        pageSize  query     integer  false  "Page size (default: 10, max: 100)"
// @Param        types     query     []string false  "Filter by item types"
// @Success      200       {object}  object{items=[]models.CapabilityItem,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      500       {object}  object{error=string}
// @Router       /marketplace/items/trending [get]
func (h *RecommendHandler) GetTrending(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "10"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}
	types := c.QueryArray("types")

	items, total, err := h.recommendSvc.GetTrendingItems(c.Request.Context(), page, pageSize, types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ResolveItemListLocale(c, items)

	offset := (page - 1) * pageSize
	c.JSON(http.StatusOK, gin.H{
		"items":    items,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"hasMore":  int64(offset+pageSize) < total,
	})
}

// GetNewAndNoteworthy godoc
// @Summary      Get new and noteworthy items
// @Description  Get recently added high-quality items
// @Tags         recommendations
// @Produce      json
// @Param        page      query     integer  false  "Page number (default: 1)"
// @Param        pageSize  query     integer  false  "Page size (default: 10, max: 100)"
// @Success      200       {object}  object{items=[]models.CapabilityItem,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      500       {object}  object{error=string}
// @Router       /marketplace/items/new [get]
func (h *RecommendHandler) GetNewAndNoteworthy(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "10"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}

	items, total, err := h.recommendSvc.GetNewAndNoteworthy(c.Request.Context(), page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ResolveItemListLocale(c, items)

	offset := (page - 1) * pageSize
	c.JSON(http.StatusOK, gin.H{
		"items":    items,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"hasMore":  int64(offset+pageSize) < total,
	})
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

	// Reject unknown action types so arbitrary strings can't pollute the log.
	actionType := models.ActionType(req.ActionType)
	if !actionType.IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid actionType"})
		return
	}

	// Get user ID (OptionalAuth sets it only when a valid token is present).
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	// SRC-2026-4791: trust/counting signals (install, success, feedback, ...)
	// must be attributed to an authenticated user. Anonymous writes of these
	// would let anyone forge install counts, ratings and success rates to game
	// trending/recommendations. Low-trust browsing signals (view/click) may
	// still be recorded anonymously.
	if uid == "" && actionType.RequiresAuth() {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	// SRC-2026-4791 P1-1: cap the rate of TRUST writes per authenticated user so a
	// single account can't bulk-spam install/feedback/success/fail. Browsing
	// signals (view/click) are deliberately NOT throttled — otherwise a burst of
	// page views could exhaust the budget and 429 a legitimate install. Fails open.
	if actionType.RequiresAuth() {
		if !middleware.AllowBehavior(c.Request.Context(), h.rateLimitRedis, "ratelimit:behavior:u:"+uid, h.rateLimitPerMin, time.Minute) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests"})
			return
		}
	}

	// rating and free-text feedback only belong to feedback actions. Drop both for
	// any other action type so they can't be attached to install/view/click rows
	// and surface through the public rating/RecentFeedback stats panel
	// (SRC-2026-4791).
	rating := 0
	feedback := ""
	if actionType == models.ActionFeedback {
		rating = req.Rating
		if rating < 0 {
			rating = 0
		} else if rating > 5 {
			rating = 5
		}

		// Bound free-text feedback so a single request can't store an unbounded
		// blob. req.Feedback is valid UTF-8 (JSON-decoded); cut on a rune boundary
		// so we don't split a multibyte character into invalid UTF-8, which
		// PostgreSQL rejects on insert. Back up from the cut to the last rune start
		// (at most 3 bytes) instead of rescanning the whole slice.
		feedback = req.Feedback
		if len(feedback) > maxFeedbackLen {
			end := maxFeedbackLen
			for end > 0 && !utf8.RuneStart(feedback[end]) {
				end--
			}
			feedback = feedback[:end]
		}
	}

	behaviorReq := services.LogBehaviorRequest{
		UserID:      uid,
		ItemID:      itemID,
		ActionType:  actionType,
		Context:     models.ContextType(req.Context),
		SearchQuery: req.SearchQuery,
		SessionID:   req.SessionID,
		DurationMs:  req.DurationMs,
		Rating:      rating,
		Feedback:    feedback,
		Metadata:    req.Metadata,
	}

	log, err := h.behaviorSvc.LogBehavior(c.Request.Context(), behaviorReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, log)
}

// FavoriteItem godoc
// @Summary      Favorite item
// @Description  Mark an item as favorited for the current user
// @Tags         behavior
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{favorited=boolean,created=boolean,favoriteCount=integer}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/favorite [post]
func (h *RecommendHandler) FavoriteItem(c *gin.Context) {
	item, uid, ok := loadAccessibleItemForMutation(c)
	if !ok {
		return
	}
	count, created, err := h.behaviorSvc.FavoriteItem(c.Request.Context(), item.ID, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"favorited":     true,
		"created":       created,
		"favoriteCount": count,
	})
}

// UnfavoriteItem godoc
// @Summary      Unfavorite item
// @Description  Remove the current user's favorite mark from an item
// @Tags         behavior
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{favorited=boolean,removed=boolean,favoriteCount=integer}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/favorite [delete]
func (h *RecommendHandler) UnfavoriteItem(c *gin.Context) {
	item, uid, ok := loadAccessibleItemForMutation(c)
	if !ok {
		return
	}
	count, removed, err := h.behaviorSvc.UnfavoriteItem(c.Request.Context(), item.ID, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"favorited":     false,
		"removed":       removed,
		"favoriteCount": count,
	})
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

func loadAccessibleItemForMutation(c *gin.Context) (*models.CapabilityItem, string, bool) {
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)
	if uid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return nil, "", false
	}

	var item models.CapabilityItem
	if err := database.GetDB().Preload("Registry").First(&item, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return nil, "", false
	}

	if !canAccessItem(&item, uid) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this item"})
		return nil, "", false
	}

	return &item, uid, true
}
