package team

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  DefaultWSReadBufferSize,
	WriteBufferSize: DefaultWSWriteBufferSize,
	CheckOrigin: func(r *http.Request) bool {
		return true // origin check handled by upstream middleware
	},
}

// Handler holds the dependencies for all team HTTP/WS endpoints.
type Handler struct {
	store              *Store
	hub                *Hub
	pushAssignedTaskFn func(ctx context.Context, sessionID string, machineID string, userID string, task TeamTask) error
}

func NewHandler(store *Store, hub *Hub) *Handler {
	return &Handler{store: store, hub: hub}
}

func (h *Handler) SetAssignedTaskPusher(fn func(ctx context.Context, sessionID string, machineID string, userID string, task TeamTask) error) {
	h.pushAssignedTaskFn = fn
}

// ─── Session ───────────────────────────────────────────────────────────────

// CreateSession godoc
// @Summary      Create team session
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        body  body  object{name=string}  true  "Session data"
// @Success      201   {object}  TeamSession
// @Router       /team/sessions [post]
func (h *Handler) CreateSession(c *gin.Context) {
	userID := c.GetString("userId")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	sess := &TeamSession{
		ID:        uuid.New().String(),
		Name:      req.Name,
		CreatorID: userID,
		Status:    SessionStatusActive,
	}
	if err := h.store.CreateSession(sess); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}
	c.JSON(http.StatusCreated, sess)
}

// GetSession godoc
// @Summary      Get team session
// @Tags         team
// @Produce      json
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  TeamSession
// @Router       /team/sessions/:id [get]
func (h *Handler) GetSession(c *gin.Context) {
	sess, err := h.store.GetSession(c.Param("id"))
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.JSON(http.StatusOK, sess)
}

// ListSessions godoc
// @Summary      List sessions for the current user
// @Tags         team
// @Produce      json
// @Success      200  {object}  object{sessions=[]TeamSession}
// @Router       /team/sessions [get]
func (h *Handler) ListSessions(c *gin.Context) {
	userID := c.GetString("userId")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	sessions, err := h.store.ListSessionsByCreator(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

// UpdateSession godoc
// @Summary      Update team session status
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{status=string}  true  "Status"
// @Success      200   {object}  TeamSession
// @Router       /team/sessions/:id [patch]
// requireSessionLeader checks that the caller is the session creator or the current
// leader. Returns the session on success, writes error response and returns nil on failure.
func (h *Handler) requireSessionLeader(c *gin.Context) *TeamSession {
	sessionID := c.Param("id")
	sess, err := h.store.GetSession(sessionID)
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return nil
	}
	userID := c.GetString("userId")
	leaderMachineID := h.hub.GetLeaderMachineID(sessionID)
	if leaderMachineID == "" {
		leaderMachineID = sess.LeaderMachineID
	}
	if sess.CreatorID != userID && leaderMachineID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the session creator or leader can perform this action"})
		return nil
	}
	return sess
}

func (h *Handler) UpdateSession(c *gin.Context) {
	sess := h.requireSessionLeader(c)
	if sess == nil {
		return
	}
	var req struct {
		Status string `json:"status"`
		Name   string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	updates := map[string]any{}
	if req.Status != "" {
		updates["status"] = req.Status
	}
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
		return
	}

	if err := h.store.UpdateSession(sess.ID, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		return
	}

	updated, _ := h.store.GetSession(sess.ID)
	c.JSON(http.StatusOK, updated)
}

// DeleteSession godoc
// @Summary      Close / delete team session
// @Tags         team
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  object{message=string}
// @Router       /team/sessions/:id [delete]
func (h *Handler) DeleteSession(c *gin.Context) {
	if sess := h.requireSessionLeader(c); sess == nil {
		return
	}
	sessionID := c.Param("id")
	if err := h.store.DeleteSession(sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "session deleted"})
}

// ensureSessionMember upserts a machine membership for a session.
// It revives soft-deleted rows and also supports legacy schemas where machine_id
// was globally unique across sessions.
func (h *Handler) ensureSessionMember(
	sessionID string,
	userID string,
	machineID string,
	machineName string,
) (*TeamSessionMember, error) {
	// Fast path: already joined in this session.
	existing, err := h.store.GetMemberByMachine(sessionID, machineID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		now := time.Now()
		if err := h.store.UpdateMember(existing.ID, map[string]any{
			"user_id":        userID,
			"machine_name":   machineName,
			"status":         MemberStatusOnline,
			"last_heartbeat": now,
		}); err != nil {
			return nil, err
		}
		return h.store.GetMember(existing.ID)
	}

	now := time.Now()
	revive := func(memberID string) (*TeamSessionMember, error) {
		if err := h.store.db.Unscoped().
			Model(&TeamSessionMember{}).
			Where("id = ?", memberID).
			Updates(map[string]any{
				"session_id":     sessionID,
				"user_id":        userID,
				"machine_id":     machineID,
				"machine_name":   machineName,
				"role":           MemberRoleTeammate,
				"status":         MemberStatusOnline,
				"connected_at":   now,
				"last_heartbeat": now,
				"deleted_at":     nil,
			}).Error; err != nil {
			return nil, err
		}
		return h.store.GetMember(memberID)
	}

	// If a soft-deleted row exists for the same session+machine, revive it.
	softDeleted, err := h.store.GetMemberByMachineUnscoped(sessionID, machineID)
	if err != nil {
		return nil, err
	}
	if softDeleted != nil {
		return revive(softDeleted.ID)
	}

	member := &TeamSessionMember{
		ID:            uuid.New().String(),
		SessionID:     sessionID,
		UserID:        userID,
		MachineID:     machineID,
		MachineName:   machineName,
		Role:          MemberRoleTeammate,
		Status:        MemberStatusOnline,
		ConnectedAt:   now,
		LastHeartbeat: now,
	}
	createErr := h.store.CreateMember(member)
	if createErr == nil {
		return member, nil
	}

	// Legacy fallback: global machine uniqueness (or stale row) blocked insert.
	legacy, err := h.store.GetMemberByMachineAnySessionUnscoped(machineID)
	if err != nil {
		return nil, createErr
	}
	if legacy != nil {
		return revive(legacy.ID)
	}
	return nil, createErr
}

// assignPendingTasksIfPossible re-runs scheduling for pending tasks and emits
// fresh task.assigned events when members come online.
func (h *Handler) assignPendingTasksIfPossible(sessionID string) {
	pending, err := h.store.ListPendingTasks(sessionID)
	if err != nil || len(pending) == 0 {
		return
	}

	schedCtx, err := h.buildSchedulingContext(sessionID)
	if err != nil {
		return
	}

	scheduled := ScheduleTasks(*schedCtx, pending)
	var assigned []TeamTask
	for _, task := range scheduled {
		if task.AssignedMemberID == nil || task.Status != TaskStatusAssigned {
			continue
		}
		if err := h.store.UpdateTask(task.ID, map[string]any{
			"assigned_member_id": *task.AssignedMemberID,
			"status":             TaskStatusAssigned,
		}); err != nil {
			continue
		}
		assigned = append(assigned, task)
	}

	if len(assigned) > 0 {
		h.notifyAssignedMachines(sessionID, assigned)
	}
}

// ─── Members ───────────────────────────────────────────────────────────────

