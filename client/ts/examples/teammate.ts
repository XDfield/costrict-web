/**
 * examples/teammate.ts — TypeScript teammate example.
 *
 * This example demonstrates:
 *  - Building a TeamClient in the teammate role
 *  - Implementing TeammatePlugin to execute tasks via child_process
 *  - Implementing ExplorePlugin to answer remote file/code queries
 *  - Implementing ApprovalPlugin for user confirmation
 *  - Registering local repos with the affinity registry
 *
 * Run:
 *   npx ts-node examples/teammate.ts
 *
 * Environment variables (or edit the constants below):
 *   TEAM_SERVER_URL    — e.g. https://api.example.com
 *   TEAM_TOKEN         — JWT bearer token
 *   TEAM_SESSION_ID    — UUID of an existing session
 *   TEAM_MACHINE_ID    — stable machine identifier
 *   TEAM_REPO_URL      — remote URL of the local git repo (optional)
 *   TEAM_REPO_PATH     — local path to the repo (optional)
 */

import { execSync, exec as execCb } from 'child_process';
import * as path from 'path';
import * as readline from 'readline';
import { promisify } from 'util';

import {
  TeamClient,
  MemberRoleTeammate,
} from '../src/index.js';

import type {
  ApprovalPlugin,
  ApprovalRequest,
  ExplorePlugin,
  ExploreQueryResult,
  ExploreRequest,
  ExploreResult,
  ProgressReporter,
  Task,
  TaskResult,
  TeammatePlugin,
} from '../src/index.js';

const exec = promisify(execCb);

// ─── Config ───────────────────────────────────────────────────────────────

const SERVER_URL = process.env['TEAM_SERVER_URL'] ?? 'http://localhost:8080';
const TOKEN = process.env['TEAM_TOKEN'] ?? '';
const SESSION_ID = process.env['TEAM_SESSION_ID'] ?? '';
const MACHINE_ID = process.env['TEAM_MACHINE_ID'] ?? `teammate-${process.pid}`;
const REPO_URL = process.env['TEAM_REPO_URL'] ?? '';
const REPO_PATH = process.env['TEAM_REPO_PATH'] ?? process.cwd();

// ─── TeammatePlugin implementation ────────────────────────────────────────

/**
 * ShellExecutor interprets the task description as a shell command.
 *
 * In production, replace executeTask with an AI agent call that:
 *  1. Parses the task description into actionable steps
 *  2. Uses task.fileHints to scope operations to specific files
 *  3. Calls your LLM / code editor / tool chain
 *  4. Streams progress via reporter.report()
 */
class ShellExecutor implements TeammatePlugin {
  async executeTask(
    signal: AbortSignal,
    task: Task,
    reporter: ProgressReporter
  ): Promise<TaskResult> {
    console.log(`[teammate] executing task ${task.id.slice(0, 8)}: "${task.description}"`);
    reporter.report(10, 'preparing');

    // Abort support — create a child-process abort mechanism.
    const ac = new AbortController();
    const onAbort = () => ac.abort();
    signal.addEventListener('abort', onAbort, { once: true });

    try {
      reporter.report(30, 'running');
      const { stdout, stderr } = await exec(task.description, {
        cwd: REPO_PATH,
        signal: ac.signal,
        timeout: 5 * 60 * 1000, // 5 minute max per task
        maxBuffer: 4 * 1024 * 1024,
      });

      const output = [stdout, stderr].filter(Boolean).join('\n');
      reporter.report(100, 'done');
      console.log(`[teammate] task ${task.id.slice(0, 8)} completed`);
      return { output, extraData: { exitCode: 0 } };
    } catch (err: unknown) {
      const e = err as { code?: number; stdout?: string; stderr?: string };
      const output = [e.stdout, e.stderr].filter(Boolean).join('\n');
      throw new Error(`Exit ${e.code ?? 1}: ${output}`);
    } finally {
      signal.removeEventListener('abort', onAbort);
    }
  }
}

// ─── ExplorePlugin implementation ─────────────────────────────────────────

/**
 * LocalExplorer handles remote code-intelligence queries using shell tools.
 * All operations are read-only and scoped to the local filesystem.
 */
