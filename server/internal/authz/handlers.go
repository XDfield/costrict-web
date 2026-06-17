package authz

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/audit"
	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
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

// GetUserScopeHandler returns the current user's metrics-dashboard visibility
// scope (which department subtrees they may see). Any authenticated user may query
// their own scope; it carries no privileged data beyond the caller's own access.
func GetUserScopeHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(appmiddleware.UserIDKey)
		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		scope, err := svc.ResolveUserScope(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve scope"})
			return
		}

		c.JSON(http.StatusOK, scope)
	}
}

type verifyRequest struct {
	Token    string `json:"token" binding:"required"`
	Resource string `json:"resource" binding:"required"`
}

type verifyResponse struct {
	Allowed      bool     `json:"allowed"`
	Menus        []string `json:"menus,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// Scope is the caller's metrics-dashboard visibility scope, included so an
	// external service (e.g. efficiency-dashboard) can verify a token and obtain
	// the department-scope facts in a single internal round-trip. Omitted when the
	// token is not allowed (Allowed=false) or scope resolution fails (best-effort).
	Scope *scopeSummary `json:"scope,omitempty"`
}

// scopeSummary is the compact scope view embedded in the verify response: the
// fields an external consumer needs to enforce department visibility.
type scopeSummary struct {
	AllAccess           bool     `json:"allAccess"`
	VisibleDeptPrefixes []string `json:"visibleDeptPrefixes"`
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

		audit.Record(operatorID, audit.ActionSystemRoleGrant, audit.TargetUser, c.Param("userId"), gin.H{"role": req.Role})

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// ListResourcePermissionsHandler godoc
// @Summary      List resource permissions
// @Description  List all resource permissions (menu + api) for the permission matrix (platform admin only)
// @Tags         admin/permissions
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  object{permissions=[]models.ResourcePermission}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/resource-permissions [get]
func ListResourcePermissionsHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		perms, err := svc.ListResourcePermissions()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list resource permissions"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"permissions": perms})
	}
}

type updateResourcePermissionRequest struct {
	AllowedRoles []string `json:"allowedRoles"`
}

// UpdateResourcePermissionHandler godoc
// @Summary      Update resource permission
// @Description  Update the allowed roles for a single resource code (platform admin only)
// @Tags         admin/permissions
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        code  path      string                          true  "Resource code"
// @Param        body  body      updateResourcePermissionRequest true  "Allowed roles"
// @Success      200   {object}  object{success=bool}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /admin/resource-permissions/{code} [put]
func UpdateResourcePermissionHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Param("code")
		if code == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "resource code is required"})
			return
		}

		var req updateResourcePermissionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if err := svc.UpdateResourcePermission(code, req.AllowedRoles); err != nil {
			switch {
			case errors.Is(err, ErrInvalidResourceRole):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid system role in allowedRoles"})
			case errors.Is(err, ErrResourcePermissionNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "resource permission not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update resource permission"})
			}
			return
		}

		audit.Record(c.GetString(appmiddleware.UserIDKey), audit.ActionResourcePermissionUpdate, audit.TargetResourcePermission, code, gin.H{"allowedRoles": req.AllowedRoles})

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// ListPermissionGrantsHandler godoc
//
//	@Summary		List permission grants
//	@Description	List fine-grained permission grants, optionally filtered by permissionCode (platform admin only)
//	@Tags			admin/permissions
//	@Produce		json
//	@Security		BearerAuth
//	@Param			permissionCode	query		string	false	"Filter by permission code"
//	@Success		200	{object}	object{grants=[]models.PermissionGrant}
//	@Failure		401	{object}	object{error=string}
//	@Failure		403	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/permission-grants [get]
func ListPermissionGrantsHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		grants, err := svc.ListGrants(c.Query("permissionCode"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list permission grants"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"grants": grants})
	}
}

type grantPermissionRequest struct {
	PermissionCode string `json:"permissionCode" binding:"required"`
	SubjectType    string `json:"subjectType" binding:"required"` // user | department
	SubjectID      string `json:"subjectId" binding:"required"`
}

// GrantPermissionHandler godoc
//
//	@Summary		Grant fine-grained permission
//	@Description	Grant a permission to a user or department (platform admin only). For department subjects the materialized dept_path is resolved from dept-sync and stored redundantly for prefix-based inheritance.
//	@Tags			admin/permissions
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			body	body		grantPermissionRequest	true	"Grant request"
//	@Success		200	{object}	object{grant=models.PermissionGrant}
//	@Failure		400	{object}	object{error=string}
//	@Failure		401	{object}	object{error=string}
//	@Failure		403	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/permission-grants [post]
func GrantPermissionHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req grantPermissionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		// For department grants, resolve and store the dept_path redundantly so
		// CheckGrant stays a pure prefix comparison with no tree lookup.
		var deptPath string
		if req.SubjectType == models.PermissionSubjectDepartment {
			p, err := svc.ResolveDepartmentPath(req.SubjectID)
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": "failed to resolve department path from dept-sync",
					"code":  "dept_sync_unavailable",
				})
				return
			}
			deptPath = p
		}

		grant, err := svc.GrantPermission(req.PermissionCode, req.SubjectType, req.SubjectID, deptPath, operatorID)
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidSubjectType):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid subject type"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to grant permission"})
			}
			return
		}

		audit.Record(operatorID, audit.ActionPermissionGrantGrant, audit.TargetPermissionGrant, grant.ID, gin.H{
			"permissionCode": grant.PermissionCode,
			"subjectType":    grant.SubjectType,
			"subjectId":      grant.SubjectID,
			"deptPath":       grant.DeptPath,
		})

		c.JSON(http.StatusOK, gin.H{"grant": grant})
	}
}

// RevokePermissionHandler godoc
//
//	@Summary		Revoke fine-grained permission
//	@Description	Revoke a permission grant by id (platform admin only)
//	@Tags			admin/permissions
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"Grant id"
//	@Success		200	{object}	object{success=bool}
//	@Failure		401	{object}	object{error=string}
//	@Failure		403	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/permission-grants/{id} [delete]
func RevokePermissionHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "grant id is required"})
			return
		}

		if err := svc.RevokePermission(id); err != nil {
			if errors.Is(err, ErrGrantNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "permission grant not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke permission"})
			return
		}

		audit.Record(operatorID, audit.ActionPermissionGrantRevoke, audit.TargetPermissionGrant, id, nil)

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

		allowed, perms, userID, err := svc.VerifyTokenWithUser(req.Token, req.Resource)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "token verification failed"})
			return
		}

		resp := verifyResponse{Allowed: allowed}
		if perms != nil {
			resp.Menus = perms.Menus
			resp.Capabilities = perms.Capabilities
		}
		// Attach a compact scope summary so an external consumer (e.g.
		// efficiency-dashboard) gets department visibility in the same call. Only
		// when the token is allowed and the userID resolved; scope resolution is
		// best-effort and never fails the verify.
		if allowed && userID != "" {
			if scope, sErr := svc.ResolveUserScope(userID); sErr == nil && scope != nil {
				resp.Scope = &scopeSummary{
					AllAccess:           scope.AllAccess,
					VisibleDeptPrefixes: scope.VisibleDeptPrefixes,
				}
			}
		}
		c.JSON(http.StatusOK, resp)
	}
}