// JoinSession godoc
// @Summary      Join team session
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{machineId=string,machineName=string}  true  "Machine info"
// @Success      201   {object}  TeamSessionMember
// @Router       /team/sessions/:id/members [post]
func (h *Handler) JoinSession(c *gin.Context) {
	userID := c.GetString("userId")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	sessionID := c.Param("id")
	sess, err := h.store.GetSession(sessionID)
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	var req struct {
		MachineID   string `json:"machineId" binding:"required"`
		MachineName string `json:"machineName"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "machineId is required"})
		return
	}

	if h.hub.SessionConnCount(sessionID) >= DefaultMaxConnectionsPerSession {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "session is full"})
		return
	}

	member, err := h.ensureSessionMember(sessionID, userID, req.MachineID, req.MachineName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to join session"})
		return
	}
	h.assignPendingTasksIfPossible(sessionID)
	c.JSON(http.StatusCreated, member)
}

// ListMembers godoc
// @Summary      List session members
// @Tags         team
// @Produce      json
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  object{members=[]TeamSessionMember}
// @Router       /team/sessions/:id/members [get]
func (h *Handler) ListMembers(c *gin.Context) {
	members, err := h.store.ListMembers(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list members"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}

// LeaveSession godoc
// @Summary      Leave team session
// @Tags         team
// @Param        id    path  string  true  "Session ID"
// @Param        mid   path  string  true  "Member ID"
// @Success      200  {object}  object{message=string}
// @Router       /team/sessions/:id/members/:mid [delete]
func (h *Handler) LeaveSession(c *gin.Context) {
	if err := h.store.DeleteMember(c.Param("mid")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to leave session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "left session"})
}

// ─── Tasks ─────────────────────────────────────────────────────────────────

// SubmitTaskPlan godoc
// @Summary      Submit task plan (Leader)
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{tasks=[]TeamTask,fencingToken=integer}  true  "Task plan"
// @Success      201   {object}  object{tasks=[]TeamTask}
// @Router       /team/sessions/:id/tasks [post]
func (h *Handler) SubmitTaskPlan(c *gin.Context) {
	sessionID := c.Param("id")

	var req struct {
		Tasks        []TeamTask `json:"tasks" binding:"required"`
		FencingToken int64      `json:"fencingToken"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Tasks) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tasks array is required"})
		return
	}

	if !h.hub.ValidateFencingToken(sessionID, req.FencingToken) {
		c.JSON(http.StatusConflict, gin.H{"error": "stale leader: fencing token rejected"})
		return
	}

	for i := range req.Tasks {
		// Respect a client-supplied ID (needed for dependency DAGs where tasks
		// in the same batch reference each other by ID). Only generate a new UUID
		// when the client did not provide one.
		if req.Tasks[i].ID == "" {
			req.Tasks[i].ID = uuid.New().String()
		}
		req.Tasks[i].SessionID = sessionID
		req.Tasks[i].Status = TaskStatusPending
		if req.Tasks[i].Priority == 0 {
			req.Tasks[i].Priority = 5
		}
		if req.Tasks[i].MaxRetries == 0 {
			req.Tasks[i].MaxRetries = 3
		}
	}

	// Auto-assign tasks via P1-P5 repo affinity scheduling
	schedCtx, schedErr := h.buildSchedulingContext(sessionID)
	if schedErr == nil {
		req.Tasks = ScheduleTasks(*schedCtx, req.Tasks)
	}

	if err := h.store.CreateTasks(req.Tasks); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create tasks"})
		return
	}

	// Notify assigned machines
	for _, task := range req.Tasks {
		if task.AssignedMemberID == nil {
			continue
		}
		member, _ := h.store.GetMember(*task.AssignedMemberID)
		if member == nil {
			continue
		}
		h.dispatchAssignedTaskToMachine(sessionID, member, task)
	}

	c.JSON(http.StatusCreated, gin.H{"tasks": req.Tasks})
}

// DecomposeTask godoc
// @Summary      Decompose a prompt into sub-tasks using LLM (Leader)
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  DecomposeRequest  true  "Prompt to decompose"
// @Success      201   {object}  object{tasks=[]TeamTask}
// @Failure      503   {object}  object{error=string}
// @Router       /team/sessions/:id/decompose [post]
func (h *Handler) DecomposeTask(c *gin.Context) {
	sessionID := c.Param("id")
	sess, err := h.store.GetSession(sessionID)
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	var req DecomposeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}

	// Validate fencing token — only the current leader may decompose
	if req.FencingToken != 0 {
		if !h.hub.ValidateFencingToken(sessionID, req.FencingToken) {
			c.JSON(http.StatusConflict, gin.H{"error": "stale leader: fencing token rejected"})
			return
		}
	}

	createAndBroadcastFallbackTasks := func() ([]TeamTask, error) {
		tasks := buildFallbackTasks(req.Prompt, sessionID)
		schedCtx, schedErr := h.buildSchedulingContext(sessionID)
		if schedErr == nil {
			tasks = ScheduleTasks(*schedCtx, tasks)
		}
		if req.DryRun {
			return tasks, nil
		}
		if storeErr := h.store.CreateTasks(tasks); storeErr != nil {
			return nil, storeErr
		}
		h.hub.Broadcast(sessionID, newEvent(EventTaskPlanSubmit, sessionID, map[string]any{"tasks": tasks}))
		h.notifyAssignedMachines(sessionID, tasks)
		return tasks, nil
	}

	// Pick an online teammate to delegate decomposition to
	_, targetMachineID, err := pickDecomposeTarget(h.hub, h.store, sessionID)
	if err != nil {
		// No teammate available — fallback to a single task
		tasks, storeErr := createAndBroadcastFallbackTasks()
		if storeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create tasks"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"tasks": tasks})
		return
	}
	leaderMachineID := h.hub.GetLeaderMachineID(sessionID)
	if leaderMachineID == "" {
		leaderMachineID = sess.LeaderMachineID
	}
	// If decomposition target falls back to the leader machine itself, avoid
	// waiting for a decompose.result loop that may not exist in single-device mode.
	if leaderMachineID != "" && targetMachineID == leaderMachineID {
		tasks, storeErr := createAndBroadcastFallbackTasks()
		if storeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create tasks"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"tasks":    tasks,
			"degraded": true,
			"reason":   "single_leader_fallback",
			"message":  "no online teammate available; used fallback single-task decomposition",
		})
		return
	}

	requestID := uuid.New().String()

	// Register a channel to receive the decompose result
	resultCh := h.hub.RegisterDecompose(requestID)
	defer h.hub.CancelDecompose(requestID)

	// Send decompose.request to the target teammate via WS
	evt := newEvent(EventDecomposeRequest, sessionID, map[string]any{
		"requestId": requestID,
		"prompt":    req.Prompt,
		"context":   req.Context,
	})
	h.hub.SendToMachine(sessionID, targetMachineID, evt)

	// Wait up to 60 s for the decompose.result
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	select {
	case result := <-resultCh:
		// Parse the result items into TeamTask records
		itemsRaw, ok := result.Payload["tasks"].([]any)
		if !ok || len(itemsRaw) == 0 {
			tasks, storeErr := createAndBroadcastFallbackTasks()
			if storeErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create tasks"})
				return
			}
			c.JSON(http.StatusCreated, gin.H{"tasks": tasks})
			return
		}

		var tasks []TeamTask
		for _, raw := range itemsRaw {
			data, _ := json.Marshal(raw)
			var item DecomposeResultItem
			if json.Unmarshal(data, &item) != nil || item.Description == "" {
				continue
			}
			tasks = append(tasks, item.toTeamTask(sessionID))
		}
		if len(tasks) == 0 {
			tasks = buildFallbackTasks(req.Prompt, sessionID)
		}

		// Auto-assign tasks via P1-P5 repo affinity scheduling
		schedCtx, schedErr := h.buildSchedulingContext(sessionID)
		if schedErr == nil {
			tasks = ScheduleTasks(*schedCtx, tasks)
		}

		if req.DryRun {
			c.JSON(http.StatusCreated, gin.H{"tasks": tasks, "dryRun": true})
			return
		}

		if storeErr := h.store.CreateTasks(tasks); storeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create tasks"})
			return
		}

		// Broadcast task plan to all session members
		h.hub.Broadcast(sessionID, newEvent(EventTaskPlanSubmit, sessionID, map[string]any{"tasks": tasks}))

		// Notify assigned machines
		for _, task := range tasks {
			if task.AssignedMemberID == nil {
				continue
			}
			member, _ := h.store.GetMember(*task.AssignedMemberID)
			if member == nil {
				continue
			}
			h.dispatchAssignedTaskToMachine(sessionID, member, task)
		}

		c.JSON(http.StatusCreated, gin.H{"tasks": tasks})

	case <-ctx.Done():
		// Timeout — fallback to single task, but still return success with a degraded flag.
		tasks, storeErr := createAndBroadcastFallbackTasks()
		if storeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create tasks"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"tasks":    tasks,
			"degraded": true,
			"reason":   "decompose_timeout",
			"message":  "decompose request timed out (teammate did not respond within 60s); used fallback single-task decomposition",
		})
	}
}

