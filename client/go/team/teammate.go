package team

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// teammateAgent handles all teammate-role behaviour:
//   - joining the session (REST)
//   - executing assigned tasks via TeammatePlugin
//   - handling explore.request events via ExplorePlugin
//   - sending approval.request events to the leader
type teammateAgent struct {
	c        *Client
	memberID string // populated after init
}

func newTeammateAgent(c *Client) *teammateAgent {
	return &teammateAgent{c: c}
}

// init registers this machine as a session member via REST.
func (t *teammateAgent) init(_ context.Context) error {
	var resp struct {
		ID string `json:"id"`
	}
	err := t.c.doJSON("POST",
		"/api/team/sessions/"+t.c.cfg.SessionID+"/members",
		map[string]any{
			"machineId":   t.c.cfg.MachineID,
			"machineName": t.c.cfg.MachineName,
		},
		&resp,
	)
	if err != nil {
		return fmt.Errorf("join session: %w", err)
	}
	t.memberID = resp.ID
	return nil
}

// handle dispatches incoming server events relevant to the teammate role.
func (t *teammateAgent) handle(evt CloudEvent) {
	switch evt.Type {

	case EventTaskAssigned:
		taskRaw, _ := json.Marshal(evt.Payload["task"])
		var task Task
		if json.Unmarshal(taskRaw, &task) != nil || task.ID == "" {
			return
		}
		if t.c.teammatePlugin == nil {
			return
		}
		// Claim the task immediately so the leader knows it's being worked on.
		t.c.ws.send(newEvent(EventTaskClaim, t.c.cfg.SessionID, map[string]any{ //nolint:errcheck
			"taskId": task.ID,
		}))
		go t.executeTask(task)

	case EventExploreRequest:
		if t.c.explorePlugin == nil {
			return
		}
		requestID, _ := evt.Payload["requestId"].(string)
		if requestID == "" {
			return
		}
		go t.handleExplore(evt)

	case EventApprovalResponse:
		// The leader responded to our approval request. Optionally notify the plugin.
		if t.c.approvalPlugin == nil {
			return
		}
		// We receive only the response here, not a full ApprovalRequest, so we
		// build a minimal stub for the plugin in case it wants to log the outcome.
		approvalID, _ := evt.Payload["approvalId"].(string)
		status, _ := evt.Payload["status"].(string)
		feedback, _ := evt.Payload["feedback"].(string)
		if approvalID == "" {
			return
		}
		go func() {
			_ = approvalID
			_ = status
			_ = feedback
			// Host can extend via ApprovalPlugin if needed; no further action here.
		}()
	}
}

// executeTask runs the task through TeammatePlugin and reports progress/result.
func (t *teammateAgent) executeTask(task Task) {
	ctx := context.Background()
	reporter := &wsProgressReporter{c: t.c, taskID: task.ID}

	// Signal that we've started.
	t.c.ws.send(newEvent(EventTaskProgress, t.c.cfg.SessionID, map[string]any{ //nolint:errcheck
		"taskId":  task.ID,
		"percent": 0,
		"message": "started",
	}))

	result, err := t.c.teammatePlugin.ExecuteTask(ctx, task, reporter)
	if err != nil {
		t.c.ws.send(newEvent(EventTaskFail, t.c.cfg.SessionID, map[string]any{ //nolint:errcheck
			"taskId":       task.ID,
			"errorMessage": err.Error(),
		}))
		return
	}

	t.c.ws.send(newEvent(EventTaskComplete, t.c.cfg.SessionID, map[string]any{ //nolint:errcheck
		"taskId": task.ID,
		"result": result,
	}))
}

// handleExplore runs the explore request through ExplorePlugin and sends the result.
func (t *teammateAgent) handleExplore(evt CloudEvent) {
	requestID, _ := evt.Payload["requestId"].(string)
	fromMachineID, _ := evt.Payload["fromMachineId"].(string)

	// Re-marshal the payload into an ExploreRequest.
	raw, _ := json.Marshal(evt.Payload)
	var req ExploreRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		req.RequestID = requestID
	}

	result, err := t.c.explorePlugin.Explore(context.Background(), req)
	if err != nil {
		result = ExploreResult{
			RequestID: requestID,
			Error:     err.Error(),
		}
	}
	result.RequestID = requestID

	t.c.ws.send(newEvent(EventExploreResult, t.c.cfg.SessionID, map[string]any{ //nolint:errcheck
		"requestId":     requestID,
		"queryResults":  result.QueryResults,
		"fromMachineId": fromMachineID,
		"error":         result.Error,
	}))
}

// RequestApproval sends an approval.request to the leader via WebSocket.
// riskLevel should be "low", "medium", or "high".
func (t *teammateAgent) RequestApproval(toolName, description, riskLevel string, toolInput map[string]any) error {
	return t.c.ws.send(newEvent(EventApprovalRequest, t.c.cfg.SessionID, map[string]any{
		"toolName":    toolName,
		"description": description,
		"riskLevel":   riskLevel,
		"toolInput":   toolInput,
	}))
}

// RegisterRepo registers a local repository with the session's affinity registry.
func (t *teammateAgent) RegisterRepo(remoteURL, localPath, branch string, dirty bool) error {
	return t.c.doJSON("POST",
		"/api/team/sessions/"+t.c.cfg.SessionID+"/repos",
		map[string]any{
			"memberId":              t.memberID,
			"repoRemoteUrl":         remoteURL,
			"repoLocalPath":         localPath,
			"currentBranch":         branch,
			"hasUncommittedChanges": dirty,
			"lastSyncedAt":          time.Now().Format(time.RFC3339),
		},
		nil,
	)
}

// ─── wsProgressReporter ───────────────────────────────────────────────────

// wsProgressReporter implements ProgressReporter by sending task.progress
// events over the WebSocket connection.
type wsProgressReporter struct {
	c      *Client
	taskID string
}

func (r *wsProgressReporter) Report(pct int, message string) {
	r.c.ws.send(newEvent(EventTaskProgress, r.c.cfg.SessionID, map[string]any{ //nolint:errcheck
		"taskId":  r.taskID,
		"percent": pct,
		"message": message,
	}))
}
