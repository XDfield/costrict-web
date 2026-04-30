package authz

import (
	"errors"
	"net/http"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/systemrole"
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

type grantUserRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

// GrantUserRoleHandler godoc
// @Summary      Grant module permission to user
// @Description  Grant a system role (module permission) to a user by user ID (platform admin only)
// @Tags         admin/permissions
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        userId  path      string               true  "User ID"
// @Param        body    body      grantUserRoleRequest true  "Grant permission request"
// @Success      200     {object}  object{success=bool}
// @Failure      400     {object}  object{error=string}
// @Failure      401     {object}  object{error=string}
// @Failure      403     {object}  object{error=string}
// @Failure      404     {object}  object{error=string}
// @Failure      500     {object}  object{error=string}
// @Router       /admin/permissions/users/{userId}/grant [post]
func GrantUserRoleHandler(svc *systemrole.SystemRoleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req grantUserRoleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := svc.GrantRole(c.Param("userId"), req.Role, operatorID); err != nil {
			switch {
			case errors.Is(err, systemrole.ErrInvalidSystemRole):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid system role"})
			case errors.Is(err, systemrole.ErrSystemRoleUserNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to grant permission"})
			}
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
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