// ListTasks godoc
// @Summary      List session tasks
// @Tags         team
// @Produce      json
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  object{tasks=[]TeamTask}
// @Router       /team/sessions/:id/tasks [get]
func (h *Handler) ListTasks(c *gin.Context) {
	tasks, err := h.store.ListTasks(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

// TerminateTask godoc
// @Summary      Terminate a task (Leader)
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id      path  string  true  "Session ID"
// @Param        taskId  path  string  true  "Task ID"
// @Param        body    body  object{reason=string,fencingToken=integer}  false  "Termination options"
// @Success      200  {object}  TeamTask
// @Router       /team/sessions/:id/tasks/:taskId/terminate [post]
func (h *Handler) TerminateTask(c *gin.Context) {
	sessionID := c.Param("id")
	taskID := c.Param("taskId")

	sess, err := h.store.GetSession(sessionID)
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	var req struct {
		Reason       string `json:"reason"`
		FencingToken int64  `json:"fencingToken"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		// allow empty body
		req = struct {
			Reason       string `json:"reason"`
			FencingToken int64  `json:"fencingToken"`
		}{}
	}

	leaderMachineID := h.hub.GetLeaderMachineID(sessionID)
	if leaderMachineID == "" {
		leaderMachineID = sess.LeaderMachineID
	}
	if leaderMachineID != "" && req.FencingToken == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fencingToken is required"})
		return
	}
	if req.FencingToken != 0 && !h.hub.ValidateFencingToken(sessionID, req.FencingToken) {
		c.JSON(http.StatusConflict, gin.H{"error": "stale leader: fencing token rejected"})
		return
	}

	task, err := h.store.GetTask(taskID)
	if err != nil || task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if task.SessionID != sessionID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task does not belong to session"})
		return
	}

	reason := req.Reason
	if reason == "" {
		reason = "terminated by leader"
	}

	if err := h.store.UpdateTask(taskID, map[string]any{
		"status":        TaskStatusInterrupted,
		"error_message": reason,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to terminate task"})
		return
	}

	updated, _ := h.store.GetTask(taskID)
	if updated != nil {
		interruptedPayload := map[string]any{
			"taskId": taskID,
			"reason": reason,
		}
		if updated.AssignedMemberID != nil {
			if member, _ := h.store.GetMember(*updated.AssignedMemberID); member != nil {
				interruptedPayload["machineId"] = member.MachineID
				h.hub.SendToMachine(sessionID, member.MachineID, newEvent(EventTaskTerminate, sessionID, map[string]any{
					"taskId": taskID,
					"reason": reason,
				}))
			}
		}
		h.hub.Broadcast(sessionID, newEvent(EventTaskInterrupted, sessionID, interruptedPayload))
	}

	c.JSON(http.StatusOK, updated)
}

// GetTask godoc
// @Summary      Get task
// @Tags         team
// @Produce      json
// @Param        taskId  path  string  true  "Task ID"
// @Success      200  {object}  TeamTask
// @Router       /team/tasks/:taskId [get]
func (h *Handler) GetTask(c *gin.Context) {
	task, err := h.store.GetTask(c.Param("taskId"))
	if err != nil || task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	c.JSON(http.StatusOK, task)
}

// UpdateTask godoc
// @Summary      Update task status/result (Teammate)
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        taskId  path  string  true  "Task ID"
// @Param        body    body  object{status=string,result=object,errorMessage=string}  true  "Task update"
// @Success      200  {object}  TeamTask
// @Router       /team/tasks/:taskId [patch]
func (h *Handler) UpdateTask(c *gin.Context) {
	taskID := c.Param("taskId")

	var req struct {
		Status       string         `json:"status"`
		Result       map[string]any `json:"result"`
		ErrorMessage string         `json:"errorMessage"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	updates := map[string]any{}
	now := time.Now()
	if req.Status != "" {
		updates["status"] = req.Status
		switch req.Status {
		case TaskStatusRunning:
			updates["started_at"] = now
		case TaskStatusCompleted:
			updates["completed_at"] = now
		case TaskStatusFailed:
			updates["error_message"] = req.ErrorMessage
		}
	}
	if req.Result != nil {
		data, _ := json.Marshal(req.Result)
		updates["result"] = data
	}

	if err := h.store.UpdateTask(taskID, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update task"})
		return
	}

	task, _ := h.store.GetTask(taskID)

	// Broadcast status change to the session
	if task != nil {
		evt := newEvent(EventSessionUpdated, task.SessionID, map[string]any{
			"taskId": taskID,
			"status": req.Status,
		})
		h.hub.Broadcast(task.SessionID, evt)

		// Unlock any dependent tasks whose all dependencies are now completed
		if req.Status == TaskStatusCompleted {
			h.notifyUnlockedTasks(task.SessionID, taskID)
		}
	}

	c.JSON(http.StatusOK, task)
}

// ─── Approvals ─────────────────────────────────────────────────────────────

// ListApprovals godoc
// @Summary      List pending approvals
// @Tags         team
// @Produce      json
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  object{approvals=[]TeamApprovalRequest}
// @Router       /team/sessions/:id/approvals [get]
func (h *Handler) ListApprovals(c *gin.Context) {
	approvals, err := h.store.ListPendingApprovals(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list approvals"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"approvals": approvals})
}

// RespondApproval godoc
// @Summary      Respond to approval request (Leader)
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        approvalId  path  string  true  "Approval ID"
// @Param        body        body  object{status=string,feedback=string}  true  "Response"
// @Success      200         {object}  TeamApprovalRequest
// @Router       /team/approvals/:approvalId [patch]
func (h *Handler) RespondApproval(c *gin.Context) {
	approvalID := c.Param("approvalId")
	// Only the leader can respond to approvals
	approval, err := h.store.GetApproval(approvalID)
	if err != nil || approval == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "approval not found"})
		return
	}
	leaderMachineID := h.hub.GetLeaderMachineID(approval.SessionID)
	machineID := c.Query("machineId")
	if machineID == "" {
		machineID = c.GetString("machineId")
	}
	if leaderMachineID != "" && machineID != leaderMachineID {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the leader can respond to approvals"})
		return
	}

	var req struct {
		Status   string `json:"status" binding:"required"` // approved | rejected
		Feedback string `json:"feedback"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status is required"})
		return
	}

	now := time.Now()
	updates := map[string]any{
		"status":      req.Status,
		"feedback":    req.Feedback,
		"resolved_at": now,
	}
	if err := h.store.UpdateApproval(approvalID, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update approval"})
		return
	}

	approval, _ = h.store.GetApproval(approvalID)
	if approval != nil {
		// Notify the requesting member
		member, _ := h.store.GetMember(approval.RequesterID)
		if member != nil {
			evt := newEvent(EventApprovalResponse, approval.SessionID, map[string]any{
				"approvalId": approvalID,
				"status":     req.Status,
				"feedback":   req.Feedback,
			})
			h.hub.SendToMachine(approval.SessionID, member.MachineID, evt)
		}
	}

	c.JSON(http.StatusOK, approval)
}

// ─── Repo Affinity ─────────────────────────────────────────────────────────

// RegisterRepo godoc
// @Summary      Register / update local repository info (Teammate)
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  TeamRepoAffinity  true  "Repo info"
// @Success      200   {object}  TeamRepoAffinity
// @Router       /team/sessions/:id/repos [post]
func (h *Handler) RegisterRepo(c *gin.Context) {
	sessionID := c.Param("id")
	var req TeamRepoAffinity
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.SessionID = sessionID
	req.ID = uuid.New().String()
	if req.LastSyncedAt.IsZero() {
		req.LastSyncedAt = time.Now()
	}

	if err := h.store.UpsertRepoAffinity(&req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register repo"})
		return
	}
	c.JSON(http.StatusOK, req)
}

// QueryRepos godoc
// @Summary      Query repo affinity registry
// @Tags         team
// @Produce      json
// @Param        id          path   string  true   "Session ID"
// @Param        remoteUrl   query  string  false  "Filter by repo remote URL"
// @Param        memberId    query  string  false  "Filter by member ID"
// @Success      200  {object}  object{repos=[]TeamRepoAffinity}
// @Router       /team/sessions/:id/repos [get]
func (h *Handler) QueryRepos(c *gin.Context) {
	sessionID := c.Param("id")
	remoteURL := c.Query("remoteUrl")
	memberID := c.Query("memberId")

	var repos []TeamRepoAffinity
	var err error
	switch {
	case remoteURL != "":
		repos, err = h.store.ListReposByURL(sessionID, remoteURL)
	case memberID != "":
		repos, err = h.store.ListReposByMember(sessionID, memberID)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "remoteUrl or memberId query param required"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query repos"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"repos": repos})
}

// ─── Progress ──────────────────────────────────────────────────────────────

// GetProgress godoc
// @Summary      Get session progress snapshot
// @Tags         team
// @Produce      json
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  SessionProgress
// @Router       /team/sessions/:id/progress [get]
func (h *Handler) GetProgress(c *gin.Context) {
	progress, err := h.store.GetProgress(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get progress"})
		return
	}
	c.JSON(http.StatusOK, progress)
}

// ─── Leader election ───────────────────────────────────────────────────────

// ElectLeader godoc
// @Summary      Attempt leader election
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{machineId=string}  true  "Candidate"
// @Success      200   {object}  object{elected=bool,fencingToken=integer,leaderId=string}
// @Router       /team/sessions/:id/leader/elect [post]
func (h *Handler) ElectLeader(c *gin.Context) {
	sessionID := c.Param("id")
	var req struct {
		MachineID            string   `json:"machineId" binding:"required"`
		Repos                []string `json:"repos,omitempty"`
		HeartbeatSuccessRate float64  `json:"heartbeatSuccessRate,omitempty"`
		CPUIdlePercent       float64  `json:"cpuIdlePercent,omitempty"`
		MemoryFreeMB         float64  `json:"memoryFreeMB,omitempty"`
		RTTMs                float64  `json:"rttMs,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "machineId is required"})
		return
	}

	// Persist capability data regardless of election outcome
	m, _ := h.store.GetMemberByMachine(sessionID, req.MachineID)
	if m != nil {
		caps := LeaderCapability{
			MachineID:            req.MachineID,
			RepoURLs:             req.Repos,
			HeartbeatSuccessRate: req.HeartbeatSuccessRate,
			CPUIdlePercent:       req.CPUIdlePercent,
			MemoryFreeMB:         req.MemoryFreeMB,
			RTTMs:                req.RTTMs,
		}
		h.store.UpdateMemberCapabilities(m.ID, caps) //nolint:errcheck
	}

	token, elected := h.hub.TryAcquireLeader(sessionID, req.MachineID)

	// Compute leader score for the candidate
	var score *LeaderScore
	if elected {
		// Fetch session target repos for scoring context
		targetRepos, _ := h.store.GetSessionTargetRepos(sessionID)
		s := ScoreLeaderCandidate(LeaderCapability{
			MachineID:            req.MachineID,
			RepoURLs:             req.Repos,
			HeartbeatSuccessRate: req.HeartbeatSuccessRate,
			CPUIdlePercent:       req.CPUIdlePercent,
			MemoryFreeMB:         req.MemoryFreeMB,
			RTTMs:                req.RTTMs,
		}, targetRepos)
		score = &s

		// Update member role and persist leader info to DB
		h.store.UpdateSession(sessionID, map[string]any{ //nolint:errcheck
			"leader_machine_id": req.MachineID,
			"fencing_token":     token,
		})
		if m != nil {
			h.store.UpdateMember(m.ID, map[string]any{"role": MemberRoleLeader}) //nolint:errcheck
		}
		broadcastPayload := map[string]any{
			"leaderId":     req.MachineID,
			"fencingToken": token,
			"score":        score,
		}
		h.hub.Broadcast(sessionID, newEvent(EventLeaderElected, sessionID, broadcastPayload))

			// Reconcile orphaned tasks from previous leader crash
			go h.reconcileTasksOnLeaderChange(sessionID)

			// Send session snapshot to new leader
			snapshot := h.buildSessionSnapshot(sessionID)
			h.hub.SendToMachine(sessionID, req.MachineID, newEvent("leader.snapshot", sessionID, snapshot))
	}

	resp := gin.H{
		"elected":      elected,
		"fencingToken": token,
		"leaderId":     h.hub.GetLeaderMachineID(sessionID),
	}
	if score != nil {
		resp["score"] = score
	}
	c.JSON(http.StatusOK, resp)
}

// LeaderHeartbeat godoc
// @Summary      Leader lock renewal heartbeat
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{machineId=string}  true  "Leader identity"
// @Success      200   {object}  object{renewed=bool}
// @Router       /team/sessions/:id/leader/heartbeat [post]
func (h *Handler) LeaderHeartbeat(c *gin.Context) {
	sessionID := c.Param("id")
	var req struct {
		MachineID            string   `json:"machineId" binding:"required"`
		Repos                []string `json:"repos,omitempty"`
		HeartbeatSuccessRate float64  `json:"heartbeatSuccessRate,omitempty"`
		CPUIdlePercent       float64  `json:"cpuIdlePercent,omitempty"`
		MemoryFreeMB         float64  `json:"memoryFreeMB,omitempty"`
		RTTMs                float64  `json:"rttMs,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "machineId is required"})
		return
	}

	// Persist capability updates from the leader
	m, _ := h.store.GetMemberByMachine(sessionID, req.MachineID)
	if m != nil {
		caps := LeaderCapability{
			MachineID:            req.MachineID,
			RepoURLs:             req.Repos,
			HeartbeatSuccessRate: req.HeartbeatSuccessRate,
			CPUIdlePercent:       req.CPUIdlePercent,
			MemoryFreeMB:         req.MemoryFreeMB,
			RTTMs:                req.RTTMs,
		}
		h.store.UpdateMemberCapabilities(m.ID, caps) //nolint:errcheck
	}

	renewed := h.hub.RenewLeader(sessionID, req.MachineID)
	if !renewed {
		// Lock expired: broadcast leader expiry so teammates re-elect
		h.hub.Broadcast(sessionID, newEvent(EventLeaderExpired, sessionID, map[string]any{
			"expiredLeaderId": req.MachineID,
		}))
	}
	c.JSON(http.StatusOK, gin.H{"renewed": renewed})
}

// GetLeader godoc
// @Summary      Get current leader info
// @Tags         team
// @Produce      json
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  object{leaderId=string}
// @Router       /team/sessions/:id/leader [get]
func (h *Handler) GetLeader(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"leaderId": h.hub.GetLeaderMachineID(c.Param("id")),
	})
}

// ─── Remote Explore ────────────────────────────────────────────────────────

// Explore godoc
// @Summary      Synchronous remote code explore (Leader → Teammate)
// @Description  Leader sends explore queries targeting a specific Teammate machine.
//
//	The cloud server forwards the request via WebSocket, waits up to 30 s
//	for the result, and returns it synchronously.
//
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{targetMachineId=string,queries=[]object}  true  "Explore request"
// @Success      200   {object}  object{result=object}
// @Failure      504   {object}  object{error=string}
// @Router       /team/sessions/:id/explore [post]
func (h *Handler) Explore(c *gin.Context) {
	sessionID := c.Param("id")

	var req struct {
		TargetMachineID string `json:"targetMachineId" binding:"required"`
		Queries         []any  `json:"queries"`
		FencingToken    int64  `json:"fencingToken,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targetMachineId is required"})
		return
	}

	// Only the current leader may issue explore requests
	if req.FencingToken != 0 {
		if !h.hub.ValidateFencingToken(sessionID, req.FencingToken) {
			c.JSON(http.StatusConflict, gin.H{"error": "stale leader: fencing token rejected"})
			return
		}
	}

	requestID := uuid.New().String()

	// Register a channel to receive the explore result
	resultCh := h.hub.RegisterExplore(requestID)
	defer h.hub.CancelExplore(requestID)

	// Forward the explore.request to the target machine via WS (or backlog if offline)
	evt := newEvent(EventExploreRequest, sessionID, map[string]any{
		"requestId":       requestID,
		"targetMachineId": req.TargetMachineID,
		"fromMachineId":   h.hub.GetLeaderMachineID(sessionID),
		"queries":         req.Queries,
	})
	h.hub.SendToMachine(sessionID, req.TargetMachineID, evt)

	// Wait up to 30 s for the explore.result
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	select {
	case result := <-resultCh:
		c.JSON(http.StatusOK, gin.H{"result": result.Payload})
	case <-ctx.Done():
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "explore request timed out (target machine did not respond within 30s)"})
	}
}

