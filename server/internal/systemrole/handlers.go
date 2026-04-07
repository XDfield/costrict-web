package systemrole

import (
	"errors"
	"net/http"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

type userRolesResponse struct {
	UserID       string   `json:"userId"`
	Roles        []string `json:"roles"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// GetUserSystemRolesHandler godoc
// @Summary      Get user system roles
// @Description  Get system roles of a specified user (platform admin only)
// @Tags         admin/system-roles
// @Produce      json
// @Security     BearerAuth
// @Param        userId  path      string  true  "User ID"
// @Success      200     {object}  object{userId=string,roles=[]string}
// @Failure      401     {object}  object{error=string}
// @Failure      403     {object}  object{error=string}
// @Failure      500     {object}  object{error=string}
// @Router       /admin/system-roles/users/{userId} [get]
func GetUserSystemRolesHandler(svc *SystemRoleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("userId")
		roles, err := svc.ListRoles(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query system roles"})
			return
		}
		c.JSON(http.StatusOK, userRolesResponse{UserID: userID, Roles: roles})
	}
}

// GrantSystemRoleHandler godoc
// @Summary      Grant system role
// @Description  Grant a system role to a user (platform admin only)
// @Tags         admin/system-roles
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        userId  path      string               true  "User ID"
// @Param        body    body      object{role=string}  true  "Grant role request"
// @Success      200     {object}  object{success=bool}
// @Failure      400     {object}  object{error=string}
// @Failure      401     {object}  object{error=string}
// @Failure      403     {object}  object{error=string}
// @Failure      404     {object}  object{error=string}
// @Failure      500     {object}  object{error=string}
// @Router       /admin/system-roles/users/{userId} [post]
func GrantSystemRoleHandler(svc *SystemRoleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req struct {
			Role string `json:"role" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := svc.GrantRole(c.Param("userId"), req.Role, operatorID); err != nil {
			switch {
			case errors.Is(err, ErrInvalidSystemRole):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid system role"})
			case errors.Is(err, ErrSystemRoleUserNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to grant system role"})
			}
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// RevokeSystemRoleHandler godoc
// @Summary      Revoke system role
// @Description  Revoke a system role from a user (platform admin only)
// @Tags         admin/system-roles
// @Produce      json
// @Security     BearerAuth
// @Param        userId  path      string  true  "User ID"
// @Param        role    path      string  true  "Role"
// @Success      200     {object}  object{success=bool}
// @Failure      400     {object}  object{error=string}
// @Failure      401     {object}  object{error=string}
// @Failure      403     {object}  object{error=string}
// @Failure      500     {object}  object{error=string}
// @Router       /admin/system-roles/users/{userId}/{role} [delete]
func RevokeSystemRoleHandler(svc *SystemRoleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		if err := svc.RevokeRole(c.Param("userId"), c.Param("role"), operatorID); err != nil {
			switch {
			case errors.Is(err, ErrInvalidSystemRole):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid system role"})
			case errors.Is(err, ErrCannotRevokeLastPlatformAdmin):
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot revoke last platform admin"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke system role"})
			}
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// ListUsersBySystemRoleHandler godoc
// @Summary      List users by system role
// @Description  List members by target system role (platform admin only)
// @Tags         admin/system-roles
// @Produce      json
// @Security     BearerAuth
// @Param        role  query     string  true  "Role"
// @Success      200   {object}  object{role=string,users=[]object}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /admin/system-roles [get]
func ListUsersBySystemRoleHandler(svc *SystemRoleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		role := c.Query("role")
		if !IsValidRole(role) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid system role"})
			return
		}

		users, err := svc.ListUsersByRole(role)
		if err != nil {
			if errors.Is(err, ErrInvalidSystemRole) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid system role"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"role": role, "users": users})
	}
}

// GetMySystemRolesHandler godoc
// @Summary      Get current user system roles
// @Description  Get current authenticated user's system roles and capabilities
// @Tags         auth
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  object{userId=string,roles=[]string,capabilities=[]string}
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /auth/system-roles/me [get]
func GetMySystemRolesHandler(svc *SystemRoleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		roles, err := svc.ListRoles(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query system roles"})
			return
		}
		capabilities := CapabilitiesForRoles(roles)
		c.JSON(http.StatusOK, userRolesResponse{UserID: userID, Roles: roles, Capabilities: capabilities})
	}
}
