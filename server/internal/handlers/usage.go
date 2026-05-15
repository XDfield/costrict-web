package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

type UsageHandler struct {
	usageSvc *services.UsageService
}

func NewUsageHandler(usageSvc *services.UsageService) *UsageHandler {
	return &UsageHandler{usageSvc: usageSvc}
}

// Report godoc
// @Summary      Report session usage
// @Description  Acknowledge usage report requests (deprecated — no longer processed).
// @Tags         usage
// @Accept       json
// @Produce      json
// @Success      200  {object}  object{message=string}
// @Router       /usage/report [post]
// Deprecated: The usage report feature has been removed. This endpoint now
// always returns 200 to avoid breaking existing CLI clients that still send
// reports. It can be fully removed once all client versions are updated.
func (h *UsageHandler) Report(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// Activity godoc
// @Summary      Query usage activity
// @Description  Query daily request activity for all users under the specified git repository URL.
// @Tags         usage
// @Produce      json
// @Security     BearerAuth
// @Param        git_repo_url  query     string  true   "Git repository URL"
// @Param        days          query     int     false  "Range in days (1-90, default 7)"
// @Success      200           {object}  services.UsageActivityResponse
// @Failure      400           {object}  object{error=string}
// @Failure      401           {object}  object{error=string}
// @Failure      500           {object}  object{error=string}
// @Router       /usage/activity [get]
func (h *UsageHandler) Activity(c *gin.Context) {
	if c.GetString(middleware.UserIDKey) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	gitRepoURL := c.Query("git_repo_url")
	if gitRepoURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "git_repo_url is required"})
		return
	}

	days := 7
	if raw := c.Query("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 90 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days must be between 1 and 90"})
			return
		}
		days = parsed
	}

	resp, err := h.usageSvc.GetActivity(gitRepoURL, days, map[string]string{})
	if err != nil {
		switch {
		case errors.Is(err, services.ErrInvalidRepoURL):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, services.ErrUsageQueryFailed):
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, resp)
}