// ─── WebSocket ─────────────────────────────────────────────────────────────

// ServeWS handles the WebSocket upgrade and event loop.
// WS /ws/sessions/:id?token=<auth>&machineId=<id>&userId=<id>&lastEventId=<id>
func (h *Handler) ServeWS(c *gin.Context) {
	sessionID := c.Param("id")
	machineID := c.Query("machineId")
	userID := c.Query("userId")
	if userID == "" {
		userID = c.GetString("userId") // set by auth middleware if JWT present
	}

	if machineID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "machineId query param required"})
		return
	}

	// Verify session exists
	sess, err := h.store.GetSession(sessionID)
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	wsConn := &WSConnection{
		ID:           uuid.New().String(),
		UserID:       userID,
		MachineID:    machineID,
		SessionID:    sessionID,
		Conn:         conn,
		Send:         make(chan []byte, DefaultSendChannelCapacity),
		Done:         make(chan struct{}),
		LastActivity: time.Now().UnixMilli(),
	}

	h.hub.Register(wsConn)
	h.hub.MarkPresence(sessionID, machineID)

	// Update member status to online
	if m, _ := h.store.GetMemberByMachine(sessionID, machineID); m != nil {
		h.store.UpdateMember(m.ID, map[string]any{ //nolint:errcheck
			"status":         MemberStatusOnline,
			"last_heartbeat": time.Now(),
		})
	}

	// Drain any queued backlog for this machine
	backlog := h.hub.DrainBacklog(sessionID, machineID)
	for _, evt := range backlog {
		if data, err := jsonMarshal(evt); err == nil {
			wsConn.Send <- data
		}
	}

	// Replay events since lastEventId (for reconnection catch-up)
	lastEventID := c.Query("lastEventId")
	if lastEventID != "" {
		replayed := h.hub.ReplayEvents(sessionID, lastEventID)
		for _, evt := range replayed {
			if data, err := jsonMarshal(evt); err == nil {
				select {
				case wsConn.Send <- data:
				default:
				}
			}
		}
	}

	// Notify session peers that this machine came online
	h.hub.Broadcast(sessionID, newEvent(EventTeammateStatus, sessionID, map[string]any{
		"machineId": machineID,
		"status":    MemberStatusOnline,
	}))

	go h.wsWritePump(wsConn)
	h.wsReadPump(wsConn) // blocks until connection closes
}

