# 项目成员仓库活动视图实施进度

基于 `docs/proposals/PROJECT_MEMBER_REPO_ACTIVITY_DESIGN.md` v1.0.1，并结合当前代码实际实现状态整理。

> **当前代码现状（已核对）**
>
> - `Project` / `ProjectMember` / `ProjectInvitation` 已实现
> - `ProjectRepository` **尚未实现**
> - `/api/usage/report` 已注册，但当前仅做参数校验与日志输出，**未持久化入库**
> - `/api/usage/activity` 已注册，但当前返回 `501 Not Implemented`
> - 提案中的 `session_usage_reports` / SQLite usage 聚合链路 **尚未落地**

因此，本任务应按以下两阶段推进：

1. **先补 usage 基础能力**（存储、查询、聚合）
2. **再补项目仓库活动视图**（项目仓库绑定 + 项目域聚合）

---

## 一、现状核对（对应当前代码）

### 1.1 已存在能力

- [x] `server/internal/models/models.go` 已包含 `Project`
- [x] `server/internal/models/models.go` 已包含 `ProjectMember`
- [x] `server/internal/models/models.go` 已包含 `ProjectInvitation`
- [x] `server/internal/project/service.go` 已实现项目 CRUD / 成员管理 / 邀请管理
- [x] `server/internal/project/project.go` 已注册 `/api/projects` 与 `/api/invitations` 路由
- [x] `server/internal/handlers/usage.go` 已注册 `/api/usage/report`
- [x] `server/internal/handlers/usage.go` 已注册 `/api/usage/activity`

### 1.2 已确认缺口

- [x] 当前不存在 `ProjectRepository` 模型
- [x] 当前不存在 `SessionUsageReport` 模型
- [x] 当前不存在 `session_usage_reports` 表实现
- [x] 当前 `UsageService.BatchUpsert(...)` 仅记录日志，未执行入库
- [x] 当前 `UsageService.GetActivity(...)` 未实现查询逻辑
- [x] 当前不存在 `/api/projects/:id/repositories`
- [x] 当前不存在 `/api/projects/:id/repo-activity`

---

## 二、Phase A：补 usage 基础能力（前置依赖）

> 说明：项目成员仓库活动视图依赖 usage 数据可查询，因此该阶段必须先完成。

### 2.1 存储方案确认

- [x] 明确 usage 数据存储方案：SQLite / PostgreSQL / 其他持久化方案
- [x] 确认是否继续沿用提案中的“业务 PG + usage SQLite”双存储设计
- [ ] 若不再使用 SQLite，补充新的实现说明并同步更新进度文档/提案
- [x] 明确 usage 数据与业务库之间的边界与职责

### 2.2 usage 数据模型

- [x] 新增 `SessionUsageReport` 模型（或等价持久化结构）
- [x] 字段覆盖：`user_id`、`device_id`、`session_id`、`request_id`、`message_id`
- [x] 字段覆盖：`date`、`updated`、`model_id`、`provider_id`
- [x] 字段覆盖：`input_tokens`、`output_tokens`、`reasoning_tokens`
- [x] 字段覆盖：`cache_read_tokens`、`cache_write_tokens`、`cost`
- [x] 字段覆盖：`rounds`、`git_repo_url`、`git_worktree`
- [x] 明确幂等键设计（如 `request_id` 或其他组合键）

### 2.3 migration / AutoMigrate

- [x] 为 usage 存储新增表结构迁移
- [x] 添加基础索引：`git_repo_url`
- [x] 添加基础索引：`user_id`
- [x] 添加基础索引：`date`
- [x] 添加聚合索引：`(git_repo_url, user_id, date)`
- [x] 如有必要添加辅助索引：`(git_repo_url, date)`

### 2.4 git repo URL 规范化

- [x] 实现统一的 `git_repo_url` 标准化函数
- [x] 规则覆盖：去掉 `.git`
- [x] 规则覆盖：SSH 转 HTTPS
- [x] 规则覆盖：去掉末尾 `/`
- [x] 规则覆盖：统一小写
- [x] 在 usage 上报入库前执行 URL 标准化

### 2.5 usage service 实现

