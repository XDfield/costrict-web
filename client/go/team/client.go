package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config holds all the settings needed to connect to a team session.
type Config struct {
	// ServerURL is the base HTTP/HTTPS URL of the costrict server,
	// e.g. "https://api.example.com".  The client derives the WebSocket
	// URL automatically (https → wss, http → ws).
	ServerURL string

	// Token is the JWT bearer token for authentication.
	Token string

	// SessionID is the UUID of the existing team session to join.
	SessionID string

	// MachineID is a stable, unique identifier for this machine.
	// Must be consistent across reconnects so the server can route
	// offline messages back to this machine.
	MachineID string

	// MachineName is a human-readable label for this machine (optional).
	MachineName string

	// Role is either "leader" or "teammate".
	Role string
}

// Client is the top-level entry point for the Cloud Team Agent SDK.
// Create one with New, register plugins with the With* methods, then call Start.
//
// Usage (leader):
//
//	c := team.New(cfg).
//	    WithLeaderPlugin(myPlanner).
//	    WithApprovalPlugin(myApprover)
//	if err := c.Start(ctx); err != nil { ... }
//
// Usage (teammate):
//
//	c := team.New(cfg).
//	    WithTeammatePlugin(myExecutor).
//	    WithExplorePlugin(myExplorer)
//	if err := c.Start(ctx); err != nil { ... }
type Client struct {
	cfg        Config
	httpClient *http.Client

	ws       *wsConn
	leader   *leaderAgent
	teammate *teammateAgent

	// Plugin slots — set via With* methods before calling Start.
	leaderPlugin   LeaderPlugin
	teammatePlugin TeammatePlugin
	approvalPlugin ApprovalPlugin
	explorePlugin  ExplorePlugin

	cancelFn context.CancelFunc
}

// New creates a Client from cfg.  No network activity happens until Start is called.
func New(cfg Config) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		ws:         newWSConn(cfg),
	}
}

// ─── Fluent plugin registration ───────────────────────────────────────────

// WithLeaderPlugin registers the planning plugin (leader role only).
func (c *Client) WithLeaderPlugin(p LeaderPlugin) *Client {
	c.leaderPlugin = p
	return c
}

// WithTeammatePlugin registers the task-execution plugin (teammate role only).
func (c *Client) WithTeammatePlugin(p TeammatePlugin) *Client {
	c.teammatePlugin = p
	return c
}

// WithApprovalPlugin registers the approval-display plugin (both roles).
func (c *Client) WithApprovalPlugin(p ApprovalPlugin) *Client {
	c.approvalPlugin = p
	return c
}

// WithExplorePlugin registers the local code-query plugin (teammate role only).
func (c *Client) WithExplorePlugin(p ExplorePlugin) *Client {
	c.explorePlugin = p
	return c
}

// ─── Lifecycle ────────────────────────────────────────────────────────────

// Start connects to the server and begins processing events.
// It blocks until ctx is cancelled (or a fatal error occurs).
//
// For the leader role, call SubmitPlan after Start returns (or from a separate
// goroutine while Start is running).
func (c *Client) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancelFn = cancel
	defer cancel()

	// Start the WebSocket connection loop in the background.
	go c.ws.run(ctx)

	// Role-specific initialisation via REST.
	switch c.cfg.Role {
	case MemberRoleLeader:
		c.leader = newLeaderAgent(c)
		if err := c.leader.init(ctx); err != nil {
			return fmt.Errorf("leader init: %w", err)
		}
	case MemberRoleTeammate:
		c.teammate = newTeammateAgent(c)
		if err := c.teammate.init(ctx); err != nil {
			return fmt.Errorf("teammate init: %w", err)
		}
	default:
		return fmt.Errorf("unknown role %q (must be %q or %q)",
			c.cfg.Role, MemberRoleLeader, MemberRoleTeammate)
	}

	// Event dispatch loop.
	for {
		select {
		case evt := <-c.ws.inbound:
			c.dispatch(evt)
		case <-ctx.Done():
			c.stop()
			return ctx.Err()
		}
	}
}

// SubmitPlan calls LeaderPlugin.PlanTasks and submits the resulting task plan
// to the server.  Must only be called from the leader role after Start.
func (c *Client) SubmitPlan(ctx context.Context, goal string) error {
	if c.leader == nil {
		return fmt.Errorf("SubmitPlan requires Role = %q", MemberRoleLeader)
	}
	return c.leader.submitPlan(ctx, goal)
}

// Stop signals the client to shut down.  It is safe to call from any goroutine.
func (c *Client) Stop() {
	if c.cancelFn != nil {
		c.cancelFn()
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────

func (c *Client) stop() {
	c.ws.close()
	if c.leader != nil {
		c.leader.stop()
	}
}

func (c *Client) dispatch(evt CloudEvent) {
	if c.leader != nil {
		c.leader.handle(evt)
	}
	if c.teammate != nil {
		c.teammate.handle(evt)
	}
}

// doJSON executes a REST request against the server and decodes the JSON response.
// body and out may both be nil.
func (c *Client) doJSON(method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	apiURL := strings.TrimRight(c.cfg.ServerURL, "/") + path
	req, err := http.NewRequest(method, apiURL, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e) //nolint:errcheck
		return fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, path, e.Error)
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// fetchMembers calls GET /api/team/sessions/:id/members and returns the list.
func (c *Client) fetchMembers() ([]Member, error) {
	var resp struct {
		Members []Member `json:"members"`
	}
	err := c.doJSON("GET", "/api/team/sessions/"+c.cfg.SessionID+"/members", nil, &resp)
	return resp.Members, err
}

// newEvent builds a CloudEvent with a fresh UUID and the current timestamp.
func newEvent(eventType, sessionID string, payload map[string]any) CloudEvent {
	return CloudEvent{
		EventID:   uuid.New().String(),
		Type:      eventType,
		SessionID: sessionID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	}
}
