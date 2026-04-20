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

// RegisterRoutes mounts all Cloud Team REST and WebSocket endpoints.
//
//	REST base:  /api/team
//	WebSocket:  /ws/sessions/:id
func (m *Module) RegisterRoutes(apiGroup, wsGroup *gin.RouterGroup) {
	team := apiGroup.Group("/team")
	{
		sessions := team.Group("/sessions")
		{
			sessions.POST("", m.Handler.CreateSession)
			sessions.GET("", m.Handler.ListSessions)
			sessions.GET("/:id", m.Handler.GetSession)
			sessions.PATCH("/:id", m.Handler.UpdateSession)
			sessions.DELETE("/:id", m.Handler.DeleteSession)

			sessions.POST("/:id/members", m.Handler.JoinSession)
			sessions.GET("/:id/members", m.Handler.ListMembers)
			sessions.DELETE("/:id/members/:mid", m.Handler.LeaveSession)

			sessions.POST("/:id/tasks", m.Handler.SubmitTaskPlan)
			sessions.GET("/:id/tasks", m.Handler.ListTasks)
			sessions.POST("/:id/tasks/:taskId/terminate", m.Handler.TerminateTask)
			sessions.POST("/:id/decompose", m.Handler.DecomposeTask)

			sessions.GET("/:id/approvals", m.Handler.ListApprovals)

			sessions.POST("/:id/repos", m.Handler.RegisterRepo)
			sessions.GET("/:id/repos", m.Handler.QueryRepos)

			sessions.GET("/:id/progress", m.Handler.GetProgress)

			sessions.POST("/:id/explore", m.Handler.Explore)

			sessions.POST("/:id/leader/elect", m.Handler.ElectLeader)
			sessions.POST("/:id/leader/heartbeat", m.Handler.LeaderHeartbeat)
			sessions.GET("/:id/leader", m.Handler.GetLeader)
		}

		tasks := team.Group("/tasks")
		{
			tasks.GET("/:taskId", m.Handler.GetTask)
			tasks.PATCH("/:taskId", m.Handler.UpdateTask)
		}

		approvals := team.Group("/approvals")
		{
			approvals.PATCH("/:approvalId", m.Handler.RespondApproval)
		}
	}

	// WebSocket endpoint – auth is handled per-connection via ?token= query param
	// or by the upstream middleware that already set c.GetString("userId").
	wsGroup.GET("/sessions/:id", m.Handler.ServeWS)
}