- [x] 重构 `server/internal/services/usage.go`
- [x] `BatchUpsert(...)` 从“仅日志输出”改为“校验 + 入库/幂等更新`
- [x] 保留必要日志，但不能替代持久化
- [x] 处理重复上报幂等逻辑
- [x] 明确部分失败时的 accepted / skipped / errors 语义

### 2.6 usage activity 查询能力

- [x] 完成 `UsageService.GetActivity(gitRepoURL, days, names)` 实现
- [x] 支持按仓库聚合用户活跃度
- [x] 支持按日返回请求数曲线
- [x] 支持最近 `days`（1-90）范围查询
- [x] 正确填充 `UsageActivityResponse`

### 2.7 usage handler / API

- [x] 保持 `POST /api/usage/report` 与现有请求结构兼容
- [x] 将 `GET /api/usage/activity` 从 `501` 改为真实查询结果
- [x] 明确错误码：参数错误 / 未认证 / 查询失败
- [ ] 补全 Swagger / OpenAPI 中 usage 接口的真实返回说明

### 2.8 usage 测试

- [x] `BatchUpsert` 基础入库测试
- [x] `BatchUpsert` 幂等测试
- [x] `git_repo_url` 标准化测试
- [x] `GetActivity` 单仓库聚合测试
- [x] `GetActivity` 多用户按日统计测试
- [x] `/api/usage/report` handler 测试
- [x] `/api/usage/activity` handler 测试

---

## 三、Phase B：项目仓库绑定能力

> 说明：该阶段建立“项目 ↔ 仓库”显式关系，为项目视图聚合提供边界。

### 3.1 数据模型（`server/internal/models/models.go`）

- [x] 追加 `ProjectRepository` 模型
  ```go
  type ProjectRepository struct {
      ID             string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      ProjectID      string         `gorm:"not null;index"                                 json:"projectId"`
      GitRepoURL     string         `gorm:"not null;index"                                 json:"gitRepoUrl"`
      DisplayName    string         `                                                        json:"displayName,omitempty"`
      Source         string         `gorm:"not null;default:'manual'"                      json:"source"`
      BoundByUserID  string         `gorm:"not null;index"                                 json:"boundByUserId"`
      LastActivityAt *time.Time     `gorm:"index"                                          json:"lastActivityAt,omitempty"`
      CreatedAt      time.Time      `                                                        json:"createdAt"`
      UpdatedAt      time.Time      `                                                        json:"updatedAt"`
      DeletedAt      gorm.DeletedAt `gorm:"index"                                          json:"-"`
  }
  ```
- [x] 添加唯一索引：`UNIQUE(project_id, git_repo_url)`
- [x] 添加索引：`project_id`、`git_repo_url`、`last_activity_at`
- [x] 明确 `Source` 枚举：`manual` / `imported` / `detected`

### 3.2 migration / AutoMigrate

- [x] `AutoMigrate` 追加 `ProjectRepository`
- [ ] 或新增 SQL migration：创建 `project_repositories` 表与相关索引
- [x] 确认与现有 `Project`、`ProjectMember` 软删除语义保持一致

### 3.3 project types

- [x] 新增 `CreateProjectRepositoryRequest`
- [x] 新增 `ProjectRepositoryResponse`
- [x] 新增 `ListProjectRepositoriesResponse`

### 3.4 project service

- [x] `ListRepositories(projectID, userID string) ([]models.ProjectRepository, error)`
- [x] `BindRepository(projectID, operatorID, gitRepoURL, displayName string) (*models.ProjectRepository, error)`
- [x] `UnbindRepository(projectID, repoBindingID, operatorID string) error`
- [x] 校验：查看项目仓库列表需为项目成员
- [x] 校验：绑定/解绑项目仓库需为项目管理员
- [x] 校验：归档项目禁止绑定/解绑仓库
- [x] 校验：按 `(project_id, git_repo_url)` 去重
- [x] 校验：绑定时执行 `git_repo_url` 标准化

### 3.5 project handlers / routes

- [x] `GET /api/projects/:id/repositories` — 列出项目仓库
- [x] `POST /api/projects/:id/repositories` — 绑定项目仓库
- [x] `DELETE /api/projects/:id/repositories/:repoBindingId` — 解绑项目仓库
- [x] 将仓库管理接口注册到现有 `project.Module`

### 3.6 测试

- [x] 重复绑定同一仓库去重测试
- [x] 非管理员绑定/解绑仓库权限测试
- [x] 归档项目禁止绑定/解绑仓库测试
- [x] 列出项目仓库接口测试
- [x] 绑定项目仓库接口测试
- [x] 解绑项目仓库接口测试

---

## 四、Phase C：项目成员仓库活动聚合视图

> 说明：该阶段依赖 Phase A 的 usage 查询能力和 Phase B 的项目仓库绑定能力。

### 4.1 返回结构定义

- [x] 定义项目仓库活动总览响应结构（`project` / `range` / `summary` / `members` / `repositories`）
- [x] 定义成员视角结构：`activeRepoCount`、`totalRequests`、`activeRepos`
- [x] 定义仓库视角结构：`activeMemberCount`、`totalRequests`、`activeMembers`
- [x] 定义聚合明细结构：`requestCount`、`lastActiveDate`、`inputTokens`、`outputTokens`、`cost`

### 4.2 服务层实现

- [x] 新增 `GetProjectRepoActivity(projectID, userID string, days int, includeInactive bool)`
- [x] 校验调用人是项目成员
- [x] 校验 `days` 取值范围：1-90，默认 7
- [x] 从业务库查询项目基本信息
- [x] 从业务库查询项目成员
- [x] 从业务库查询项目仓库
- [x] 提取成员 `user_id` 与仓库 `git_repo_url` 列表
- [x] 调用 usage 聚合查询获得成员-仓库活动关系
- [x] 当项目未绑定仓库时返回空仓库视图语义
- [x] 当最近 N 天无活动时返回“全量成员/仓库但 active 为空”的语义

### 4.3 应用层结果拼装

- [x] 将 `user_id` 映射为用户名/显示名/角色
- [x] 将 `git_repo_url` 映射回项目仓库元信息
- [x] 组装 `member -> repos[]` 视图
- [x] 组装 `repo -> members[]` 视图
- [x] 计算 `summary.member_count`
- [x] 计算 `summary.repository_count`
- [x] 计算 `summary.active_member_count`
- [x] 计算 `summary.active_repository_count`
- [x] 计算 `summary.total_requests`
- [x] 支持 `includeInactive=true/false` 过滤逻辑

### 4.4 handler / route

- [x] `GET /api/projects/:id/repo-activity?days=7` — 项目仓库活动总览
- [x] 参数解析：`days`
- [x] 参数解析：`includeInactive`
- [x] 返回 200 响应结构与提案目标保持一致

### 4.5 可选增强接口

- [ ] `GET /api/projects/:id/members/:userId/repo-activity?days=30` — 单成员仓库活动详情【可选】

### 4.6 测试

- [ ] 活动聚合服务在空仓库场景下测试
- [ ] 活动聚合服务在无活动场景下测试
- [ ] 活动聚合服务在多成员多仓库场景下测试
- [ ] `includeInactive=true/false` 过滤逻辑测试
- [ ] 项目仓库活动总览接口测试
- [ ] 无权限访问项目仓库活动接口测试
- [ ] `days` 参数非法值测试
- [ ] 成员在项目外仓库有 activity 时不应出现在当前项目视图

---

## 五、前端接入（项目详情页）

### 5.1 视图结构

- [ ] 项目详情页新增 `成员仓库活动` tab
- [ ] 顶部汇总卡片：成员数 / 仓库数 / 活跃成员数 / 活跃仓库数 / 总请求数
- [ ] 成员视角表格：成员名 / 角色 / 活跃仓库数 / 总请求数 / 最近活跃仓库列表 / 最近活跃时间
- [ ] 仓库视角表格：仓库名 / 活跃成员数 / 总请求数 / 活跃成员列表 / 最近活跃时间

### 5.2 交互能力

- [ ] 支持时间范围切换：`7d / 30d / 90d`
- [ ] 支持筛选“仅显示有活动成员/仓库”
- [ ] 支持点击成员展开其各仓库活跃详情
- [ ] 支持点击仓库展开其成员活跃详情
- [ ] 区分空态：无绑定仓库 / 有仓库但无活动

---

## 六、Swagger / API 文档

- [ ] 为 usage 接口补充真实实现后的 Swagger 注释与返回说明
- [ ] 为项目仓库管理接口补充 Swagger 注释
- [ ] 为项目仓库活动总览接口补充 Swagger 注释
- [ ] 如实现可选详情接口，补充对应 Swagger 注释
- [ ] 更新 OpenAPI 文档生成产物（如项目已有对应流程）

---

## 七、进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 现状核对 | 已完成 |
| 二 | Phase A：usage 基础能力 | 已完成 |
| 三 | Phase B：项目仓库绑定能力 | 已完成 |
| 四 | Phase C：项目成员仓库活动聚合视图 | 基本完成（测试待补齐） |
| 五 | 前端接入 | 未开始 |
| 六 | Swagger / API 文档 | 未开始 |

---

## 八、进度记录

| 日期 | 内容 |
|------|------|
| 2026-04-01 | 创建初版进度文档，基于项目成员仓库活动视图提案初始化任务清单 |
| 2026-04-01 | 根据最新代码实现状态修正文档：确认当前尚无 `ProjectRepository`、尚无 usage 持久化、`/api/usage/activity` 未实现，因此调整为“先补 usage 基础能力，再补项目仓库活动视图”的分阶段计划 |
| 2026-04-01 | 已按“业务 PG + usage SQLite”完成后端第一版实现：新增 `SessionUsageReport`、`ProjectRepository`、usage SQLite 初始化、`/api/usage/activity` 真正查询、项目仓库管理接口与 `/api/projects/:id/repo-activity` 聚合接口；已补充 usage 与项目仓库管理相关测试 |
| 2026-04-01 | 已将 usage 能力重构为可插拔 provider 架构：当前保留 `SQLiteUsageProvider` 作为临时实现，`UsageService` 改为依赖 provider 接口，后续可无缝切换第三方上报/聚合服务 |

---

## 九、实施说明

### 当前关键判断

1. **当前最大的前置缺口不是项目视图，而是 usage 基础能力未落地。**
2. **repo activity 功能不能直接开始实现，必须先让 usage 数据可持久化、可聚合、可查询。**
3. **项目侧目前只有成员关系，没有项目仓库边界，因此也无法稳定回答“成员在哪些项目仓库有活动”。**

### 核心实现原则

1. **先基础、后视图**：先补 usage 存储与查询，再补项目域活动视图。
2. **项目范围显式化**：通过 `ProjectRepository` 明确项目关注仓库边界。
3. **关联键统一**：项目侧与 usage 侧统一使用标准化后的 `git_repo_url`。
4. **最小数据暴露**：仅返回当前项目成员 + 当前项目绑定仓库范围内的数据。
5. **usage 可替换**：`UsageService` 只依赖 provider 接口，当前 SQLite 仅为临时适配层，后续应由第三方上报/聚合实现替换。

### 主要风险与注意事项

1. **存储方案漂移风险**：若提案中的 SQLite 方案已不适合，需要先明确新的 usage 落库方案。
2. **URL 标准化不一致风险**：若项目侧与 usage 侧规范不一致，会导致活动数据无法命中。
3. **空态语义一致性**：需明确区分“未绑定仓库”和“有仓库但无活动”。
4. **权限边界**：避免把成员在项目外仓库的 activity 暴露到当前项目视图。

### 后续可扩展项

1. 项目成员活跃度排行
2. 项目仓库活跃度趋势图
3. 项目 token / cost 统计视图
4. 自动发现候选仓库并提示管理员绑定

---

## 参考文档

- [项目成员仓库活动视图技术提案](../docs/proposals/PROJECT_MEMBER_REPO_ACTIVITY_DESIGN.md)
- [项目管理技术提案](../docs/proposals/PROJECT_MANAGEMENT_DESIGN.md)
- [Session Usage Statistics 技术提案](../docs/proposals/SESSION_USAGE_STATISTICS_DESIGN.md)
