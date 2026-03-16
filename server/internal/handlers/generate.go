package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// GenerateHandler handles skill analysis and improvement requests
type GenerateHandler struct {
	generateSvc *services.GenerateService
}

// NewGenerateHandler creates a new generate handler
func NewGenerateHandler(generateSvc *services.GenerateService) *GenerateHandler {
	return &GenerateHandler{generateSvc: generateSvc}
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

	// Convert improvements (not used in service currently, kept for future use)
	_ = req.Improvements

	item, err := h.generateSvc.ImproveSkill(c.Request.Context(), itemID, nil, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, item)
}
