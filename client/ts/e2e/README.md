# Cloud Team Agent E2E Tests

端到端测试套件，用于验证 Cloud Team Agent SDK 的核心功能。

## 测试场景覆盖

| 测试场景 | 描述 | 文件 |
|---------|------|------|
| Basic Task Execution | 基础任务下发和执行 | `team-e2e.test.ts` |
| Task Dependency Chain | 任务依赖链执行顺序 | `team-e2e.test.ts` |
| Parallel Task Execution | 并行任务执行 | `team-e2e.test.ts` |
| Diamond Dependency Graph | 菱形依赖图 | `team-e2e.test.ts` |
| Approval Workflow | 审批流程测试 | `team-e2e.test.ts` |
| Multiple Teammates | 多 Teammate 协作 | `team-e2e.test.ts` |
| Task Retry | 任务失败重试 | `team-e2e.test.ts` |

## 快速开始

### 1. 安装依赖

```bash
cd costrict-web/client/ts
npm install
```

### 2. 配置环境变量

```bash
export E2E_SERVER_URL="http://localhost:8080"
export E2E_TOKEN="your-jwt-token"
export E2E_SESSION_ID="optional-session-id"
```

### 3. 运行测试

```bash
# 运行所有 E2E 测试
npm run test:e2e:ts

# 或直接使用 ts-node
npx ts-node --project tsconfig.e2e.json e2e/team-e2e.test.ts
```

## 环境变量

| 变量名 | 必填 | 默认值 | 说明 |
|--------|------|--------|------|
| `E2E_SERVER_URL` | 否 | `http://localhost:8080` | Team 服务端点 |
| `E2E_TOKEN` | 是 | - | JWT 认证令牌 |
| `E2E_SESSION_ID` | 否 | 自动生成 | 测试会话 ID |
| `E2E_TIMEOUT` | 否 | `30000` | 测试超时时间(ms) |

## 测试架构

```
e2e/
├── team-e2e.test.ts    # 主测试文件（测试场景）
├── helpers.ts          # 测试工具函数
├── mocks.ts            # Mock 插件实现
└── README.md           # 本文档
```

### 核心组件

#### TaskExecutionCollector

跟踪任务执行状态的工具类：

```typescript
const collector = new TaskExecutionCollector();

// 获取执行统计
console.log(collector.getCompletedCount());  // 完成的任务数
console.log(collector.getFailedCount());     // 失败的任务数
console.log(collector.getExecutionOrder());  // 执行顺序

// 打印报告
collector.printReport();
```

#### Mock Plugins

**MockLeaderPlugin**: 预定义任务计划的 Leader 插件

```typescript
const leaderPlugin = new MockLeaderPlugin([
  { id: 'task-1', description: 'Task 1', priority: 9 },
  { id: 'task-2', description: 'Task 2', priority: 8, dependencies: ['task-1'] },
]);
```

**MockTeammatePlugin**: 模拟任务执行的 Teammate 插件

```typescript
const collector = new TaskExecutionCollector();
const teammatePlugin = new MockTeammatePlugin(
  collector,
  async (task, reporter) => {
    reporter.report(50, 'executing');
    return { output: 'done' };
  },
  100  // 执行延迟(ms)
);
```

**FailingTeammatePlugin**: 模拟任务失败（用于测试重试）

```typescript
const failingPlugin = new FailingTeammatePlugin(
  ['task-1', 'task-2'],  // 会失败的任务ID
  2  // 失败2次后才成功
);
```

#### TaskPlanBuilder

流式 API 构建任务计划：

```typescript
// 线性依赖链
const linear = TaskPlanBuilder.linear('A', 'B', 'C');
// A → B → C

// 并行任务
const parallel = TaskPlanBuilder.parallel(3, 'Task');
// Task 1, Task 2, Task 3 (无依赖)

// 自定义构建
const custom = TaskPlanBuilder.create()
  .addTask('First')
  .addDependentTask('Second', 'First')
  .build();
```

