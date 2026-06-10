package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

func MulticaUpdateCheckHandler(svc *services.MulticaUpdateService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req services.MulticaUpdateCheckRequest
		if err := c.ShouldBindQuery(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "platform and version are required"})
			return
		}

		result, err := svc.CheckForUpdate(req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update check failed"})
			return
		}

		c.JSON(http.StatusOK, result)
	}
}
