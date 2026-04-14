package team

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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
	store *Store
	hub   *Hub
}

func NewHandler(store *Store, hub *Hub) *Handler {
	return &Handler{store: store, hub: hub}
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

// UpdateSession godoc
// @Summary      Update team session status
// @Tags         team
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "Session ID"
// @Param        body  body  object{status=string}  true  "Status"
// @Success      200   {object}  TeamSession
// @Router       /team/sessions/:id [patch]
func (h *Handler) UpdateSession(c *gin.Context) {
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

	if err := h.store.UpdateSession(c.Param("id"), updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update session"})
		return
	}

	sess, _ := h.store.GetSession(c.Param("id"))
	c.JSON(http.StatusOK, sess)
}

// DeleteSession godoc
// @Summary      Close / delete team session
// @Tags         team
// @Param        id  path  string  true  "Session ID"
// @Success      200  {object}  object{message=string}
// @Router       /team/sessions/:id [delete]
func (h *Handler) DeleteSession(c *gin.Context) {
	sessionID := c.Param("id")
	if err := h.store.DeleteSession(sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "session deleted"})
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

	// Idempotent: return existing member if already joined
	existing, _ := h.store.GetMemberByMachine(sessionID, req.MachineID)
	if existing != nil {
		c.JSON(http.StatusOK, existing)
		return
	}

	if h.hub.SessionConnCount(sessionID) >= DefaultMaxConnectionsPerSession {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "session is full"})
		return
	}

	m := &TeamSessionMember{
		ID:            uuid.New().String(),
		SessionID:     sessionID,
		UserID:        userID,
		MachineID:     req.MachineID,
		MachineName:   req.MachineName,
		Role:          MemberRoleTeammate,
		Status:        MemberStatusOnline,
		ConnectedAt:   time.Now(),
		LastHeartbeat: time.Now(),
	}
	if err := h.store.CreateMember(m); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to join session"})
		return
	}
	c.JSON(http.StatusCreated, m)
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
		evt := newEvent(EventTaskAssigned, sessionID, map[string]any{"task": task})
		h.hub.SendToMachine(sessionID, member.MachineID, evt)
	}

	c.JSON(http.StatusCreated, gin.H{"tasks": req.Tasks})
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

	approval, _ := h.store.GetApproval(approvalID)
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
		MachineID string `json:"machineId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "machineId is required"})
		return
	}

	token, elected := h.hub.TryAcquireLeader(sessionID, req.MachineID)
	if elected {
		// Update member role and persist leader info to DB
		h.store.UpdateSession(sessionID, map[string]any{ //nolint:errcheck
			"leader_machine_id": req.MachineID,
			"fencing_token":     token,
		})
		m, _ := h.store.GetMemberByMachine(sessionID, req.MachineID)
		if m != nil {
			h.store.UpdateMember(m.ID, map[string]any{"role": MemberRoleLeader}) //nolint:errcheck
		}
		h.hub.Broadcast(sessionID, newEvent(EventLeaderElected, sessionID, map[string]any{
			"leaderId":     req.MachineID,
			"fencingToken": token,
		}))
	}

	c.JSON(http.StatusOK, gin.H{
		"elected":      elected,
		"fencingToken": token,
		"leaderId":     h.hub.GetLeaderMachineID(sessionID),
	})
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
		MachineID string `json:"machineId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "machineId is required"})
		return
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
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targetMachineId is required"})
		return
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
		existing, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
		if existing == nil {
			m := &TeamSessionMember{
				ID:            uuid.New().String(),
				SessionID:     conn.SessionID,
				UserID:        conn.UserID,
				MachineID:     conn.MachineID,
				MachineName:   machineName,
				Role:          MemberRoleTeammate,
				Status:        MemberStatusOnline,
				ConnectedAt:   time.Now(),
				LastHeartbeat: time.Now(),
			}
			h.store.CreateMember(m) //nolint:errcheck
		} else {
			h.store.UpdateMember(existing.ID, map[string]any{ //nolint:errcheck
				"status":         MemberStatusOnline,
				"last_heartbeat": time.Now(),
			})
		}
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
			h.hub.SendToMachine(conn.SessionID, member.MachineID,
				newEvent(EventTaskAssigned, conn.SessionID, map[string]any{"task": task}))
		}

	case EventLeaderHeartbeat:
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
						h.hub.SendToMachine(conn.SessionID, member.MachineID,
							newEvent(EventTaskAssigned, conn.SessionID, map[string]any{"task": retried}))
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

	case EventLeaderElect:
		token, elected := h.hub.TryAcquireLeader(conn.SessionID, conn.MachineID)
		if elected {
			h.store.UpdateSession(conn.SessionID, map[string]any{ //nolint:errcheck
				"leader_machine_id": conn.MachineID,
				"fencing_token":     token,
			})
			m, _ := h.store.GetMemberByMachine(conn.SessionID, conn.MachineID)
			if m != nil {
				h.store.UpdateMember(m.ID, map[string]any{"role": MemberRoleLeader}) //nolint:errcheck
			}
			h.hub.Broadcast(conn.SessionID, newEvent(EventLeaderElected, conn.SessionID, map[string]any{
				"leaderId":     conn.MachineID,
				"fencingToken": token,
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
		h.hub.SendToMachine(sessionID, member.MachineID,
			newEvent(EventTaskAssigned, sessionID, map[string]any{"task": t}))
	}
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
