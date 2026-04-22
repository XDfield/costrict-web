/**
 * examples/leader.ts — TypeScript leader example.
 *
 * This example demonstrates:
 *  - Building a TeamClient in the leader role
 *  - Implementing LeaderPlugin with a simple multi-task dependency DAG
 *  - Implementing ApprovalPlugin with a stdin y/n prompt
 *  - Submitting a plan after connecting
 *  - Polling session progress
 *
 * Run:
 *   npx ts-node examples/leader.ts
 *
 * Environment variables (or edit the constants below):
 *   TEAM_SERVER_URL  — e.g. https://api.example.com
 *   TEAM_TOKEN       — JWT bearer token
 *   TEAM_SESSION_ID  — UUID of an existing session
 *   TEAM_MACHINE_ID  — stable machine identifier
 *   TEAM_GOAL        — free-text goal to plan (default: "refactor the auth module")
 */

import * as readline from 'readline';
import { v4 as uuidv4 } from 'uuid';

import {
  TeamClient,
  MemberRoleLeader,
  MemberStatusOnline,
  MemberRoleTeammate,
} from '../src/index.js';

import type {
  ApprovalPlugin,
  ApprovalRequest,
  LeaderPlugin,
  Member,
  PlanTasksInput,
  TaskSpec,
} from '../src/index.js';

// ─── Config ───────────────────────────────────────────────────────────────

const SERVER_URL = process.env['TEAM_SERVER_URL'] ?? 'http://localhost:8080';
const TOKEN = process.env['TEAM_TOKEN'] ?? '';
const SESSION_ID = process.env['TEAM_SESSION_ID'] ?? '';
const MACHINE_ID = process.env['TEAM_MACHINE_ID'] ?? `leader-${process.pid}`;
const GOAL =
  process.env['TEAM_GOAL'] ?? 'refactor the authentication module';

// ─── LeaderPlugin implementation ──────────────────────────────────────────

/**
 * SimplePlanner creates a 3-task DAG: analyse → implement → verify.
 *
 * In production, replace planTasks with an LLM call that decomposes the goal
 * into a richer task graph.  Pre-assigning `id` values is required when tasks
 * reference each other via `dependencies`.
 */
class SimplePlanner implements LeaderPlugin {
  async planTasks(_signal: AbortSignal, req: PlanTasksInput): Promise<TaskSpec[]> {
    console.log(
      `[leader] planning tasks for goal: "${req.goal}" (${req.members.length} members online)`
    );

    // Pick the first online teammate (if any) for assignment.
    const assignee = pickTeammate(req.members);
    if (assignee) {
      console.log(`[leader] assigning tasks to ${assignee.machineName ?? assignee.machineId}`);
    } else {
      console.log('[leader] no online teammates — tasks will remain unassigned');
    }

    // Pre-assign UUIDs so dependency references in the same batch are stable.
    const idAnalyse = uuidv4();
    const idImplement = uuidv4();
    const idVerify = uuidv4();

    return [
      {
        id: idAnalyse,
        description: `Analyse codebase — ${req.goal}`,
        priority: 9,
        assignedMemberId: assignee?.id,
        repoAffinity: [],   // set to ['https://github.com/org/repo'] to target a specific repo
        fileHints: [],      // set to file paths for focused execution
      },
      {
        id: idImplement,
        description: `Implement changes — ${req.goal}`,
        priority: 8,
        dependencies: [idAnalyse],   // ← only starts after analyse completes
        assignedMemberId: assignee?.id,
      },
      {
        id: idVerify,
        description: `Verify and test — ${req.goal}`,
        priority: 7,
        dependencies: [idImplement], // ← only starts after implement completes
        assignedMemberId: assignee?.id,
      },
    ];
  }
}

// ─── ApprovalPlugin implementation ────────────────────────────────────────

