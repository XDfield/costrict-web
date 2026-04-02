package systemrole

import (
	"net/http"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func RequirePlatformAdmin(db *gorm.DB) gin.HandlerFunc {
	service := NewSystemRoleService(db)
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		hasRole, err := service.HasRole(userID, SystemRolePlatformAdmin)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify system role"})
			return
		}
		if !hasRole {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Platform admin role required"})
			return
		}

		c.Next()
	}
}

func RequireBusinessAdminOrAbove(db *gorm.DB) gin.HandlerFunc {
	service := NewSystemRoleService(db)
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		hasRole, err := service.HasAnyRole(userID, SystemRoleBusinessAdmin, SystemRolePlatformAdmin)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify system role"})
			return
		}
		if !hasRole {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Business admin role required"})
			return
		}

		c.Next()
	}
}
