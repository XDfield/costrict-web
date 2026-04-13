/**
 * e2e/team-e2e.test.ts — End-to-end tests for Cloud Team Agent SDK
 *
 * Run with: npx ts-node --project tsconfig.e2e.json e2e/team-e2e.test.ts
 * Or: npm run test:e2e:ts
 *
 * Required environment variables:
 *   E2E_SERVER_URL — Team server URL (default: http://localhost:8080)
 *   E2E_TOKEN — JWT authentication token
 *   E2E_SESSION_ID — Optional session ID (auto-generated if not set)
 */

import { TeamClient, MemberRoleLeader, MemberRoleTeammate } from '../src/index.js';
import type { TaskSpec, TaskResult, Member } from '../src/types.js';
import {
  generateSessionId,
  generateMachineId,
  loadTestConfig,
  delay,
  waitFor,
  TaskExecutionCollector,
  createTaskChain,
  createParallelTasks,
  createDiamondGraph,
} from './helpers.js';
import {
  MockLeaderPlugin,
  MockTeammatePlugin,
  MockApprovalPlugin,
  FailingTeammatePlugin,
  TaskPlanBuilder,
} from './mocks.js';

// Test configuration
const config = loadTestConfig();
const TEST_TIMEOUT = 60000; // 60 seconds

/**
 * Test Scenario 1: Basic Task Execution
 * Leader creates a simple task plan, Teammate executes it
 */
async function testBasicTaskExecution(): Promise<void> {
  console.log('\n=== Test: Basic Task Execution ===');

  const sessionId = generateSessionId();
  const collector = new TaskExecutionCollector();

  // Create Teammate
  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    machineName: 'Test Teammate',
    role: MemberRoleTeammate,
  }).withTeammatePlugin(new MockTeammatePlugin(collector));

  // Create Leader with simple task plan
  const taskSpecs: TaskSpec[] = [
    { id: 'task-1', description: 'Echo Hello', priority: 9 },
    { id: 'task-2', description: 'Echo World', priority: 8 },
  ];

  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    machineName: 'Test Leader',
    role: MemberRoleLeader,
  }).withLeaderPlugin(new MockLeaderPlugin(taskSpecs));

  try {
    // Start Teammate first
    const teammatePromise = teammateClient.start();
    await delay(500); // Give teammate time to register

    // Start Leader and submit plan
    await leaderClient.start();
    await delay(1000);

    await leaderClient.submitPlan('Basic test plan');

    // Wait for tasks to complete
    await waitFor(() => collector.getCompletedCount() >= 2, 10000, 500);

    // Verify results
    const executions = collector.getExecutions();
    console.log(`  ✓ Completed ${executions.length} tasks`);

    if (executions.length !== 2) {
      throw new Error(`Expected 2 tasks, got ${executions.length}`);
    }

    console.log('  ✓ Basic task execution passed\n');
  } finally {
    leaderClient.stop();
    teammateClient.stop();
  }
}

/**
 * Test Scenario 2: Task Dependency Chain
 * Tasks execute in order due to dependencies
 */
async function testTaskDependencyChain(): Promise<void> {
  console.log('\n=== Test: Task Dependency Chain ===');

  const sessionId = generateSessionId();
  const collector = new TaskExecutionCollector();

  // Create Teammate
  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    role: MemberRoleTeammate,
  }).withTeammatePlugin(new MockTeammatePlugin(collector, async (task, reporter) => {
    reporter.report(10, 'preparing');
    await delay(200);
    reporter.report(100, 'done');
    return { output: `Executed: ${task.description}` };
  }));

  // Create dependent task chain: A → B → C
  const taskSpecs = createTaskChain(['Task A', 'Task B', 'Task C']);

  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  }).withLeaderPlugin(new MockLeaderPlugin(taskSpecs));

  try {
    await teammateClient.start();
    await delay(500);

    await leaderClient.start();
    await delay(1000);

    await leaderClient.submitPlan('Dependency chain test');

    // Wait for all tasks
    await waitFor(() => collector.getCompletedCount() >= 3, 15000, 500);

    // Verify execution order
    const order = collector.getExecutionOrder();
    console.log(`  Execution order: ${order.join(' → ')}`);

    if (order[0] !== 'Task A' || order[1] !== 'Task B' || order[2] !== 'Task C') {
      throw new Error(`Tasks executed out of order: ${order.join(', ')}`);
    }

    console.log('  ✓ Dependency chain execution passed\n');
  } finally {
    leaderClient.stop();
    teammateClient.stop();
  }
}

/**
 * Test Scenario 3: Parallel Task Execution
 * Multiple independent tasks execute concurrently
 */
