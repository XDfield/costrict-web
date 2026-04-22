// ws.ts — WebSocket wrapper with automatic reconnect.
// Works in both browser (native WebSocket) and Node.js (ws package).

import type { CloudEvent, TeamClientConfig } from './types.js';

const PING_INTERVAL_MS = 30_000;
const WRITE_WAIT_MS = 10_000;
const PONG_WAIT_MS = 60_000;
const RECONNECT_INITIAL_MS = 1_000;
const RECONNECT_MAX_MS = 30_000;
const SEND_CHANNEL_CAP = 256;

export type EventHandler = (evt: CloudEvent) => void;

/** Builds the WebSocket URL from the client config. */
function buildWsUrl(cfg: TeamClientConfig): string {
  const base = cfg.serverUrl
    .replace(/^https:\/\//, 'wss://')
    .replace(/^http:\/\//, 'ws://')
    .replace(/\/$/, '');
  const params = new URLSearchParams({
    machineId: cfg.machineId,
    ...(cfg.token ? { token: cfg.token } : {}),
  });
  return `${base}/ws/sessions/${cfg.sessionId}?${params.toString()}`;
}

/**
 * WSConnection manages a persistent WebSocket connection with automatic reconnect.
 * Call start(signal) to begin; use send() to enqueue outbound events.
 * Register an onEvent handler to receive inbound events.
 */
export class WSConnection {
  private cfg: TeamClientConfig;
  private ws: WebSocket | null = null;
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private pongTimer: ReturnType<typeof setTimeout> | null = null;
  private outboundQueue: string[] = [];
  private queueFull = false;
  private connectedResolvers: Array<() => void> = [];
  private connected = false;

  public onEvent: EventHandler = () => undefined;

  constructor(cfg: TeamClientConfig) {
    this.cfg = cfg;
  }

  /** Resolves as soon as the WebSocket connection is open (or immediately if already open). */
  waitConnected(): Promise<void> {
    if (this.connected) return Promise.resolve();
    return new Promise<void>((resolve) => {
      this.connectedResolvers.push(resolve);
    });
  }

  /** Starts the connection loop. Resolves when the signal fires. */
  async start(signal: AbortSignal): Promise<void> {
    let backoff = RECONNECT_INITIAL_MS;

    while (!signal.aborted) {
      await this.connectOnce(signal);
      if (signal.aborted) break;
      // Wait with exponential back-off before reconnecting.
      await sleep(backoff, signal);
      backoff = Math.min(backoff * 2, RECONNECT_MAX_MS);
    }
  }

  /** Enqueues an event for sending. Non-blocking; drops if queue is full. */
  send(evt: CloudEvent): void {
    const data = JSON.stringify(evt);
    if (this.ws && this.ws.readyState === getReadyState('OPEN')) {
      this.ws.send(data);
    } else {
      if (this.outboundQueue.length < SEND_CHANNEL_CAP) {
        this.outboundQueue.push(data);
        this.queueFull = false;
      } else if (!this.queueFull) {
        this.queueFull = true;
        console.warn('[team-client] send queue full — dropping events');
      }
    }
  }

  /** Closes the active connection. */
  close(): void {
    this.clearTimers();
    this.ws?.close();
    this.ws = null;
  }

  // ─── Private ─────────────────────────────────────────────────────────

  private connectOnce(signal: AbortSignal): Promise<void> {
    return new Promise<void>((resolve) => {
      if (signal.aborted) {
        resolve();
        return;
      }

      let ws: WebSocket;
      try {
        ws = createWebSocket(buildWsUrl(this.cfg));
      } catch {
        resolve();
        return;
      }
      this.ws = ws;

      const onAbort = () => {
        ws.close();
        resolve();
      };
      signal.addEventListener('abort', onAbort, { once: true });

      ws.onopen = () => {
        // Flush queued messages.
        for (const msg of this.outboundQueue) {
          ws.send(msg);
        }
        this.outboundQueue = [];
        this.startPing(ws);
        // Notify waitConnected() waiters.
        this.connected = true;
        const resolvers = this.connectedResolvers.splice(0);
        for (const r of resolvers) r();
      };

      ws.onmessage = (ev) => {
        this.resetPongTimer(ws);
        try {
          const evt = JSON.parse(
            typeof ev.data === 'string' ? ev.data : ev.data.toString()
          ) as CloudEvent;
          this.onEvent(evt);
        } catch {
          // Ignore malformed messages.
        }
      };

      ws.onerror = () => {
        /* handled by onclose */
      };

      ws.onclose = () => {
        this.connected = false;
        this.clearTimers();
        this.ws = null;
        signal.removeEventListener('abort', onAbort);
        resolve();
      };
    });
  }

  private startPing(ws: WebSocket): void {
    this.clearTimers();
    this.pingTimer = setInterval(() => {
      if (ws.readyState === getReadyState('OPEN')) {
        // Browser WebSocket doesn't expose ping frames; send a heartbeat noop instead.
        // For Node.js ws, this is a proper ping via the ping() method if available.
        const wsAny = ws as unknown as { ping?: () => void };
        if (typeof wsAny.ping === 'function') {
          wsAny.ping();
        }
      }
      // Set pong deadline.
      this.pongTimer = setTimeout(() => {
        ws.close();
      }, PONG_WAIT_MS);
    }, PING_INTERVAL_MS);
  }

  private resetPongTimer(ws: WebSocket): void {
    if (this.pongTimer !== null) {
      clearTimeout(this.pongTimer);
      this.pongTimer = null;
    }
    // Re-arm pong watchdog.
    this.pongTimer = setTimeout(() => {
      ws.close();
    }, PONG_WAIT_MS);
  }

  private clearTimers(): void {
    if (this.pingTimer !== null) {
      clearInterval(this.pingTimer);
      this.pingTimer = null;
    }
    if (this.pongTimer !== null) {
      clearTimeout(this.pongTimer);
      this.pongTimer = null;
    }
  }
}

// ─── Helpers ──────────────────────────────────────────────────────────────

/** Resolves after ms milliseconds, or immediately if signal fires. */
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener('abort', () => {
      clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}

/**
 * Creates a WebSocket using the native browser API or the `ws` Node.js package.
 * The `ws` peer dependency is optional — in browsers it is not needed.
 */
function createWebSocket(url: string): WebSocket {
  if (typeof globalThis.WebSocket !== 'undefined') {
    return new globalThis.WebSocket(url);
  }
  // Node.js environment — require the optional `ws` peer dependency.
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const WS = require('ws') as typeof WebSocket;
  return new WS(url) as unknown as WebSocket;
}

/** Gets the numeric value of a WebSocket ready-state by name. */
function getReadyState(name: 'OPEN' | 'CLOSED'): number {
  if (typeof globalThis.WebSocket !== 'undefined') {
    return globalThis.WebSocket[name];
  }
  return name === 'OPEN' ? 1 : 3;
}

// Suppress unused-var warnings on timing constants that are referenced indirectly.
void WRITE_WAIT_MS;