## 编写新测试

### 基本模板

```typescript
async function testMyScenario(): Promise<void> {
  console.log('\n=== Test: My Scenario ===');

  const sessionId = generateSessionId();
  const collector = new TaskExecutionCollector();

  // 创建 Teammate
  const teammateClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('teammate'),
    role: MemberRoleTeammate,
  }).withTeammatePlugin(new MockTeammatePlugin(collector));

  // 创建 Leader
  const taskSpecs = TaskPlanBuilder.linear('Step 1', 'Step 2', 'Step 3');
  
  const leaderClient = new TeamClient({
    serverUrl: config.serverUrl,
    token: config.token,
    sessionId,
    machineId: generateMachineId('leader'),
    role: MemberRoleLeader,
  }).withLeaderPlugin(new MockLeaderPlugin(taskSpecs));

  try {
    // 启动客户端
    await teammateClient.start();
    await delay(500);
    await leaderClient.start();
    await delay(1000);

    // 提交计划
    await leaderClient.submitPlan('Test scenario');

    // 等待完成
    await waitFor(() => collector.getCompletedCount() >= 3, 10000);

    // 验证结果
    console.log('  ✓ Test passed\n');
  } finally {
    // 清理
    leaderClient.stop();
    teammateClient.stop();
  }
}
```

### 测试工具函数

| 函数 | 用途 |
|------|------|
| `generateSessionId()` | 生成唯一会话ID |
| `generateMachineId(prefix)` | 生成机器ID |
| `delay(ms)` | 异步延迟 |
| `waitFor(condition, timeout, interval)` | 等待条件满足 |
| `createTaskChain(descriptions)` | 创建依赖链 |
| `createParallelTasks(count)` | 创建并行任务 |
| `createDiamondGraph(...)` | 创建菱形依赖图 |

## 调试技巧

### 1. 增加日志输出

```typescript
// 在测试中添加详细日志
console.log('  Current executions:', collector.getExecutions());
console.log('  Execution order:', collector.getExecutionOrder());
```

### 2. 延长超时时间

```typescript
// 对于复杂的测试，增加超时
await waitFor(() => collector.getCompletedCount() >= 10, 60000);
```

### 3. 单测试运行

注释掉其他测试，只运行特定测试：

```typescript
const tests = [
  // { name: 'Other Test', fn: testOther },
  { name: 'My Test', fn: testMyScenario },
];
```

## CI/CD 集成

```yaml
# .github/workflows/e2e.yml
name: E2E Tests

on: [push, pull_request]

jobs:
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      
      - name: Setup Node.js
        uses: actions/setup-node@v3
        with:
          node-version: '20'
      
      - name: Install dependencies
        run: cd costrict-web/client/ts && npm ci
      
      - name: Start test server
        run: docker-compose up -d
      
      - name: Wait for server
        run: sleep 10
      
      - name: Run E2E tests
        run: cd costrict-web/client/ts && npm run test:e2e:ts
        env:
          E2E_SERVER_URL: http://localhost:8080
          E2E_TOKEN: ${{ secrets.TEST_TOKEN }}
```

## 常见问题

### Q: 测试连接失败
A: 检查服务端是否运行，以及 `E2E_TOKEN` 是否有效。

### Q: 任务未执行
A: 确保 Teammate 在 Leader 提交计划前已启动并注册。

### Q: 依赖任务顺序错误
A: 检查是否正确设置了 `dependencies` 数组，并确保任务 ID 正确。

### Q: 并行任务未并行执行
A: 这是预期行为，实际并行度取决于服务端调度和 Teammate 数量。

## 扩展测试

如需添加更多测试场景：

1. 在 `team-e2e.test.ts` 中添加新的测试函数
2. 将新测试添加到 `tests` 数组
3. 运行测试验证
4. 更新本文档
