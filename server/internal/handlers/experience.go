package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// ExperienceHandler handles experience evolution requests
type ExperienceHandler struct {
	evolutionSvc *services.EvolutionService
}

// NewExperienceHandler creates a new experience handler
func NewExperienceHandler(evolutionSvc *services.EvolutionService) *ExperienceHandler {
	return &ExperienceHandler{evolutionSvc: evolutionSvc}
}

// GetPendingExperiences godoc
// @Summary      Get pending experience candidates
// @Description  Get pending experience candidates awaiting review
// @Tags         admin/experiences
// @Produce      json
// @Param        limit   query     integer  false  "Page size (default: 20)"
// @Param        offset  query     integer  false  "Page offset (default: 0)"
// @Success      200     {object}  object{candidates=[]models.ExperienceCandidate,total=integer}
// @Failure      500     {object}  object{error=string}
// @Router       /admin/experiences/pending [get]
func (h *ExperienceHandler) GetPendingExperiences(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	candidates, total, err := h.evolutionSvc.GetPendingCandidates(c.Request.Context(), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"candidates": candidates,
		"total":      total,
	})
}

// GetExperienceByID godoc
// @Summary      Get experience candidate by ID
// @Description  Get details of a specific experience candidate
// @Tags         admin/experiences
// @Produce      json
// @Param        id   path      string  true  "Candidate ID"
// @Success      200  {object}  models.ExperienceCandidate
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/experiences/{id} [get]
func (h *ExperienceHandler) GetExperienceByID(c *gin.Context) {
	candidateID := c.Param("id")

	candidate, err := h.evolutionSvc.GetCandidateByID(c.Request.Context(), candidateID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Candidate not found"})
		return
	}

	c.JSON(http.StatusOK, candidate)
}

// ApproveExperience godoc
// @Summary      Approve an experience candidate
// @Description  Approve and promote an experience candidate to item metadata
// @Tags         admin/experiences
// @Produce      json
// @Param        id   path      string  true  "Candidate ID"
// @Success      200  {object}  models.ExperiencePromotion
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/experiences/{id}/approve [post]
func (h *ExperienceHandler) ApproveExperience(c *gin.Context) {
	candidateID := c.Param("id")

	// Get user ID
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	promotion, err := h.evolutionSvc.ApproveCandidate(c.Request.Context(), candidateID, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, promotion)
}

// RejectExperience godoc
// @Summary      Reject an experience candidate
// @Description  Reject an experience candidate
// @Tags         admin/experiences
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Candidate ID"
// @Param        body  body      object{reason=string}  false  "Rejection reason"
// @Success      200   {object}  object{message=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /admin/experiences/{id}/reject [post]
func (h *ExperienceHandler) RejectExperience(c *gin.Context) {
	candidateID := c.Param("id")

	var req struct {
		Reason string `json:"reason"`
	}
	c.ShouldBindJSON(&req)

	// Get user ID
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	err := h.evolutionSvc.RejectCandidate(c.Request.Context(), candidateID, uid, req.Reason)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Experience rejected"})
}

// AnalyzeItemPatterns godoc
// @Summary      Analyze patterns for an item
// @Description  Run pattern analysis on behavior logs for a specific item
// @Tags         experiences
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  llm.ExperienceAnalysisResult
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/analyze-patterns [post]
func (h *ExperienceHandler) AnalyzeItemPatterns(c *gin.Context) {
	itemID := c.Param("id")

	result, err := h.evolutionSvc.AnalyzePatterns(c.Request.Context(), itemID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if result == nil {
		c.JSON(http.StatusOK, gin.H{"message": "No behavior data available for analysis"})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetPromotionHistory godoc
// @Summary      Get promotion history for an item
// @Description  Get the history of experience promotions for a specific item
// @Tags         experiences
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        limit query     integer false "Number of results (default: 20)"
// @Success      200   {object}  object{promotions=[]models.ExperiencePromotion}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/promotion-history [get]
func (h *ExperienceHandler) GetPromotionHistory(c *gin.Context) {
	itemID := c.Param("id")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))

	promotions, err := h.evolutionSvc.GetPromotionHistory(c.Request.Context(), itemID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"promotions": promotions})
}

// RunAnalysis godoc
// @Summary      Run automatic analysis
// @Description  Trigger automatic pattern analysis for all items with recent activity
// @Tags         admin/experiences
// @Produce      json
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/experiences/run-analysis [post]
func (h *ExperienceHandler) RunAnalysis(c *gin.Context) {
	go func() {
		_ = h.evolutionSvc.RunAutomaticAnalysis(c.Request.Context())
	}()

	c.JSON(http.StatusOK, gin.H{"message": "Analysis started in background"})
}
