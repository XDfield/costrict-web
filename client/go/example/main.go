// example/main.go — Runnable Cloud Team Agent client example.
//
// Usage:
//
//	# Teammate — listens for assigned tasks and executes them as shell commands
//	go run ./example/main.go \
//	  --server https://api.example.com \
//	  --token "$TOKEN" \
//	  --session "$SESSION_ID" \
//	  --machine my-mac-$(hostname) \
//	  --role teammate
//
//	# Leader — submits a plan, then routes approvals via stdin prompt
//	go run ./example/main.go \
//	  --server https://api.example.com \
//	  --token "$TOKEN" \
//	  --session "$SESSION_ID" \
//	  --machine leader-$(hostname) \
//	  --role leader \
//	  --goal "refactor the authentication module"
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/costrict/costrict-web/client/go/team"
)

func main() {
	serverURL := flag.String("server", "", "Server base URL (required)")
	token := flag.String("token", "", "JWT bearer token (required)")
	sessionID := flag.String("session", "", "Team session UUID (required)")
	machineID := flag.String("machine", "", "Stable machine identifier (required)")
	machineName := flag.String("name", "", "Human-readable machine name (optional)")
	role := flag.String("role", "teammate", "Role: leader or teammate")
	goal := flag.String("goal", "", "Goal string for the leader's initial plan (leader only)")
	flag.Parse()

	if *serverURL == "" || *token == "" || *sessionID == "" || *machineID == "" {
		flag.Usage()
		os.Exit(1)
	}
	if *machineName == "" {
		hostname, _ := os.Hostname()
		*machineName = hostname
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := team.Config{
		ServerURL:   *serverURL,
		Token:       *token,
		SessionID:   *sessionID,
		MachineID:   *machineID,
		MachineName: *machineName,
		Role:        *role,
	}

	var c *team.Client

	switch *role {
	case team.MemberRoleLeader:
		c = buildLeader(cfg, *goal, ctx)
	case team.MemberRoleTeammate:
		c = buildTeammate(cfg)
	default:
		log.Fatalf("unknown role %q — must be leader or teammate", *role)
	}

	log.Printf("[%s] connecting to %s (session=%s)", *machineID, *serverURL, *sessionID)
	if err := c.Start(ctx); err != nil && err != context.Canceled {
		log.Printf("[%s] stopped: %v", *machineID, err)
	}
}

// ─── Leader setup ─────────────────────────────────────────────────────────

func buildLeader(cfg team.Config, goal string, ctx context.Context) *team.Client {
	c := team.New(cfg).
		WithLeaderPlugin(&SimplePlanner{}).
		WithApprovalPlugin(&StdinApprover{prefix: "[LEADER]"})

	if goal != "" {
		go func() {
			// Give the WS connection a moment to establish before submitting the plan.
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			log.Printf("[leader] submitting plan: %q", goal)
			if err := c.SubmitPlan(ctx, goal); err != nil {
				log.Printf("[leader] plan submission failed: %v", err)
			} else {
				log.Printf("[leader] plan submitted successfully")
			}
		}()
	}

	return c
}

// SimplePlanner creates a 3-task dependency chain from the goal string.
// In production, replace this with an LLM call or your own planning logic.
type SimplePlanner struct{}

func (p *SimplePlanner) PlanTasks(ctx context.Context, req team.PlanTasksInput) ([]team.TaskSpec, error) {
	log.Printf("[leader] planning tasks for goal: %q (%d members online)", req.Goal, len(req.Members))

	// Pre-assign UUIDs so we can wire up inter-task dependencies.
	idA := uuid.New().String()
	idB := uuid.New().String()
	idC := uuid.New().String()

	// Pick the first available teammate for assignment (if any).
	var teammateID string
	for _, m := range req.Members {
		if m.Role == team.MemberRoleTeammate && m.Status == team.MemberStatusOnline {
			teammateID = m.ID
			break
		}
	}

	makeSpec := func(id, desc string, deps []string, assignee string) team.TaskSpec {
		return team.TaskSpec{
			ID:               id,
			Description:      desc,
			Dependencies:     deps,
			AssignedMemberID: assignee,
			Priority:         5,
		}
	}

	return []team.TaskSpec{
		makeSpec(idA, fmt.Sprintf("Analyse codebase — %s", req.Goal), nil, teammateID),
		makeSpec(idB, fmt.Sprintf("Implement changes — %s", req.Goal), []string{idA}, teammateID),
		makeSpec(idC, fmt.Sprintf("Run tests and verify — %s", req.Goal), []string{idB}, teammateID),
	}, nil
}

// ─── Teammate setup ────────────────────────────────────────────────────────

func buildTeammate(cfg team.Config) *team.Client {
	return team.New(cfg).
		WithTeammatePlugin(&ShellExecutor{}).
		WithExplorePlugin(&LocalExplorer{}).
		WithApprovalPlugin(&StdinApprover{prefix: "[TEAMMATE]"})
}

// ShellExecutor interprets the task description as a shell command and runs it.
// In production, replace this with your AI agent or task runner.
type ShellExecutor struct{}

func (e *ShellExecutor) ExecuteTask(ctx context.Context, t team.Task, r team.ProgressReporter) (team.TaskResult, error) {
	log.Printf("[teammate] executing task %s: %q", t.ID[:8], t.Description)
	r.Report(10, "preparing")

	// Treat the description as a shell command for this example.
	// A real implementation would parse the description and invoke appropriate tools.
	cmd := exec.CommandContext(ctx, "sh", "-c", t.Description) //nolint:gosec
	cmd.Dir = "."

	r.Report(30, "running")

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Non-zero exit: report as task failure with the captured output.
		return team.TaskResult{}, fmt.Errorf("command failed: %w\noutput: %s", err, output)
	}

	r.Report(100, "done")
	log.Printf("[teammate] task %s completed (%d bytes output)", t.ID[:8], len(output))
	return team.TaskResult{
		Output:    string(output),
		ExtraData: map[string]any{"exitCode": 0},
	}, nil
}

