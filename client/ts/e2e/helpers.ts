/**
 * e2e/helpers.ts — Test utilities for Cloud Team Agent E2E tests
 */

import { v4 as uuidv4 } from 'uuid';
import type { Task, TaskSpec, TaskResult, Member, CloudEvent } from '../src/types.js';
import type { ProgressReporter } from '../src/plugin.js';

/**
 * Generate a unique session ID for testing
 */
export function generateSessionId(): string {
  return `test-session-${uuidv4().slice(0, 8)}`;
}

/**
 * Generate a unique machine ID for testing
 */
export function generateMachineId(prefix: string): string {
  return `${prefix}-${uuidv4().slice(0, 8)}`;
}

/**
 * Create a simple task spec for testing
 */
export function createTaskSpec(
  description: string,
  overrides: Partial<TaskSpec> = {}
): TaskSpec {
  return {
    id: uuidv4(),
    description,
    priority: 5,
    ...overrides,
  };
}

/**
 * Create a task dependency chain
 */
export function createTaskChain(descriptions: string[]): TaskSpec[] {
  const tasks: TaskSpec[] = [];
  let prevId: string | undefined;

  for (const description of descriptions) {
    const task: TaskSpec = {
      id: uuidv4(),
      description,
      priority: 10 - tasks.length,
      dependencies: prevId ? [prevId] : undefined,
    };
    tasks.push(task);
    prevId = task.id;
  }

  return tasks;
}

/**
 * Create parallel tasks (no dependencies)
 */
export function createParallelTasks(count: number, prefix = 'Task'): TaskSpec[] {
  return Array.from({ length: count }, (_, i) => ({
    id: uuidv4(),
    description: `${prefix} ${i + 1}`,
    priority: 5,
  }));
}

/**
 * Create a diamond-shaped dependency graph
 *      A
 *     / \
 *    B   C
 *     \ /
 *      D
 */
export function createDiamondGraph(
  taskA: string,
  taskB: string,
  taskC: string,
  taskD: string
): TaskSpec[] {
  const idA = uuidv4();
  const idB = uuidv4();
  const idC = uuidv4();
  const idD = uuidv4();

  return [
    { id: idA, description: taskA, priority: 10 },
    { id: idB, description: taskB, priority: 9, dependencies: [idA] },
    { id: idC, description: taskC, priority: 9, dependencies: [idA] },
    { id: idD, description: taskD, priority: 8, dependencies: [idB, idC] },
  ];
}

/**
 * Mock member factory for testing
 */
export function createMockMember(overrides: Partial<Member> = {}): Member {
  return {
    id: uuidv4(),
    sessionId: 'test-session',
    machineId: generateMachineId('machine'),
    machineName: 'Test Machine',
    role: 'teammate',
    status: 'online',
    ...overrides,
  };
}

/**
 * Create multiple mock members
 */
export function createMockMembers(count: number, role: string = 'teammate'): Member[] {
  return Array.from({ length: count }, (_, i) =>
    createMockMember({
      role,
      machineName: `Test Machine ${i + 1}`,
    })
  );
}

/**
 * Delay function for async testing
 */
export function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Wait for a condition with timeout
 */
export async function waitFor(
  condition: () => boolean | Promise<boolean>,
  timeoutMs: number = 5000,
  intervalMs: number = 100
): Promise<void> {
  const startTime = Date.now();
  while (Date.now() - startTime < timeoutMs) {
    if (await condition()) {
      return;
    }
    await delay(intervalMs);
  }
  throw new Error(`Timeout waiting for condition after ${timeoutMs}ms`);
}

/**
 * Collect all values from an async iterator
 */
export async function collectAsync<T>(
  iterator: AsyncIterable<T>,
  maxItems: number = 100
): Promise<T[]> {
  const results: T[] = [];
  for await (const item of iterator) {
    results.push(item);
    if (results.length >= maxItems) {
      break;
    }
  }
  return results;
}

/**
 * Test result collector for tracking task execution
 */
export class TaskExecutionCollector {
  private executions: Array<{
    taskId: string;
    description: string;
    status: 'started' | 'completed' | 'failed';
    startTime: number;
    endTime?: number;
    result?: TaskResult;
    error?: string;
  }> = [];

  recordStart(taskId: string, description: string): void {
    this.executions.push({
      taskId,
      description,
      status: 'started',
      startTime: Date.now(),
    });
  }

  recordComplete(taskId: string, result: TaskResult): void {
    const exec = this.executions.find((e) => e.taskId === taskId);
    if (exec) {
      exec.status = 'completed';
      exec.endTime = Date.now();
      exec.result = result;
    }
  }

  recordFailure(taskId: string, error: string): void {
    const exec = this.executions.find((e) => e.taskId === taskId);
    if (exec) {
      exec.status = 'failed';
      exec.endTime = Date.now();
      exec.error = error;
    }
  }

  getExecutions() {
    return [...this.executions];
  }

  getCompletedCount(): number {
    return this.executions.filter((e) => e.status === 'completed').length;
  }

  getFailedCount(): number {
    return this.executions.filter((e) => e.status === 'failed').length;
  }

  getExecutionOrder(): string[] {
    return this.executions
      .filter((e) => e.status === 'completed')
      .sort((a, b) => (a.endTime ?? 0) - (b.endTime ?? 0))
      .map((e) => e.description);
  }

  printReport(): void {
    console.log('\n=== Task Execution Report ===');
    console.log(`Total: ${this.executions.length}`);
    console.log(`Completed: ${this.getCompletedCount()}`);
    console.log(`Failed: ${this.getFailedCount()}`);
    console.log('\nExecution Order:');
    this.getExecutionOrder().forEach((desc, i) => {
      console.log(`  ${i + 1}. ${desc}`);
    });
    console.log('============================\n');
  }
}

/**
 * Environment variable helper
 */
export function getEnv(name: string, defaultValue?: string): string {
  const value = process.env[name] ?? defaultValue;
  if (value === undefined) {
    throw new Error(`Environment variable ${name} is required`);
  }
  return value;
}

/**
 * Test configuration
 */
export interface TestConfig {
  serverUrl: string;
  token: string;
  sessionId: string;
  timeoutMs: number;
}

export function loadTestConfig(): TestConfig {
  return {
    serverUrl: getEnv('E2E_SERVER_URL', 'http://localhost:8080'),
    token: getEnv('E2E_TOKEN', ''),
    sessionId: getEnv('E2E_SESSION_ID', generateSessionId()),
    timeoutMs: parseInt(getEnv('E2E_TIMEOUT', '30000'), 10),
  };
}

/**
 * Retry a function with exponential backoff
 */
export async function retry<T>(
  fn: () => Promise<T>,
  maxRetries: number = 3,
  delayMs: number = 1000
): Promise<T> {
  let lastError: Error;

  for (let i = 0; i < maxRetries; i++) {
    try {
      return await fn();
    } catch (error) {
      lastError = error as Error;
      if (i < maxRetries - 1) {
        await delay(delayMs * Math.pow(2, i));
      }
    }
  }

  throw lastError!;
}
