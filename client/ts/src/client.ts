// client.ts — TeamClient: top-level entry point for the Cloud Team Agent SDK.

import { v4 as uuidv4 } from 'uuid';
import type {
  CloudEvent,
  Member,
  TeamClientConfig,
} from './types.js';
import { MemberRoleLeader, MemberRoleTeammate } from './types.js';
import type {
  ApprovalPlugin,
  ExplorePlugin,
  LeaderPlugin,
  TeammatePlugin,
} from './plugin.js';
import { LeaderAgent } from './leader.js';
import { TeammateAgent } from './teammate.js';
import { WSConnection } from './ws.js';

/**
 * TeamClient is the top-level entry point for the Cloud Team Agent SDK.
 *
 * Usage (leader):
 * ```ts
 * const client = new TeamClient(cfg)
 *   .withLeaderPlugin(myPlanner)
 *   .withApprovalPlugin(myApprover);
 * await client.start(abortSignal);
 * ```
 *
 * Usage (teammate):
 * ```ts
 * const client = new TeamClient(cfg)
 *   .withTeammatePlugin(myExecutor)
 *   .withExplorePlugin(myExplorer);
 * await client.start(abortSignal);
 * ```
 */
export class TeamClient {
  private cfg: TeamClientConfig;
  private ws: WSConnection;
  private leader: LeaderAgent | null = null;
  private teammate: TeammateAgent | null = null;
  private controller: AbortController | null = null;

  // Plugin slots
  private leaderPlugin: LeaderPlugin | null = null;
  private teammatePlugin: TeammatePlugin | null = null;
  private approvalPlugin: ApprovalPlugin | null = null;
  private explorePlugin: ExplorePlugin | null = null;

  constructor(cfg: TeamClientConfig) {
    this.cfg = cfg;
    this.ws = new WSConnection(cfg);
    this.ws.onEvent = (evt) => this.dispatch(evt);
  }

  // ─── Fluent plugin registration ───────────────────────────────────────

  withLeaderPlugin(p: LeaderPlugin): this {
    this.leaderPlugin = p;
    return this;
  }

  withTeammatePlugin(p: TeammatePlugin): this {
    this.teammatePlugin = p;
    return this;
  }

  withApprovalPlugin(p: ApprovalPlugin): this {
    this.approvalPlugin = p;
    return this;
  }

  withExplorePlugin(p: ExplorePlugin): this {
    this.explorePlugin = p;
    return this;
  }

  // ─── Lifecycle ────────────────────────────────────────────────────────

  /**
   * Connects to the server and starts processing events.
   * Resolves when the signal fires or a fatal error occurs.
   */
  async start(signal?: AbortSignal): Promise<void> {
    this.controller = new AbortController();
    const innerSignal = this.controller.signal;

    // Combine caller's signal with our internal one.
    const combinedSignal = signal
      ? combineSignals(signal, innerSignal)
      : innerSignal;

    // Role-specific initialisation via REST.
    switch (this.cfg.role) {
      case MemberRoleLeader: {
        this.leader = new LeaderAgent(this.cfg, this.ws, {
          leaderPlugin: this.leaderPlugin,
          approvalPlugin: this.approvalPlugin,
          doJSON: this.doJSON.bind(this),
          fetchMembers: this.fetchMembers.bind(this),
        });
        await this.leader.init(combinedSignal);
        break;
      }
      case MemberRoleTeammate: {
        this.teammate = new TeammateAgent(this.cfg, this.ws, {
          teammatePlugin: this.teammatePlugin,
          approvalPlugin: this.approvalPlugin,
          explorePlugin: this.explorePlugin,
          doJSON: this.doJSON.bind(this),
        });
        await this.teammate.init(combinedSignal);
        break;
      }
      default:
        throw new Error(
          `Unknown role "${this.cfg.role}" — must be "${MemberRoleLeader}" or "${MemberRoleTeammate}"`
        );
    }

    // Start WS loop in the background, then wait until the connection is open.
    void this.ws.start(combinedSignal);
    await this.ws.waitConnected();
  }

  /**
   * Calls LeaderPlugin.planTasks and submits the resulting task plan.
   * Must only be called after start() and with role = "leader".
   */
  async submitPlan(goal: string, signal?: AbortSignal): Promise<void> {
    if (!this.leader) {
      throw new Error('submitPlan requires role = "leader"');
    }
    await this.leader.submitPlan(goal, signal ?? new AbortController().signal);
  }

  /** Stops the client and closes the WebSocket. */
  stop(): void {
    this.controller?.abort();
    this.ws.close();
  }

  // ─── Internal helpers ─────────────────────────────────────────────────

  private dispatch(evt: CloudEvent): void {
    this.leader?.handle(evt);
    this.teammate?.handle(evt);
  }

  /**
   * Executes a REST request against the server.
   * Returns the parsed JSON body, or undefined for empty responses.
   */
  async doJSON<T = unknown>(
    method: string,
    path: string,
    body?: unknown
  ): Promise<T> {
    const url = this.cfg.serverUrl.replace(/\/$/, '') + path;
    const res = await fetch(url, {
      method,
      headers: {
        'Content-Type': 'application/json',
        ...(this.cfg.token ? { Authorization: `Bearer ${this.cfg.token}` } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });

    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText })) as { error?: string };
      throw new Error(`HTTP ${res.status} ${path}: ${err.error ?? res.statusText}`);
    }

    const text = await res.text();
    return text ? (JSON.parse(text) as T) : (undefined as T);
  }

  async fetchMembers(): Promise<Member[]> {
    const resp = await this.doJSON<{ members: Member[] }>(
      'GET',
      `/api/team/sessions/${this.cfg.sessionId}/members`
    );
    return resp?.members ?? [];
  }

  /**
   * Creates a new team session on the server and returns the session ID.
   * Call this before constructing TeamClient instances that share the session.
   */
  static async createSession(
    serverUrl: string,
    token: string,
    name?: string
  ): Promise<string> {
    const url = serverUrl.replace(/\/$/, '') + '/api/team/sessions';
    const res = await fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ name: name ?? 'e2e-test-session' }),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText })) as { error?: string };
      throw new Error(`HTTP ${res.status} /api/team/sessions: ${err.error ?? res.statusText}`);
    }
    const data = await res.json() as { id: string };
    return data.id;
  }
}

// ─── Utilities ────────────────────────────────────────────────────────────

/** Creates a new CloudEvent with a fresh UUID and the current timestamp. */
export function newEvent(
  type: string,
  sessionId: string,
  payload?: Record<string, unknown>
): CloudEvent {
  return {
    eventId: uuidv4(),
    type,
    sessionId,
    timestamp: Date.now(),
    payload,
  };
}

/** Combines two AbortSignals into one that fires when either fires. */
function combineSignals(a: AbortSignal, b: AbortSignal): AbortSignal {
  const ctrl = new AbortController();
  const abort = () => ctrl.abort();
  a.addEventListener('abort', abort, { once: true });
  b.addEventListener('abort', abort, { once: true });
  return ctrl.signal;
}
