// types.ts — Shared event and model types for the Cloud Team Agent SDK.
// Mirrors the server's internal/team/types.go definitions.

// ─── Core event envelope ──────────────────────────────────────────────────

export interface CloudEvent {
  eventId: string;
  type: string;
  sessionId: string;
  timestamp: number;
  payload?: Record<string, unknown>;
}

// ─── Task types ───────────────────────────────────────────────────────────

/** Full task record received from the server (e.g. via task.assigned). */
export interface Task {
  id: string;
  sessionId: string;
  description: string;
  repoAffinity?: string[];
  fileHints?: string[];
  dependencies?: string[];
  assignedMemberId?: string;
  status: string;
  priority: number;
  retryCount: number;
  maxRetries: number;
  errorMessage?: string;
  createdAt: string;
  claimedAt?: string;
  startedAt?: string;
  completedAt?: string;
}

/**
 * TaskSpec is what LeaderPlugin.planTasks returns.
 * Set id to a pre-generated UUID if you want dependency DAGs to work within a
 * single batch — the server will use the provided id instead of generating one.
 */
export interface TaskSpec {
  id?: string;
  description: string;
  repoAffinity?: string[];
  fileHints?: string[];
  dependencies?: string[]; // references IDs within the same batch
  assignedMemberId?: string;
  priority?: number;
  maxRetries?: number;
}

/** Returned by TeammatePlugin.executeTask on success. */
export interface TaskResult {
  output?: string;
  files?: string[];
  extraData?: Record<string, unknown>;
}

// ─── Approval types ───────────────────────────────────────────────────────

export interface ApprovalRequest {
  id: string;
  sessionId: string;
  requesterId: string;
  toolName: string;
  toolInput: Record<string, unknown>;
  description?: string;
  riskLevel: string;
  status: string;
  createdAt: string;
}

// ─── Explore types ────────────────────────────────────────────────────────

export interface ExploreQuery {
  type:
    | 'file_tree'
    | 'symbol_search'
    | 'content_search'
    | 'git_log'
    | 'dependency_graph';
  params: Record<string, unknown>;
}

export interface ExploreRequest {
  requestId: string;
  sessionId: string;
  fromMachineId: string;
  queries: ExploreQuery[];
}

export interface ExploreQueryResult {
  type: string;
  output: string;
  truncated: boolean;
}

export interface ExploreResult {
  requestId: string;
  queryResults: ExploreQueryResult[];
  error?: string;
}

// ─── Member type ──────────────────────────────────────────────────────────

export interface Member {
  id: string;
  sessionId: string;
  machineId: string;
  machineName?: string;
  role: string;
  status: string;
}

// ─── PlanTasksInput ───────────────────────────────────────────────────────

export interface PlanTasksInput {
  goal: string;
  sessionId: string;
  /** Current session participants — use to make assignment decisions. */
  members: Member[];
}

// ─── Config ───────────────────────────────────────────────────────────────

export interface TeamClientConfig {
  /** Base HTTP/HTTPS URL of the costrict server, e.g. "https://api.example.com". */
  serverUrl: string;
  /** JWT bearer token for authentication. */
  token: string;
  /** UUID of the existing team session to join. */
  sessionId: string;
  /** Stable, unique identifier for this machine. Must be consistent across reconnects. */
  machineId: string;
  /** Human-readable label for this machine (optional). */
  machineName?: string;
  /** Either "leader" or "teammate". */
  role: string;
}

// ─── Event type constants (Client → Cloud) ────────────────────────────────

export const EventSessionCreate = 'session.create';
export const EventSessionJoin = 'session.join';
export const EventTaskPlanSubmit = 'task.plan.submit';
export const EventTaskClaim = 'task.claim';
export const EventTaskProgress = 'task.progress';
export const EventTaskComplete = 'task.complete';
export const EventTaskFail = 'task.fail';
export const EventApprovalRequest = 'approval.request';
export const EventApprovalRespond = 'approval.respond';
export const EventMessageSend = 'message.send';
export const EventRepoRegister = 'repo.register';
export const EventExploreRequest = 'explore.request';
export const EventExploreResult = 'explore.result';
export const EventLeaderElect = 'leader.elect';
export const EventLeaderHeartbeat = 'leader.heartbeat';

// ─── Event type constants (Cloud → Client) ────────────────────────────────

export const EventTaskAssigned = 'task.assigned';
export const EventApprovalPush = 'approval.push';
export const EventApprovalResponse = 'approval.response';
export const EventMessageReceive = 'message.receive';
export const EventSessionUpdated = 'session.updated';
export const EventTeammateStatus = 'teammate.status';
export const EventLeaderElected = 'leader.elected';
export const EventLeaderExpired = 'leader.expired';
export const EventError = 'error';

// ─── Status constants ─────────────────────────────────────────────────────

export const SessionStatusActive = 'active';
export const SessionStatusPaused = 'paused';
export const SessionStatusCompleted = 'completed';
export const SessionStatusFailed = 'failed';

export const MemberStatusOnline = 'online';
export const MemberStatusOffline = 'offline';
export const MemberStatusBusy = 'busy';

export const MemberRoleLeader = 'leader';
export const MemberRoleTeammate = 'teammate';

export const TaskStatusPending = 'pending';
export const TaskStatusAssigned = 'assigned';
export const TaskStatusClaimed = 'claimed';
export const TaskStatusRunning = 'running';
export const TaskStatusCompleted = 'completed';
export const TaskStatusFailed = 'failed';
