// leader.ts — Leader role: election, heartbeat, plan submission, approval routing.

import { v4 as uuidv4 } from 'uuid';
import type { ApprovalRequest, CloudEvent, Member, TaskSpec, TeamClientConfig } from './types.js';
import {
  EventApprovalPush,
  EventApprovalRequest,
  EventLeaderExpired,
} from './types.js';
import type { ApprovalPlugin, LeaderPlugin } from './plugin.js';
import type { WSConnection } from './ws.js';
import { newEvent } from './client.js';

const HEARTBEAT_INTERVAL_MS = 10_000;

export interface LeaderAgentDeps {
  leaderPlugin: LeaderPlugin | null;
  approvalPlugin: ApprovalPlugin | null;
  doJSON: <T>(method: string, path: string, body?: unknown) => Promise<T>;
  fetchMembers: () => Promise<Member[]>;
}

/** LeaderAgent owns all leader-role logic and handles relevant inbound events. */
export class LeaderAgent {
  private cfg: TeamClientConfig;
  private ws: WSConnection;
  private deps: LeaderAgentDeps;
  private fencingToken = 0;
  private heartbeatTimer: ReturnType<typeof setInterval> | null = null;

  constructor(cfg: TeamClientConfig, ws: WSConnection, deps: LeaderAgentDeps) {
    this.cfg = cfg;
    this.ws = ws;
    this.deps = deps;
  }

  /**
   * Performs REST-based initialisation:
   * 1. Attempts leader election.
   * 2. Starts the heartbeat loop.
   */
  async init(_signal: AbortSignal): Promise<void> {
    const resp = await this.deps.doJSON<{
      elected: boolean;
      fencingToken: number;
      leaderId: string;
    }>(
      'POST',
      `/api/team/sessions/${this.cfg.sessionId}/leader/elect`,
      { machineId: this.cfg.machineId }
    );
    this.fencingToken = resp.fencingToken;
    this.startHeartbeat();
  }

  /**
   * Calls LeaderPlugin.planTasks and POSTs the resulting tasks to the server.
   * Pre-assigns UUIDs so dependency references within the batch are stable.
   */
  async submitPlan(goal: string, signal: AbortSignal): Promise<void> {
    if (!this.deps.leaderPlugin) {
      throw new Error('No LeaderPlugin registered');
    }

    const members = await this.deps.fetchMembers();
    const specs = await this.deps.leaderPlugin.planTasks(signal, {
      goal,
      sessionId: this.cfg.sessionId,
      members,
    });

    if (!specs.length) {
      throw new Error('LeaderPlugin returned an empty plan');
    }

    // Pre-assign stable IDs so dependency references survive serialisation.
    const enriched: TaskSpec[] = specs.map((s) => ({
      ...s,
      id: s.id ?? uuidv4(),
    }));

    await this.deps.doJSON('POST', `/api/team/sessions/${this.cfg.sessionId}/tasks`, {
      tasks: enriched,
      fencingToken: this.fencingToken,
    });
  }

  /** Dispatches inbound events relevant to the leader role. */
  handle(evt: CloudEvent): void {
    switch (evt.type) {
      case EventApprovalPush:
        this.handleApprovalPush(evt);
        break;

      case EventLeaderExpired:
        this.handleLeaderExpired();
        break;
    }
  }

  /** Sends an approval.request WS event (for leader-originated tool approval). */
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

  stop(): void {
    if (this.heartbeatTimer !== null) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  // ─── Private ─────────────────────────────────────────────────────────

  private startHeartbeat(): void {
    this.heartbeatTimer = setInterval(async () => {
      try {
        await this.deps.doJSON<{ renewed: boolean }>(
          'POST',
          `/api/team/sessions/${this.cfg.sessionId}/leader/heartbeat`,
          { machineId: this.cfg.machineId }
        );
      } catch {
        // Best-effort — the server will broadcast leader.expired if the lock lapses.
      }
    }, HEARTBEAT_INTERVAL_MS);
  }

  private handleApprovalPush(evt: CloudEvent): void {
    if (!this.deps.approvalPlugin) return;

    const approval = evt.payload?.['approval'] as ApprovalRequest | undefined;
    if (!approval) return;

    void (async () => {
      const ctrl = new AbortController();
      try {
        const { approved, note } = await this.deps.approvalPlugin!.handleApproval(
          ctrl.signal,
          approval
        );
        await this.deps.doJSON('PATCH', `/api/team/approvals/${approval.id}`, {
          status: approved ? 'approved' : 'rejected',
          feedback: note ?? '',
        });
      } catch {
        // Swallow errors — the approval will time out on the server side.
      }
    })();
  }

  private handleLeaderExpired(): void {
    void (async () => {
      try {
        const resp = await this.deps.doJSON<{
          elected: boolean;
          fencingToken: number;
        }>(
          'POST',
          `/api/team/sessions/${this.cfg.sessionId}/leader/elect`,
          { machineId: this.cfg.machineId }
        );
        if (resp.elected) {
          this.fencingToken = resp.fencingToken;
          this.startHeartbeat();
        }
      } catch {
        // Another machine may have been elected.
      }
    })();
  }
}
