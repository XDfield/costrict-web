package kanban

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// OverviewResponse is a placeholder for kanban metrics data.
type OverviewResponse struct {
	TotalProjects   int64   `json:"totalProjects"`
	TotalUsers      int64   `json:"totalUsers"`
	TotalDevices    int64   `json:"totalDevices"`
	TotalRequests   int64   `json:"totalRequests"`
	OnlineDevices   int64   `json:"onlineDevices"`
	ActiveUsers7d   int64   `json:"activeUsers7d"`
	InputTokens7d   int64   `json:"inputTokens7d"`
	OutputTokens7d  int64   `json:"outputTokens7d"`
	Cost7d          float64 `json:"cost7d"`
}

// GetOverviewHandler returns placeholder kanban overview data.
func GetOverviewHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, OverviewResponse{
			TotalProjects:  0,
			TotalUsers:     0,
			TotalDevices:   0,
			TotalRequests:  0,
			OnlineDevices:  0,
			ActiveUsers7d:  0,
			InputTokens7d:  0,
			OutputTokens7d: 0,
			Cost7d:         0,
		})
	}
}
