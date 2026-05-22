> **实现状态：已实施**
>
> - 状态：✅ 已实施
> - 实现时间：2026-05-22
> - 后端模块：`server/internal/collaboration/`
> - 前端模块：`packages/app-ai-native/src/pages/collaboration/`
> - 数据库迁移：`server/migrations/20260522100000_add_collaboration_spaces.sql` 等

---

# Multica 协作体系嵌入技术提案

## 目录

- [概述](#概述)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [后端模块设计](#后端模块设计)
- [API 设计](#api-设计)
- [前端模块设计](#前端模块设计)
- [解耦策略](#解耦策略)
- [实施记录](#实施记录)

---

## 概述

### 背景与动机

将 Multica 的 Workspace + Issues + Projects + Squads 协作体系嵌入现有平台：

- **后端**：`costrict-web`（Go + Gin + GORM + PostgreSQL）
- **前端**：`app-ai-native`（SolidJS + Vite + TailwindCSS v4）

Multica 与现有平台的技术栈差异：
- Multica 前端是 React 生态（TanStack Query + Zustand），与 SolidJS 不兼容
- costrict-web 已有设备侧 Workspace/Project，但 **没有 Issue/Squad 系统**
- 需要 **解耦嵌入**，最小化对原有框架的破坏

### 设计原则

- **解耦优先**：新建独立模块，不改造现有路由/模型/页面
- **概念区分**：Multica 的 Workspace 映射为 "Space"，避免与现有设备侧 Workspace 冲突
- **技术栈适配**：前端基于 SolidJS 重新实现，不尝试复用 React 组件
- **扩展不改造**：现有 Project 仅增加可选 `SpaceID` 字段，原有路由零变更

---

## 架构设计

### 整体定位

```
app-ai-native 左侧菜单栏
├─ Store
├─ Workspace（设备侧，现有）
├─ Kanban（现有）
├─ 【新增】Collaboration（协作空间）
│   ├─ 二级侧边栏：Issues
│   ├─ 二级侧边栏：Projects
│   └─ 二级侧边栏：Squads
└─ Console

costrict-web 后端路由
├─ /api/workspaces（设备侧，现有）
├─ /api/projects（现有）
├─ /api/kanban（现有）
├─ 【新增】/api/spaces（协作空间）
├─ 【新增】/api/issues
├─ 【新增】/api/squads
└─ /api/team（现有）
```

### 新增功能域策略

在现有平台旁并排新增一套完整的协作空间能力，采用 "新增功能域" 策略：

| 层面 | 策略 |
|------|------|
| 后端模块 | 新建 `internal/collaboration/`，不修改已有 `internal/project/` 等模块 |
| 后端路由 | 新增 `/api/spaces`、`/api/issues`、`/api/squads`，原有路由零变更 |
| 后端模型 | 新增独立表，现有表只增加可选字段（`Project.SpaceID`） |
| 前端路由 | 新增 `/collaboration/*` 路由域，原有路由不变 |
| 前端布局 | 新增 `CollaborationLayout`，不修改 `RootLayout`、`ConsoleLayout` |
| 前端页面 | 新增 `pages/collaboration/` 目录，从零实现 |

---

## 数据模型

### 数据库迁移文件

| 文件 | 内容 |
|------|------|
| `20260522100000_add_collaboration_spaces.sql` | `spaces` 表 + `space_members` 表 |
| `20260522100100_add_collaboration_issues.sql` | `issues` 表 + `issue_comments` 表 |
| `20260522100200_add_collaboration_squads.sql` | `squads` 表 + `squad_members` 表 |
| `20260522100300_add_project_space_id.sql` | `projects` 表增加 `space_id` 可选字段 |

### Space（协作空间）

```go
type Space struct {
    ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    Name        string         `gorm:"not null"`
    Slug        string         `gorm:"uniqueIndex;not null"`
    Description string
    Settings    datatypes.JSON `gorm:"type:jsonb;default:'{}'"`
    CreatedAt   time.Time
    UpdatedAt   time.Time
    DeletedAt   gorm.DeletedAt `gorm:"index"`
}
```

### SpaceMember（空间成员）

```go
type SpaceMember struct {
    ID        string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    SpaceID   string    `gorm:"not null;index"`
    UserID    string    `gorm:"not null;index"`
    Role      string    `gorm:"not null;default:'member'"` // owner | admin | member
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"`
}
```

### Issue（事务）

```go
type Issue struct {
    ID            string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    SpaceID       string         `gorm:"not null;index"`
    Number        int            `gorm:"not null"` // 空间内自增序号
    Title         string         `gorm:"not null"`
    Description   string         `gorm:"type:text"`
    Status        string         `gorm:"not null;default:'backlog'"` // backlog | todo | in_progress | in_review | done | blocked | cancelled
    Priority      string         `gorm:"not null;default:'none'"`    // urgent | high | medium | low | none
    AssigneeType  *string        // member | squad
    AssigneeID    *string
    CreatorID     string         `gorm:"not null;index"`
    ParentIssueID *string
    Position      float64        `gorm:"not null;default:0"`
    DueDate       *time.Time
    Metadata      datatypes.JSON `gorm:"type:jsonb;default:'{}'"`
    CreatedAt     time.Time
    UpdatedAt     time.Time
    DeletedAt     gorm.DeletedAt `gorm:"index"`
}
```

### Squad（小队）

```go
type Squad struct {
    ID           string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    SpaceID      string     `gorm:"not null;index"`
    Name         string     `gorm:"not null"`
    Description  string     `gorm:"type:text"`
    LeaderID     string     `gorm:"index"`
    Instructions string     `gorm:"type:text"`
    AvatarURL    string
    ArchivedAt   *time.Time `gorm:"index"`
    CreatedAt    time.Time
    UpdatedAt    time.Time
}
```

### SquadMember（小队成员）

```go
type SquadMember struct {
    SquadID  string    `gorm:"primaryKey"`
    UserID   string    `gorm:"primaryKey"`
    Role     string    `gorm:"not null;default:'member'"` // leader | member
    JoinedAt time.Time `gorm:"not null"`
}
```

---

## 后端模块设计

### 目录结构

```
server/internal/collaboration/
├── models.go          # GORM 模型定义
├── dto.go             # 请求/响应 DTO
├── service.go         # 业务服务层
├── module.go          # 模块入口：New() + RegisterRoutes() + 中间件
├── space_handler.go   # Spaces CRUD + 成员管理
├── issue_handler.go   # Issues CRUD + 评论
└── squad_handler.go   # Squads CRUD + 成员管理
```

### 模块自注册模式

遵循现有项目模块的自注册模式：

```go
func New(db *gorm.DB) *Module {
    return &Module{service: NewCollaborationService(db)}
}

func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
    // Spaces、Issues、Squads 路由注册
}
```

在 `main.go` 中注册：

```go
collabModule := collaboration.New(db)
collabModule.RegisterRoutes(authed)
```

### 空间上下文中间件

通过 `X-Space-Slug` header 或 `?spaceSlug` query param 解析当前 Space，校验成员身份后注入 gin context：

```go
func (m *Module) spaceContextMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        slug := c.GetHeader("X-Space-Slug")
        if slug == "" {
            slug = c.Query("spaceSlug")
        }
        // 校验 space 存在性和成员身份
        // 注入 spaceID 和 memberRole 到 context
    }
}
```

### 权限中间件

| 中间件 | 权限要求 |
|--------|----------|
| `requireSpaceMember` | 只读（所有成员） |
| `requireSpaceAdmin` | 写操作（admin/owner） |
| `requireSpaceOwner` | 删除空间（owner） |

---

## API 设计

### Spaces

| 方法 | 路由 | 中间件 | 说明 |
|------|------|--------|------|
| GET | `/api/spaces` | auth | 列出我参与的空间 |
| POST | `/api/spaces` | auth | 创建空间（自动成为 owner） |
| GET | `/api/spaces/:slug` | auth | 获取空间详情 + 成员列表 |
| PUT | `/api/spaces/:slug` | requireSpaceAdmin | 更新空间 |
| DELETE | `/api/spaces/:slug` | requireSpaceOwner | 删除空间 |
| GET | `/api/spaces/:slug/members` | requireSpaceMember | 列出成员 |
| POST | `/api/spaces/:slug/members` | requireSpaceAdmin | 添加成员 |
| DELETE | `/api/spaces/:slug/members/:userId` | requireSpaceAdmin | 移除成员 |

### Issues

| 方法 | 路由 | 中间件 | 说明 |
|------|------|--------|------|
| GET | `/api/issues` | spaceContext + member | 列出事务（支持 status/priority/assigneeId 过滤） |
| POST | `/api/issues` | spaceContext + member | 创建事务 |
| GET | `/api/issues/:id` | spaceContext + member | 获取事务详情 + 评论 |
| PUT | `/api/issues/:id` | spaceContext + member | 更新事务 |
| DELETE | `/api/issues/:id` | spaceContext + member | 删除事务 |
| GET | `/api/issues/:id/comments` | spaceContext + member | 列出评论 |
| POST | `/api/issues/:id/comments` | spaceContext + member | 添加评论 |

### Squads

| 方法 | 路由 | 中间件 | 说明 |
|------|------|--------|------|
| GET | `/api/squads` | spaceContext + member | 列出小队（支持 includeArchived） |
| POST | `/api/squads` | spaceContext + member | 创建小队 |
| GET | `/api/squads/:id` | spaceContext + member | 获取小队详情 + 成员 |
| PUT | `/api/squads/:id` | spaceContext + member | 更新小队 |
| DELETE | `/api/squads/:id` | spaceContext + member | 删除小队 |
| GET | `/api/squads/:id/members` | spaceContext + member | 列出小队成员 |
| POST | `/api/squads/:id/members` | spaceContext + member | 添加小队成员 |
| DELETE | `/api/squads/:id/members/:userId` | spaceContext + member | 移除小队成员 |

---

## 前端模块设计

### 目录结构

```
src/pages/collaboration/
├── collaboration-layout.tsx       # 二级布局：侧边栏 + 内容区
├── collaboration-sidebar.tsx      # 二级侧边栏菜单
├── lib/
│   └── menu-registry.ts           # 菜单注册表（Issues/Projects/Squads）
└── pages/
    ├── issues-page.tsx            # 事务列表
    ├── projects-page.tsx          # 项目列表（按 Space 过滤）
    └── squads-page.tsx            # 小队列表

src/services/
└── collaboration.ts               # API 服务层
```

### 路由配置

```tsx
{
  path: "/collaboration",
  component: CollaborationLayout,
  auth: true,
  menu: "collaboration",
  children: [
    { path: "/issues", component: CollaborationIssuesPage },
    { path: "/projects", component: CollaborationProjectsPage },
    { path: "/squads", component: CollaborationSquadsPage },
    { path: "/", component: () => <Navigate href="/collaboration/issues" /> },
  ]
}
```

### API 服务层

所有请求自动附加 `X-Space-Slug` header，Space slug 从 `localStorage` 读取：

```ts
function headers() {
  return {
    "Content-Type": "application/json",
    "X-Space-Slug": localStorage.getItem("currentSpaceSlug") || "",
  }
}
```

### 左侧导航

在根布局的顶部导航区新增 `NavButton`：

```tsx
<Show when={auth.canAccessMenu("collaboration")}>
  <NavButton
    label={language.t("sidebar.collaboration")}
    active={isCollaboration()}
    onClick={() => navigate("/collaboration/issues")}
    node={<Users size={18} strokeWidth={1.75} />}
  />
</Show>
```

权限码 `"collaboration"` 由后端 `/api/auth/permissions` 的 `menus` 数组返回。

---

## 解耦策略

| 层面 | 解耦措施 |
|------|----------|
| **后端模块** | 新建 `internal/collaboration/`，不修改已有模块代码 |
| **后端路由** | 新增 `/api/spaces`、`/api/issues`、`/api/squads`，原有路由零变更 |
| **后端模型** | 新增独立表，现有表只增加可选字段（`Project.SpaceID`），不破坏已有数据 |
| **前端路由** | 新增 `/collaboration/*` 路由域，原有 `/workspace`、`/projects`、`/kanban` 路由不变 |
| **前端布局** | 新增 `CollaborationLayout`，不修改 `RootLayout`、`WorkspaceLayout`、`ConsoleLayout` |
| **前端页面** | 新增 `pages/collaboration/` 目录，所有页面从零实现，不依赖 Multica 的 React 组件 |
| **前端状态** | Space 上下文独立管理（`localStorage` 中的 `currentSpaceSlug`），不影响 device-workspace 上下文 |
| **权限** | 新增 `"collaboration"` 菜单码，原有权限体系不变 |

---

## 实施记录

### 已创建的文件

#### 后端

| 文件 | 说明 |
|------|------|
| `server/internal/collaboration/models.go` | Space、SpaceMember、Issue、IssueComment、Squad、SquadMember GORM 模型 |
| `server/internal/collaboration/dto.go` | 请求/响应 DTO 定义 |
| `server/internal/collaboration/service.go` | 业务服务层（CRUD + 权限校验） |
| `server/internal/collaboration/module.go` | 模块入口、路由注册、中间件 |
| `server/internal/collaboration/space_handler.go` | Space CRUD + 成员管理 handlers |
| `server/internal/collaboration/issue_handler.go` | Issue CRUD + 评论 handlers |
| `server/internal/collaboration/squad_handler.go` | Squad CRUD + 成员管理 handlers |
| `server/migrations/20260522100000_add_collaboration_spaces.sql` | spaces + space_members 表 |
| `server/migrations/20260522100100_add_collaboration_issues.sql` | issues + issue_comments 表 |
| `server/migrations/20260522100200_add_collaboration_squads.sql` | squads + squad_members 表 |
| `server/migrations/20260522100300_add_project_space_id.sql` | projects 表增加 space_id 字段 |

#### 前端

| 文件 | 说明 |
|------|------|
| `src/pages/collaboration/collaboration-layout.tsx` | 二级布局组件 |
| `src/pages/collaboration/collaboration-sidebar.tsx` | 二级侧边栏（Issues/Projects/Squads） |
| `src/pages/collaboration/lib/menu-registry.ts` | 菜单注册表 |
| `src/pages/collaboration/pages/issues-page.tsx` | 事务列表页 |
| `src/pages/collaboration/pages/projects-page.tsx` | 项目列表页 |
| `src/pages/collaboration/pages/squads-page.tsx` | 小队列表页 |
| `src/services/collaboration.ts` | API 服务层 |

#### 修改的文件

| 文件 | 修改内容 |
|------|----------|
| `server/cmd/api/main.go` | 导入并注册 collaboration 模块路由 |
| `server/internal/models/models.go` | Project 模型增加 `SpaceID *string` |
| `src/routes.tsx` | 新增 `/collaboration/*` 路由配置 |
| `src/pages/root-layout.tsx` | 左侧导航新增 Collaboration 入口 |
| `src/i18n/en.ts` | 新增 collaboration 英文翻译键值 |
| `src/i18n/zh.ts` | 新增 collaboration 中文翻译键值 |

### 编译验证

- `cd server && go build ./...` — 通过
- `cd packages/app-ai-native && pnpm tsc --noEmit` — 通过
