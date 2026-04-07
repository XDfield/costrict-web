> **实现状态：待实现**
>
> - 状态：📋 设计阶段
> - 涉及模块：`项目管理`、`Session Usage Statistics`、`项目视图`
> - 前置依赖：
>   - `docs/proposals/PROJECT_MANAGEMENT_DESIGN.md`
>   - `docs/proposals/SESSION_USAGE_STATISTICS_DESIGN.md`
> - 说明：在项目视图中展示“项目成员分别在哪些 git_repo 下有活动”，并为后续仓库级活跃度分析提供统一的数据与接口设计。

---

# 项目成员仓库活动视图技术提案

## 目录

- [概述](#概述)
- [背景与问题定义](#背景与问题定义)
- [设计目标](#设计目标)
- [总体设计](#总体设计)
- [核心设计决策](#核心设计决策)
- [数据模型设计](#数据模型设计)
- [查询与聚合设计](#查询与聚合设计)
- [API 设计](#api-设计)
- [前端视图建议](#前端视图建议)
- [权限与安全](#权限与安全)
- [性能与扩展性](#性能与扩展性)
- [与现有提案的衔接](#与现有提案的衔接)
- [实施计划](#实施计划)

---

## 概述

基于 `SESSION_USAGE_STATISTICS_DESIGN.md`，服务端已经规划接收用户按 `git_repo_url` 维度上报的 session usage 数据；基于 `PROJECT_MANAGEMENT_DESIGN.md`，系统也已经规划引入“项目”和“项目成员”概念。

当前新增需求是：

> 在项目视图中，可以看到**对应成员在哪些 git_repo 下有活动**。

这个需求本质上不是单纯的 usage 统计展示，而是一个**项目域视角下的成员-仓库活动关联查询**问题，需要同时回答三个问题：

1. 一个项目下包含哪些仓库？
2. 一个项目下有哪些成员？
3. 在给定时间范围内，这些成员在哪些项目仓库上产生过 usage 活动？

因此，本提案的核心目标是补齐“项目 ↔ git_repo”绑定关系，并在项目成员与 usage 数据之间建立稳定的查询链路。

---

## 背景与问题定义

### 现状

从现有两个提案看：

- **项目管理提案**已经有 `Project`、`ProjectMember`、`ProjectInvitation`，但还没有“项目仓库”模型
- **Session Usage Statistics 提案**已经有 `session_usage_reports`，每条记录中带有 `user_id` 与 `git_repo_url`

这意味着当前系统能够回答：

- 某个用户在哪个仓库有 usage 活动
- 某个仓库有哪些用户有 usage 活动

但还**不能稳定回答**：

- “某个项目下的成员在哪些仓库有活动”

因为缺少“这个仓库属于哪个项目”的显式关系。

### 如果不补项目仓库绑定，会有什么问题

如果只依赖 usage 表中自然出现的 `git_repo_url`，会出现以下歧义：

1. **无法界定项目范围**
   - 某成员可能在很多仓库有活动，但并非都属于当前项目
2. **无法做项目视图筛选**
   - UI 无法知道哪些 repo 应纳入该项目的分析面板
3. **无法支撑项目治理**
   - 项目管理员无法维护“项目关心哪些仓库”
4. **后续扩展困难**
   - 项目级 repo 排行、项目级 token/cost 聚合等能力都依赖项目仓库关系

所以，本提案建议把问题拆成两部分：

- **项目侧**：引入项目仓库绑定关系
- **统计侧**：基于项目成员 + 项目仓库 + usage 上报，构建项目成员仓库活动视图

---

## 设计目标

### 核心目标

1. 在项目详情页中展示项目成员的仓库活动分布
2. 支持查看“某成员在哪些项目仓库下有活动”
3. 支持查看“某仓库下有哪些项目成员有活动”
4. 支持按最近 `N` 天进行统计，默认 7 天

### 非目标

本提案**不尝试**解决以下问题：

1. 不自动推断项目包含哪些仓库，项目仓库关系默认由业务显式维护
2. 不引入复杂的仓库权限同步逻辑（如直接同步 GitHub org/repo 权限）
3. 不在本阶段做全量多维 BI 报表，仅解决项目视图所需能力
4. 不修改 usage 数据的核心粒度，仍保持“一次 assistant 请求一条记录”

---

## 总体设计

### 设计思路

新增一个“项目仓库绑定”概念：

```text
Project
  ├─ ProjectMember       -> 项目有哪些成员
  ├─ ProjectRepository   -> 项目关注哪些 git_repo
  └─ SessionUsageReport  -> 成员在这些 git_repo 上是否有活动
```

项目视图查询流程：

```text
1. 从业务库 PostgreSQL 获取：
   - project members
   - project repositories

2. 从 usage SQLite 获取：
   - user_id ∈ 项目成员
   - git_repo ∈ 项目仓库
   - date ∈ 最近 N 天
   的 usage 聚合结果

3. 在应用层组装：
   - 成员 -> 活跃仓库列表
   - 仓库 -> 活跃成员列表
   - 成员/仓库矩阵统计
```

### 为什么采用应用层双库聚合

当前设计中：

- 项目数据在 **PostgreSQL**
- usage 数据在独立 **SQLite**

两者是有意隔离的，因此不适合做数据库层 join。最合理的方式是：

1. 先在 PostgreSQL 查出项目成员、项目仓库
2. 再把这些条件带入 SQLite 做聚合查询
3. 最后在 Go 服务层完成结果拼装

这也与 `SESSION_USAGE_STATISTICS_DESIGN.md` 中“统计数据与业务 PostgreSQL 完全隔离”的原则一致。

---

## 核心设计决策

### 决策 1：项目仓库必须显式建模

**选择**：新增 `ProjectRepository` 表。

**原因**：

- usage 只说明“某用户在哪个 repo 有活动”，不能说明“该 repo 属于哪个项目”
- 项目视图必须有明确的仓库边界
- 便于项目管理员维护项目关注仓库列表

### 决策 2：直接复用已标准化的 `git_repo_url`

**选择**：项目仓库表直接存储与 usage 上报一致的标准化 `git_repo_url`。

建议规则与 usage 提案保持一致：

- 去掉 `.git`
- SSH 转 HTTPS
- 去掉末尾 `/`
- 统一小写

例如：

- `git@github.com:zgsm-ai/opencode.git`
- `https://github.com/zgsm-ai/opencode/`

最终都规范化为：

`https://github.com/zgsm-ai/opencode`

由于数据上报阶段已经完成 repo 标准化，因此项目侧只需要复用同一套规则保存 `git_repo_url`，后续即可直接与 usage 数据关联，无需额外再引入 `normalized_repo` 字段。

### 决策 3：项目视图接口返回“成员视角 + 仓库视角”两套结构

**选择**：一个接口同时返回：

- `members`: 每个成员活跃过哪些 repo
- `repositories`: 每个 repo 下有哪些成员活跃

**原因**：

- 前端无需为同一张视图发两次请求
- 一次查询结果可直接支持表格、卡片、筛选器和详情抽屉

### 决策 4：统计口径以“有活动”与“请求次数”并存

**选择**：同时返回：

- `active_repo_count` / `active_member_count`
- `request_count`

**原因**：

- 用户需求首先关注“在哪些 repo 下有活动”
- 但 UI 一般还会希望知道活跃强度，不应只返回布尔型结果

---

## 数据模型设计

### 一、新增 ProjectRepository 表

建议在 `server/internal/models/models.go` 中新增：

```go
type ProjectRepository struct {
    ID              string         // uuid PK
    ProjectID       string         // 关联 Project
    GitRepoURL      string         // 标准化后的 repo URL，用于查询与展示
    DisplayName     string         // 可选显示名，如 "opencode"
    Source          string         // "manual" | "imported" | "detected"
    BoundByUserID   string         // 绑定操作人
    LastActivityAt  *time.Time     // 最近一次项目范围内观测到活动的时间（可选缓存）
    CreatedAt       time.Time
    UpdatedAt       time.Time
    DeletedAt       gorm.DeletedAt
}
```

**索引建议：**

- `idx_project_repo_unique`: `(project_id, git_repo_url)` UNIQUE
- `idx_project_repo_project`: `(project_id)`
- `idx_project_repo_url`: `(git_repo_url)`

### 二、复用现有 SessionUsageReport 的 `git_repo_url`

由于 usage 上报时已经完成 repo 标准化，因此本提案不要求为 `SessionUsageReport` 再新增 `normalized_repo` 字段。

项目视图直接使用现有字段进行关联：

```go
type SessionUsageReport struct {
    // ... existing fields
    GitRepoURL string // 已标准化
}
```

**索引建议：**

- 继续复用 usage 提案中的索引：`(git_repo_url, user_id, date)`
- 如项目视图查询成为热点，可保留 `(git_repo_url, date)` 作为辅助索引

### 三、是否需要缓存汇总表

本阶段**不新增**项目级活动汇总表。

原因：

1. 项目视图查询规模通常较小
2. usage 底表已具备 `repo + user + date` 索引
3. 提前引入汇总表会增加写放大与一致性成本

后续如果出现大规模查询，再考虑新增日汇总表：

- `project_member_repo_daily_stats`

但这不是当前阶段必需项。

---

## 查询与聚合设计

### 查询目标

给定：

- `project_id`
- `days`（默认 7，范围 1-90）

返回：

1. 项目成员列表
2. 项目仓库列表
3. 在时间范围内的成员-仓库活动关系
4. 每个成员对应的活跃 repo 列表
5. 每个 repo 对应的活跃成员列表

### 服务层流程

```text
ProjectRepoActivityService.GetProjectRepoActivity(projectID, days)
  1. 校验调用人是项目成员
  2. 从 PG 查询 project members
  3. 从 PG 查询 project repositories
  4. 提取 member user_ids 与 repository git_repo_url 列表
  5. 从 usage SQLite 聚合查询
  6. 应用层组装 members/repositories/summary
  7. 返回给 handler
```

### SQLite 聚合 SQL

按成员-仓库粒度聚合：

```sql
SELECT
    user_id,
    git_repo_url,
    COUNT(*)              AS request_count,
    MIN(date)             AS first_active_date,
    MAX(date)             AS last_active_date,
    SUM(input_tokens)     AS input_tokens,
    SUM(output_tokens)    AS output_tokens,
    SUM(cost)             AS total_cost
FROM session_usage_reports
WHERE git_repo_url IN (? ...)
  AND user_id IN (? ...)
  AND date >= date('now', ?)
GROUP BY user_id, git_repo_url
ORDER BY request_count DESC;
```

日期参数例如 `-6 days` 表示最近 7 天。

### 应用层组装结果

应用层需要完成以下补充逻辑：

1. 把 `user_id` 映射为用户名/显示名
2. 把 `git_repo_url` 映射回项目中的 repo 元信息
3. 构建两个视图：
   - `member -> repos[]`
   - `repo -> members[]`
4. 计算汇总字段：
   - 项目活跃成员数
   - 项目活跃仓库数
   - 总请求数

### 空数据语义

若项目已经绑定仓库，但最近 N 天无活动，应返回：

- `members`: 全量项目成员，但其 `activeRepos` 为空
- `repositories`: 全量项目仓库，但其 `activeMembers` 为空
- `summary.active_member_count = 0`
- `summary.active_repo_count = 0`

这样前端可以明确区分：

- “项目没有绑定仓库”
- “项目已绑定仓库，但最近没有 activity”

---

## API 设计

### 一、项目仓库管理接口

这部分是项目视图能力的前置接口。

#### 1. 列出项目仓库

```http
GET /api/projects/:id/repositories
```

权限：项目成员

响应：

```json
{
  "repositories": [
    {
      "id": "repo-bind-1",
      "projectId": "project-1",
      "gitRepoUrl": "https://github.com/zgsm-ai/opencode",
      "displayName": "opencode",
      "source": "manual",
      "lastActivityAt": "2026-04-01T10:00:00Z"
    }
  ]
}
```

#### 2. 绑定项目仓库

```http
POST /api/projects/:id/repositories
```

权限：项目管理员

请求：

```json
{
  "gitRepoUrl": "git@github.com:zgsm-ai/opencode.git",
  "displayName": "opencode"
}
```

服务端处理：

1. 规范化 URL
2. 按 `(project_id, git_repo_url)` 去重
3. 保存标准化后的 `gitRepoUrl`

#### 3. 解绑项目仓库

```http
DELETE /api/projects/:id/repositories/:repoBindingId
```

权限：项目管理员

### 二、项目成员仓库活动视图接口

#### 1. 项目仓库活动总览

```http
GET /api/projects/:id/repo-activity?days=7
```

权限：项目成员

**Query 参数：**

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `days` | int | N | 7 | 统计窗口，1-90 |
| `includeInactive` | bool | N | true | 是否返回无活动的成员/仓库 |

**Response 200:**

```json
{
  "project": {
    "id": "project-1",
    "name": "AI 能力平台"
  },
  "range": {
    "days": 7,
    "from": "2026-03-26",
    "to": "2026-04-01"
  },
  "summary": {
    "member_count": 4,
    "repository_count": 3,
    "active_member_count": 2,
    "active_repository_count": 2,
    "total_requests": 31
  },
  "members": [
    {
      "userId": "user-alice",
      "username": "alice",
      "role": "admin",
      "activeRepoCount": 2,
      "totalRequests": 18,
      "activeRepos": [
        {
          "repositoryId": "repo-bind-1",
          "displayName": "opencode",
          "gitRepoUrl": "https://github.com/zgsm-ai/opencode",
          "requestCount": 12,
          "lastActiveDate": "2026-04-01",
          "inputTokens": 120000,
          "outputTokens": 45000,
          "cost": 8.52
        },
        {
          "repositoryId": "repo-bind-2",
          "displayName": "costrict-web",
          "gitRepoUrl": "https://github.com/costrict/costrict-web",
          "requestCount": 6,
          "lastActiveDate": "2026-03-30",
          "inputTokens": 40000,
          "outputTokens": 12000,
          "cost": 2.10
        }
      ]
    },
    {
      "userId": "user-bob",
      "username": "bob",
      "role": "member",
      "activeRepoCount": 0,
      "totalRequests": 0,
      "activeRepos": []
    }
  ],
  "repositories": [
    {
      "repositoryId": "repo-bind-1",
      "displayName": "opencode",
      "gitRepoUrl": "https://github.com/zgsm-ai/opencode",
      "activeMemberCount": 1,
      "totalRequests": 12,
      "activeMembers": [
        {
          "userId": "user-alice",
          "username": "alice",
          "requestCount": 12,
          "lastActiveDate": "2026-04-01"
        }
      ]
    }
  ]
}
```

#### 2. 单成员项目仓库活动详情（可选增强接口）

```http
GET /api/projects/:id/members/:userId/repo-activity?days=30
```

权限：项目成员

用途：

- 点击项目成员后查看其在项目各仓库中的详细活跃情况
- 便于前端延迟加载更细粒度数据

本阶段不是必需接口，可在第一期先只做总览接口。

---

## 前端视图建议

### 视图形态建议

项目详情页中新增一个 tab：

- `成员仓库活动`

推荐包含三块区域：

#### 1. 顶部汇总卡片

- 项目成员数
- 项目仓库数
- 最近 N 天活跃成员数
- 最近 N 天活跃仓库数
- 最近 N 天总请求数

#### 2. 成员视角表格

列建议：

- 成员名
- 角色
- 活跃仓库数
- 总请求数
- 最近活跃仓库列表（tag）
- 最近活跃时间

这块直接对应用户的核心诉求：

> 可以看到对应成员在哪些 git_repo 下有活动

#### 3. 仓库视角表格

列建议：

- 仓库名
- 活跃成员数
- 总请求数
- 活跃成员列表
- 最近活跃时间

### 交互建议

1. 支持 `7d / 30d / 90d` 切换
2. 支持筛选“仅显示有活动成员”
3. 点击成员展开该成员在各 repo 的详情
4. 点击 repo 展开该 repo 下各成员活跃详情

---

## 权限与安全

### 权限建议

| 场景 | 权限 |
|------|------|
| 查看项目仓库列表 | 项目成员 |
| 查看项目成员仓库活动 | 项目成员 |
| 绑定/解绑项目仓库 | 项目管理员 |

### 数据边界

项目视图查询时，只能返回：

1. 当前项目成员的数据
2. 当前项目绑定仓库的数据

即使某个成员在其他 repo 也有 activity，也**不能**出现在当前项目视图中。

### 隐私说明

本功能展示的是：

- 成员在项目仓库上的活跃情况

而不是：

- 成员完整跨项目的全部 usage 轨迹

因此该设计天然符合“按项目范围最小暴露”的原则。

---

## 性能与扩展性

### 查询规模评估

以单项目估算：

- 成员数：20~50
- 绑定仓库数：5~20
- 时间窗口：7~30 天

对应 SQLite 聚合扫描量通常在可控范围内，且已有 usage 索引支撑。

### 推荐索引

为支撑本提案，建议继续使用 usage 表已有的 repo 维度索引：

```sql
CREATE INDEX IF NOT EXISTS idx_usage_repo_user_date
ON session_usage_reports (git_repo_url, user_id, date);

CREATE INDEX IF NOT EXISTS idx_usage_repo_date
ON session_usage_reports (git_repo_url, date);
```

### 未来扩展方向

基于当前结构，后续可以自然扩展：

1. **项目仓库活跃度趋势图**
   - 按 repo + date 聚合
2. **项目成员活跃度排行**
   - 按 user_id 汇总请求数
3. **项目 token/cost 视图**
   - 在成员-仓库聚合中增加 token/cost 展示
4. **自动发现候选仓库**
   - 根据项目成员近期 activity 提示管理员“是否绑定此仓库”

---

## 与现有提案的衔接

### 对 `PROJECT_MANAGEMENT_DESIGN.md` 的补充

建议在项目管理提案中补充：

1. 数据模型新增 `ProjectRepository`
2. API 端点新增：
   - `GET /api/projects/:id/repositories`
   - `POST /api/projects/:id/repositories`
   - `DELETE /api/projects/:id/repositories/:repoBindingId`
3. 项目详情页扩展“成员仓库活动”视图

### 对 `SESSION_USAGE_STATISTICS_DESIGN.md` 的补充

建议补充以下内容：

1. 明确 `git_repo_url` 入库值就是标准化后的仓库 URL，并作为项目侧关联键
2. 新增一个项目视图专用查询接口，或在 usage service 中增加项目域聚合能力
3. 如需稳妥，可在服务端保留轻量校验，确保上报值符合标准化格式约束

### 为什么不直接复用 `/api/usage/activity`

`/api/usage/activity` 的定位是：

- 针对单个 `git_repo_url`，看该 repo 下各用户的活跃度曲线

而本提案需要的是：

- 针对一个 `project`，看该项目下**多 repo × 多成员**的活动分布

因此二者虽然底层数据相同，但查询入口和返回结构不同，建议单独设计项目域接口，而不是硬扩展 `/api/usage/activity`。

---

## 实施计划

### Phase 1：补齐数据模型（0.5d）

1. 新增 `ProjectRepository` model
2. 为项目模块增加仓库绑定相关 service / handler 设计
3. 明确项目仓库与 usage 统一复用标准化后的 `git_repo_url`

### Phase 2：实现项目仓库管理接口（0.5d）

1. 列出项目仓库
2. 绑定项目仓库
3. 解绑项目仓库

### Phase 3：实现项目成员仓库活动聚合查询（1d）

1. PG 查询项目成员与项目仓库
2. SQLite 聚合 usage 数据
3. 应用层拼装成员/仓库双视图响应

### Phase 4：项目视图前端接入（0.5d ~ 1d）

1. 新增“成员仓库活动”tab
2. 实现成员视角表格
3. 实现仓库视角表格与时间范围切换

### Phase 5：测试与联调（0.5d）

1. URL normalize 一致性测试
2. 项目仓库绑定接口测试
3. 项目视图活动聚合测试
4. 无活动/无仓库/无权限等边界场景测试

---

## 总结

要在项目视图中回答“成员在哪些 git_repo 下有活动”，关键不是再加一个统计接口，而是要建立完整的：

**项目成员 + 项目仓库 + usage 活动** 三段式关系。

本提案的核心落点是：

1. 为项目补充 `ProjectRepository` 显式绑定关系
2. 直接复用已标准化的 `git_repo_url` 作为稳定关联键
3. 通过“PG 查项目元数据 + SQLite 查 usage 聚合 + 应用层拼装”的方式提供项目视图能力

这样设计的优点是：

- 与现有项目提案和 usage 提案天然兼容
- 不破坏“业务数据与统计数据隔离”的前提
- 能直接支撑你希望的项目视图展示诉求
- 也为后续项目级仓库排行、成员活跃度、token/cost 分析预留了统一扩展路径

---

**文档版本：** 1.0.1
**创建日期：** 2026-04-01
**更新日期：** 2026-04-01
**维护者：** CoStrict Team
