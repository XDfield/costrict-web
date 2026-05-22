package collaboration

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (m *Module) listSpacesHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := currentUserID(c)
		spaces, err := m.service.ListSpaces(userID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, SpacesResponse{Spaces: spaces})
	}
}

func (m *Module) createSpaceHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateSpaceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		space, err := m.service.CreateSpace(currentUserID(c), &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, SpaceResponse{Space: space})
	}
}

func (m *Module) getSpaceHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}

		member, _ := m.service.GetSpaceMember(space.ID, currentUserID(c))
		role := ""
		if member != nil {
			role = member.Role
		}

		members, _ := m.service.ListSpaceMembers(space.ID)
		c.JSON(http.StatusOK, SpaceResponse{Space: space, Members: members, MyRole: role})
	}
}

func (m *Module) updateSpaceHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UpdateSpaceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		slug := c.Param("slug")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}
		updated, err := m.service.UpdateSpace(space.ID, &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, SpaceResponse{Space: updated})
	}
}

func (m *Module) deleteSpaceHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}
		if err := m.service.DeleteSpace(space.ID); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusNoContent, gin.H{})
	}
}

func (m *Module) listMembersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}
		members, err := m.service.ListSpaceMembers(space.ID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, MembersResponse{Members: members})
	}
}

func (m *Module) addMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req AddMemberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		slug := c.Param("slug")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}
		member, err := m.service.AddSpaceMember(space.ID, &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, member)
	}
}

func (m *Module) removeMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		userID := c.Param("userId")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}
		if err := m.service.RemoveSpaceMember(space.ID, userID); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusNoContent, gin.H{})
	}
}

func (m *Module) listSpaceProjectsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			writeError(c, err)
			return
		}
		projects, err := m.service.ListSpaceProjects(space.ID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"projects": projects})
	}
}
