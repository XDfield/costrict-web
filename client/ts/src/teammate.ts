// teammate.ts — Teammate role: session join, task execution, explore, approvals.

import type { CloudEvent, ExploreRequest, Task, TeamClientConfig } from './types.js';
import {
  EventApprovalRequest,
  EventExploreRequest,
  EventTaskAssigned,
  EventTaskClaim,
  EventTaskComplete,
  EventTaskFail,
  EventTaskProgress,
} from './types.js';
import type { ApprovalPlugin, ExplorePlugin, ProgressReporter, TeammatePlugin } from './plugin.js';
import type { WSConnection } from './ws.js';
import { newEvent } from './client.js';

export interface TeammateAgentDeps {
  teammatePlugin: TeammatePlugin | null;
  approvalPlugin: ApprovalPlugin | null;
  explorePlugin: ExplorePlugin | null;
  doJSON: <T>(method: string, path: string, body?: unknown) => Promise<T>;
}

/** TeammateAgent owns all teammate-role logic and handles relevant inbound events. */
export class TeammateAgent {
  private cfg: TeamClientConfig;
  private ws: WSConnection;
  private deps: TeammateAgentDeps;
  private memberId = '';

  constructor(cfg: TeamClientConfig, ws: WSConnection, deps: TeammateAgentDeps) {
    this.cfg = cfg;
    this.ws = ws;
    this.deps = deps;
  }

  /** Registers this machine as a session member via REST. */
  async init(_signal: AbortSignal): Promise<void> {
    const resp = await this.deps.doJSON<{ id: string }>(
      'POST',
      `/api/team/sessions/${this.cfg.sessionId}/members`,
      {
        machineId: this.cfg.machineId,
        machineName: this.cfg.machineName ?? '',
      }
    );
    this.memberId = resp?.id ?? '';
  }

  /** Dispatches inbound events relevant to the teammate role. */
  handle(evt: CloudEvent): void {
    switch (evt.type) {
      case EventTaskAssigned:
        this.handleTaskAssigned(evt);
        break;

      case EventExploreRequest:
        this.handleExploreRequest(evt);
        break;
    }
  }

  /**
   * Sends an approval.request to the leader via WebSocket.
   * riskLevel should be "low", "medium", or "high".
   */
  requestApproval(
    toolName: string,
    description: string,
    riskLevel: string,
    toolInput: Record<string, unknown>
  ): void {
    this.ws.send(
      newEvent(EventApprovalRequest, this.cfg.sessionId, {
        toolName,
        description,
        riskLevel,
        toolInput,
      })
    );
  }

  /**
   * Registers a local repository with the session's affinity registry.
   * Call this on startup so the leader can schedule repo-specific tasks here.
   */
  async registerRepo(
    remoteUrl: string,
    localPath: string,
    branch: string,
    dirty: boolean
  ): Promise<void> {
    await this.deps.doJSON('POST', `/api/team/sessions/${this.cfg.sessionId}/repos`, {
      memberId: this.memberId,
      repoRemoteUrl: remoteUrl,
      repoLocalPath: localPath,
      currentBranch: branch,
      hasUncommittedChanges: dirty,
      lastSyncedAt: new Date().toISOString(),
    });
  }

  // ─── Private ─────────────────────────────────────────────────────────

  private handleTaskAssigned(evt: CloudEvent): void {
    if (!this.deps.teammatePlugin) return;

    const task = evt.payload?.['task'] as Task | undefined;
    if (!task?.id) return;

    // Claim immediately so the leader knows this task is being worked on.
    this.ws.send(
      newEvent(EventTaskClaim, this.cfg.sessionId, { taskId: task.id })
    );

    void this.executeTask(task);
  }

  private async executeTask(task: Task): Promise<void> {
    const ctrl = new AbortController();
    const reporter: ProgressReporter = {
      report: (pct, message) => {
        this.ws.send(
          newEvent(EventTaskProgress, this.cfg.sessionId, {
            taskId: task.id,
            percent: pct,
            message,
          })
        );
      },
    };

    // Signal start.
    this.ws.send(
      newEvent(EventTaskProgress, this.cfg.sessionId, {
        taskId: task.id,
        percent: 0,
        message: 'started',
      })
    );

    try {
      const result = await this.deps.teammatePlugin!.executeTask(
        ctrl.signal,
        task,
        reporter
      );
      this.ws.send(
        newEvent(EventTaskComplete, this.cfg.sessionId, {
          taskId: task.id,
          result,
        })
      );
    } catch (err) {
      this.ws.send(
        newEvent(EventTaskFail, this.cfg.sessionId, {
          taskId: task.id,
          errorMessage: err instanceof Error ? err.message : String(err),
        })
      );
    }
  }

  private handleExploreRequest(evt: CloudEvent): void {
    if (!this.deps.explorePlugin) return;

    const requestId = evt.payload?.['requestId'] as string | undefined;
    const fromMachineId = evt.payload?.['fromMachineId'] as string | undefined;
    if (!requestId) return;

    const req: ExploreRequest = {
      requestId,
      sessionId: this.cfg.sessionId,
      fromMachineId: fromMachineId ?? '',
      queries: (evt.payload?.['queries'] as ExploreRequest['queries']) ?? [],
    };

    void (async () => {
      const ctrl = new AbortController();
      try {
        const result = await this.deps.explorePlugin!.explore(ctrl.signal, req);
        this.ws.send(
          newEvent('explore.result', this.cfg.sessionId, {
            requestId: result.requestId,
            queryResults: result.queryResults,
            fromMachineId,
            error: result.error,
          })
        );
      } catch (err) {
        this.ws.send(
          newEvent('explore.result', this.cfg.sessionId, {
            requestId,
            queryResults: [],
            fromMachineId,
            error: err instanceof Error ? err.message : String(err),
          })
        );
      }
    })();
  }
}
