package collaboration

import (
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	spaceIDKey   = "collabSpaceID"
	spaceRoleKey = "collabSpaceRole"
)

type Module struct {
	service *CollaborationService
}

func New(db *gorm.DB) *Module {
	return &Module{service: NewCollaborationService(db)}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
	// Spaces (no space context required)
	spaces := apiGroup.Group("/spaces")
	{
		spaces.GET("", m.listSpacesHandler())
		spaces.POST("", m.createSpaceHandler())
		spaces.GET("/:slug", m.getSpaceHandler())
		spaces.PUT("/:slug", m.requireSpaceAdmin(), m.updateSpaceHandler())
		spaces.DELETE("/:slug", m.requireSpaceOwner(), m.deleteSpaceHandler())
		spaces.GET("/:slug/members", m.requireSpaceMember(), m.listMembersHandler())
		spaces.POST("/:slug/members", m.requireSpaceAdmin(), m.addMemberHandler())
		spaces.DELETE("/:slug/members/:userId", m.requireSpaceAdmin(), m.removeMemberHandler())
	}

	// Issues (require space context)
	issues := apiGroup.Group("/issues")
	issues.Use(m.spaceContextMiddleware())
	{
		issues.GET("", m.requireSpaceMember(), m.listIssuesHandler())
		issues.POST("", m.requireSpaceMember(), m.createIssueHandler())
		issues.GET("/:id", m.requireSpaceMember(), m.getIssueHandler())
		issues.PUT("/:id", m.requireSpaceMember(), m.updateIssueHandler())
		issues.DELETE("/:id", m.requireSpaceMember(), m.deleteIssueHandler())
		issues.GET("/:id/comments", m.requireSpaceMember(), m.listCommentsHandler())
		issues.POST("/:id/comments", m.requireSpaceMember(), m.createCommentHandler())
	}

	// Squads (require space context)
	squads := apiGroup.Group("/squads")
	squads.Use(m.spaceContextMiddleware())
	{
		squads.GET("", m.requireSpaceMember(), m.listSquadsHandler())
		squads.POST("", m.requireSpaceMember(), m.createSquadHandler())
		squads.GET("/:id", m.requireSpaceMember(), m.getSquadHandler())
		squads.PUT("/:id", m.requireSpaceMember(), m.updateSquadHandler())
		squads.DELETE("/:id", m.requireSpaceMember(), m.deleteSquadHandler())
		squads.GET("/:id/members", m.requireSpaceMember(), m.listSquadMembersHandler())
		squads.POST("/:id/members", m.requireSpaceMember(), m.addSquadMemberHandler())
		squads.DELETE("/:id/members/:userId", m.requireSpaceMember(), m.removeSquadMemberHandler())
	}
}

// ------------------------------------------------------------------
// Middleware helpers
// ------------------------------------------------------------------

func currentUserID(c *gin.Context) string {
	return c.GetString(middleware.UserIDKey)
}

func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrSpaceNotFound), errors.Is(err, ErrIssueNotFound), errors.Is(err, ErrSquadNotFound), errors.Is(err, ErrSquadMemberNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrNotSpaceMember), errors.Is(err, ErrCannotRemoveOwner), errors.Is(err, ErrLastAdmin):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, ErrSpaceSlugExists), errors.Is(err, ErrInvalidRole), errors.Is(err, ErrMemberExists), errors.Is(err, ErrSquadMemberExists):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
	}
}

// spaceContextMiddleware reads X-Space-Slug header or ?spaceSlug query param
// and injects spaceID / memberRole into gin context.
func (m *Module) spaceContextMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := strings.TrimSpace(c.GetHeader("X-Space-Slug"))
		if slug == "" {
			slug = strings.TrimSpace(c.Query("spaceSlug"))
		}
		if slug == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "space slug is required (X-Space-Slug header or spaceSlug query param)"})
			c.Abort()
			return
		}

		space, err := m.service.GetSpaceBySlug(slug)
		if err != nil {
			if errors.Is(err, ErrSpaceNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			c.Abort()
			return
		}

		member, err := m.service.GetSpaceMember(space.ID, currentUserID(c))
		if err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": ErrNotSpaceMember.Error()})
			c.Abort()
			return
		}

		c.Set(spaceIDKey, space.ID)
		c.Set(spaceRoleKey, member.Role)
		c.Next()
	}
}

func (m *Module) requireSpaceMember() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, exists := c.Get(spaceIDKey)
		if !exists {
			// Try to resolve from URL param for routes that don't use spaceContextMiddleware
			slug := c.Param("slug")
			if slug == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "space context required"})
				c.Abort()
				return
			}
			space, err := m.service.GetSpaceBySlug(slug)
			if err != nil {
				writeError(c, err)
				c.Abort()
				return
			}
			_, err = m.service.GetSpaceMember(space.ID, currentUserID(c))
			if err != nil {
				c.JSON(http.StatusForbidden, gin.H{"error": ErrNotSpaceMember.Error()})
				c.Abort()
				return
			}
			c.Set(spaceIDKey, space.ID)
			c.Set(spaceRoleKey, "member")
		} else {
			role, _ := c.Get(spaceRoleKey)
			if role == "" {
				c.JSON(http.StatusForbidden, gin.H{"error": ErrNotSpaceMember.Error()})
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

func (m *Module) requireSpaceAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, exists := c.Get(spaceIDKey)
		if !exists {
			slug := c.Param("slug")
			if slug == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "space context required"})
				c.Abort()
				return
			}
			space, err := m.service.GetSpaceBySlug(slug)
			if err != nil {
				writeError(c, err)
				c.Abort()
				return
			}
			member, err := m.service.GetSpaceMember(space.ID, currentUserID(c))
			if err != nil {
				c.JSON(http.StatusForbidden, gin.H{"error": ErrNotSpaceMember.Error()})
				c.Abort()
				return
			}
			c.Set(spaceIDKey, space.ID)
			c.Set(spaceRoleKey, member.Role)
		}

		role, _ := c.Get(spaceRoleKey)
		roleStr, _ := role.(string)
		if roleStr != "owner" && roleStr != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": ErrPermissionDenied.Error()})
			c.Abort()
			return
		}
		c.Next()
	}
}

func (m *Module) requireSpaceOwner() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, exists := c.Get(spaceIDKey)
		if !exists {
			slug := c.Param("slug")
			if slug == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "space context required"})
				c.Abort()
				return
			}
			space, err := m.service.GetSpaceBySlug(slug)
			if err != nil {
				writeError(c, err)
				c.Abort()
				return
			}
			member, err := m.service.GetSpaceMember(space.ID, currentUserID(c))
			if err != nil {
				c.JSON(http.StatusForbidden, gin.H{"error": ErrNotSpaceMember.Error()})
				c.Abort()
				return
			}
			c.Set(spaceIDKey, space.ID)
			c.Set(spaceRoleKey, member.Role)
		}

		role, _ := c.Get(spaceRoleKey)
		roleStr, _ := role.(string)
		if roleStr != "owner" {
			c.JSON(http.StatusForbidden, gin.H{"error": ErrPermissionDenied.Error()})
			c.Abort()
			return
		}
		c.Next()
	}
}

func getSpaceID(c *gin.Context) string {
	id, _ := c.Get(spaceIDKey)
	s, _ := id.(string)
	return s
}
