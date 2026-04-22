/**
 * @costrict/team-client
 *
 * Cloud Team Agent client SDK.
 *
 * Quick start (leader):
 * ```ts
 * import { TeamClient, MemberRoleLeader } from '@costrict/team-client';
 *
 * const client = new TeamClient({
 *   serverUrl: 'https://api.example.com',
 *   token: myJWT,
 *   sessionId: sessionId,
 *   machineId: 'machine-a',
 *   role: MemberRoleLeader,
 * }).withLeaderPlugin(myPlanner).withApprovalPlugin(myApprover);
 *
 * const ac = new AbortController();
 * await client.start(ac.signal);
 * ```
 *
 * Quick start (teammate):
 * ```ts
 * const client = new TeamClient({ ..., role: MemberRoleTeammate })
 *   .withTeammatePlugin(myExecutor)
 *   .withExplorePlugin(myExplorer);
 *
 * await client.start(ac.signal);
 * ```
 */

// Main class
export { TeamClient, newEvent } from './client.js';

// Plugin interfaces
export type {
  LeaderPlugin,
  TeammatePlugin,
  ApprovalPlugin,
  ExplorePlugin,
  ProgressReporter,
} from './plugin.js';

// Types
export type {
  CloudEvent,
  TeamClientConfig,
  Task,
  TaskSpec,
  TaskResult,
  ApprovalRequest,
  ExploreQuery,
  ExploreRequest,
  ExploreQueryResult,
  ExploreResult,
  Member,
  PlanTasksInput,
} from './types.js';

// Constants — event types
export {
  EventSessionCreate,
  EventSessionJoin,
  EventTaskPlanSubmit,
  EventTaskClaim,
  EventTaskProgress,
  EventTaskComplete,
  EventTaskFail,
  EventApprovalRequest,
  EventApprovalRespond,
  EventMessageSend,
  EventRepoRegister,
  EventExploreRequest,
  EventExploreResult,
  EventLeaderElect,
  EventLeaderHeartbeat,
  EventTaskAssigned,
  EventApprovalPush,
  EventApprovalResponse,
  EventMessageReceive,
  EventSessionUpdated,
  EventTeammateStatus,
  EventLeaderElected,
  EventLeaderExpired,
  EventError,
} from './types.js';

// Constants — status & roles
export {
  SessionStatusActive,
  SessionStatusPaused,
  SessionStatusCompleted,
  SessionStatusFailed,
  MemberStatusOnline,
  MemberStatusOffline,
  MemberStatusBusy,
  MemberRoleLeader,
  MemberRoleTeammate,
  TaskStatusPending,
  TaskStatusAssigned,
  TaskStatusClaimed,
  TaskStatusRunning,
  TaskStatusCompleted,
  TaskStatusFailed,
} from './types.js';
