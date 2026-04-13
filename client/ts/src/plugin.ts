// plugin.ts — Plugin interfaces that host applications implement to customise
// Cloud Team Agent behaviour.  All four interfaces are independent; register
// only the ones your role requires.

import type {
  ApprovalRequest,
  ExploreRequest,
  ExploreResult,
  PlanTasksInput,
  Task,
  TaskResult,
  TaskSpec,
} from './types.js';

// ─── LeaderPlugin ─────────────────────────────────────────────────────────

/**
 * Decomposes a natural-language goal into an ordered task DAG.
 * Inject your LLM / planning logic here.
 *
 * The SDK calls planTasks when the host application calls client.submitPlan().
 * Return an empty array to indicate that no tasks are needed.
 */
export interface LeaderPlugin {
  planTasks(signal: AbortSignal, req: PlanTasksInput): Promise<TaskSpec[]>;
}

// ─── TeammatePlugin ───────────────────────────────────────────────────────

/**
 * Executes an assigned task and streams progress updates.
 * Inject your shell runner, code executor, or AI agent here.
 *
 * The SDK calls executeTask for each incoming task.assigned event.
 * Throw an Error (or return a rejected Promise) to mark the task as failed.
 */
export interface TeammatePlugin {
  executeTask(
    signal: AbortSignal,
    task: Task,
    reporter: ProgressReporter
  ): Promise<TaskResult>;
}

// ─── ApprovalPlugin ───────────────────────────────────────────────────────

/**
 * Displays an approval request to the user and collects a decision.
 * Inject a CLI prompt, GUI dialog, or any other UI here.
 *
 * The SDK calls handleApproval for each incoming approval.push event (leader)
 * and exposes the response result to teammates via approval.response events.
 */
export interface ApprovalPlugin {
  handleApproval(
    signal: AbortSignal,
    req: ApprovalRequest
  ): Promise<{ approved: boolean; note?: string }>;
}

// ─── ExplorePlugin ────────────────────────────────────────────────────────

/**
 * Executes local file / code queries on behalf of a remote explore.request.
 * Allowed operations: file tree, symbol search, content search, git log,
 * dependency graph — read-only, sandboxed to the local repository.
 *
 * The SDK calls explore when the server routes an explore.request to this machine.
 */
export interface ExplorePlugin {
  explore(signal: AbortSignal, req: ExploreRequest): Promise<ExploreResult>;
}

// ─── ProgressReporter ─────────────────────────────────────────────────────

/**
 * Lets TeammatePlugin stream incremental progress back to the session
 * without blocking the execution loop.
 */
export interface ProgressReporter {
  report(pct: number, message: string): void;
}
