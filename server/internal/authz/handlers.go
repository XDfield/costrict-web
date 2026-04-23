package authz

import (
	"net/http"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// GetUserPermissionsHandler returns the current user's permission snapshot.
func GetUserPermissionsHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		perms, err := svc.GetUserPermissions(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query permissions"})
			return
		}

		c.JSON(http.StatusOK, perms)
	}
}

type verifyRequest struct {
	Token    string `json:"token" binding:"required"`
	Resource string `json:"resource" binding:"required"`
}

type verifyResponse struct {
	Allowed      bool             `json:"allowed"`
	Menus        []string         `json:"menus,omitempty"`
	Capabilities []string         `json:"capabilities,omitempty"`
}

// VerifyTokenHandler is the internal endpoint for gateway or other services
// to validate a user's token against a specific resource.
func VerifyTokenHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req verifyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		allowed, perms, err := svc.VerifyToken(req.Token, req.Resource)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "token verification failed"})
			return
		}

		resp := verifyResponse{Allowed: allowed}
		if perms != nil {
			resp.Menus = perms.Menus
			resp.Capabilities = perms.Capabilities
		}
		c.JSON(http.StatusOK, resp)
	}
}
