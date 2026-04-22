package team

import "context"

// LeaderPlugin decomposes a natural-language goal into an ordered task DAG.
// Inject your LLM / planning logic here.
// The SDK calls PlanTasks when the host application calls Client.SubmitPlan.
type LeaderPlugin interface {
	PlanTasks(ctx context.Context, req PlanTasksInput) ([]TaskSpec, error)
}

// TeammatePlugin executes an assigned task and streams progress updates.
// Inject your shell runner / code executor here.
// The SDK calls ExecuteTask for each incoming task.assigned event.
type TeammatePlugin interface {
	ExecuteTask(ctx context.Context, task Task, reporter ProgressReporter) (TaskResult, error)
}

// ApprovalPlugin displays an incoming approval request to the user and collects
// a decision. Inject a CLI prompt, GUI dialog, or any other UI here.
// The SDK calls HandleApproval for each incoming approval.push event (leader role)
// or approval.response event (teammate role).
type ApprovalPlugin interface {
	HandleApproval(ctx context.Context, req ApprovalRequest) (approved bool, note string, err error)
}

// ExplorePlugin executes local file / code queries on behalf of a remote
// explore.request. The SDK calls Explore when the server routes an
// explore.request to this machine.
// Allowed operations: file tree listing, symbol search, content search,
// git log, dependency graph — read-only, sandboxed to the local repo.
type ExplorePlugin interface {
	Explore(ctx context.Context, req ExploreRequest) (ExploreResult, error)
}

// ProgressReporter lets TeammatePlugin stream incremental progress updates
// back to the session without blocking the execution goroutine.
type ProgressReporter interface {
	Report(pct int, message string)
}
