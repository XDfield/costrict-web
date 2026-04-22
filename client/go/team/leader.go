package team

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// leaderAgent handles all leader-role behaviour:
//   - leader election and lock renewal (heartbeat)
//   - task plan submission
//   - routing incoming approval.push events to the ApprovalPlugin
//   - re-election when the leader lock expires
type leaderAgent struct {
	c            *Client
	fencingToken int64
	cancelHB     context.CancelFunc
}

func newLeaderAgent(c *Client) *leaderAgent {
	return &leaderAgent{c: c}
}

// init performs the REST calls needed before the event loop starts:
// 1. Attempts leader election.
// 2. Starts the heartbeat goroutine to renew the lock every 10 s.
func (l *leaderAgent) init(ctx context.Context) error {
	var resp struct {
		Elected      bool   `json:"elected"`
		FencingToken int64  `json:"fencingToken"`
		LeaderID     string `json:"leaderId"`
	}
	if err := l.c.doJSON("POST",
		"/api/team/sessions/"+l.c.cfg.SessionID+"/leader/elect",
		map[string]any{"machineId": l.c.cfg.MachineID},
		&resp,
	); err != nil {
		return fmt.Errorf("leader election: %w", err)
	}
	l.fencingToken = resp.FencingToken

	hbCtx, cancel := context.WithCancel(ctx)
	l.cancelHB = cancel
	go l.heartbeatLoop(hbCtx)

	return nil
}

// heartbeatLoop renews the Redis leader lock every leaderHeartbeatSec seconds.
func (l *leaderAgent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(leaderHeartbeatSec * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			var resp struct {
				Renewed bool `json:"renewed"`
			}
			// Best-effort — ignore errors here, the server will broadcast
			// leader.expired if the lock expires.
			l.c.doJSON("POST", //nolint:errcheck
				"/api/team/sessions/"+l.c.cfg.SessionID+"/leader/heartbeat",
				map[string]any{"machineId": l.c.cfg.MachineID},
				&resp)
		case <-ctx.Done():
			return
		}
	}
}

// submitPlan calls LeaderPlugin.PlanTasks then POSTs the resulting tasks to
// the server.  It pre-assigns UUIDs to each task so that dependency IDs are
// stable at creation time (the server respects provided IDs).
func (l *leaderAgent) submitPlan(ctx context.Context, goal string) error {
	if l.c.leaderPlugin == nil {
		return fmt.Errorf("no LeaderPlugin registered")
	}

	members, err := l.c.fetchMembers()
	if err != nil {
		return fmt.Errorf("fetch members: %w", err)
	}

	specs, err := l.c.leaderPlugin.PlanTasks(ctx, PlanTasksInput{
		Goal:      goal,
		SessionID: l.c.cfg.SessionID,
		Members:   members,
	})
	if err != nil {
		return fmt.Errorf("plan tasks: %w", err)
	}
	if len(specs) == 0 {
		return fmt.Errorf("LeaderPlugin returned an empty plan")
	}

	// Pre-assign stable UUIDs so dependency references within the batch
	// are preserved when the server creates the tasks.
	for i := range specs {
		if specs[i].ID == "" {
			specs[i].ID = uuid.New().String()
		}
	}

	var result struct {
		Tasks []Task `json:"tasks"`
	}
	return l.c.doJSON("POST",
		"/api/team/sessions/"+l.c.cfg.SessionID+"/tasks",
		map[string]any{
			"tasks":        specs,
			"fencingToken": l.fencingToken,
		},
		&result,
	)
}

// handle dispatches incoming server events relevant to the leader role.
func (l *leaderAgent) handle(evt CloudEvent) {
	switch evt.Type {

	case EventApprovalPush:
		// Extract the nested approval object and forward to ApprovalPlugin.
		if l.c.approvalPlugin == nil {
			return
		}
		approvalRaw, _ := json.Marshal(evt.Payload["approval"])
		var req ApprovalRequest
		if json.Unmarshal(approvalRaw, &req) != nil {
			return
		}
		go func() {
			ctx := context.Background()
			approved, note, err := l.c.approvalPlugin.HandleApproval(ctx, req)
			if err != nil {
				return
			}
			status := "approved"
			if !approved {
				status = "rejected"
			}
			l.c.doJSON("PATCH", "/api/team/approvals/"+req.ID, //nolint:errcheck
				map[string]any{"status": status, "feedback": note}, nil)
		}()

	case EventLeaderExpired:
		// Our lock expired — try to re-acquire.
		go func() {
			var resp struct {
				Elected      bool  `json:"elected"`
				FencingToken int64 `json:"fencingToken"`
			}
			if err := l.c.doJSON("POST",
				"/api/team/sessions/"+l.c.cfg.SessionID+"/leader/elect",
				map[string]any{"machineId": l.c.cfg.MachineID},
				&resp,
			); err == nil && resp.Elected {
				l.fencingToken = resp.FencingToken
			}
		}()

	case EventTeammateStatus:
		// Teammate came online or went offline — useful for dashboards.
		// The host application can embed a custom handler via a callback if needed.
	}
}

// stop cancels the heartbeat goroutine.
func (l *leaderAgent) stop() {
	if l.cancelHB != nil {
		l.cancelHB()
	}
}

// RequestApproval sends an approval.request event via WebSocket.
// Leaders can use this to request user confirmation for risky operations.
func (l *leaderAgent) RequestApproval(toolName, description, riskLevel string, toolInput map[string]any) error {
	return l.c.ws.send(newEvent(EventApprovalRequest, l.c.cfg.SessionID, map[string]any{
		"toolName":    toolName,
		"description": description,
		"riskLevel":   riskLevel,
		"toolInput":   toolInput,
	}))
}

// RegisterRepo registers a local repository with the session's affinity registry.
func (l *leaderAgent) RegisterRepo(remoteURL, localPath, branch string, dirty bool) error {
	return l.c.doJSON("POST",
		"/api/team/sessions/"+l.c.cfg.SessionID+"/repos",
		map[string]any{
			"memberId":              l.findMemberID(),
			"repoRemoteUrl":         remoteURL,
			"repoLocalPath":         localPath,
			"currentBranch":         branch,
			"hasUncommittedChanges": dirty,
			"lastSyncedAt":          time.Now().Format(time.RFC3339),
		},
		nil,
	)
}

func (l *leaderAgent) findMemberID() string {
	members, _ := l.c.fetchMembers()
	for _, m := range members {
		if m.MachineID == l.c.cfg.MachineID {
			return m.ID
		}
	}
	return ""
}
