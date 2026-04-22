/**
 * e2e/mocks.ts — Mock plugin implementations for E2E testing
 */

import { v4 as uuidv4 } from 'uuid';
import type {
  ApprovalRequest,
  ExploreQueryResult,
  ExploreRequest,
  ExploreResult,
  Member,
  PlanTasksInput,
  Task,
  TaskResult,
  TaskSpec,
} from '../src/types.js';
import type {
  ApprovalPlugin,
  ExplorePlugin,
  LeaderPlugin,
  ProgressReporter,
  TeammatePlugin,
} from '../src/plugin.js';
import { TaskExecutionCollector } from './helpers.js';

/**
 * Mock Leader Plugin — Creates predefined task plans
 */
export class MockLeaderPlugin implements LeaderPlugin {
  private taskSpecs: TaskSpec[];
  private onPlanCalled?: (input: PlanTasksInput) => void;

  constructor(
    taskSpecs: TaskSpec[],
    onPlanCalled?: (input: PlanTasksInput) => void
  ) {
    this.taskSpecs = taskSpecs;
    this.onPlanCalled = onPlanCalled;
  }

  async planTasks(
    _signal: AbortSignal,
    input: PlanTasksInput
  ): Promise<TaskSpec[]> {
    this.onPlanCalled?.(input);

    // Pick online teammates to assign tasks to (round-robin)
    const teammates = input.members.filter((m) => m.role === 'teammate');

    return this.taskSpecs.map((spec, i) => ({
      ...spec,
      id: spec.id ?? uuidv4(),
      assignedMemberId: teammates.length > 0
        ? teammates[i % teammates.length].id
        : spec.assignedMemberId,
    }));
  }
}

/**
 * Dynamic Leader Plugin — Creates tasks based on goal
 */
export class DynamicLeaderPlugin implements LeaderPlugin {
  private taskGenerator: (goal: string, members: Member[]) => TaskSpec[];
  private onPlanCalled?: (input: PlanTasksInput) => void;

  constructor(
    taskGenerator: (goal: string, members: Member[]) => TaskSpec[],
    onPlanCalled?: (input: PlanTasksInput) => void
  ) {
    this.taskGenerator = taskGenerator;
    this.onPlanCalled = onPlanCalled;
  }

  async planTasks(
    _signal: AbortSignal,
    input: PlanTasksInput
  ): Promise<TaskSpec[]> {
    this.onPlanCalled?.(input);
    const tasks = this.taskGenerator(input.goal, input.members);
    return tasks.map((spec) => ({
      ...spec,
      id: spec.id ?? uuidv4(),
    }));
  }
}

/**
 * Mock Teammate Plugin — Executes tasks with configurable behavior
 */
export class MockTeammatePlugin implements TeammatePlugin {
  private collector: TaskExecutionCollector;
  private executeFn: (
    task: Task,
    reporter: ProgressReporter
  ) => Promise<TaskResult>;
  private delayMs: number;

  constructor(
    collector: TaskExecutionCollector,
    executeFn?: (task: Task, reporter: ProgressReporter) => Promise<TaskResult>,
    delayMs: number = 100
  ) {
    this.collector = collector;
    this.delayMs = delayMs;
    this.executeFn =
      executeFn ??
      (async (task, reporter) => {
        reporter.report(10, 'preparing');
        await this.sleep(this.delayMs);
        reporter.report(50, 'executing');
        await this.sleep(this.delayMs);
        reporter.report(100, 'done');
        return {
          output: `Executed: ${task.description}`,
          files: [],
        };
      });
  }

  async executeTask(
    signal: AbortSignal,
    task: Task,
    reporter: ProgressReporter
  ): Promise<TaskResult> {
    this.collector.recordStart(task.id, task.description);

    try {
      const result = await this.executeFn(task, reporter);
      this.collector.recordComplete(task.id, result);
      return result;
    } catch (error) {
      const errorMsg = error instanceof Error ? error.message : String(error);
      this.collector.recordFailure(task.id, errorMsg);
      throw error;
    }
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}

/**
 * Failing Teammate Plugin — Simulates task failures for testing retry logic
 */
export class FailingTeammatePlugin implements TeammatePlugin {
  private failTaskIds: Set<string>;
  private failCount: Map<string, number>;
  private maxFailCount: number;

  constructor(
    failTaskIds: string[] = [],
    maxFailCount: number = 1 // Fail N times before succeeding
  ) {
    this.failTaskIds = new Set(failTaskIds);
    this.failCount = new Map();
    this.maxFailCount = maxFailCount;
  }

  async executeTask(
    _signal: AbortSignal,
    task: Task,
    reporter: ProgressReporter
  ): Promise<TaskResult> {
    reporter.report(10, 'preparing');

    if (this.failTaskIds.has(task.id) || this.failTaskIds.has('all')) {
      const currentFails = this.failCount.get(task.id) ?? 0;

      if (currentFails < this.maxFailCount) {
        this.failCount.set(task.id, currentFails + 1);
        reporter.report(50, 'failing');
        throw new Error(`Simulated failure for task ${task.id}`);
      }
    }

    reporter.report(100, 'done');
    return {
      output: `Successfully executed: ${task.description}`,
      files: [],
    };
  }
}

/**
 * Mock Approval Plugin — Auto-approves or rejects based on configuration
 */
export class MockApprovalPlugin implements ApprovalPlugin {
  private autoApprove: boolean;
  private shouldApproveFn?: (req: ApprovalRequest) => boolean;
  private delayMs: number;