/**
 * StdinApprover displays the approval request on stdout and reads y/n from stdin.
 *
 * In production, replace handleApproval with a GUI dialog, Slack message,
 * web notification, or any other mechanism that surfaces the decision to a human.
 */
class StdinApprover implements ApprovalPlugin {
  async handleApproval(
    _signal: AbortSignal,
    req: ApprovalRequest
  ): Promise<{ approved: boolean; note?: string }> {
    console.log('\n[APPROVAL REQUEST] ─────────────────────────────────');
    console.log(`  Tool:        ${req.toolName}`);
    console.log(`  Risk level:  ${req.riskLevel}`);
    console.log(`  Description: ${req.description}`);
    if (req.toolInput && Object.keys(req.toolInput).length) {
      console.log(`  Input:       ${JSON.stringify(req.toolInput)}`);
    }
    console.log('─────────────────────────────────────────────────────');

    const answer = await prompt('  Approve? [y/N]: ');
    const approved = answer.trim().toLowerCase() === 'y';
    console.log(`  → ${approved ? 'Approved' : 'Rejected'}\n`);
    return { approved };
  }
}

// ─── Main ─────────────────────────────────────────────────────────────────

async function main(): Promise<void> {
  if (!TOKEN || !SESSION_ID) {
    console.error(
      'Set TEAM_TOKEN and TEAM_SESSION_ID environment variables before running.'
    );
    process.exit(1);
  }

  const ac = new AbortController();
  process.on('SIGINT', () => {
    console.log('\n[leader] shutting down...');
    ac.abort();
  });

  const client = new TeamClient({
    serverUrl: SERVER_URL,
    token: TOKEN,
    sessionId: SESSION_ID,
    machineId: MACHINE_ID,
    machineName: `Leader (${MACHINE_ID})`,
    role: MemberRoleLeader,
  })
    .withLeaderPlugin(new SimplePlanner())
    .withApprovalPlugin(new StdinApprover());

  console.log(`[leader] connecting to ${SERVER_URL} (session=${SESSION_ID})`);

  // Submit the plan a couple of seconds after the WS connection is ready.
  setTimeout(async () => {
    if (ac.signal.aborted) return;
    console.log(`[leader] submitting plan: "${GOAL}"`);
    try {
      await client.submitPlan(GOAL, ac.signal);
      console.log('[leader] plan submitted — waiting for task events...');
    } catch (err) {
      console.error('[leader] plan submission failed:', err);
    }
  }, 2500);

  // Poll progress every 10 s.
  const progressInterval = setInterval(async () => {
    if (ac.signal.aborted) {
      clearInterval(progressInterval);
      return;
    }
    try {
      const prog = await client.doJSON<SessionProgress>(
        'GET',
        `/api/team/sessions/${SESSION_ID}/progress`
      );
      console.log(
        `[leader] progress — total: ${prog.totalTasks}, ` +
        `completed: ${prog.completedTasks}, ` +
        `running: ${prog.runningTasks}, ` +
        `failed: ${prog.failedTasks}, ` +
        `pending: ${prog.pendingTasks}`
      );
    } catch {
      // swallow transient errors
    }
  }, 10_000);

  try {
    await client.start(ac.signal);
  } finally {
    clearInterval(progressInterval);
  }
}

// ─── Helpers ──────────────────────────────────────────────────────────────

function pickTeammate(members: Member[]): Member | undefined {
  return members.find(
    (m) => m.role === MemberRoleTeammate && m.status === MemberStatusOnline
  );
}

function prompt(question: string): Promise<string> {
  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
    terminal: false,
  });
  return new Promise((resolve) => {
    rl.question(question, (answer) => {
      rl.close();
      resolve(answer);
    });
  });
}

interface SessionProgress {
  totalTasks: number;
  completedTasks: number;
  failedTasks: number;
  runningTasks: number;
  pendingTasks: number;
}

main().catch((err) => {
  console.error('[leader] fatal error:', err);
  process.exit(1);
});
