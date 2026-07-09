package team

import (
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Module is the entry point for the Cloud Team feature.
// Initialise it with New() and call RegisterRoutes() to wire it in.
type Module struct {
	Store   *Store
	Hub     *Hub
	Handler *Handler
}

func New(db *gorm.DB, rc *redis.Client) *Module {
	store := NewStore(db)
	hub := NewHub(rc)
	handler := NewHandler(store, hub)
	return &Module{
		Store:   store,
		Hub:     hub,
		Handler: handler,
	}
}

// RegisterRoutes was the entry point for Cloud Team REST and WebSocket endpoints.
//
// 已废弃，这个不用了。
// The Cloud Team feature has been deprecated; no routes are registered here anymore.
//
//	REST base:  /api/team
//	WebSocket:  /ws/sessions/:id
func (m *Module) RegisterRoutes(apiGroup, wsGroup *gin.RouterGroup) {
	// 已废弃，这个不用了。
	// All Cloud Team REST and WebSocket endpoints are disabled.
	return
}
