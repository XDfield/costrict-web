package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// SearchHandler handles search requests
type SearchHandler struct {
	searchSvc *services.SearchService
}

// NewSearchHandler creates a new search handler
func NewSearchHandler(searchSvc *services.SearchService) *SearchHandler {
	return &SearchHandler{searchSvc: searchSvc}
}

// SemanticSearch godoc
// @Summary      Semantic search for capabilities
// @Description  Search for capability items using semantic similarity
// @Tags         search
// @Accept       json
// @Produce      json
// @Param        body  body      object{query=string,limit=integer,offset=integer,types=[]string,categories=[]string,registryIds=[]string,minScore=number}  true  "Search request"
// @Success      200   {object}  services.SearchResult
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/search [post]
func (h *SearchHandler) SemanticSearch(c *gin.Context) {
	var req struct {
		Query       string   `json:"query" binding:"required"`
		Limit       int      `json:"limit"`
		Offset      int      `json:"offset"`
		Types       []string `json:"types"`
		Categories  []string `json:"categories"`
		RegistryIDs []string `json:"registryIds"`
		MinScore    float64  `json:"minScore"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Get visible registry IDs
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)
	visibleRegistries := buildVisibleRegistryIDs(database.GetDB(), uid)

	// Filter registry IDs to only visible ones
	if len(req.RegistryIDs) > 0 {
		req.RegistryIDs = intersectStrings(req.RegistryIDs, visibleRegistries)
	} else {
		req.RegistryIDs = visibleRegistries
	}

	searchReq := services.SearchRequest{
		Query:       req.Query,
		Limit:       req.Limit,
		Offset:      req.Offset,
		Types:       req.Types,
		Categories:  req.Categories,
		RegistryIDs: req.RegistryIDs,
		MinScore:    req.MinScore,
	}

	result, err := h.searchSvc.SemanticSearch(c.Request.Context(), searchReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// HybridSearch godoc
// @Summary      Hybrid search for capabilities
// @Description  Search using both semantic similarity and keyword matching
// @Tags         search
// @Accept       json
// @Produce      json
// @Param        body  body      object{query=string,limit=integer,offset=integer,types=[]string,categories=[]string,registryIds=[]string}  true  "Search request"
// @Success      200   {object}  services.SearchResult
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/hybrid-search [post]
func (h *SearchHandler) HybridSearch(c *gin.Context) {
	var req struct {
		Query       string   `json:"query" binding:"required"`
		Limit       int      `json:"limit"`
		Offset      int      `json:"offset"`
		Types       []string `json:"types"`
		Categories  []string `json:"categories"`
		RegistryIDs []string `json:"registryIds"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Get visible registry IDs
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)
	visibleRegistries := buildVisibleRegistryIDs(database.GetDB(), uid)

	if len(req.RegistryIDs) > 0 {
		req.RegistryIDs = intersectStrings(req.RegistryIDs, visibleRegistries)
	} else {
		req.RegistryIDs = visibleRegistries
	}

	searchReq := services.SearchRequest{
		Query:       req.Query,
		Limit:       req.Limit,
		Offset:      req.Offset,
		Types:       req.Types,
		Categories:  req.Categories,
		RegistryIDs: req.RegistryIDs,
	}

	result, err := h.searchSvc.HybridSearch(c.Request.Context(), searchReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// FindSimilar godoc
// @Summary      Find similar items
// @Description  Find items similar to a given item using vector similarity
// @Tags         search
// @Produce      json
// @Param        id     path      string  true  "Item ID"
// @Param        limit  query     integer false "Number of results (default: 10)"
// @Success      200    {object}  object{items=[]services.SearchResultItem}
// @Failure      404    {object}  object{error=string}
// @Failure      500    {object}  object{error=string}
// @Router       /items/{id}/similar [get]
func (h *SearchHandler) FindSimilar(c *gin.Context) {
	itemID := c.Param("id")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))

	items, err := h.searchSvc.FindSimilar(c.Request.Context(), itemID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

func intersectStrings(a, b []string) []string {
	m := make(map[string]bool)
	for _, s := range b {
		m[s] = true
	}
	var result []string
	for _, s := range a {
		if m[s] {
			result = append(result, s)
		}
	}
	return result
}
