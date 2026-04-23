package authz

import (
	"net/http"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// RequirePermission returns a middleware that checks whether the current user
// has access to the given resource code.
func RequirePermission(svc *Service, resourceCode string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		allowed, err := svc.HasPermission(userID, resourceCode)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify permission"})
			return
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Permission denied"})
			return
		}

		c.Next()
	}
}

// RequireAnyPermission returns a middleware that checks whether the current user
// has access to at least one of the given resource codes.
func RequireAnyPermission(svc *Service, resourceCodes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		for _, code := range resourceCodes {
			allowed, err := svc.HasPermission(userID, code)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify permission"})
				return
			}
			if allowed {
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Permission denied"})
	}
}