// wsWritePump serialises outbound messages from the Send channel to the wire.
func (h *Handler) wsWritePump(conn *WSConnection) {
	ticker := time.NewTicker(WSPingIntervalSec * time.Second)
	defer func() {
		ticker.Stop()
		conn.Conn.Close()
	}()

	writeDeadline := func() {
		conn.Conn.SetWriteDeadline(time.Now().Add(WSWriteWaitSec * time.Second)) //nolint:errcheck
	}

	for {
		select {
		case msg, ok := <-conn.Send:
			writeDeadline()
			if !ok {
				conn.Conn.WriteMessage(websocket.CloseMessage, []byte{}) //nolint:errcheck
				return
			}
			if err := conn.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}

		case <-ticker.C:
			writeDeadline()
			if err := conn.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-conn.Done:
			return
		}
	}
}

// wsReadPump reads inbound events from the client and dispatches them.
func (h *Handler) wsReadPump(conn *WSConnection) {
	defer func() {
		h.hub.Unregister(conn)
		close(conn.Done)
		conn.Conn.Close()

		// Mark member offline
		if m, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID); m != nil {
			h.store.UpdateMember(m.ID, map[string]any{"status": MemberStatusOffline}) //nolint:errcheck
		}

		// Interrupt tasks currently running on this machine
		h.interruptTasksForMember(conn.SessionID, conn.MachineID)

		// Release leader lock if this machine was the leader
		h.hub.ReleaseLeader(conn.SessionID, conn.MachineID)

		// Broadcast offline status
		h.hub.Broadcast(conn.SessionID, newEvent(EventTeammateStatus, conn.SessionID, map[string]any{
			"machineId": conn.MachineID,
			"status":    MemberStatusOffline,
		}))
	}()

	conn.Conn.SetReadDeadline(time.Now().Add(WSPongWaitSec * time.Second)) //nolint:errcheck
	conn.Conn.SetPongHandler(func(string) error {
		conn.Conn.SetReadDeadline(time.Now().Add(WSPongWaitSec * time.Second)) //nolint:errcheck
		conn.LastActivity = time.Now().UnixMilli()
		h.hub.MarkPresence(conn.SessionID, conn.MachineID)
		return nil
	})

	for {
		_, raw, err := conn.Conn.ReadMessage()
		if err != nil {
			return
		}
		conn.LastActivity = time.Now().UnixMilli()

		var evt CloudEvent
		if err := json.Unmarshal(raw, &evt); err != nil {
			continue
		}
		h.dispatchClientEvent(conn, evt)
	}
}

