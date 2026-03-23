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
// @Param        body  body      object{query=string,page=integer,pageSize=integer,types=[]string,categories=[]string,registryIds=[]string,minScore=number}  true  "Search request"
// @Success      200   {object}  services.SearchResult
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/search [post]
func (h *SearchHandler) SemanticSearch(c *gin.Context) {
	var req struct {
		Query       string   `json:"query" binding:"required"`
		Page        int      `json:"page"`
		PageSize    int      `json:"pageSize"`
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
		Page:        req.Page,
		PageSize:    req.PageSize,
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
// @Param        body  body      object{query=string,page=integer,pageSize=integer,types=[]string,categories=[]string,registryIds=[]string}  true  "Search request"
// @Success      200   {object}  services.SearchResult
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/hybrid-search [post]
func (h *SearchHandler) HybridSearch(c *gin.Context) {
	var req struct {
		Query       string   `json:"query" binding:"required"`
		Page        int      `json:"page"`
		PageSize    int      `json:"pageSize"`
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
		Page:        req.Page,
		PageSize:    req.PageSize,
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
// @Param        id       path      string  true  "Item ID"
// @Param        page     query     integer false "Page number (default: 1)"
// @Param        pageSize query     integer false "Page size (default: 10)"
// @Success      200      {object}  object{items=[]services.SearchResultItem,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      404      {object}  object{error=string}
// @Failure      500      {object}  object{error=string}
// @Router       /items/{id}/similar [get]
func (h *SearchHandler) FindSimilar(c *gin.Context) {
	itemID := c.Param("id")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "10"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}

	items, total, err := h.searchSvc.FindSimilar(c.Request.Context(), itemID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	offset := (page - 1) * pageSize
	c.JSON(http.StatusOK, gin.H{
		"items":    items,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
		"hasMore":  int64(offset+pageSize) < total,
	})
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
