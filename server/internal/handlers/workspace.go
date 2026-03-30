package handlers

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// CreateWorkspaceHandler godoc
// @Summary      Create a new workspace
// @Description  Create a workspace for the authenticated user with at least one directory
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        body  body      services.CreateWorkspaceRequest  true  "Workspace creation data"
// @Success      201   {object}  object{workspace=services.WorkspaceWithDeviceStatus}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces [post]
func CreateWorkspaceHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req services.CreateWorkspaceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		workspace, err := svc.CreateWorkspace(userID, req)
		if err != nil {
			if errors.Is(err, services.ErrWorkspaceNameExists) {
				c.JSON(http.StatusConflict, gin.H{"error": "workspace name already exists"})
				return
			}
			if errors.Is(err, services.ErrWorkspaceDirectoryRequired) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "at least one directory is required"})
				return
			}
			if errors.Is(err, services.ErrDeviceNotFound) || errors.Is(err, services.ErrDeviceNotOwned) {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if errors.Is(err, services.ErrDirectoryPathDuplicate) {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"workspace": workspace})
	}
}

// ListWorkspacesHandler godoc
// @Summary      List user workspaces
// @Description  Get all workspaces for the authenticated user
// @Tags         workspaces
// @Produce      json
// @Success      200   {object}  object{workspaces=[]services.WorkspaceWithDeviceStatus}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces [get]
func ListWorkspacesHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaces, err := svc.ListWorkspaces(userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workspaces"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"workspaces": workspaces})
	}
}

// GetWorkspaceHandler godoc
// @Summary      Get workspace details
// @Description  Get details of a specific workspace including directories
// @Tags         workspaces
// @Produce      json
// @Param        workspaceID  path      string  true  "Workspace ID"
// @Success      200   {object}  object{workspace=services.WorkspaceWithDeviceStatus}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID} [get]
func GetWorkspaceHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")
		workspace, err := svc.GetWorkspace(workspaceID, userID)
		if err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get workspace"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"workspace": workspace})
	}
}

// GetDefaultWorkspaceHandler godoc
// @Summary      Get default workspace
// @Description  Get the default workspace for the authenticated user
// @Tags         workspaces
// @Produce      json
// @Success      200   {object}  object{workspace=services.WorkspaceWithDeviceStatus}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/default [get]
func GetDefaultWorkspaceHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspace, err := svc.GetDefaultWorkspace(userID)
		if err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "no default workspace found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get default workspace"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"workspace": workspace})
	}
}

// UpdateWorkspaceHandler godoc
// @Summary      Update workspace
// @Description  Update workspace information
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        workspaceID  path      string  true  "Workspace ID"
// @Param        body         body      services.UpdateWorkspaceRequest  true  "Workspace update data"
// @Success      200   {object}  object{workspace=services.WorkspaceWithDeviceStatus}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID} [put]
func UpdateWorkspaceHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")

		var req services.UpdateWorkspaceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		workspace, err := svc.UpdateWorkspace(workspaceID, userID, req)
		if err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			if errors.Is(err, services.ErrWorkspaceNameExists) {
				c.JSON(http.StatusConflict, gin.H{"error": "workspace name already exists"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workspace"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"workspace": workspace})
	}
}

// DeleteWorkspaceHandler godoc
// @Summary      Delete workspace
// @Description  Delete a workspace and all its directories
// @Tags         workspaces
// @Param        workspaceID  path      string  true  "Workspace ID"
// @Success      204
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID} [delete]
func DeleteWorkspaceHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")
		if err := svc.DeleteWorkspace(workspaceID, userID); err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			if errors.Is(err, services.ErrDefaultWorkspaceCannotDelete) {
				c.JSON(http.StatusForbidden, gin.H{"error": "cannot delete default workspace"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete workspace"})
			return
		}

		c.Status(http.StatusNoContent)
	}
}

// SetDefaultWorkspaceHandler godoc
// @Summary      Set default workspace
// @Description  Set a workspace as the default for the authenticated user
// @Tags         workspaces
// @Param        workspaceID  path      string  true  "Workspace ID"
// @Success      200   {object}  object{message=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID}/set-default [post]
func SetDefaultWorkspaceHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")
		if err := svc.SetDefaultWorkspace(workspaceID, userID); err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set default workspace"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Default workspace updated"})
	}
}

// AddWorkspaceDirectoryHandler godoc
// @Summary      Add directory to workspace
// @Description  Add a new directory to an existing workspace
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        workspaceID  path      string  true  "Workspace ID"
// @Param        body         body      services.CreateDirectoryRequest  true  "Directory creation data"
// @Success      201   {object}  object{directory=object}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID}/directories [post]
func AddWorkspaceDirectoryHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")

		var req services.CreateDirectoryRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		directory, err := svc.AddDirectory(workspaceID, userID, req)
		if err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add directory"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"directory": directory})
	}
}

// UpdateWorkspaceDirectoryHandler godoc
// @Summary      Update workspace directory
// @Description  Update a directory in a workspace
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        workspaceID   path      string  true  "Workspace ID"
// @Param        directoryID   path      string  true  "Directory ID"
// @Param        body          body      services.UpdateDirectoryRequest  true  "Directory update data"
// @Success      200   {object}  object{directory=object}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID}/directories/{directoryID} [put]
func UpdateWorkspaceDirectoryHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")
		directoryID := c.Param("directoryID")

		var req services.UpdateDirectoryRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		directory, err := svc.UpdateDirectory(workspaceID, directoryID, userID, req)
		if err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) || errors.Is(err, services.ErrWorkspaceDirectoryNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "directory not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update directory"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"directory": directory})
	}
}

// DeleteWorkspaceDirectoryHandler godoc
// @Summary      Delete workspace directory
// @Description  Delete a directory from a workspace
// @Tags         workspaces
// @Param        workspaceID   path      string  true  "Workspace ID"
// @Param        directoryID   path      string  true  "Directory ID"
// @Success      204
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID}/directories/{directoryID} [delete]
func DeleteWorkspaceDirectoryHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")
		directoryID := c.Param("directoryID")

		if err := svc.DeleteDirectory(workspaceID, directoryID, userID); err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) || errors.Is(err, services.ErrWorkspaceDirectoryNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "directory not found"})
				return
			}
			if errors.Is(err, services.ErrWorkspaceDirectoryRequired) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "workspace must have at least one directory"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete directory"})
			return
		}

		c.Status(http.StatusNoContent)
	}
}

// ReorderWorkspaceDirectoriesHandler godoc
// @Summary      Reorder workspace directories
// @Description  Reorder directories in a workspace
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        workspaceID   path      string  true  "Workspace ID"
// @Param        body          body      services.ReorderDirectoriesRequest  true  "Directory order data"
// @Success      200   {object}  object{message=string}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /workspaces/{workspaceID}/directories/reorder [post]
func ReorderWorkspaceDirectoriesHandler(svc *services.WorkspaceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString(middleware.UserIDKey)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		workspaceID := c.Param("workspaceID")

		var req services.ReorderDirectoriesRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := svc.ReorderDirectories(workspaceID, userID, req); err != nil {
			if errors.Is(err, services.ErrWorkspaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reorder directories"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Directories reordered"})
	}
}

