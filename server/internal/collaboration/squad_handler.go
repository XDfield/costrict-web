package collaboration

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func (m *Module) listSquadsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := getSpaceID(c)
		includeArchived, _ := strconv.ParseBool(c.Query("includeArchived"))
		squads, err := m.service.ListSquads(spaceID, includeArchived)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, SquadsResponse{Squads: squads})
	}
}

func (m *Module) createSquadHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateSquadRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		spaceID := getSpaceID(c)
		squad, err := m.service.CreateSquad(spaceID, &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, SquadResponse{Squad: squad})
	}
}

func (m *Module) getSquadHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := getSpaceID(c)
		squadID := c.Param("id")
		squad, err := m.service.GetSquad(spaceID, squadID)
		if err != nil {
			writeError(c, err)
			return
		}
		members, _ := m.service.ListSquadMembers(squad.ID)
		c.JSON(http.StatusOK, SquadResponse{Squad: squad, Members: members})
	}
}

func (m *Module) updateSquadHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UpdateSquadRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		spaceID := getSpaceID(c)
		squadID := c.Param("id")
		squad, err := m.service.UpdateSquad(spaceID, squadID, &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, SquadResponse{Squad: squad})
	}
}

func (m *Module) deleteSquadHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := getSpaceID(c)
		squadID := c.Param("id")
		if err := m.service.DeleteSquad(spaceID, squadID); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusNoContent, gin.H{})
	}
}

func (m *Module) listSquadMembersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		squadID := c.Param("id")
		members, err := m.service.ListSquadMembers(squadID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, SquadMembersResponse{Members: members})
	}
}

func (m *Module) addSquadMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req AddSquadMemberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		squadID := c.Param("id")
		member, err := m.service.AddSquadMember(squadID, &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, member)
	}
}

func (m *Module) removeSquadMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		squadID := c.Param("id")
		userID := c.Param("userId")
		if err := m.service.RemoveSquadMember(squadID, userID); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusNoContent, gin.H{})
	}
}