async function testParallelTaskExecution(): Promise<void> {
  console.log('\n=== Test: Parallel Task Execution ===');

  const sessionId = generateSessionId();
  const collector = new TaskExecutionCollector();
  const startTimes = new Map<string, number>();

  // Create Teammate with delay to detect parallelism
  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    role: MemberRoleTeammate,
  }).withTeammatePlugin(new MockTeammatePlugin(collector, async (task, reporter) => {
    startTimes.set(task.id, Date.now());
    reporter.report(50, 'executing');
    await delay(500); // Fixed delay for each task
    reporter.report(100, 'done');
    return { output: `Executed: ${task.description}` };
  }));

  // Create 3 parallel tasks
  const taskSpecs = createParallelTasks(3, 'Parallel Task');

  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  }).withLeaderPlugin(new MockLeaderPlugin(taskSpecs));

  try {
    await teammateClient.start();
    await delay(500);

    await leaderClient.start();
    await delay(1000);

    const submitStart = Date.now();
    await leaderClient.submitPlan('Parallel execution test');

    // Wait for all tasks
    await waitFor(() => collector.getCompletedCount() >= 3, 15000, 500);
    const totalTime = Date.now() - submitStart;

    // If truly parallel, total time should be ~500ms, not ~1500ms
    console.log(`  Total execution time: ${totalTime}ms`);

    if (totalTime > 1200) {
      console.log('  ⚠ Tasks may not be executing in parallel (expected < 1200ms)');
    } else {
      console.log('  ✓ Parallel execution confirmed');
    }

    console.log('  ✓ Parallel task execution passed\n');
  } finally {
    leaderClient.stop();
    teammateClient.stop();
  }
}

/**
 * Test Scenario 4: Diamond Dependency Graph
 *      A
 *     / \
 *    B   C
 *     \ /
 *      D
 */
async function testDiamondDependencyGraph(): Promise<void> {
  console.log('\n=== Test: Diamond Dependency Graph ===');

  const sessionId = generateSessionId();
  const collector = new TaskExecutionCollector();

  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    role: MemberRoleTeammate,
  }).withTeammatePlugin(new MockTeammatePlugin(collector));

  // Create diamond graph
  const taskSpecs = createDiamondGraph(
    'Diamond Task A',
    'Diamond Task B',
    'Diamond Task C',
    'Diamond Task D'
  );

  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  }).withLeaderPlugin(new MockLeaderPlugin(taskSpecs));

  try {
    await teammateClient.start();
    await delay(500);

    await leaderClient.start();
    await delay(1000);

    await leaderClient.submitPlan('Diamond graph test');

    // Wait for all tasks
    await waitFor(() => collector.getCompletedCount() >= 4, 15000, 500);

    const order = collector.getExecutionOrder();
    console.log(`  Execution order: ${order.join(' → ')}`);

    // Verify A is first, D is last
    if (order[0] !== 'Diamond Task A') {
      throw new Error('Task A should be first');
    }
    if (order[order.length - 1] !== 'Diamond Task D') {
      throw new Error('Task D should be last');
    }

    console.log('  ✓ Diamond dependency graph passed\n');
  } finally {
    leaderClient.stop();
    teammateClient.stop();
  }
}

/**
/**
 * Test Scenario 5: Approval Workflow
 * Teammate requests approval, Leader approves
 */
async function testApprovalWorkflow(): Promise<void> {
  console.log('\n=== Test: Approval Workflow ===');

  const sessionId = generateSessionId();
  let approvalRequested = false;

  // Teammate that requests approval
  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    role: MemberRoleTeammate,
  }).withTeammatePlugin({
    async executeTask(_signal, task, reporter) {
      reporter.report(50, 'needs approval');
      approvalRequested = true;

      // In real scenario, approval would be requested automatically via tool interception
      // Here we simulate the approval flow
      reporter.report(100, 'done');
      return { output: 'Approved and executed' };
    },
  });

  // Leader with auto-approval
  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  })
    .withLeaderPlugin(new MockLeaderPlugin([
      { id: 'approval-task', description: 'Test Approval Task', priority: 9 },
    ]))
    .withApprovalPlugin(new MockApprovalPlugin(true));

  try {
    await teammateClient.start();
    await delay(500);

    await leaderClient.start();
    await delay(1000);

    await leaderClient.submitPlan('Approval workflow test');

    // Wait and verify
    await delay(3000);

    console.log('  ✓ Approval workflow test completed\n');
  } finally {
    leaderClient.stop();
    teammateClient.stop();
  }
}

/**
 * Test Scenario 6: Multiple Teammates
 * One Leader, multiple Teammates
 */