// ─── LocalExplorer ─────────────────────────────────────────────────────────

// LocalExplorer handles remote explore requests using common Unix tools.
// Only read-only, sandboxed operations are allowed.
type LocalExplorer struct{}

func (e *LocalExplorer) Explore(_ context.Context, req team.ExploreRequest) (team.ExploreResult, error) {
	log.Printf("[teammate] handling explore request %s (%d queries)", req.RequestID[:8], len(req.Queries))

	results := make([]team.ExploreQueryResult, 0, len(req.Queries))
	for _, q := range req.Queries {
		r := team.ExploreQueryResult{Type: q.Type}

		switch q.Type {
		case "file_tree":
			path := stringParam(q.Params, "path", ".")
			out, err := exec.Command( //nolint:gosec
				"find", path, "-type", "f",
				"-not", "-path", "*/.*",   // exclude hidden files
				"-not", "-path", "*/vendor/*",
				"-not", "-path", "*/node_modules/*",
			).Output()
			if err != nil {
				r.Output = fmt.Sprintf("error: %v", err)
			} else {
				r.Output = limitOutput(string(out), 8192)
				r.Truncated = len(out) > 8192
			}

		case "content_search":
			pattern := stringParam(q.Params, "pattern", "")
			dir := stringParam(q.Params, "dir", ".")
			if pattern == "" {
				r.Output = "error: pattern is required"
				break
			}
			// Use ripgrep if available, fall back to grep.
			cmd := exec.Command("rg", "--no-heading", "-n", "-m", "50", pattern, dir) //nolint:gosec
			if _, err := exec.LookPath("rg"); err != nil {
				cmd = exec.Command("grep", "-rn", "--include=*", pattern, dir) //nolint:gosec
			}
			out, _ := cmd.Output()
			r.Output = limitOutput(string(out), 8192)
			r.Truncated = len(out) > 8192

		case "git_log":
			dir := stringParam(q.Params, "dir", ".")
			n := intParam(q.Params, "n", 20)
			out, err := exec.Command( //nolint:gosec
				"git", "-C", dir, "log",
				fmt.Sprintf("-n%d", n),
				"--oneline",
			).Output()
			if err != nil {
				r.Output = fmt.Sprintf("error: %v (is %s a git repo?)", err, dir)
			} else {
				r.Output = string(out)
			}

		case "symbol_search":
			symbol := stringParam(q.Params, "symbol", "")
			dir := stringParam(q.Params, "dir", ".")
			if symbol == "" {
				r.Output = "error: symbol is required"
				break
			}
			out, _ := exec.Command("rg", "--no-heading", "-n", "-w", symbol, dir).Output() //nolint:gosec
			r.Output = limitOutput(string(out), 8192)

		case "dependency_graph":
			entry := stringParam(q.Params, "entry", ".")
			out, err := exec.Command("go", "list", "-deps", entry).Output() //nolint:gosec
			if err != nil {
				r.Output = fmt.Sprintf("error: %v", err)
			} else {
				r.Output = limitOutput(string(out), 8192)
			}

		default:
			r.Output = fmt.Sprintf("unsupported query type %q", q.Type)
		}

		results = append(results, r)
	}

	return team.ExploreResult{
		RequestID:    req.RequestID,
		QueryResults: results,
	}, nil
}

// ─── StdinApprover ─────────────────────────────────────────────────────────

// StdinApprover presents approval requests on stdout and reads y/n from stdin.
type StdinApprover struct {
	prefix string
}

func (a *StdinApprover) HandleApproval(_ context.Context, req team.ApprovalRequest) (bool, string, error) {
	fmt.Printf("\n%s ─────────────────── APPROVAL REQUEST ───────────────────\n", a.prefix)
	fmt.Printf("  Tool:        %s\n", req.ToolName)
	fmt.Printf("  Risk level:  %s\n", req.RiskLevel)
	fmt.Printf("  Description: %s\n", req.Description)
	if len(req.ToolInput) > 0 {
		fmt.Printf("  Input:       %v\n", req.ToolInput)
	}
	fmt.Printf("%s ──────────────────────────────────────────────────────────\n", a.prefix)
	fmt.Printf("%s Approve? [y/N]: ", a.prefix)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	answer := strings.TrimSpace(scanner.Text())
	approved := strings.EqualFold(answer, "y")

	if approved {
		fmt.Printf("%s Approved.\n", a.prefix)
	} else {
		fmt.Printf("%s Rejected.\n", a.prefix)
	}
	return approved, "", nil
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func stringParam(params map[string]any, key, def string) string {
	if v, ok := params[key].(string); ok && v != "" {
		return v
	}
	return def
}

func intParam(params map[string]any, key string, def int) int {
	switch v := params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func limitOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