class LocalExplorer implements ExplorePlugin {
  async explore(_signal: AbortSignal, req: ExploreRequest): Promise<ExploreResult> {
    console.log(
      `[teammate] handling explore request ${req.requestId.slice(0, 8)} (${req.queries.length} queries)`
    );

    const queryResults: ExploreQueryResult[] = [];

    for (const q of req.queries) {
      const result: ExploreQueryResult = { type: q.type, output: '', truncated: false };

      try {
        switch (q.type) {
          case 'file_tree': {
            const dir = str(q.params['path'], REPO_PATH);
            const { stdout } = await exec(
              `find "${dir}" -type f -not -path "*/.*" -not -path "*/node_modules/*" -not -path "*/vendor/*"`,
              { timeout: 10_000 }
            );
            result.output = truncate(stdout, 8192);
            result.truncated = stdout.length > 8192;
            break;
          }

          case 'content_search': {
            const pattern = str(q.params['pattern'], '');
            const dir = str(q.params['dir'], REPO_PATH);
            const fileGlob = str(q.params['fileGlob'], '');
            if (!pattern) { result.output = 'error: pattern required'; break; }
            // Prefer ripgrep, fall back to grep.
            const cmd = hasCommand('rg')
              ? `rg --no-heading -n -m 50 ${fileGlob ? `--glob "${fileGlob}"` : ''} "${pattern}" "${dir}"`
              : `grep -rn "${pattern}" "${dir}"`;
            const { stdout } = await exec(cmd, { timeout: 15_000 }).catch(() => ({ stdout: '' }));
            result.output = truncate(stdout, 8192);
            result.truncated = stdout.length > 8192;
            break;
          }

          case 'symbol_search': {
            const symbol = str(q.params['symbol'], '');
            const dir = str(q.params['dir'], REPO_PATH);
            if (!symbol) { result.output = 'error: symbol required'; break; }
            const cmd = hasCommand('rg')
              ? `rg --no-heading -n -w "${symbol}" "${dir}"`
              : `grep -rn "\\b${symbol}\\b" "${dir}"`;
            const { stdout } = await exec(cmd, { timeout: 15_000 }).catch(() => ({ stdout: '' }));
            result.output = truncate(stdout, 8192);
            break;
          }

          case 'git_log': {
            const dir = str(q.params['dir'], REPO_PATH);
            const n = num(q.params['n'], 20);
            const { stdout } = await exec(
              `git -C "${dir}" log --oneline -${n}`,
              { timeout: 10_000 }
            );
            result.output = stdout;
            break;
          }

          case 'dependency_graph': {
            const entry = str(q.params['entry'], '.');
            const ext = path.extname(entry);
            let cmd: string;
            if (ext === '.ts' || ext === '.js') {
              cmd = `node -e "console.log(JSON.stringify(Object.keys(require.resolve.paths('${entry}'))))"`;
            } else {
              // Go module graph
              cmd = `go list -deps ${entry}`;
            }
            const { stdout } = await exec(cmd, {
              cwd: REPO_PATH,
              timeout: 30_000,
            }).catch((e: Error) => ({ stdout: `error: ${e.message}` }));
            result.output = truncate(stdout, 8192);
            break;
          }

          default:
            result.output = `unsupported query type "${q.type}"`;
        }
      } catch (err) {
        result.output = `error: ${err instanceof Error ? err.message : String(err)}`;
      }

      queryResults.push(result);
    }

    return { requestId: req.requestId, queryResults };
  }
}

// ─── ApprovalPlugin implementation ────────────────────────────────────────

/**
 * StdinApprover presents approval requests on stdout and reads y/n from stdin.
 *
 * In production, replace with a GUI dialog, desktop notification, or
 * a webhook to a chat system (Slack, Teams, etc.).
 */
class StdinApprover implements ApprovalPlugin {
  async handleApproval(
    _signal: AbortSignal,
    req: ApprovalRequest
  ): Promise<{ approved: boolean; note?: string }> {
    console.log('\n[APPROVAL REQUEST] ─────────────────────────────────');
    console.log(`  Tool:        ${req.toolName}`);
    console.log(`  Risk level:  ${req.riskLevel}`);
    console.log(`  Description: ${req.description ?? '(no description)'}`);
    if (req.toolInput && Object.keys(req.toolInput).length) {
      console.log(`  Input:       ${JSON.stringify(req.toolInput, null, 2)}`);
    }
    console.log('─────────────────────────────────────────────────────');

    const answer = await prompt('  Approve? [y/N]: ');
    const approved = answer.trim().toLowerCase() === 'y';
    console.log(approved ? '  → Approved\n' : '  → Rejected\n');
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
    console.log('\n[teammate] shutting down...');
    ac.abort();
  });

  const client = new TeamClient({
    serverUrl: SERVER_URL,
    token: TOKEN,
    sessionId: SESSION_ID,
    machineId: MACHINE_ID,
    machineName: `Teammate (${MACHINE_ID})`,
    role: MemberRoleTeammate,
  })
    .withTeammatePlugin(new ShellExecutor())
    .withExplorePlugin(new LocalExplorer())
    .withApprovalPlugin(new StdinApprover());

  console.log(`[teammate] connecting to ${SERVER_URL} (session=${SESSION_ID})`);
  console.log(`[teammate] working directory: ${REPO_PATH}`);

  // Register the local repo so the leader can schedule repo-affinity tasks here.
  if (REPO_URL) {
    setTimeout(async () => {
      if (ac.signal.aborted) return;
      try {
        // Detect current branch.
        let branch = 'main';
        let dirty = false;
        try {
          branch = execSync('git rev-parse --abbrev-ref HEAD', { cwd: REPO_PATH })
            .toString().trim();
          dirty = execSync('git status --porcelain', { cwd: REPO_PATH })
            .toString().trim().length > 0;
        } catch { /* not a git repo */ }

        await client.doJSON('POST', `/api/team/sessions/${SESSION_ID}/repos`, {
          machineId: MACHINE_ID,
          repoRemoteUrl: REPO_URL,
          repoLocalPath: REPO_PATH,
          currentBranch: branch,
          hasUncommittedChanges: dirty,
          lastSyncedAt: new Date().toISOString(),
        });
        console.log(`[teammate] registered repo: ${REPO_URL} (branch=${branch}, dirty=${dirty})`);
      } catch (err) {
        console.warn('[teammate] repo registration failed:', err);
      }
    }, 1500);
  }

  try {
    await client.start(ac.signal);
  } catch (err) {
    if (!ac.signal.aborted) {
      console.error('[teammate] stopped with error:', err);
    }
  }
}

// ─── Helpers ──────────────────────────────────────────────────────────────

function str(v: unknown, def: string): string {
  return typeof v === 'string' && v ? v : def;
}

function num(v: unknown, def: number): number {
  return typeof v === 'number' ? v : def;
}

function truncate(s: string, max: number): string {
  return s.length > max ? s.slice(0, max) : s;
}

function hasCommand(cmd: string): boolean {
  try { execSync(`which ${cmd}`, { stdio: 'ignore' }); return true; } catch { return false; }
}

function prompt(question: string): Promise<string> {
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  return new Promise((resolve) => {
    rl.question(question, (answer) => { rl.close(); resolve(answer); });
  });
}

main().catch((err) => {
  console.error('[teammate] fatal error:', err);
  process.exit(1);
});