  constructor(
    autoApprove: boolean = true,
    shouldApproveFn?: (req: ApprovalRequest) => boolean,
    delayMs: number = 100
  ) {
    this.autoApprove = autoApprove;
    this.shouldApproveFn = shouldApproveFn;
    this.delayMs = delayMs;
  }

  async handleApproval(
    _signal: AbortSignal,
    req: ApprovalRequest
  ): Promise<{ approved: boolean; note?: string }> {
    await this.sleep(this.delayMs);

    const approved = this.shouldApproveFn
      ? this.shouldApproveFn(req)
      : this.autoApprove;

    return {
      approved,
      note: approved ? 'Auto-approved by test' : 'Auto-rejected by test',
    };
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}

/**
 * Mock Explore Plugin — Returns predefined or dynamic explore results
 */
export class MockExplorePlugin implements ExplorePlugin {
  private results: Map<string, ExploreQueryResult>;
  private handler?: (query: ExploreQuery) => ExploreQueryResult;

  constructor(
    results?: Map<string, ExploreQueryResult>,
    handler?: (query: ExploreQuery) => ExploreQueryResult
  ) {
    this.results = results ?? new Map();
    this.handler = handler;
  }

  async explore(
    _signal: AbortSignal,
    req: ExploreRequest
  ): Promise<ExploreResult> {
    const queryResults: ExploreQueryResult[] = [];

    for (const query of req.queries) {
      let result: ExploreQueryResult;

      if (this.handler) {
        result = this.handler(query);
      } else {
        result =
          this.results.get(query.type) ??
          this.createDefaultResult(query.type);
      }

      queryResults.push(result);
    }

    return {
      requestId: req.requestId,
      queryResults,
    };
  }

  private createDefaultResult(type: string): ExploreQueryResult {
    return {
      type,
      output: `Mock ${type} result`,
      truncated: false,
    };
  }
}

/**
 * Simple Explore Plugin — Returns file tree and search results from local FS
 */
export class SimpleExplorePlugin implements ExplorePlugin {
  private basePath: string;

  constructor(basePath: string = process.cwd()) {
    this.basePath = basePath;
  }

  async explore(
    _signal: AbortSignal,
    req: ExploreRequest
  ): Promise<ExploreResult> {
    const queryResults: ExploreQueryResult[] = [];

    for (const query of req.queries) {
      const result = await this.handleQuery(query);
      queryResults.push(result);
    }

    return {
      requestId: req.requestId,
      queryResults,
    };
  }

  private async handleQuery(query: ExploreQuery): Promise<ExploreQueryResult> {
    switch (query.type) {
      case 'file_tree':
        return {
          type: 'file_tree',
          output: JSON.stringify({ path: this.basePath, files: ['mock'] }),
          truncated: false,
        };

      case 'content_search':
        return {
          type: 'content_search',
          output: `Search results for: ${query.params['pattern']}`,
          truncated: false,
        };

      case 'symbol_search':
        return {
          type: 'symbol_search',
          output: `Symbol: ${query.params['symbol']}`,
          truncated: false,
        };

      case 'git_log':
        return {
          type: 'git_log',
          output: 'abc123 Mock commit message',
          truncated: false,
        };

      default:
        return {
          type: query.type,
          output: 'Unknown query type',
          truncated: false,
        };
    }
  }
}

// Type for ExploreQuery since it's not exported directly
interface ExploreQuery {
  type: string;
  params: Record<string, unknown>;
}

/**
 * Task Plan Builder — Fluent API for creating test task plans
 */
export class TaskPlanBuilder {
  private tasks: TaskSpec[] = [];

  addTask(description: string, overrides: Partial<TaskSpec> = {}): this {
    this.tasks.push({
      id: uuidv4(),
      description,
      priority: 5,
      ...overrides,
    });
    return this;
  }

  addDependentTask(
    description: string,
    dependsOn: string | string[],
    overrides: Partial<TaskSpec> = {}
  ): this {
    const dependencies = Array.isArray(dependsOn) ? dependsOn : [dependsOn];
    this.tasks.push({
      id: uuidv4(),
      description,
      priority: 5,
      dependencies,
      ...overrides,
    });
    return this;
  }

  build(): TaskSpec[] {
    return [...this.tasks];
  }

  static create(): TaskPlanBuilder {
    return new TaskPlanBuilder();
  }

  static linear(...descriptions: string[]): TaskSpec[] {
    const builder = new TaskPlanBuilder();
    let prevId: string | undefined;

    for (const desc of descriptions) {
      const task: TaskSpec = {
        id: uuidv4(),
        description: desc,
        priority: 10 - builder.tasks.length,
      };

      if (prevId) {
        task.dependencies = [prevId];
      }

      builder.tasks.push(task);
      prevId = task.id;
    }

    return builder.build();
  }

  static parallel(count: number, prefix = 'Task'): TaskSpec[] {
    return Array.from({ length: count }, (_, i) => ({
      id: uuidv4(),
      description: `${prefix} ${i + 1}`,
      priority: 5,
    }));
  }
}
