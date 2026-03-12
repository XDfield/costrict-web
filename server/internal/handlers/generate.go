package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// GenerateHandler handles skill generation requests
type GenerateHandler struct {
	generateSvc *services.GenerateService
}

// NewGenerateHandler creates a new generate handler
func NewGenerateHandler(generateSvc *services.GenerateService) *GenerateHandler {
	return &GenerateHandler{generateSvc: generateSvc}
}

// GenerateSkill godoc
// @Summary      Generate a skill using AI
// @Description  Generate a skill definition based on a natural language prompt
// @Tags         generate
// @Accept       json
// @Produce      json
// @Param        body  body      object{prompt=string,context=string,itemType=string,category=string,registryId=string,saveItem=boolean}  true  "Generation request"
// @Success      200   {object}  services.GenerateResponse
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /marketplace/items/generate [post]
func (h *GenerateHandler) GenerateSkill(c *gin.Context) {
	var req struct {
		Prompt     string `json:"prompt" binding:"required"`
		Context    string `json:"context"`
		ItemType   string `json:"itemType"`
		Category   string `json:"category"`
		RegistryID string `json:"registryId"`
		SaveItem   bool   `json:"saveItem"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Get user ID
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	genReq := services.GenerateRequest{
		Prompt:     req.Prompt,
		Context:    req.Context,
		ItemType:   req.ItemType,
		Category:   req.Category,
		RegistryID: req.RegistryID,
		CreatedBy:  uid,
		SaveItem:   req.SaveItem,
	}

	result, err := h.generateSvc.GenerateSkill(c.Request.Context(), genReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// AnalyzeSkill godoc
// @Summary      Analyze a skill
// @Description  Analyze an existing skill and get improvement suggestions
// @Tags         generate
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  llm.SkillAnalysis
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/analyze [post]
func (h *GenerateHandler) AnalyzeSkill(c *gin.Context) {
	itemID := c.Param("id")

	analysis, err := h.generateSvc.AnalyzeSkill(c.Request.Context(), itemID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, analysis)
}

// ImproveSkill godoc
// @Summary      Improve a skill
// @Description  Apply suggested improvements to a skill
// @Tags         generate
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{improvements=[]llm.SkillImprovement}  true  "Improvements to apply"
// @Success      200   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/improve [post]
func (h *GenerateHandler) ImproveSkill(c *gin.Context) {
	itemID := c.Param("id")

	var req struct {
		Improvements []struct {
			Field     string `json:"field"`
			Current   string `json:"current"`
			Suggested string `json:"suggested"`
			Reason    string `json:"reason"`
		} `json:"improvements" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Get user ID
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	// Convert improvements
	improvements := make([]interface{}, len(req.Improvements))
	for i, imp := range req.Improvements {
		improvements[i] = imp
	}

	item, err := h.generateSvc.ImproveSkill(c.Request.Context(), itemID, nil, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, item)
}