// dispatchClientEvent handles inbound CloudEvents from a connected machine.
func (h *Handler) dispatchClientEvent(conn *WSConnection, evt CloudEvent) {
	switch evt.Type {

	case EventSessionCreate:
		// Client creates a session over the WS connection.
		// The session UUID comes from the WS URL (:id). If the session doesn't
		// exist yet the server creates it and the connecting machine becomes leader.
		name, _ := evt.Payload["name"].(string)
		if name == "" {
			name = "Session " + conn.SessionID[:8]
		}
		existing, _ := h.store.GetSession(conn.SessionID)
		if existing == nil {
			sess := &TeamSession{
				ID:              conn.SessionID,
				Name:            name,
				CreatorID:       conn.UserID,
				Status:          SessionStatusActive,
				LeaderMachineID: conn.MachineID,
			}
			h.store.CreateSession(sess) //nolint:errcheck
		}
		// Attempt leader election for the creator
		token, elected := h.hub.TryAcquireLeader(conn.SessionID, conn.MachineID)
		if elected {
			h.store.UpdateSession(conn.SessionID, map[string]any{ //nolint:errcheck
				"leader_machine_id": conn.MachineID,
				"fencing_token":     token,
			})
			h.hub.Broadcast(conn.SessionID, newEvent(EventLeaderElected, conn.SessionID, map[string]any{
				"leaderId":     conn.MachineID,
				"fencingToken": token,
			}))
		}

	case EventSessionJoin:
		// Upsert the member record for this machine (idempotent reconnect support).
		machineName, _ := evt.Payload["machineName"].(string)
		h.ensureSessionMember(conn.SessionID, conn.UserID, conn.MachineID, machineName) //nolint:errcheck
		h.assignPendingTasksIfPossible(conn.SessionID)
		h.hub.Broadcast(conn.SessionID, newEvent(EventTeammateStatus, conn.SessionID, map[string]any{
			"machineId": conn.MachineID,
			"status":    MemberStatusOnline,
		}))

	case EventTaskPlanSubmit:
		// Leader submits a task plan over WS. Mirrors the REST SubmitTaskPlan logic.
		fencingTokenF, _ := evt.Payload["fencingToken"].(float64)
		if !h.hub.ValidateFencingToken(conn.SessionID, int64(fencingTokenF)) {
			h.hub.Send(conn.ID, newEvent(EventError, conn.SessionID, map[string]any{
				"message": "stale leader: fencing token rejected",
			}))
			return
		}
		tasksRaw, ok := evt.Payload["tasks"].([]any)
		if !ok || len(tasksRaw) == 0 {
			return
		}
		var tasks []TeamTask
		for _, tr := range tasksRaw {
			taskData, _ := json.Marshal(tr)
			var t TeamTask
			if json.Unmarshal(taskData, &t) == nil {
				if t.ID == "" {
					t.ID = uuid.New().String()
				}
				t.SessionID = conn.SessionID
				t.Status = TaskStatusPending
				if t.Priority == 0 {
					t.Priority = 5
				}
				if t.MaxRetries == 0 {
					t.MaxRetries = 3
				}
				tasks = append(tasks, t)
			}
		}
		if len(tasks) == 0 {
			return
		}
		// Auto-assign tasks via P1-P5 repo affinity scheduling
		schedCtx, schedErr := h.buildSchedulingContext(conn.SessionID)
		if schedErr == nil {
			tasks = ScheduleTasks(*schedCtx, tasks)
		}

		if err := h.store.CreateTasks(tasks); err != nil {
			return
		}
		for _, task := range tasks {
			if task.AssignedMemberID == nil {
				continue
			}
			member, _ := h.store.GetMember(*task.AssignedMemberID)
			if member == nil {
				continue
			}
			h.dispatchAssignedTaskToMachine(conn.SessionID, member, task)
		}

	case EventLeaderHeartbeat:
		// Parse optional capability updates from heartbeat event
		var hbcaps LeaderCapability
		hbcaps.MachineID = conn.MachineID
		if repos, ok := evt.Payload["repos"].([]any); ok {
			for _, r := range repos {
				if s, ok := r.(string); ok {
					hbcaps.RepoURLs = append(hbcaps.RepoURLs, s)
				}
			}
		}
		if v, ok := evt.Payload["heartbeatSuccessRate"].(float64); ok {
			hbcaps.HeartbeatSuccessRate = v
		}
		if v, ok := evt.Payload["cpuIdlePercent"].(float64); ok {
			hbcaps.CPUIdlePercent = v
		}
		if v, ok := evt.Payload["memoryFreeMB"].(float64); ok {
			hbcaps.MemoryFreeMB = v
		}
		if v, ok := evt.Payload["rttMs"].(float64); ok {
			hbcaps.RTTMs = v
		}
		if len(hbcaps.RepoURLs) > 0 || hbcaps.HeartbeatSuccessRate > 0 || hbcaps.CPUIdlePercent > 0 || hbcaps.MemoryFreeMB > 0 || hbcaps.RTTMs > 0 {
			m, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
			if m != nil {
				h.store.UpdateMemberCapabilities(m.ID, hbcaps) //nolint:errcheck
			}
		}
		h.hub.RenewLeader(conn.SessionID, conn.MachineID)
		h.hub.MarkPresence(conn.SessionID, conn.MachineID)

	case EventTaskProgress:
		taskID, _ := evt.Payload["taskId"].(string)
		if taskID != "" {
			h.store.UpdateTask(taskID, map[string]any{"status": TaskStatusRunning}) //nolint:errcheck
		}
		// Relay progress to all session members (so leader's dashboard updates)
		h.hub.Broadcast(conn.SessionID, evt)

	case EventTaskComplete:
		taskID, _ := evt.Payload["taskId"].(string)
		if taskID != "" {
			now := time.Now()
			result, _ := json.Marshal(evt.Payload["result"])
			h.store.UpdateTask(taskID, map[string]any{ //nolint:errcheck
				"status":       TaskStatusCompleted,
				"completed_at": now,
				"result":       result,
			})
			h.notifyUnlockedTasks(conn.SessionID, taskID)
		}
		h.hub.Broadcast(conn.SessionID, evt)

	case EventTaskFail:
		taskID, _ := evt.Payload["taskId"].(string)
		errMsg, _ := evt.Payload["errorMessage"].(string)
		if taskID != "" {
			retried, err := h.store.RetryTask(taskID)
			if err == nil && retried != nil {
				// Re-dispatch to the same assigned member
				if retried.AssignedMemberID != nil {
					member, _ := h.store.GetMember(*retried.AssignedMemberID)
					if member != nil {
						h.dispatchAssignedTaskToMachine(conn.SessionID, member, *retried)
					}
				}
			} else {
				h.store.UpdateTask(taskID, map[string]any{ //nolint:errcheck
					"status":        TaskStatusFailed,
					"error_message": errMsg,
				})
			}
		}
		h.hub.Broadcast(conn.SessionID, evt)

	case EventTaskClaim:
		taskID, _ := evt.Payload["taskId"].(string)
		if taskID != "" {
			member, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
			if member != nil {
				h.store.ClaimTask(taskID, member.ID) //nolint:errcheck
			}
		}

	case EventApprovalRequest:
		// Resolve the requesting member — RequesterID must be a TeamSessionMember UUID.
		requester, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
		if requester == nil {
			return // machine not registered as a session member; ignore
		}
		// Persist approval and push to leader
		approval := &TeamApprovalRequest{
			ID:          uuid.New().String(),
			SessionID:   conn.SessionID,
			RequesterID: requester.ID,
		}
		if toolName, ok := evt.Payload["toolName"].(string); ok {
			approval.ToolName = toolName
		}
		if desc, ok := evt.Payload["description"].(string); ok {
			approval.Description = desc
		}
		if risk, ok := evt.Payload["riskLevel"].(string); ok {
			approval.RiskLevel = risk
		} else {
			approval.RiskLevel = "medium"
		}
		if inp, ok := evt.Payload["toolInput"]; ok {
			data, _ := json.Marshal(inp)
			approval.ToolInput = data
		}
		h.store.CreateApproval(approval) //nolint:errcheck

		// Forward to the session's leader machine
		sess, _ := h.store.GetSession(conn.SessionID)
		if sess != nil && sess.LeaderMachineID != "" {
			fwd := newEvent(EventApprovalPush, conn.SessionID, map[string]any{
				"approval": approval,
			})
			h.hub.SendToMachine(conn.SessionID, sess.LeaderMachineID, fwd)
		}

	case EventApprovalRespond:
		approvalID, _ := evt.Payload["approvalId"].(string)
		status, _ := evt.Payload["status"].(string)
		feedback, _ := evt.Payload["feedback"].(string)
		if approvalID != "" && status != "" {
			now := time.Now()
			h.store.UpdateApproval(approvalID, map[string]any{ //nolint:errcheck
				"status":      status,
				"feedback":    feedback,
				"resolved_at": now,
			})
			approval, _ := h.store.GetApproval(approvalID)
			if approval != nil {
				member, _ := h.store.GetMember(approval.RequesterID)
				if member != nil {
					h.hub.SendToMachine(conn.SessionID, member.MachineID,
						newEvent(EventApprovalResponse, conn.SessionID, map[string]any{
							"approvalId": approvalID,
							"status":     status,
							"feedback":   feedback,
						}))
				}
			}
		}

	case EventMessageSend:
		to, _ := evt.Payload["to"].(string)
		if to == "broadcast" {
			h.hub.Broadcast(conn.SessionID, newEvent(EventMessageReceive, conn.SessionID, evt.Payload))
		} else if to != "" {
			h.hub.SendToMachine(conn.SessionID, to, newEvent(EventMessageReceive, conn.SessionID, evt.Payload))
		}

	case EventRepoRegister:
		repoURL, _ := evt.Payload["repoRemoteUrl"].(string)
		if repoURL == "" {
			return
		}
		member, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
		if member == nil {
			return
		}
		affinity := &TeamRepoAffinity{
			ID:            uuid.New().String(),
			SessionID:     conn.SessionID,
			MemberID:      member.ID,
			RepoRemoteURL: repoURL,
			LastSyncedAt:  time.Now(),
		}
		if path, ok := evt.Payload["repoLocalPath"].(string); ok {
			affinity.RepoLocalPath = path
		}
		if branch, ok := evt.Payload["currentBranch"].(string); ok {
			affinity.CurrentBranch = branch
		}
		if dirty, ok := evt.Payload["hasUncommittedChanges"].(bool); ok {
			affinity.HasUncommittedChanges = dirty
		}
		h.store.UpsertRepoAffinity(affinity) //nolint:errcheck

	case EventExploreRequest:
		// Route explore request to the target machine
		targetMachineID, _ := evt.Payload["targetMachineId"].(string)
		if targetMachineID != "" {
			h.hub.SendToMachine(conn.SessionID, targetMachineID, evt)
		}

	case EventExploreResult:
		requestID, _ := evt.Payload["requestId"].(string)
		// If a synchronous HTTP explore call is waiting, deliver to it.
		if requestID != "" {
			h.hub.DeliverExplore(requestID, evt)
		}
		// Also route back to the leader machine via WS (for streaming dashboards).
		fromMachineID, _ := evt.Payload["fromMachineId"].(string)
		if fromMachineID != "" {
			h.hub.SendToMachine(conn.SessionID, fromMachineID, evt)
		}

	case EventDecomposeResult:
		requestID, _ := evt.Payload["requestId"].(string)
		// If a synchronous HTTP decompose call is waiting, deliver to it.
		if requestID != "" {
			h.hub.DeliverDecompose(requestID, evt)
		}

	case EventLeaderElect:
		// Parse capability data from WS event (backward compatible)
		var caps LeaderCapability
		caps.MachineID = conn.MachineID
		if repos, ok := evt.Payload["repos"].([]any); ok {
			for _, r := range repos {
				if s, ok := r.(string); ok {
					caps.RepoURLs = append(caps.RepoURLs, s)
				}
			}
		}
		if v, ok := evt.Payload["heartbeatSuccessRate"].(float64); ok {
			caps.HeartbeatSuccessRate = v
		}
		if v, ok := evt.Payload["cpuIdlePercent"].(float64); ok {
			caps.CPUIdlePercent = v
		}
		if v, ok := evt.Payload["memoryFreeMB"].(float64); ok {
			caps.MemoryFreeMB = v
		}
		if v, ok := evt.Payload["rttMs"].(float64); ok {
			caps.RTTMs = v
		}

		// Persist capabilities regardless of election outcome
		m, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
		if m != nil {
			h.store.UpdateMemberCapabilities(m.ID, caps) //nolint:errcheck
		}

		token, elected := h.hub.TryAcquireLeader(conn.SessionID, conn.MachineID)
		if elected {
			targetRepos, _ := h.store.GetSessionTargetRepos(conn.SessionID)
			score := ScoreLeaderCandidate(caps, targetRepos)

			h.store.UpdateSession(conn.SessionID, map[string]any{ //nolint:errcheck
				"leader_machine_id": conn.MachineID,
				"fencing_token":     token,
			})
			if m != nil {
				h.store.UpdateMember(m.ID, map[string]any{"role": MemberRoleLeader}) //nolint:errcheck
			}
			h.hub.Broadcast(conn.SessionID, newEvent(EventLeaderElected, conn.SessionID, map[string]any{
				"leaderId":     conn.MachineID,
				"fencingToken": token,
				"score":        score,
			}))
		}
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// notifyUnlockedTasks unlocks dependent tasks whose all dependencies are now
// completed and pushes task.assigned events to their assigned machines.
func (h *Handler) notifyUnlockedTasks(sessionID, completedTaskID string) {
	unlocked, err := h.store.UnlockDependentTasks(sessionID, completedTaskID)
	if err != nil || len(unlocked) == 0 {
		return
	}
	for _, t := range unlocked {
		if t.AssignedMemberID == nil {
			continue
		}
		member, _ := h.store.GetMember(*t.AssignedMemberID)
		if member == nil {
			continue
		}
		h.dispatchAssignedTaskToMachine(sessionID, member, t)
	}
}

// interruptTasksForMember finds all running tasks assigned to a member
// whose machineID matches, transitions them to "interrupted", and broadcasts
// the change so the leader and other teammates see it immediately.
func (h *Handler) interruptTasksForMember(sessionID, machineID string) {
	member, _ := h.store.GetMemberByMachine(sessionID, machineID)
	if member == nil {
		return
	}
	tasks, err := h.store.ListTasks(sessionID)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if t.AssignedMemberID == nil || *t.AssignedMemberID != member.ID {
			continue
		}
		if t.Status != TaskStatusRunning && t.Status != TaskStatusClaimed && t.Status != TaskStatusAssigned {
			continue
		}
		h.store.UpdateTask(t.ID, map[string]any{ //nolint:errcheck
			"status": TaskStatusInterrupted,
		})
		h.hub.Broadcast(sessionID, newEvent(EventTaskInterrupted, sessionID, map[string]any{
			"taskId":    t.ID,
			"machineId": machineID,
		}))
	}
}

// ─── Auto-Explore for Context ──────────────────────────────────────────────

// autoExploreForContext runs lightweight file_tree explore queries against
// online teammates that hold repos referenced in this session. Results are
// aggregated and returned as context for task decomposition.
func (h *Handler) autoExploreForContext(sessionID, _ string) map[string]any {
	affinities, err := h.store.ListAllRepoAffinities(sessionID)
	if err != nil || len(affinities) == 0 {
		return nil
	}

	members, err := h.store.ListMembers(sessionID)
	if err != nil || len(members) == 0 {
		return nil
	}

	// Build repoURL → first online teammate machineID mapping
	repoToMachine := make(map[string]string)
	for _, a := range affinities {
		if _, exists := repoToMachine[a.RepoRemoteURL]; exists {
			continue
		}
		for _, m := range members {
			if m.MachineID != "" && h.hub.IsMachineOnline(sessionID, m.MachineID) {
				repoToMachine[a.RepoRemoteURL] = m.MachineID
				break
			}
		}
	}

	if len(repoToMachine) == 0 {
		return nil
	}

	type exploreResult struct {
		repoURL string
		data    map[string]any
	}
	resultCh := make(chan exploreResult, len(repoToMachine))

	for repoURL, machineID := range repoToMachine {
		go func(rURL, mID string) {
			requestID := uuid.New().String()
			ch := h.hub.RegisterExplore(requestID)
			defer h.hub.CancelExplore(requestID)

			evt := newEvent(EventExploreRequest, sessionID, map[string]any{
				"requestId":       requestID,
				"targetMachineId": mID,
				"fromMachineId":   h.hub.GetLeaderMachineID(sessionID),
				"queries": []map[string]any{
					{"type": "file_tree", "params": map[string]any{}},
				},
			})
			h.hub.SendToMachine(sessionID, mID, evt)

			select {
			case result := <-ch:
				resultCh <- exploreResult{repoURL: rURL, data: result.Payload}
			case <-time.After(10 * time.Second):
				resultCh <- exploreResult{repoURL: rURL, data: nil}
			}
		}(repoURL, machineID)
	}

	results := make([]map[string]any, 0, len(repoToMachine))
	for i := 0; i < len(repoToMachine); i++ {
		r := <-resultCh
		if r.data != nil {
			results = append(results, map[string]any{
				"repoRemoteUrl": r.repoURL,
				"result":        r.data,
			})
		}
	}

	if len(results) == 0 {
		return nil
	}
	return map[string]any{"exploreResults": results}
}

// ─── Orchestrate ───────────────────────────────────────────────────────────

// OrchestrateTask godoc
// @Summary      Orchestrate explore→decompose→schedule pipeline
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{prompt=string,fencingToken=integer}  true  "Orchestrate request"
// @Success      201   {object}  object{tasks=[]TeamTask,dryRun=bool}
// @Router       /team/sessions/:id/orchestrate [post]
func (h *Handler) OrchestrateTask(c *gin.Context) {
	sessionID := c.Param("id")
	sess, err := h.store.GetSession(sessionID)
	if err != nil || sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	var req struct {
		Prompt       string `json:"prompt" binding:"required"`
		FencingToken int64  `json:"fencingToken"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}

	if req.FencingToken != 0 {
		if !h.hub.ValidateFencingToken(sessionID, req.FencingToken) {
			c.JSON(http.StatusConflict, gin.H{"error": "stale leader: fencing token rejected"})
			return
		}
	}

	// Phase 1: Auto-explore
	h.hub.Broadcast(sessionID, newEvent(EventOrchestrateProgress, sessionID, map[string]any{
		"phase": "exploring", "message": "Exploring codebases...",
	}))
	exploreCtx := h.autoExploreForContext(sessionID, req.Prompt)

	// Phase 2: Decompose
	h.hub.Broadcast(sessionID, newEvent(EventOrchestrateProgress, sessionID, map[string]any{
		"phase": "decomposing", "message": "Decomposing into tasks...",
	}))


	// Try to delegate decomposition to an online teammate
	_, targetMachineID, pickErr := pickDecomposeTarget(h.hub, h.store, sessionID)
	leaderMachineID := h.hub.GetLeaderMachineID(sessionID)
	if leaderMachineID == "" {
		leaderMachineID = sess.LeaderMachineID
	}

	// If no teammate or only leader available, use fallback
	if pickErr != nil || (leaderMachineID != "" && targetMachineID == leaderMachineID) {
		tasks := buildFallbackTasks(req.Prompt, sessionID)
		schedCtx, schedErr := h.buildSchedulingContext(sessionID)
		if schedErr == nil {
			tasks = ScheduleTasks(*schedCtx, tasks)
		}
		c.JSON(http.StatusCreated, gin.H{
			"tasks":  tasks,
			"dryRun": true,
			"context": exploreCtx,
		})
		return
	}

	// Send decompose.request to teammate
	requestID := uuid.New().String()
	resultCh := h.hub.RegisterDecompose(requestID)
	defer h.hub.CancelDecompose(requestID)

	evt := newEvent(EventDecomposeRequest, sessionID, map[string]any{
		"requestId": requestID,
		"prompt":    req.Prompt,
		"context":   exploreCtx,
	})
	h.hub.SendToMachine(sessionID, targetMachineID, evt)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	select {
	case result := <-resultCh:
		itemsRaw, ok := result.Payload["tasks"].([]any)
		if !ok || len(itemsRaw) == 0 {
			tasks := buildFallbackTasks(req.Prompt, sessionID)
			schedCtx, _ := h.buildSchedulingContext(sessionID)
			if schedCtx != nil {
				tasks = ScheduleTasks(*schedCtx, tasks)
			}
			c.JSON(http.StatusCreated, gin.H{"tasks": tasks, "dryRun": true, "context": exploreCtx})
			return
		}

		var tasks []TeamTask
		for _, raw := range itemsRaw {
			data, _ := json.Marshal(raw)
			var item DecomposeResultItem
			if json.Unmarshal(data, &item) != nil || item.Description == "" {
				continue
			}
			tasks = append(tasks, item.toTeamTask(sessionID))
		}
		if len(tasks) == 0 {
			tasks = buildFallbackTasks(req.Prompt, sessionID)
		}

		schedCtx, _ := h.buildSchedulingContext(sessionID)
		if schedCtx != nil {
			tasks = ScheduleTasks(*schedCtx, tasks)
		}

		c.JSON(http.StatusCreated, gin.H{"tasks": tasks, "dryRun": true, "context": exploreCtx})

	case <-ctx.Done():
		tasks := buildFallbackTasks(req.Prompt, sessionID)
		schedCtx, _ := h.buildSchedulingContext(sessionID)
		if schedCtx != nil {
			tasks = ScheduleTasks(*schedCtx, tasks)
		}
		c.JSON(http.StatusCreated, gin.H{
			"tasks":     tasks,
			"dryRun":    true,
			"context":   exploreCtx,
			"degraded":  true,
			"reason":    "decompose_timeout",
			"message":   "decompose request timed out; used fallback single-task decomposition",
		})
	}
}

// buildSchedulingContext assembles the data needed by ScheduleTasks from the
// current session state. It filters members to only those with active WS
// connections (Hub.IsMachineOnline is the source of truth).
func (h *Handler) buildSchedulingContext(sessionID string) (*SchedulingContext, error) {
	allMembers, err := h.store.ListMembers(sessionID)
	if err != nil {
		return nil, err
	}
	// Keep only members whose machine has a live WS connection
	var onlineMembers []TeamSessionMember
	for _, m := range allMembers {
		if h.hub.IsMachineOnline(sessionID, m.MachineID) {
			onlineMembers = append(onlineMembers, m)
		}
	}

	affinities, err := h.store.ListAllRepoAffinities(sessionID)
	if err != nil {
		return nil, err
	}

	running, err := h.store.GetRunningAssignedTasks(sessionID)
	if err != nil {
		return nil, err
	}

	memberLoad, err := h.store.GetMemberLoadInfo(sessionID)
	if err != nil {
		return nil, err
	}

	return &SchedulingContext{
		Members:        onlineMembers,
		RepoAffinities: affinities,
		RunningTasks:   running,
		MemberLoad:     memberLoad,
	}, nil
}

func newEvent(eventType, sessionID string, payload map[string]any) CloudEvent {
	return CloudEvent{
		EventID:   uuid.New().String(),
		Type:      eventType,
		SessionID: sessionID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// notifyAssignedMachines sends task.assigned events to all machines that
// received an assignment in the current batch.
func (h *Handler) notifyAssignedMachines(sessionID string, tasks []TeamTask) {
	for _, task := range tasks {
		if task.AssignedMemberID == nil {
			continue
		}
		member, _ := h.store.GetMember(*task.AssignedMemberID)
		if member == nil {
			continue
		}
		h.dispatchAssignedTaskToMachine(sessionID, member, task)
	}
}

func (h *Handler) dispatchAssignedTaskToMachine(sessionID string, member *TeamSessionMember, task TeamTask) {
	if member == nil {
		return
	}
	machineID := member.MachineID

	h.hub.SendToMachine(sessionID, machineID,
		newEvent(EventTaskAssigned, sessionID, map[string]any{"task": task}))

	if h.pushAssignedTaskFn == nil || machineID == "" {
		return
	}

	// Push through the cloud gateway path asynchronously so HTTP/WS handlers
	// never block on device reachability.
	go func(pushedTask TeamTask, targetMachineID string, targetUserID string) {
		if err := h.pushAssignedTaskFn(context.Background(), sessionID, targetMachineID, targetUserID, pushedTask); err != nil {
			logger.Warn("[team] push assigned task failed session=%s task=%s machine=%s: %v", sessionID, pushedTask.ID, targetMachineID, err)
		}
	}(task, machineID, member.UserID)
}

// ─── Leader Crash Recovery ────────────────────────────────────────────────

// reconcileTasksOnLeaderChange checks all in-flight tasks after a new leader
// is elected, resets tasks whose assigned machines are offline, and
// re-dispatches them.
func (h *Handler) reconcileTasksOnLeaderChange(sessionID string) {
	tasks, err := h.store.ListTasks(sessionID)
	if err != nil || len(tasks) == 0 {
		return
	}

	reset := false
	for _, t := range tasks {
		if t.Status != TaskStatusRunning && t.Status != TaskStatusClaimed && t.Status != TaskStatusAssigned {
			continue
		}
		if t.AssignedMemberID == nil {
			// No assignee — just reset to pending
			h.store.UpdateTask(t.ID, map[string]any{ //nolint:errcheck
				"status":             TaskStatusPending,
				"assigned_member_id": nil,
			})
			h.hub.Broadcast(sessionID, newEvent(EventTaskInterrupted, sessionID, map[string]any{
				"taskId": t.ID,
				"reason": "leader_change",
			}))
			reset = true
			continue
		}

		// Look up the assigned member's machine
		member, _ := h.store.GetMember(*t.AssignedMemberID)
		if member == nil || member.MachineID == "" {
			continue
		}

		if !h.hub.IsMachineOnline(sessionID, member.MachineID) {
			h.store.UpdateTask(t.ID, map[string]any{ //nolint:errcheck
				"status":             TaskStatusPending,
				"assigned_member_id": nil,
			})
			h.hub.Broadcast(sessionID, newEvent(EventTaskInterrupted, sessionID, map[string]any{
				"taskId":    t.ID,
				"machineId": member.MachineID,
				"reason":    "leader_change_machine_offline",
			}))
			reset = true
		}
	}

	if reset {
		h.assignPendingTasksIfPossible(sessionID)
	}
}

// buildSessionSnapshot assembles a full session state snapshot for a newly
// elected leader, including tasks, approvals, teammates, and repos.
func (h *Handler) buildSessionSnapshot(sessionID string) map[string]any {
	snapshot := map[string]any{}

	tasks, _ := h.store.ListTasks(sessionID)
	if tasks != nil {
		snapshot["tasks"] = tasks
	}

	approvals, _ := h.store.ListPendingApprovals(sessionID)
	if approvals != nil {
		snapshot["approvals"] = approvals
	}

	members, _ := h.store.ListMembers(sessionID)
	if members != nil {
		snapshot["teammates"] = members
	}

	repos, _ := h.store.ListAllRepoAffinities(sessionID)
	if repos != nil {
		snapshot["repos"] = repos
	}

	snapshot["timestamp"] = time.Now().UnixMilli()
	return snapshot
}