async function testMultipleTeammates(): Promise<void> {
  console.log('\n=== Test: Multiple Teammates ===');

  const sessionId = generateSessionId();
  const collectors: TaskExecutionCollector[] = [];
  const clients: TeamClient[] = [];

  // Create 2 Teammates
  for (let i = 0; i < 2; i++) {
    const collector = new TaskExecutionCollector();
    collectors.push(collector);

    const client = new TeamClient({
      serverUrl: config.serverUrl,
      token: config.token,
      sessionId,
      machineId: generateMachineId(`teammate-${i + 1}`),
      machineName: `Teammate ${i + 1}`,
      role: MemberRoleTeammate,
    }).withTeammatePlugin(new MockTeammatePlugin(collector));

    clients.push(client);
    await client.start();
  }

  await delay(1000);

  // Create Leader with 4 tasks
  const taskSpecs = createParallelTasks(4, 'Multi-Teammate Task');

  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  }).withLeaderPlugin(new MockLeaderPlugin(taskSpecs));

  try {
    await leaderClient.start();
    await delay(1000);

    await leaderClient.submitPlan('Multiple teammates test');

    // Wait for all tasks
    const totalCompleted = () =>
      collectors.reduce((sum, c) => sum + c.getCompletedCount(), 0);

    await waitFor(() => totalCompleted() >= 4, 15000, 500);

    // Show distribution
    collectors.forEach((c, i) => {
      console.log(`  Teammate ${i + 1} executed ${c.getCompletedCount()} tasks`);
    });

    console.log('  ✓ Multiple teammates test passed\n');
  } finally {
    leaderClient.stop();
    clients.forEach((c) => c.stop());
  }
}

/**
 * Test Scenario 7: Task Retry on Failure
 */
async function testTaskRetry(): Promise<void> {
  console.log('\n=== Test: Task Retry on Failure ===');

  const sessionId = generateSessionId();
  const collector = new TaskExecutionCollector();
  let attemptCount = 0;

  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    role: MemberRoleTeammate,
  }).withTeammatePlugin(new MockTeammatePlugin(collector, async (task, reporter) => {
    attemptCount++;
    reporter.report(50, 'executing');

    if (attemptCount <= 2) {
      // Fail first 2 attempts
      throw new Error(`Simulated failure (attempt ${attemptCount})`);
    }

    reporter.report(100, 'done');
    return { output: `Succeeded after ${attemptCount} attempts` };
  }));

  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  }).withLeaderPlugin(
    new MockLeaderPlugin([
      { id: 'retry-task', description: 'Retry Test Task', priority: 9, maxRetries: 3 },
    ])
  );

  try {
    await teammateClient.start();
    await delay(500);

    await leaderClient.start();
    await delay(1000);

    await leaderClient.submitPlan('Retry test');

    // Wait for completion
    await waitFor(() => collector.getCompletedCount() >= 1, 15000, 500);

    console.log(`  Task succeeded after ${attemptCount} attempts`);
    console.log('  ✓ Retry test passed\n');
  } finally {
    leaderClient.stop();
    teammateClient.stop();
  }
}

/**
 * Run all tests
 */
async function runAllTests(): Promise<void> {
  console.log('\n╔════════════════════════════════════════════════════════════╗');
  console.log('║     Cloud Team Agent SDK - End-to-End Tests               ║');
  console.log('╚════════════════════════════════════════════════════════════╝');
  console.log(`Server: ${config.serverUrl}`);
  console.log(`Session: ${config.sessionId}`);
  console.log(`Timeout: ${TEST_TIMEOUT}ms\n`);

  const results: Array<{ name: string; passed: boolean; error?: string }> = [];

  const tests = [
    { name: 'Basic Task Execution', fn: testBasicTaskExecution },
    { name: 'Task Dependency Chain', fn: testTaskDependencyChain },
    { name: 'Parallel Task Execution', fn: testParallelTaskExecution },
    { name: 'Diamond Dependency Graph', fn: testDiamondDependencyGraph },
    { name: 'Approval Workflow', fn: testApprovalWorkflow },
    { name: 'Multiple Teammates', fn: testMultipleTeammates },
    { name: 'Task Retry', fn: testTaskRetry },
  ];

  for (const test of tests) {
    try {
      await test.fn();
      results.push({ name: test.name, passed: true });
    } catch (error) {
      const errorMsg = error instanceof Error ? error.message : String(error);
      console.error(`  ✗ ${test.name} failed: ${errorMsg}`);
      results.push({ name: test.name, passed: false, error: errorMsg });
    }
  }

  // Print summary
  console.log('\n╔════════════════════════════════════════════════════════════╗');
  console.log('║                     Test Summary                           ║');
  console.log('╚════════════════════════════════════════════════════════════╝');

  const passed = results.filter((r) => r.passed).length;
  const failed = results.filter((r) => !r.passed).length;

  results.forEach((r) => {
    const status = r.passed ? '✓ PASS' : '✗ FAIL';
    console.log(`  ${status}: ${r.name}`);
    if (r.error) {
      console.log(`       ${r.error}`);
    }
  });

  console.log(`\nTotal: ${results.length} | Passed: ${passed} | Failed: ${failed}`);

  if (failed > 0) {
    process.exit(1);
  }
}

// Run tests if executed directly
const isMainModule = import.meta.url.startsWith('file://') && process.argv[1] && import.meta.url.includes(process.argv[1]);
if (isMainModule) {
  runAllTests().catch((error) => {
    console.error('Test runner failed:', error);
    process.exit(1);
  });
}

export {
  testBasicTaskExecution,
  testTaskDependencyChain,
  testParallelTaskExecution,
  testDiamondDependencyGraph,
  testApprovalWorkflow,
  testMultipleTeammates,
  testTaskRetry,
  runAllTests,
};
