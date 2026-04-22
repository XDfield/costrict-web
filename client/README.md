# Cloud Team Agent — Client SDK

Cross-machine multi-agent collaboration SDK. One machine acts as the **Leader** (plans work, handles approvals), any number of machines act as **Teammates** (execute tasks, answer explore queries). All communication flows through the cloud control plane.

```
Leader ──REST/WS──▶ Cloud Control Plane ──WS──▶ Teammate A
                                         ──WS──▶ Teammate B
```

Both a **Go** package and a **TypeScript** package are provided — they expose identical plugin interfaces so you can swap host environments without rewriting business logic.

---

## Table of Contents

1. [Architecture](#architecture)
2. [Go SDK](#go-sdk)
   - [Installation](#go-installation)
   - [Quick Start — Leader](#go-leader-quick-start)
   - [Quick Start — Teammate](#go-teammate-quick-start)
3. [TypeScript SDK](#typescript-sdk)
   - [Installation](#ts-installation)
   - [Quick Start — Leader](#ts-leader-quick-start)
   - [Quick Start — Teammate](#ts-teammate-quick-start)
4. [Plugin Reference](#plugin-reference)
5. [Config Reference](#config-reference)
6. [Task DAG & Dependencies](#task-dag--dependencies)
7. [Repo Affinity](#repo-affinity)
8. [REST API Reference](#rest-api-reference)
9. [WebSocket Event Reference](#websocket-event-reference)
10. [Examples](#examples)

---

## Architecture

### Roles

| Role | Responsibilities |
|------|-----------------|
| **Leader** | Creates / joins a session, decomposes goals into a task DAG, distributes tasks to Teammates, handles approvals |
| **Teammate** | Joins a session, executes assigned tasks, answers remote explore requests, requests approvals for risky operations |

### Session lifecycle

```
1. Leader   →  POST /api/team/sessions            (create session)
2. Leader   →  POST /sessions/:id/leader/elect    (acquire leader lock)
3. Teammate →  POST /sessions/:id/members         (register machine)
4. Both     →  WS  /ws/sessions/:id               (open event channel)
5. Leader   →  POST /sessions/:id/tasks           (submit task plan)
6. Server   →  WS task.assigned  ──▶ Teammate     (dispatch tasks)
7. Teammate →  WS task.progress/complete/fail     (report execution)
8. Teammate →  WS approval.request ──▶ Leader     (ask permission)
9. Leader   →  PATCH /approvals/:id               (respond to approval)
```

### Plugin architecture

The SDK exposes four extension points. Register only the ones your role needs:

```
┌─────────────┐   PlanTasks()        ┌──────────────────────┐
│ LeaderPlugin│◀──────────────────── │  Client (leader role) │
└─────────────┘                      └──────────────────────┘
                                                │
┌──────────────┐  HandleApproval()             │  approval.push
│ApprovalPlugin│◀──────────────────────────────┤
└──────────────┘                               │
                                               │
┌───────────────┐  ExecuteTask()    ┌──────────────────────┐
│TeammatePlugin │◀───────────────── │ Client (teammate role)│
└───────────────┘                   └──────────────────────┘
                                               │
┌───────────────┐  Explore()                   │  explore.request
│ ExplorePlugin │◀──────────────────────────────┘
└───────────────┘
```

---

## Go SDK

### Go Installation

The Go SDK lives in `client/go/` in the same `go.work` workspace as the server.

If you're working inside the monorepo:

```bash
# The go.work at the repo root already includes client/go.
# Just import the package in your code:
import "github.com/costrict/costrict-web/client/go/team"
```

If you're using it as a standalone dependency:

```bash
go get github.com/costrict/costrict-web/client/go@latest
```

### Go Leader Quick Start

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"

    "github.com/costrict/costrict-web/client/go/team"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
    defer stop()

    c := team.New(team.Config{
        ServerURL:   "https://api.example.com",
        Token:       os.Getenv("TEAM_TOKEN"),
        SessionID:   os.Getenv("TEAM_SESSION_ID"),
        MachineID:   "leader-machine-1",
        MachineName: "My Leader",
        Role:        team.MemberRoleLeader,
    }).
        WithLeaderPlugin(&MyPlanner{}).
        WithApprovalPlugin(&CLIApprover{})

    // SubmitPlan can be called after Start in a separate goroutine.
    go func() {
        if err := c.SubmitPlan(ctx, "refactor the auth module"); err != nil {
            log.Println("plan error:", err)
        }
    }()

    if err := c.Start(ctx); err != nil {
        log.Println("client stopped:", err)
    }
}
```

### Go Teammate Quick Start

```go
c := team.New(team.Config{
    ServerURL:   "https://api.example.com",
    Token:       os.Getenv("TEAM_TOKEN"),
    SessionID:   os.Getenv("TEAM_SESSION_ID"),
    MachineID:   "teammate-machine-a",
    MachineName: "Machine A",
    Role:        team.MemberRoleTeammate,
}).
    WithTeammatePlugin(&ShellExecutor{}).
    WithExplorePlugin(&LocalExplorer{}).
    WithApprovalPlugin(&CLIApprover{})

if err := c.Start(ctx); err != nil {
    log.Println("client stopped:", err)
}
```

---

## TypeScript SDK

### TS Installation

```bash
# Inside the monorepo (portal or other package):
npm install ../client/ts

# Or from npm (once published):
npm install @costrict/team-client
# Peer dependency for Node.js environments:
npm install ws
```

### TS Leader Quick Start

```ts
import { TeamClient, MemberRoleLeader } from '@costrict/team-client';

const ac = new AbortController();
process.on('SIGINT', () => ac.abort());

const client = new TeamClient({
  serverUrl: 'https://api.example.com',
  token: process.env.TEAM_TOKEN!,
  sessionId: process.env.TEAM_SESSION_ID!,
  machineId: 'leader-machine-1',
  machineName: 'My Leader',
  role: MemberRoleLeader,
})
  .withLeaderPlugin(new MyPlanner())
  .withApprovalPlugin(new CLIApprover());

// Submit a plan once the client is connected.
client.start(ac.signal).catch(console.error);

setTimeout(async () => {
  await client.submitPlan('refactor the auth module');
}, 2000);
```

### TS Teammate Quick Start

```ts
import { TeamClient, MemberRoleTeammate } from '@costrict/team-client';

const client = new TeamClient({
  serverUrl: 'https://api.example.com',
  token: process.env.TEAM_TOKEN!,
  sessionId: process.env.TEAM_SESSION_ID!,
  machineId: 'teammate-machine-a',
  machineName: 'Machine A',
  role: MemberRoleTeammate,
})
  .withTeammatePlugin(new ShellExecutor())
  .withExplorePlugin(new LocalExplorer())
  .withApprovalPlugin(new CLIApprover());

await client.start(ac.signal);
```

---

## Plugin Reference

### LeaderPlugin

Called when `client.SubmitPlan(goal)` is invoked. Receives session context (including the list of online Teammates) and must return an ordered list of `TaskSpec`s.

**Go:**
```go
type LeaderPlugin interface {
    PlanTasks(ctx context.Context, req team.PlanTasksInput) ([]team.TaskSpec, error)
}

// PlanTasksInput provides context for planning decisions:
type PlanTasksInput struct {
    Goal      string
    SessionID string
    Members   []Member // online session participants
}
```

**TypeScript:**
```ts
interface LeaderPlugin {
    planTasks(signal: AbortSignal, req: PlanTasksInput): Promise<TaskSpec[]>;
}
```

**Implementation tips:**
- Call your LLM with `req.Goal` and the member list to produce an intelligent plan
- Pre-set `TaskSpec.ID` (UUID) on each spec to wire up `Dependencies` across the batch
- Set `TaskSpec.AssignedMemberId` using a member's `id` from `req.Members` to target a specific machine
- Use `TaskSpec.RepoAffinity` with repo remote URLs to let the server schedule to the right machine
- Return an empty slice to cancel the plan

**Example (Go):**
```go
type LLMPlanner struct{ llmClient *openai.Client }

func (p *LLMPlanner) PlanTasks(ctx context.Context, req team.PlanTasksInput) ([]team.TaskSpec, error) {
    prompt := fmt.Sprintf("Break this goal into coding tasks: %s\nTeammates: %v", req.Goal, req.Members)
    // ... call LLM, parse result into TaskSpecs ...
    idA := uuid.New().String()
    idB := uuid.New().String()
    return []team.TaskSpec{
        {ID: idA, Description: "Write tests for auth module", Priority: 9,
            RepoAffinity: []string{"https://github.com/org/repo"}},
        {ID: idB, Description: "Refactor auth module", Dependencies: []string{idA},
            RepoAffinity: []string{"https://github.com/org/repo"}},
    }, nil
}
```

---

### TeammatePlugin

Called for each `task.assigned` event. Runs in its own goroutine — report progress via `reporter` and return a `TaskResult` on completion.

**Go:**
```go
type TeammatePlugin interface {
    ExecuteTask(ctx context.Context, task team.Task, reporter team.ProgressReporter) (team.TaskResult, error)
}

type ProgressReporter interface {
    Report(pct int, message string)
}
```

**TypeScript:**
```ts
interface TeammatePlugin {
    executeTask(signal: AbortSignal, task: Task, reporter: ProgressReporter): Promise<TaskResult>;
}
```

**Implementation tips:**
- Call `reporter.Report(pct, msg)` periodically to update the leader's dashboard
- Return an error / rejected Promise to mark the task `failed`; the server will broadcast the failure and the leader can reassign
- Use `task.FileHints` to scope your operations to specific files
- Use `ctx` / `signal` cancellation to stop long-running work if the session ends

**Example (Go):**
```go
type AIExecutor struct{ agent *MyAIAgent }

func (e *AIExecutor) ExecuteTask(ctx context.Context, t team.Task, r team.ProgressReporter) (team.TaskResult, error) {
    r.Report(5, "analysing task")
    plan, err := e.agent.Plan(ctx, t.Description, t.FileHints)
    if err != nil {
        return team.TaskResult{}, fmt.Errorf("planning failed: %w", err)
    }
    r.Report(20, "executing")
    output, err := e.agent.Execute(ctx, plan)
    if err != nil {
        return team.TaskResult{}, err
    }
    r.Report(100, "done")
    return team.TaskResult{Output: output}, nil
}
```

---

### ApprovalPlugin

Called on both roles:
- **Leader** receives `approval.push` when a Teammate requests permission → call `PATCH /approvals/:id`
- **Teammate** can surface the `approval.response` result if needed

**Go:**
```go
type ApprovalPlugin interface {
    HandleApproval(ctx context.Context, req team.ApprovalRequest) (approved bool, note string, err error)
}
```

**TypeScript:**
```ts
interface ApprovalPlugin {
    handleApproval(signal: AbortSignal, req: ApprovalRequest): Promise<{ approved: boolean; note?: string }>;
}
```

**Approval payload fields:**

| Field | Description |
|-------|-------------|
| `ToolName` | The tool requesting permission (e.g. `"bash"`, `"file_write"`) |
| `Description` | Human-readable description of what the tool will do |
| `RiskLevel` | `"low"` / `"medium"` / `"high"` |
| `ToolInput` | The exact parameters the tool will be called with |

**Example (Go — CLI prompt):**
```go
type CLIApprover struct{}

func (a *CLIApprover) HandleApproval(_ context.Context, req team.ApprovalRequest) (bool, string, error) {
    fmt.Printf("\n[APPROVAL REQUEST] %s — risk: %s\n", req.ToolName, req.RiskLevel)
    fmt.Printf("  %s\n", req.Description)
    fmt.Printf("  input: %v\n", req.ToolInput)
    fmt.Print("  Approve? [y/N]: ")
    var answer string
    fmt.Scan(&answer)
    return strings.EqualFold(answer, "y"), "", nil
}
```

---

### ExplorePlugin

Called when the Leader sends a remote explore request targeting this Teammate's machine.

**Go:**
```go
type ExplorePlugin interface {
    Explore(ctx context.Context, req team.ExploreRequest) (team.ExploreResult, error)
}
```

**TypeScript:**
```ts
interface ExplorePlugin {
    explore(signal: AbortSignal, req: ExploreRequest): Promise<ExploreResult>;
}
```

**Query types:**

| Type | Params | Description |
|------|--------|-------------|
| `file_tree` | `path: string` | List files under a directory |
| `symbol_search` | `symbol: string, dir?: string` | Find symbol definitions / usages |
| `content_search` | `pattern: string, dir?: string, fileGlob?: string` | Full-text search |
| `git_log` | `dir: string, n?: int` | Recent commits |
| `dependency_graph` | `entry: string` | Import / module dependency graph |

**Example (Go — sandboxed shell queries):**
```go
type LocalExplorer struct{}

func (e *LocalExplorer) Explore(_ context.Context, req team.ExploreRequest) (team.ExploreResult, error) {
    results := make([]team.ExploreQueryResult, 0, len(req.Queries))
    for _, q := range req.Queries {
        r := team.ExploreQueryResult{Type: q.Type}
        switch q.Type {
        case "file_tree":
            path, _ := q.Params["path"].(string)
            out, _ := exec.Command("find", orDot(path), "-type", "f",
                "-not", "-path", "*/.*").Output()
            r.Output = truncate(string(out), 8192)
        case "content_search":
            pattern, _ := q.Params["pattern"].(string)
            dir, _ := q.Params["dir"].(string)
            out, _ := exec.Command("rg", "--no-heading", "-n", "-m", "20",
                pattern, orDot(dir)).Output()
            r.Output = string(out)
        case "git_log":
            dir, _ := q.Params["dir"].(string)
            out, _ := exec.Command("git", "-C", orDot(dir), "log",
                "--oneline", "-20").Output()
            r.Output = string(out)
        }
        results = append(results, r)
    }
    return team.ExploreResult{RequestID: req.RequestID, QueryResults: results}, nil
}

func orDot(s string) string {
    if s == "" { return "." }
    return s
}
```

---

## Config Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `ServerURL` / `serverUrl` | string | ✓ | Base HTTP/HTTPS URL of the server, e.g. `"https://api.example.com"` |
| `Token` / `token` | string | ✓ | JWT bearer token obtained from Casdoor login |
| `SessionID` / `sessionId` | string | ✓ | UUID of the team session to join |
| `MachineID` / `machineId` | string | ✓ | Stable, unique ID for this machine — **must be consistent across reconnects** |
| `MachineName` / `machineName` | string | | Human-readable display name shown in dashboards |
| `Role` / `role` | string | ✓ | `"leader"` or `"teammate"` |

**Creating a session (before constructing the client):**

```bash
# REST — create session first, then use the returned id as SessionID
curl -X POST https://api.example.com/api/team/sessions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "Sprint 42 refactor"}'
```

---

## Task DAG & Dependencies

Tasks within a single plan submission can depend on each other. The server unlocks a dependent task (moves it to `pending`) only after **all** its declared dependencies have `completed`.

```go
// Pre-assign UUIDs so dependency references survive serialisation.
idLint    := uuid.New().String()
idTest    := uuid.New().String()
idBuild   := uuid.New().String()

specs := []team.TaskSpec{
    {ID: idLint,  Description: "Run linter",      Priority: 9},
    {ID: idTest,  Description: "Run unit tests",  Priority: 8, Dependencies: []string{idLint}},
    {ID: idBuild, Description: "Build artifacts", Priority: 7, Dependencies: []string{idTest}},
}
```

```
lint ──▶ test ──▶ build
```

When `lint` completes the server automatically unlocks `test` and pushes a `task.assigned` event to the assigned Teammate.

> **Note:** The SDK pre-assigns UUIDs to all `TaskSpec`s that lack one before POSTing to the server. The server respects provided IDs, so dependency references are always stable.

---

## Repo Affinity

Set `TaskSpec.RepoAffinity` to a list of git remote URLs. The scheduler will prefer assigning the task to a Teammate that has already registered those repos.

Register a repo on the Teammate side:

**Go:**
```go
// Call after Start(), before or right after the client connects.
err := c.RegisterRepo(
    "https://github.com/org/repo",  // remote URL (used as the affinity key)
    "/home/user/projects/repo",     // local path
    "feat/auth-refactor",           // current branch
    true,                           // has uncommitted changes
)
```

**TypeScript:**
```ts
// Available on TeammateAgent via the client internals:
await client.registerRepo(
  'https://github.com/org/repo',
  '/home/user/projects/repo',
  'feat/auth-refactor',
  true,
);
```

> `RegisterRepo` is also available on the Go Client as a convenience wrapper.  
> Internally it calls `POST /api/team/sessions/:id/repos`.

---

## REST API Reference

All REST endpoints require `Authorization: Bearer <token>` unless noted.

### Sessions

| Method | Path | Body / Query | Description |
|--------|------|--------------|-------------|
| `POST` | `/api/team/sessions` | `{name}` | Create a session |
| `GET` | `/api/team/sessions/:id` | — | Get session details |
| `PATCH` | `/api/team/sessions/:id` | `{status?, name?}` | Update session |
| `DELETE` | `/api/team/sessions/:id` | — | Delete session |

### Members

| Method | Path | Body / Query | Description |
|--------|------|--------------|-------------|
| `POST` | `/api/team/sessions/:id/members` | `{machineId, machineName?}` | Join session |
| `GET` | `/api/team/sessions/:id/members` | — | List members |
| `DELETE` | `/api/team/sessions/:id/members/:mid` | — | Leave session |

### Tasks

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `POST` | `/api/team/sessions/:id/tasks` | `{tasks[], fencingToken}` | Submit task plan |
| `GET` | `/api/team/sessions/:id/tasks` | — | List all tasks |
| `GET` | `/api/team/tasks/:taskId` | — | Get single task |
| `PATCH` | `/api/team/tasks/:taskId` | `{status, result?, errorMessage?}` | Update task status |

### Approvals

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `GET` | `/api/team/sessions/:id/approvals` | — | List pending approvals |
| `PATCH` | `/api/team/approvals/:approvalId` | `{status, feedback?}` | Respond to approval |

### Leader Election

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `POST` | `/api/team/sessions/:id/leader/elect` | `{machineId}` | Attempt election |
| `POST` | `/api/team/sessions/:id/leader/heartbeat` | `{machineId}` | Renew leader lock (every 10 s) |
| `GET` | `/api/team/sessions/:id/leader` | — | Get current leader |

### Repos & Progress

| Method | Path | Body / Query | Description |
|--------|------|--------------|-------------|
| `POST` | `/api/team/sessions/:id/repos` | `{memberId, repoRemoteUrl, repoLocalPath, currentBranch, hasUncommittedChanges, lastSyncedAt}` | Register local repo |
| `GET` | `/api/team/sessions/:id/repos` | `?remoteUrl=` or `?memberId=` | Query repo affinity |
| `GET` | `/api/team/sessions/:id/progress` | — | Get session progress snapshot |

### Remote Explore

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `POST` | `/api/team/sessions/:id/explore` | `{targetMachineId, queries[]}` | Synchronous explore (30 s timeout) |

---

## WebSocket Event Reference

**Connect:** `wss://<serverUrl>/ws/sessions/<sessionId>?machineId=<machineId>&token=<token>`

All messages use the `CloudEvent` envelope:

```json
{
  "eventId": "uuid",
  "type": "event.type",
  "sessionId": "uuid",
  "timestamp": 1713100000000,
  "payload": { ... }
}
```

### Client → Server Events

| Type | Payload | Description |
|------|---------|-------------|
| `task.claim` | `{taskId}` | Claim a pending task (called automatically by the SDK) |
| `task.progress` | `{taskId, percent, message}` | Report execution progress |
| `task.complete` | `{taskId, result}` | Mark task completed |
| `task.fail` | `{taskId, errorMessage}` | Mark task failed |
| `approval.request` | `{toolName, description, riskLevel, toolInput}` | Request leader approval |
| `approval.respond` | `{approvalId, status, feedback?}` | Leader responds to approval |
| `explore.result` | `{requestId, queryResults[], fromMachineId, error?}` | Return explore results |
| `repo.register` | `{repoRemoteUrl, repoLocalPath, currentBranch, hasUncommittedChanges}` | Register local repo |
| `leader.elect` | — | Trigger leader election |
| `leader.heartbeat` | — | Renew leader lock (sent automatically every 10 s) |
| `message.send` | `{to, body}` | Send a message (`to` = machineId or `"broadcast"`) |

### Server → Client Events

| Type | Payload | Description |
|------|---------|-------------|
| `task.assigned` | `{task: Task}` | A new task has been assigned to this machine |
| `approval.push` | `{approval: ApprovalRequest}` | Teammate is requesting approval (leader only) |
| `approval.response` | `{approvalId, status, feedback}` | Leader responded to approval request |
| `explore.request` | `{requestId, fromMachineId, queries[]}` | Leader is requesting code exploration |
| `teammate.status` | `{machineId, status}` | A member came online / went offline |
| `leader.elected` | `{leaderId, fencingToken}` | A new leader was elected |
| `leader.expired` | `{expiredLeaderId}` | Leader lock expired |
| `session.updated` | `{taskId, status}` | Task status changed |
| `message.receive` | `{from, body}` | Message from another machine |
| `error` | `{message}` | Server-side error notification |

---

## Examples

Runnable examples are in:

- **Go:** [`client/go/example/main.go`](go/example/main.go) — CLI tool, supports both leader and teammate modes
- **TypeScript Leader:** [`client/ts/examples/leader.ts`](ts/examples/leader.ts)
- **TypeScript Teammate:** [`client/ts/examples/teammate.ts`](ts/examples/teammate.ts)

### Running the Go example

```bash
# Teammate
go run ./client/go/example/main.go \
  --server https://api.example.com \
  --token "$TOKEN" \
  --session "$SESSION_ID" \
  --machine my-mac-$(hostname) \
  --role teammate

# Leader (submits a plan immediately after connecting)
go run ./client/go/example/main.go \
  --server https://api.example.com \
  --token "$TOKEN" \
  --session "$SESSION_ID" \
  --machine leader-$(hostname) \
  --role leader \
  --goal "refactor the authentication module"
```

### Running the TypeScript example

```bash
cd client/ts
npm install
npx ts-node examples/leader.ts
# or
npx ts-node examples/teammate.ts
```
