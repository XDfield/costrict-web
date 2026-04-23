> **实现状态：已实现**
>
> - 状态：✅ 已实现（2026/04/23）
> - 实现位置：
>   - `server/internal/systemrole/` — 角色服务与管理 API
>   - `server/internal/authz/` — 通用权限中心（新增）
>   - `server/internal/kanban/` — 指标看板示例模块（新增）
> - 数据模型：`UserSystemRole` 已在 `server/internal/models/models.go` 中定义，对应表 `user_system_roles` 已通过迁移创建
> - 前端动态菜单：已基于 `/api/auth/permissions` 接口实现权限驱动的 Console Sidebar 渲染
> - 说明：本提案用于为 Server 引入系统级角色能力，支持“平台管理员”和“业务管理成员”两类角色，并为后续全局后台权限治理提供基础。

---

# 系统级角色与管理权限技术提案

## 目录

- [概述](#概述)
- [设计目标](#设计目标)
- [角色定义](#角色定义)
- [设计原则](#设计原则)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [权限模型](#权限模型)
- [模块设计](#模块设计)
- [API 设计](#api-设计)
- [鉴权与中间件设计](#鉴权与中间件设计)
- [与 Casdoor 的职责边界](#与-casdoor-的职责边界)
- [安全与审计](#安全与审计)
- [实施计划](#实施计划)

---

## 概述

### 背景与动机

当前 Server 已基于 Casdoor 实现统一登录认证，并在本地维护 `users` 表用于承接业务用户实体。但系统仍缺少“系统级角色”能力，导致以下问题：

1. 某些后台接口虽然标注为“admin only”，实际上只校验了登录态，未真正校验管理员身份。
2. 缺少统一的“平台管理员”角色，无法对系统配置、全局资源、系统渠道等能力做严格控制。
3. 缺少“业务管理成员”角色，无法为领导、运营、分析人员提供全局看数但低风险的访问能力。
4. 缺少统一的系统级权限模型，未来扩展更多后台角色时成本较高。

因此，需要在 Server 侧引入一套独立于认证体系的“系统级授权模型”，用于承接平台内部后台治理与全局数据访问能力。

### 核心结论

本提案建议采用以下职责分离方案：

- **Casdoor**：负责用户认证、身份来源、基础资料同步
- **Server**：负责系统级角色、业务授权、接口鉴权、角色授予与审计

即：**Casdoor 管身份，Server 管授权。**

---

## 设计目标

本提案的目标如下：

1. 为 Server 引入两类系统级角色：
   - `platform_admin`：平台管理员
   - `business_admin`：业务管理成员
2. 支持平台管理员授予或撤销其他用户的系统级角色。
3. 为全局后台接口提供统一的系统角色校验中间件。
4. 让“平台管理员”天然拥有“业务管理成员”的全部能力。
5. 为“领导看数据”“全局报表”“业务态势分析”等场景提供可复用的角色能力模型。
6. 为后续扩展更多系统角色预留空间，而不是将权限逻辑散落在各业务模块中。

---

## 角色定义

### 1. 平台管理员 `platform_admin`

平台管理员是系统内部的最高权限角色，负责平台治理、系统配置管理和系统级角色管理。

典型能力包括：

- 管理系统级角色（授予 / 撤销）
- 管理系统配置与高风险后台操作
- 管理系统通知渠道、全局策略等平台级资源
- 查看所有业务看板和全局统计数据
- 访问需最高权限的后台管理接口

### 2. 业务管理成员 `business_admin`

业务管理成员主要面向领导、运营、分析等“看数”场景，重点是全局只读或轻量管理能力，不承担平台治理职责。

典型能力包括：

- 查看全局业务数据
- 查看经营分析、趋势报表、运营看板
- 访问部分业务后台只读能力
- 执行低风险的业务管理动作（若后续确有需要）

明确不包含的能力：

- 不可授予或撤销系统角色
- 不可修改高风险系统配置
- 不可执行平台级安全治理操作
- 不可执行高危删除类或基础设施控制类操作

### 角色继承关系

采用简单继承模型：

```text
platform_admin  >  business_admin
```

也即：

- `platform_admin` 自动拥有 `business_admin` 的全部权限能力
- `business_admin` 不反向拥有 `platform_admin` 权限

---

## 设计原则

### 1. 授权在 Server，本地生效

系统级角色作为平台内部业务授权，应由 Server 本地持有与判断，而不是依赖 Casdoor Token Claim 做最终判定。

### 2. 角色与能力分离

代码中不应在各处直接写死：

- `role == "platform_admin"`
- `role == "business_admin"`

更推荐抽象成“能力”，如：

- `CanManageSystemRoles`
- `CanManageSystemSettings`
- `CanViewGlobalBusinessData`
- `CanAccessBusinessDashboard`

角色只负责映射能力，避免后续权限逻辑失控。

### 3. 高权限操作必须可审计

角色授予、撤销、平台级配置修改等操作需要保留操作者与时间信息，便于追溯。

### 4. 默认最小权限

对“业务管理成员”默认采用“可看、少改、不可授权”的设计，避免将领导看数角色演变成高危运维角色。

---

## 架构设计

### 整体定位

```text
Casdoor
  │
  │  OAuth / OIDC 认证
  ▼
RequireAuth Middleware
  │
  │  注入 userId / accessToken
  ▼
Authz Middleware (通用权限中心)
  │
  ├── RequirePermission("admin.system-roles")
  ├── RequirePermission("admin.notification-channels")
  └── RequirePermission("api.kanban.overview")
  ▼
SystemRole Service (底层角色查询)
  ▼
业务模块 Handler / Service
```

### 目录结构

```text
server/internal/systemrole/     # 角色服务与管理 API
├── systemrole.go
├── service.go
├── middleware.go               # 保留的硬编码角色中间件（已逐步迁移至 authz）
├── types.go                    # 角色常量、能力常量、CapabilityProvider
└── handlers.go

server/internal/authz/          # 通用权限中心（新增）
├── authz.go                    # 模块初始化、路由注册
├── registry.go                 # 菜单/API 资源权限注册表
├── service.go                  # 权限计算、token 远程校验
├── middleware.go               # RequirePermission / RequireAnyPermission
└── handlers.go                 # /api/auth/permissions + /internal/auth/verify

server/internal/kanban/         # 指标看板示例模块（新增）
├── kanban.go
└── handlers.go
```

### 与现有模块的关系

- 复用 `server/internal/middleware/auth.go` 中已有的登录态解析
- 复用 `server/internal/user/` 中本地用户实体与用户查询能力
- 为 `notification`、统计分析、后台管理等模块提供统一角色校验入口

---

## 数据模型

## 方案选择

由于当前已经明确存在两类系统级角色，且未来大概率继续扩展，因此不建议只在 `users` 表上添加多个布尔字段。

推荐新增独立角色表：`user_system_roles`。

### 表结构定义

```sql
CREATE TABLE user_system_roles (
    id VARCHAR(36) NOT NULL PRIMARY KEY,
    user_id VARCHAR(191) NOT NULL COMMENT '关联 users.id',
    role VARCHAR(64) NOT NULL COMMENT 'system role: platform_admin | business_admin',
    granted_by VARCHAR(191) NULL COMMENT '授予者 user_id',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL,

    UNIQUE KEY uk_user_system_role (user_id, role),
    KEY idx_user_system_roles_user_id (user_id),
    KEY idx_user_system_roles_role (role),
    KEY idx_user_system_roles_granted_by (granted_by)
);
```

### GORM 模型定义

```go
type UserSystemRole struct {
    ID        string         `gorm:"primaryKey;size:36" json:"id"`
    UserID    string         `gorm:"uniqueIndex:uk_user_system_role,priority:1;index;not null;size:191" json:"user_id"`
    Role      string         `gorm:"uniqueIndex:uk_user_system_role,priority:2;index;not null;size:64" json:"role"`
    GrantedBy *string        `gorm:"index;size:191" json:"granted_by"`
    CreatedAt time.Time      `json:"created_at"`
    UpdatedAt time.Time      `json:"updated_at"`
    DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (UserSystemRole) TableName() string {
    return "user_system_roles"
}
```

### 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 主键，UUID |
| `user_id` | string | 用户 ID，关联本地 `users.id` |
| `role` | string | 系统角色名 |
| `granted_by` | string \/ null | 授予者 ID |
| `created_at` | timestamp | 角色授予时间 |
| `updated_at` | timestamp | 更新时间 |
| `deleted_at` | timestamp | 软删除时间 |

### 角色枚举约束

当前仅允许以下值：

- `platform_admin`
- `business_admin`

后续若扩展更多系统角色，应通过常量与白名单统一维护。

---

## 权限模型

### 能力抽象

建议抽象以下能力：

| 能力 | 说明 |
|------|------|
| `CanManageSystemRoles` | 管理系统级角色 |
| `CanManageSystemSettings` | 管理系统配置 |
| `CanManageSystemChannels` | 管理系统通知渠道 |
| `CanViewGlobalBusinessData` | 查看全局业务数据 |
| `CanAccessBusinessDashboard` | 访问经营看板/统计后台 |

### 角色到能力映射

#### `platform_admin`

- `CanManageSystemRoles = true`
- `CanManageSystemSettings = true`
- `CanManageSystemChannels = true`
- `CanViewGlobalBusinessData = true`
- `CanAccessBusinessDashboard = true`

#### `business_admin`

- `CanManageSystemRoles = false`
- `CanManageSystemSettings = false`
- `CanManageSystemChannels = false`（如需只读能力，可后续细化）
- `CanViewGlobalBusinessData = true`
- `CanAccessBusinessDashboard = true`

### 权限边界建议

#### 平台管理员可访问的能力

- 系统通知渠道增删改查
- 系统级配置修改
- 系统角色管理
- 全局后台管理入口
- 所有业务数据与统计分析能力

#### 业务管理成员可访问的能力

- 全局经营分析看板
- 业务统计报表
- 只读型后台数据查询接口
- 有明确低风险边界的轻量运营功能

#### 业务管理成员默认不可访问

- 系统角色授予/撤销
- 系统通知渠道管理
- 高危删除接口
- 平台配置修改
- 安全相关后台能力

---

## 模块设计

### 角色常量

```go
const (
    SystemRolePlatformAdmin = "platform_admin"
    SystemRoleBusinessAdmin = "business_admin"
)
```

### SystemRoleService

已实现的核心方法：

```go
type SystemRoleService struct {
    db *gorm.DB
}

func (s *SystemRoleService) ListRoles(userID string) ([]string, error)
func (s *SystemRoleService) GetExpandedRoles(userID string) ([]string, error)
func (s *SystemRoleService) GetCapabilities(userID string) ([]string, error)
func (s *SystemRoleService) HasRole(userID, role string) (bool, error)
func (s *SystemRoleService) HasAnyRole(userID string, roles ...string) (bool, error)
func (s *SystemRoleService) GrantRole(userID, role, operatorID string) error
func (s *SystemRoleService) RevokeRole(userID, role, operatorID string) error
func (s *SystemRoleService) ListUsersByRole(role string) ([]models.User, error)
```

### Authz Service（通用权限中心）

新增 `server/internal/authz/service.go`，提供基于资源码（resource code）的通用权限判定：

```go
type Service struct {
    db                 *gorm.DB
    roleProvider       RoleProvider
    capabilityProvider CapabilityProvider
    casdoorEndpoint    string
    jwksProvider       *middleware.JWKSProvider
}

func (s *Service) GetUserPermissions(userID string) (*PermissionResult, error)
func (s *Service) HasPermission(userID, resourceCode string) (bool, error)
func (s *Service) VerifyToken(token, resourceCode string) (bool, *PermissionResult, error)
```

其中 `PermissionResult` 结构：

```go
type PermissionResult struct {
    Menus        []string `json:"menus"`
    APIs         []string `json:"apis"`
    Capabilities []string `json:"capabilities"`
}
```

### 权限注册表

`server/internal/authz/registry.go` 中维护统一的资源-角色映射，新增菜单或 API 时只需在此注册：

```go
var MenuResources = ResourceRegistry{
    "console.capabilities":    {RolePlatformAdmin},
    "console.devices":         {},                          // 所有登录用户
    "console.kanban":          {RoleBusinessAdmin, RolePlatformAdmin},
}

var APIResources = ResourceRegistry{
    "admin.system-roles":          {RolePlatformAdmin},
    "admin.notification-channels": {RolePlatformAdmin},
    "api.kanban.overview":         {RoleBusinessAdmin, RolePlatformAdmin},
}
```

### 关键业务规则

#### 1. 只有平台管理员可以授予或撤销系统角色

角色管理属于平台治理能力，不应开放给 `business_admin`。

#### 2. 不允许删除最后一个平台管理员

为避免系统进入“无最高管理员”状态，撤销 `platform_admin` 时需检查：

- 当前是否仅剩最后一个 `platform_admin`
- 若是，则拒绝撤销

#### 3. 平台管理员天然兼容业务管理成员能力

在中间件与能力判断中，`platform_admin` 应自动满足 `business_admin` 的访问要求。

#### 4. 授予角色前应校验用户存在

被授予系统角色的用户必须已在本地 `users` 表存在。若不存在，应提示用户先完成登录或由系统先同步用户数据。

---

## API 设计

### 系统角色管理接口（要求 `platform_admin`）

#### 1. 查询用户系统角色

`GET /admin/system-roles/users/:userId`

响应示例：

```json
{
  "userId": "u_123",
  "roles": ["business_admin"]
}
```

#### 2. 授予系统角色

`POST /admin/system-roles/users/:userId`

请求体：

```json
{
  "role": "platform_admin"
}
```

响应示例：

```json
{
  "success": true
}
```

#### 3. 撤销系统角色

`DELETE /admin/system-roles/users/:userId/:role`

响应示例：

```json
{
  "success": true
}
```

#### 4. 按角色查询成员列表

`GET /admin/system-roles?role=business_admin`

响应示例：

```json
{
  "role": "business_admin",
  "users": [
    {
      "id": "u_1",
      "username": "alice",
      "display_name": "Alice"
    }
  ]
}
```

#### 5. 查询当前用户系统角色

`GET /auth/system-roles/me`

响应示例：

```json
{
  "userId": "u_1",
  "roles": ["platform_admin"],
  "capabilities": [
    "CanManageSystemRoles",
    "CanManageSystemSettings",
    "CanManageSystemChannels",
    "CanViewGlobalBusinessData",
    "CanAccessBusinessDashboard"
  ]
}
```

---

### 通用权限接口（新增）

#### 6. 查询当前用户完整权限快照

`GET /api/auth/permissions`

用途：

- 前端首次加载时获取当前用户可见的菜单列表、可访问的 API 列表和能力列表
- 驱动前端动态菜单渲染，无权限入口自动隐去
- 替代前端直接调用 `/auth/system-roles/me` 做硬编码角色判断

响应示例：

```json
{
  "menus": [
    "console.capabilities",
    "console.devices",
    "console.kanban"
  ],
  "apis": [
    "api.kanban.overview"
  ],
  "capabilities": [
    "CanViewGlobalBusinessData",
    "CanAccessBusinessDashboard"
  ]
}
```

#### 7. 网关/内部服务 Token 校验

`POST /internal/auth/verify`

保护方式：`InternalAuth`（`X-Internal-Secret`）

请求体：

```json
{
  "token": "Bearer eyJ...",
  "resource": "api.kanban.overview"
}
```

响应示例：

```json
{
  "allowed": true,
  "menus": ["console.devices", "console.kanban"],
  "capabilities": ["CanViewGlobalBusinessData"]
}
```

用途：

- 网关层（gateway）或其他内部服务可通过此接口校验任意 token 对指定资源的访问权限
- 网关如需拦截某次请求，可提取用户 token 调用此接口，根据 `allowed` 决定是否放行或返回 403
- 当前 `gateway/` 服务暂不直接面向浏览器用户，但已预留标准化内部验证能力供后续扩展

---

## 鉴权与中间件设计

### 已实现的中间件

#### 1. RequirePermission（通用权限中间件，推荐优先使用）

位于 `server/internal/authz/middleware.go`，基于资源码（resource code）做权限判定：

```go
func RequirePermission(svc *authz.Service, resourceCode string) gin.HandlerFunc
func RequireAnyPermission(svc *authz.Service, resourceCodes ...string) gin.HandlerFunc
```

执行流程：

1. 从上下文获取 `userId`
2. 若为空，返回 `401 Unauthorized`
3. 调用 `authz.Service.HasPermission(userID, resourceCode)`
4. 根据 `authz/registry.go` 中的资源-角色映射判断是否允许
5. 若无权限，返回 `403 Forbidden`
6. 校验通过后进入下游 Handler

示例用法（Kanban 模块）：

```go
kanban := apiGroup.Group("/kanban")
kanban.Use(authz.RequirePermission(authzSvc, "api.kanban.overview"))
{
    kanban.GET("/overview", GetOverviewHandler())
}
```

#### 2. RequirePlatformAdmin（保留，逐步迁移）

位于 `server/internal/systemrole/middleware.go`，硬编码校验 `platform_admin` 角色。

当前 `/admin/system-roles` 和 `/admin/notification-channels` 仍使用此中间件，后续可逐步迁移至 `RequirePermission`。

#### 3. RequireBusinessAdminOrAbove（保留，逐步迁移）

位于 `server/internal/systemrole/middleware.go`，硬编码校验 `business_admin` 或 `platform_admin` 角色。

当前尚未在任何路由上使用，建议新路由直接采用 `RequirePermission`。

### 对现有代码的改造建议

当前部分接口虽然文档标注为“admin only”，但仅校验是否登录，例如：

- `server/internal/notification/handlers.go` 下的 `/admin/notification-channels`

这些接口已使用 `RequirePlatformAdmin` 保护，后续新增后台接口应优先使用 `RequirePermission("admin.xxx")`，避免硬编码角色名。

---

## 前端动态菜单渲染

### 权限感知初始化

前端 `AuthProvider`（`portal/packages/app-ai-native/src/context/auth.tsx`）在 `onMount` 时执行两步加载：

1. `GET /api/auth/me` — 获取用户基本信息
2. `GET /api/auth/permissions` — 获取完整权限快照

权限快照结构：

```ts
interface UserPermissions {
  menus: string[]        // 可见菜单 code 列表
  apis: string[]         // 可访问 API code 列表
  capabilities: string[] // 能力列表
}
```

### 动态菜单过滤

Console Sidebar（`console-sidebar.tsx`）不再使用硬编码 `NAV` 数组，而是：

1. 定义完整菜单注册表 `ALL_CONSOLE_MENUS`（`console/lib/menu-registry.ts`），每项包含 `code`、`href`、`labelKey`、`icon`
2. 使用 `auth.canAccessMenu(code)` 过滤，只渲染 `permissions.menus` 中包含的项
3. 无权限的入口自动隐去

示例：

```ts
const ALL_CONSOLE_MENUS = [
  { code: "console.capabilities", href: "/console/capabilities", labelKey: "...", icon: "sparkles" },
  { code: "console.devices",      href: "/console/devices",      labelKey: "...", icon: "server" },
  { code: "console.kanban",       href: "/console/kanban",       labelKey: "...", icon: "chart" },
]

const visibleMenus = () => ALL_CONSOLE_MENUS.filter(item => auth.canAccessMenu(item.code))
```

### 好处

- 新增菜单只需在 `registry.go`（后端）和 `menu-registry.ts`（前端）各注册一行
- 前端无需硬编码角色判断逻辑
- 菜单可见性完全由后端权限接口驱动

---

## 网关层权限拦截

### 现状

`gateway/` 服务当前只做设备隧道和反向代理，**不做用户权限校验**。

### 已预留能力

通过 `POST /internal/auth/verify` 接口，网关或任何内部服务都可以远程校验 token 权限：

```text
Gateway 收到请求
  │
  ├── 提取用户 token（Bearer Token）
  ├── 调用 POST /internal/auth/verify
  │     { "token": "Bearer eyJ...", "resource": "api.xxx.yyy" }
  ├── 根据返回的 "allowed" 决定是否放行
  └── 若 allowed=false，直接返回 403，不转发到后端
```

### 接入方式

如需在 `gateway/internal/router.go` 中集成，可在代理链路中加入：

```go
// 伪代码示例
func PermissionCheckMiddleware(authzURL, internalSecret string) gin.HandlerFunc {
    return func(c *gin.Context) {
        token := ExtractToken(c.Request)
        allowed, err := VerifyViaInternalAPI(authzURL, internalSecret, token, "api.target.resource")
        if err != nil || !allowed {
            c.AbortWithStatusJSON(403, gin.H{"error": "Permission denied"})
            return
        }
        c.Next()
    }
}
```

---

## 与 Casdoor 的职责边界

### Casdoor 负责

- 登录认证
- OAuth / OIDC Token 签发
- 用户基础身份与用户资料来源
- 用户搜索与同步上游数据

### Server 负责

- 系统级角色存储
- 系统角色管理 API
- 权限判断与中间件控制
- 业务接口访问控制
- 审计与风控规则

### 为什么不直接把系统角色完全放到 Casdoor

原因如下：

1. 系统级角色本质上是本平台内部授权，而不是单纯身份信息。
2. 若依赖 Casdoor Claim，会面临 Token 刷新与权限变更延迟问题。
3. Server 侧接口权限需要本地快速判断，不适合每次依赖外部角色查询。
4. 角色授予、撤销、审计等业务规则在本地更容易控制。

### 可选扩展

若未来希望多个系统共享平台管理员角色，可考虑将 Casdoor 作为“上游角色源”，但 Server 仍应保留本地角色落地与最终判定机制。

---

## 安全与审计

### 安全要求

1. 所有系统角色管理接口必须要求 `platform_admin`
2. 角色授予/撤销前应校验目标用户存在
3. 禁止撤销最后一个 `platform_admin`
4. 高权限接口返回错误时避免暴露过多内部细节

### 审计建议

当前最小可行审计信息：

- `granted_by`
- `created_at`
- `updated_at`

后续可扩展独立审计日志表，记录：

- 操作者 ID
- 目标用户 ID
- 操作类型（grant / revoke）
- 角色名
- 操作时间
- 请求来源 IP / trace 信息

---

## 实施计划

### 第一阶段：数据层与服务层 ✅ 已完成

1. ~~创建迁移：新增 `user_system_roles` 表~~（已存在：`20260406100000_create_user_system_roles_table.sql`）
2. ~~在 `server/internal/models/models.go` 中添加 `UserSystemRole`~~ ✅
3. ~~新增 `server/internal/systemrole/service.go`~~ ✅
4. ~~实现角色查询、授予、撤销与列表能力~~ ✅

### 第二阶段：鉴权中间件接入 ✅ 已完成

1. ~~实现 `RequirePlatformAdmin`~~ ✅
2. ~~实现 `RequireBusinessAdminOrAbove`~~ ✅
3. ~~接入现有“admin only”接口~~ ✅（`/admin/notification-channels` 已接入）
4. ~~修正原有仅登录校验的伪管理员接口~~ ✅

### 第三阶段：通用权限中心 + 前端动态菜单 ✅ 已完成

1. ~~新增 `server/internal/authz/` 通用权限中心~~ ✅
   - `registry.go` — 资源-角色映射表
   - `service.go` — 权限计算 + token 远程校验
   - `middleware.go` — `RequirePermission` 通用中间件
   - `handlers.go` — `/api/auth/permissions` + `/internal/auth/verify`
2. ~~前端 `AuthContext` 调用 `/api/auth/permissions`~~ ✅
3. ~~Console Sidebar 基于 `permissions.menus` 动态渲染~~ ✅
4. ~~新增 `/console/kanban` 路由与页面~~ ✅

### 第四阶段：业务看板接入 ✅ 已完成

1. ~~新增 `server/internal/kanban/` 指标看板模块~~ ✅
2. ~~`GET /api/kanban/overview` 受 `RequirePermission("api.kanban.overview")` 保护~~ ✅
3. ~~前端 Kanban 页面调用受保护 API 展示占位数据~~ ✅
4. 后续：填充真实统计数据（项目数、用户数、设备数、请求量等）

---

## 总结

本提案通过在 Server 侧引入 `platform_admin` 与 `business_admin` 两类系统级角色，建立了清晰的“认证与授权分离”架构：

- Casdoor 负责认证
- Server 负责系统级授权

其中：

- `platform_admin` 面向平台治理与高权限后台能力
- `business_admin` 面向领导看数与业务分析场景

该设计既满足当前需求，也为后续扩展更多系统角色、能力矩阵和审计治理打下基础。
