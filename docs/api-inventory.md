# costrict-web 对外 API 清单（收敛盘点用）

> 生成日期：2026-08-04
> 数据来源：
> - `server/docs/swagger.json`（192 个端点，basePath `/api`，host `localhost:8080`）
> - `server/cmd/api/main.go` 路由直接注册（部分未在 swagger 中体现）
> - `server/internal/<module>/*.go` 各域 Module.RegisterRoutes（**全部未进 swagger**）
> - `gateway/internal/router.go`（5 个端点，无 swagger）
>
> **关键提醒**：
> 1. `swagger.json` 是 **stale** 的——很多模块新增路由（cloud / memory / channel / kanban / clawagent / authz / enterprise / settings / audit / adminuser / deptsync / adminitem / adminimport / project 等）**未生成注解**，不在 swagger 里。注：`systemrole` 和 `notification` 主体已在 swagger（仅 `/admin/announcements` 缺）。
> 2. swagger 中的 `/team/*` 共 **17 个路径 / 24 个操作**属于**已废弃**模块（`internal/team/module.go` 的 `RegisterRoutes` 是空函数），代码里实际没有注册，可以一并收敛掉。
> 3. 网关层（`gateway/`）独立部署，无 swagger，路由见 `gateway/internal/router.go`。

---

## 一、Server（Go + Gin）

按暴露面/认证方式分组。每条均含 BasePath `/api`（除非额外标注）。

### 1. Public / 匿名（OptionalAuth，无登录可读）

| 方法 | 路径 | 来源 | 说明 |
|---|---|---|---|
| GET | `/health` | main.go:472 | 健康检查（root，**非 /api**） |
| GET | `/swagger/*any` | main.go:482 | Swagger UI（仅 dev/debug） |
| GET | `/api/auth/callback` | main.go:532 | OAuth 回调 |
| GET | `/api/auth/login` | main.go:533 | 登录入口 |
| GET | `/api/auth/bind/callback` | main.go:536 | 第三方账号绑定回调 |
| GET | `/api/updates/check` | main.go:540 | 更新检查 |
| GET | `/api/multica/updates/check` | main.go:541 | Multica 更新检查 |
| GET | `/api/registries/public` | main.go:542 | 公共 registry 列表 |
| GET | `/api/registries/:id` | main.go:543 | registry 详情 |
| GET | `/api/registries/:id/items` | main.go:544 | registry 下 items |
| GET | `/api/registry/:repo/access` | main.go:545 | registry 访问权限元信息 |
| GET | `/api/registry/:repo/index.json` | main.go:546 | registry 索引 |
| GET | `/api/registry/:repo/:itemType/:slug/*file` | main.go:547 | registry 文件下载（csc 客户端用） |
| GET | `/api/plugins/:slug/download` | main.go:548 | Plugin zip 下载 |
| GET | `/api/marketplace/:repo/marketplace.json` | main.go:549 | Marketplace 索引 |
| GET | `/api/tenants/suggest` | main.go:556 | 租户建议（登录前 picker 用） |
| GET | `/api/items` | main.go:561 | Items 列表（公开可见性） |
| GET | `/api/items/:id` | main.go:562 | Item 详情 |
| GET | `/api/items/:id/assets` | main.go:563 | Item 资源 |
| GET | `/api/items/:id/versions` | main.go:564 | Item 版本 |
| GET | `/api/items/:id/versions/:version` | main.go:565 | Item 指定版本 |
| GET | `/api/items/:id/artifacts` | main.go:566 | Item artifacts |
| GET | `/api/items/:id/download` | main.go:567 | Item 下载 |
| GET | `/api/items/:id/scan-status` | main.go:568 | Item 安全扫描状态 |
| GET | `/api/items/:id/scan-results` | main.go:569 | Item 扫描结果 |
| GET | `/api/scan-results/:id` | main.go:570 | 单次扫描结果 |
| GET | `/api/artifacts/:id/download` | main.go:571 | Artifact 下载 |
| GET | `/api/users/names` | main.go:574 | 用户名查询 |
| GET | `/api/users/info` | main.go:575 | 用户基本信息 |
| GET | `/api/categories` | main.go:578 | 分类列表 |
| GET | `/api/categories/:id` | main.go:579 | 分类详情 |
| GET | `/api/tags` | main.go:582 | 标签列表 |
| GET | `/api/tags/:id` | main.go:583 | 标签详情 |
| GET | `/api/items/:id/similar` | main.go:594 | 相似 items |
| GET | `/api/items/filter-options` | main.go:595 | 过滤选项 |
| GET | `/api/items/:id/stats` | main.go:597 | Item 统计 |
| GET | `/api/plugins/builtin` | main.go:670 | 内置 plugin 列表 |
| POST | `/api/marketplace/items/search` | main.go:588 | 语义搜索 |
| POST | `/api/marketplace/items/hybrid-search` | main.go:589 | 混合搜索 |
| POST | `/api/marketplace/items/recommend` | main.go:590 | 推荐 |
| GET | `/api/marketplace/items/trending` | main.go:591 | 热门 |
| GET | `/api/marketplace/items/new` | main.go:592 | 新品 |
| GET | `/api/webhooks/channels/:type` | channel/channel.go:19 | Channel webhook 入口（GET） |
| POST | `/api/webhooks/channels/:type` | channel/channel.go:20 | Channel webhook 入口（POST） |

### 2. Authenticated user（RequireAuth）

#### 2.1 认证 / 当前用户

| 方法 | 路径 | 来源 | 说明 |
|---|---|---|---|
| GET | `/auth/resolve` | main.go:534 | 解析当前登录态 |
| GET | `/auth/me` | main.go:608 | 当前用户 |
| GET | `/me` | main.go:609 | 当前用户简表 |
| GET | `/auth/identities` | main.go:610 | 已绑定第三方身份 |
| POST | `/auth/identities/:provider/unbind` | main.go:614 | 解绑第三方 |
| POST | `/auth/bind/start` | main.go:611 | 发起绑定 |
| POST | `/auth/bind/confirm-merge` | main.go:612 | 确认账号合并 |
| POST | `/auth/bind/cancel-merge` | main.go:613 | 取消合并 |
| POST | `/auth/logout` | main.go:535 | 注销 |
| GET | `/me/username-available` | main.go:621 | 用户名可用性（profile-complete gate） |
| POST | `/me/complete-registration` | main.go:622 | 完成注册（profile-complete gate） |
| PATCH | `/me/profile` | main.go:623 | 更新资料（profile-complete gate） |
| POST | `/me/suggest-profile` | main.go:624 | 资料建议 |
| GET | `/auth/system-roles/me` | systemrole/systemrole.go:29 | 我的系统角色 |
| GET | `/users/search` | main.go:798 | 用户搜索 |
| GET | `/users/me/behavior/summary` | main.go:799 | 行为摘要 |

#### 2.2 Items / Plugins / Categories / Tags（创作 & 治理）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/items/my` | main.go:667 |
| POST | `/items` | main.go:668 |
| POST | `/plugins/upload` | main.go:669 |
| PUT | `/items/:id` | main.go:671 |
| POST | `/items/:id/check-consistency` | main.go:672 |
| POST | `/items/:id/fork` | main.go:673 |
| DELETE | `/items` | main.go:674（批量删除） |
| DELETE | `/items/:id` | main.go:675 |
| PUT | `/items/:id/move` | main.go:676 |
| PUT | `/items/:id/transfer` | main.go:677 |
| POST | `/items/:id/distribute` | main.go:678 |
| GET | `/items/:id/distributions` | main.go:679 |
| POST | `/items/:id/scan` | main.go:680 |
| POST | `/scan-jobs/:id/cancel` | main.go:681 |
| POST | `/items/:id/favorite` | main.go:682 |
| DELETE | `/items/:id/favorite` | main.go:683 |
| PUT | `/items/:id/mcp-config` | main.go:684 |
| POST | `/items/:id/analyze` | swagger |
| POST | `/items/:id/behavior` | swagger |
| POST | `/items/:id/improve` | swagger |
| PUT | `/items/:id/move` | swagger（重复） |
| PUT | `/items/:id/transfer` | swagger（重复） |
| POST | `/items/:id/tags` | main.go:702 |
| POST | `/categories` | main.go:699 |
| PUT | `/categories/:id` | main.go:700 |
| DELETE | `/categories/:id` | main.go:701 |
| POST | `/artifacts/upload` | main.go:696 |
| DELETE | `/artifacts/:id` | main.go:697 |

#### 2.3 Distributions（分发链路）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/distributions/my/sent` | main.go:687 |
| GET | `/distributions/my/received` | main.go:688 |
| GET | `/distributions/my/authority` | main.go:689 |
| GET | `/distributions/eligible-users` | main.go:690 |
| PUT | `/distributions/:id` | main.go:691 |
| DELETE | `/distributions/:id` | main.go:692 |
| POST | `/distributions/:id/dismiss` | main.go:693 |
| POST | `/distributions/:id/read` | main.go:694 |

#### 2.4 Repositories / Registries / Sync

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/repositories` | main.go:629 |
| GET | `/repositories/my` | main.go:630 |
| POST | `/repositories` | main.go:631 |
| GET | `/repositories/:id` | main.go:632 |
| PUT | `/repositories/:id` | main.go:633 |
| DELETE | `/repositories/:id` | main.go:634 |
| GET | `/repositories/:id/members` | main.go:635 |
| POST | `/repositories/:id/members` | main.go:636 |
| PUT | `/repositories/:id/members/:userId` | main.go:637 |
| DELETE | `/repositories/:id/members/:userId` | main.go:638 |
| GET | `/repositories/:id/invitations` | main.go:639 |
| POST | `/repositories/:id/invitations` | main.go:640 |
| DELETE | `/repositories/:id/invitations/:invId` | main.go:641 |
| GET | `/repositories/:id/registry` | main.go:642 |
| GET | `/repositories/:id/registries` | main.go:643 |
| POST | `/repositories/:id/registries` | main.go:644 |
| PUT | `/repositories/:id/registries/:regId` | main.go:645 |
| DELETE | `/repositories/:id/registries/:regId` | main.go:646 |
| POST | `/repositories/:id/sync` | main.go:647 |
| POST | `/repositories/:id/sync/cancel` | main.go:648 |
| GET | `/repositories/:id/sync-status` | main.go:649 |
| GET | `/repositories/:id/sync-logs` | main.go:650 |
| GET | `/repositories/:id/sync-jobs` | main.go:651 |
| GET | `/registries` | main.go:654 |
| GET | `/registries/my` | main.go:655 |
| POST | `/registries` | main.go:656 |
| PUT | `/registries/:id` | main.go:657 |
| PUT | `/registries/:id/transfer` | main.go:658 |
| DELETE | `/registries/:id` | main.go:659 |
| POST | `/registries/:id/items` | main.go:660 |
| POST | `/registries/:id/sync` | main.go:661 |
| POST | `/registries/:id/sync/cancel` | main.go:662 |
| GET | `/registries/:id/sync-status` | main.go:663 |
| GET | `/registries/:id/sync-logs` | main.go:664 |
| GET | `/registries/:id/sync-jobs` | main.go:665 |
| GET | `/sync-logs/:id` | main.go:805 |
| GET | `/sync-jobs/:id` | main.go:806 |

#### 2.5 Projects / Invitations（项目空间）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/projects` | project/project.go:22 |
| POST | `/projects` | project/project.go:23 |
| GET | `/projects/:id` | project/project.go:24 |
| GET | `/projects/:id/basic` | project/project.go:25 |
| PUT | `/projects/:id` | project/project.go:26 |
| PUT | `/projects/:id/pin` | project/project.go:27 |
| PUT | `/projects/:id/archive-time` | project/project.go:28 |
| DELETE | `/projects/:id` | project/project.go:29 |
| POST | `/projects/:id/archive` | project/project.go:30 |
| POST | `/projects/:id/unarchive` | project/project.go:31 |
| GET | `/projects/:id/members` | project/project.go:32 |
| DELETE | `/projects/:id/members/:userId` | project/project.go:33 |
| PUT | `/projects/:id/members/:userId/role` | project/project.go:34 |
| POST | `/projects/:id/invitations` | project/project.go:35 |
| GET | `/projects/:id/invitations` | project/project.go:36 |
| GET | `/projects/:id/repositories` | project/project.go:37 |
| POST | `/projects/:id/repositories` | project/project.go:38 |
| DELETE | `/projects/:id/repositories/:repoBindingId` | project/project.go:39 |
| GET | `/invitations` | project/project.go:44（**未在 swagger**） |
| GET | `/invitations/my` | main.go:801 |
| POST | `/invitations/:id/accept` | main.go:802 |
| POST | `/invitations/:id/decline` | main.go:803 |
| POST | `/invitations/:id/respond` | project/project.go:45 |
| DELETE | `/invitations/:id` | project/project.go:46 |

#### 2.6 Devices / Workspaces

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/devices/register` | main.go:813 |
| GET | `/devices` | main.go:814 |
| GET | `/devices/:deviceID` | main.go:815 |
| PUT | `/devices/:deviceID` | main.go:816 |
| DELETE | `/devices/:deviceID` | main.go:817 |
| POST | `/devices/:deviceID/token/rotate` | main.go:818 |
| PUT | `/devices/:deviceID/fingerprint` | main.go:819 |
| POST | `/cloud/device/gateway-assign` | main.go:989 (root, **非 /api**) |
| GET | `/workspaces` | main.go:826 |
| POST | `/workspaces` | main.go:825 |
| GET | `/workspaces/default` | main.go:828 |
| GET | `/workspaces/:workspaceID` | main.go:829 |
| PUT | `/workspaces/:workspaceID` | main.go:830 |
| DELETE | `/workspaces/:workspaceID` | main.go:831 |
| POST | `/workspaces/:workspaceID/set-default` | main.go:832 |
| POST | `/workspaces/:workspaceID/directories` | main.go:833 |
| POST | `/workspaces/:workspaceID/directories/reorder` | main.go:834 |
| PUT | `/workspaces/:workspaceID/directories/:directoryID` | main.go:835 |
| DELETE | `/workspaces/:workspaceID/directories/:directoryID` | main.go:836 |
| GET | `/workspaces/:workspaceID/devices` | main.go:822 |

#### 2.7 Cloud（设备事件 / SSE / 命令）

> 全部 **未在 swagger**。`r.Group("/cloud")` + RequireAuth（device 通知接口除外）。

| 方法 | 路径 | 来源 | 说明 |
|---|---|---|---|
| GET | `/cloud/workspace/:workspaceID/event` | cloud/cloud.go:30 | 用户 SSE |
| POST | `/cloud/session/:sessionID/subscribe` | cloud/cloud.go:31 | 订阅 session |
| POST | `/cloud/session/:sessionID/unsubscribe` | cloud/cloud.go:32 | 取消订阅 |
| POST | `/cloud/event` | cloud/cloud.go:33 | 设备上报事件 |
| POST | `/cloud/command` | cloud/cloud.go:34 | 用户下发命令 |
| GET | `/cloud/stats` | cloud/cloud.go:35 | 连接统计 |
| POST | `/cloud/device/notify` | cloud/cloud.go:41 | 设备→用户通知（device auth） |
| POST | `/cloud/device/notify/responded` | cloud/cloud.go:42 | 通知已响应（device auth） |
| POST | `/cloud/devices/:deviceID/commands/:commandID/result` | cloud/cloud.go:43 | 命令结果回传 |
| Any | `/cloud/device/:deviceID/proxy/*path` | main.go:1126 | 设备 HTTP 代理 |
| Any | `/cloud/sessions/:sessionID/proxy/*path` | main.go:1130 | 会话 HTTP 代理（Multica 解析） |

#### 2.8 ClawAgent（个人 AI 助手，未在 swagger）

> `r.Group("/api", RequireAuth)` → `agent := g.Group("/clawagent")`

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/clawagent/chat` | clawagent/runtime.go:493 |
| GET | `/clawagent/sessions` | runtime.go:496 |
| GET | `/clawagent/sessions/:id` | runtime.go:497 |
| DELETE | `/clawagent/sessions/:id` | runtime.go:498 |
| GET | `/clawagent/personas` | runtime.go:501 |
| POST | `/clawagent/personas` | runtime.go:502 |
| PUT | `/clawagent/personas/:id` | runtime.go:503 |
| DELETE | `/clawagent/personas/:id` | runtime.go:504 |
| POST | `/clawagent/personas/:id/default` | runtime.go:505 |
| GET | `/clawagent/providers` | runtime.go:508 |
| POST | `/clawagent/providers` | runtime.go:509 |
| PUT | `/clawagent/providers/:id` | runtime.go:510 |
| DELETE | `/clawagent/providers/:id` | runtime.go:511 |
| POST | `/clawagent/providers/:id/test` | runtime.go:512 |
| GET | `/clawagent/memory` | runtime.go:515 |
| PUT | `/clawagent/memory` | runtime.go:516 |
| GET | `/clawagent/workspaces` | runtime.go:519 |
| GET | `/clawagent/workspaces/:id/tasks` | runtime.go:520 |
| GET | `/clawagent/workspaces/:id/tasks/:taskId` | runtime.go:521 |
| POST | `/clawagent/workspaces/:id/tasks/:taskId/abort` | runtime.go:522 |

#### 2.9 OpenAI 兼容接口（未在 swagger）

| 方法 | 路径 | 来源 | 说明 |
|---|---|---|---|
| Any | `/v1/chat/completions` | clawagent/setup.go:34 | OpenAI 协议兼容入口（**root，非 /api**） |

#### 2.10 Memory（未在 swagger）

> `memories := apiGroup.Group("/memories")`

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/memories` | memory/memory.go:23 |
| POST | `/memories` | memory/memory.go:24 |
| GET | `/memories/:id` | memory/memory.go:25 |
| PUT | `/memories/:id` | memory/memory.go:26 |
| DELETE | `/memories/:id` | memory/memory.go:27 |
| GET | `/memories/:id/content` | memory/memory.go:28 |
| GET | `/memories/:id/versions` | memory/memory.go:29 |
| GET | `/memories/:id/versions/:version/content` | memory/memory.go:30 |

#### 2.11 Channels（个人通知渠道，未在 swagger）

> `channels := authedGroup.Group("/channels")`

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/channels/available` | channel/channel.go:24 |
| GET | `/channels` | channel/channel.go:25 |
| POST | `/channels` | channel/channel.go:26 |
| GET | `/channels/:id` | channel/channel.go:27 |
| PUT | `/channels/:id` | channel/channel.go:28 |
| DELETE | `/channels/:id` | channel/channel.go:29 |
| POST | `/channels/:id/test` | channel/channel.go:30 |
| POST | `/channels/wechat/login/qrcode` | channel/channel.go:31 |
| GET | `/channels/wechat/login/status` | channel/channel.go:32 |

#### 2.12 Notification Channels（swagger 中有，模块挂载位置）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/notification-channels` | notification/notification.go:38 |
| POST | `/notification-channels` | notification/notification.go:39 |
| GET | `/notification-channels/available` | notification/notification.go:37 |
| GET | `/notification-channels/:id` | notification/notification.go:40 |
| PUT | `/notification-channels/:id` | notification/notification.go:41 |
| DELETE | `/notification-channels/:id` | notification/notification.go:42 |
| POST | `/notification-channels/:id/test` | notification/notification.go:43 |
| GET | `/notification-channels/:id/logs` | notification/notification.go:44 |

#### 2.13 Kanban / KB / Authz（用户视角）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/kanban/overview` | kanban/kanban.go:18 |
| POST | `/kb/ensure` | main.go:796 | 用户侧 KB 初始化 |
| GET | `/auth/permissions` | authz/authz.go:29 |
| GET | `/auth/dept-scope` | authz/authz.go:35 |

### 3. Tenant Admin（RequireTenantAdmin）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/tenant/users` | main.go:744 |
| PUT | `/tenant/users/:id/status` | main.go:751 |
| GET | `/tenant/config` | main.go:759 |
| PUT | `/tenant/config` | main.go:760 |
| GET | `/tenant/provider-mapping` | main.go:770 |
| PUT | `/tenant/provider-mapping` | main.go:771 |
| GET | `/tenant/audit-logs` | main.go:791 |

### 4. Platform Admin（RequirePlatformAdmin）

#### 4.1 main.go 直接注册（`/admin/*`）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/admin/distributions` | main.go:856 |
| GET | `/admin/distributions/:id/receipts` | main.go:857 |
| POST | `/admin/tenants/:tenant_id/teams/:team_id/sync` | main.go:862 |
| POST | `/admin/users/provision/backfill` | main.go:871 |
| GET | `/platform/tenants` | main.go:723 |
| POST | `/platform/tenants` | main.go:724 |
| GET | `/platform/tenants/:id` | main.go:725 |
| PATCH | `/platform/tenants/:id` | main.go:726 |
| POST | `/platform/tenants/:id/suspend` | main.go:727 |
| POST | `/platform/tenants/:id/restore` | main.go:728 |
| POST | `/platform/tenants/:id/delete` | main.go:729 |
| GET | `/platform/audit-logs` | main.go:784 |

#### 4.2 Tag 治理（`platformAdmin := authed.Group("")`）

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/tags` | main.go:707 |
| PUT | `/tags/:id` | main.go:708 |
| DELETE | `/tags/:id` | main.go:709 |

#### 4.3 系统角色（systemrole 模块）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/admin/system-roles` | systemrole/systemrole.go:21 |
| GET | `/admin/system-roles/users/:userId` | systemrole/systemrole.go:22 |
| POST | `/admin/system-roles/users/:userId` | systemrole/systemrole.go:23 |
| DELETE | `/admin/system-roles/users/:userId/:role` | systemrole/systemrole.go:24 |

#### 4.4 Authz Admin（资源权限）

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/admin/permissions/users/:userId/grant` | authz/authz.go:40 |
| GET | `/admin/resource-permissions` | authz/authz.go:44 |
| PUT | `/admin/resource-permissions/:code` | authz/authz.go:45 |
| GET | `/admin/permission-grants` | authz/authz.go:49 |
| POST | `/admin/permission-grants` | authz/authz.go:50 |
| DELETE | `/admin/permission-grants/:id` | authz/authz.go:51 |

#### 4.5 Notification Admin（系统通知渠道 + 公告）

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/admin/notification-channels` | notification/notification.go:23 |
| POST | `/admin/notification-channels` | notification/notification.go:24 |
| PUT | `/admin/notification-channels/:id` | notification/notification.go:25 |
| DELETE | `/admin/notification-channels/:id` | notification/notification.go:26 |
| POST | `/admin/announcements` | notification/notification.go:32（broadcast） |

#### 4.6 Settings / Enterprise customers / Audit log

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/admin/settings` | settings/settings.go:27 |
| PUT | `/admin/settings/:key` | settings/settings.go:28 |
| GET | `/enterprise-customers` | enterprise/enterprise.go:22（**用户也能读**） |
| GET | `/admin/enterprise-customers` | enterprise/enterprise.go:29 |
| POST | `/admin/enterprise-customers` | enterprise/enterprise.go:30 |
| PUT | `/admin/enterprise-customers/:id` | enterprise/enterprise.go:31 |
| DELETE | `/admin/enterprise-customers/:id` | enterprise/enterprise.go:32 |
| GET | `/admin/audit-logs` | audit/module.go:29 |

#### 4.7 Admin User / Department / Item / Import

| 方法 | 路径 | 来源 |
|---|---|---|
| GET | `/admin/users` | adminuser/adminuser.go:62 |
| GET | `/admin/users/:id/profile` | adminuser/adminuser.go:63 |
| PUT | `/admin/users/:id/profile` | adminuser/adminuser.go:64 |
| PUT | `/admin/users/:id/status` | adminuser/adminuser.go:65 |
| GET | `/admin/organizations` | adminuser/adminuser.go:66 |
| GET | `/admin/departments/tree` | deptsync/handlers.go:29 |
| GET | `/admin/departments/children` | deptsync/handlers.go:30 |
| GET | `/admin/departments/:id/users` | deptsync/handlers.go:31 |
| GET | `/admin/items` | adminitem/adminitem.go:29 |
| GET | `/admin/items/export.csv` | adminitem/adminitem.go:30 |
| PUT | `/admin/items/:id/status` | adminitem/adminitem.go:31 |
| POST | `/admin/items/batch-delete` | adminitem/adminitem.go:32 |
| POST | `/admin/items/batch-status` | adminitem/adminitem.go:33 |
| DELETE | `/admin/items/:id` | adminitem/adminitem.go:34 |
| POST | `/admin/import-jobs` | adminimport/adminimport.go:28 |
| GET | `/admin/import-jobs` | adminimport/adminimport.go:29 |
| GET | `/admin/import-jobs/:id` | adminimport/adminimport.go:30 |
| POST | `/admin/import-jobs/:id/confirm` | adminimport/adminimport.go:31 |
| GET | `/admin/import-jobs/:id/errors.log` | adminimport/adminimport.go:32 |
| GET | `/admin/import-stats` | adminimport/adminimport.go:33 |

### 5. Internal 服务间（X-Internal-Service-Token，**未在 swagger**）

#### 5.1 `/internal/*`（gateway/authz 内部调用）

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/internal/gateway/register` | gateway/handlers.go:445 |
| POST | `/internal/gateway/:gatewayID/heartbeat` | gateway/handlers.go:446 |
| DELETE | `/internal/gateway/:gatewayID` | gateway/handlers.go:447 |
| POST | `/internal/gateway/device/online` | gateway/handlers.go:448 |
| POST | `/internal/gateway/device/offline` | gateway/handlers.go:449 |
| POST | `/internal/gateway/device/verify-token` | gateway/handlers.go:450 |
| POST | `/internal/auth/verify` | authz/authz.go:56 |

#### 5.2 `/api/internal/*`（team-namespace v1.1 + git-server + 用户事件）

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/api/internal/teams` | main.go:997 |
| GET | `/api/internal/teams` | main.go:998 |
| GET | `/api/internal/teams/:team_id` | main.go:999 |
| PATCH | `/api/internal/teams/:team_id` | main.go:1000 |
| POST | `/api/internal/teams/:team_id/members:sync` | main.go:1001 |
| POST | `/api/internal/teams/:team_id/dissolve` | main.go:1002 |
| POST | `/api/internal/teams/:team_id/bot-token:rotate` | main.go:1003 |
| POST | `/api/internal/workflow/init` | main.go:1004 |
| POST | `/api/internal/git-servers` | main.go:1012 |
| GET | `/api/internal/git-servers` | main.go:1013 |
| GET | `/api/internal/git-servers/:server_id` | main.go:1014 |
| PUT | `/api/internal/git-servers/:server_id` | main.go:1015 |
| DELETE | `/api/internal/git-servers/:server_id` | main.go:1016 |
| PUT | `/api/internal/tenants/:tenant_id/git-server` | main.go:1017 |
| GET | `/api/internal/tenants/:tenant_id/git-server` | main.go:1018 |
| DELETE | `/api/internal/tenants/:tenant_id/git-server` | main.go:1019 |
| POST | `/api/internal/users/created` | main.go:1026 |

#### 5.3 设备心跳（无 /api 前缀，无需 user JWT）

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/api/devices/:deviceID/heartbeat` | main.go:451（device token） |

### 6. 系统令牌（SystemTokenAuth）

| 方法 | 路径 | 来源 |
|---|---|---|
| POST | `/api/releases` | main.go:558（CI 发布 webhook） |

### 7. 第三方集成 / HMAC / Webhook

| 方法 | 路径 | 来源 | 说明 |
|---|---|---|---|
| POST | `/api/integrations/multica/events` | main.go:1035（条件挂载） | Multica 通知桥，HMAC |
| POST | `/api/webhooks/github` | swagger | GitHub webhook |

---

## 二、Gateway（独立部署，无 swagger）

> 路由全部来自 `gateway/internal/router.go`。无 `/api` 前缀。

| 方法 | 路径 | 认证 | 用途 |
|---|---|---|---|
| GET | `/health` | 无 | K8s liveness/readiness |
| GET | `/status` | 无 | 网关运行时连接数 / 容量 |
| GET | `/device/:deviceID/tunnel` | Device token | 设备长连接隧道（SSE/WebSocket upgrade） |
| POST | `/internal/device/:deviceID/close` | `INTERNAL_SECRET` | API server → Gateway：主动关闭某设备的隧道 |
| Any | `/device/:deviceID/proxy/*path` | `INTERNAL_SECRET` | API server → Gateway → Device HTTP 透传 |

---

## 三、收敛候选盘点（建议优先讨论）

按"看起来功能重复 / 路径分叉 / 已废弃"维度，把最容易下手的几个列出：

### A. 已废弃 — 可直接删除
- **`/api/team/*`（17 路径 / 24 操作，swagger stale）**：`internal/team/module.go` 的 `RegisterRoutes` 是空函数。swagger 中残留以下路径均为死路由：
  - `/team/sessions`、`/team/sessions/:id`、`/team/sessions/:id/members`、`/team/sessions/:id/members/:mid`、`/team/sessions/:id/approvals`、`/team/sessions/:id/decompose`、`/team/sessions/:id/explore`、`/team/sessions/:id/leader`、`/team/sessions/:id/leader/elect`、`/team/sessions/:id/leader/heartbeat`、`/team/sessions/:id/orchestrate`、`/team/sessions/:id/progress`、`/team/sessions/:id/repos`、`/team/sessions/:id/tasks`、`/team/sessions/:id/tasks/:taskId/terminate`、`/team/approvals/:approvalId`、`/team/tasks/:taskId`
  - 但 `/api/internal/teams*`（7 条）是 **cs-user ↔ server 的 team-namespace RPC**，**仍要保留**。

### B. 路径风格不统一 — 建议二选一
- 设备 token 轮换：已统一为 `POST /api/devices/:deviceID/token/rotate`（main.go:818）。历史遗留的 swagger 注释 `/devices/{deviceID}/rotate-token` + 生成的 `swagger.json` / `docs.go` / `swagger.yaml` + 前端 `deviceApi.rotateToken` 的 `/rotate-token` 路径已于 2026-08-04 统一修正为 `/token/rotate`。
- Item 移动 / 转让：swagger 与 main.go 完全重合，需确认是否两套都在用。
- 注意：`/invitations/my`（main.go:801）与 project 模块注册的 `/invitations`（project/project.go:44，handler 为 `ListMyInvitationsHandler`）是**不同路径**但**返回内容相同**（都是"我的邀请"），疑似历史遗留的双注册 → 建议合并到一处。

### C. Internal 双前缀（`/internal` vs `/api/internal`）
- `/internal/gateway/*` 与 `/internal/auth/verify` 走 `r.Group("/internal")`
- `/api/internal/teams`、`/api/internal/git-servers`、`/api/internal/workflow/init`、`/api/internal/users/created` 走 `r.Group("/api/internal")`（按 TEAM_NAMESPACE_API_REFERENCE.md 规范保留）

  → 建议把 `/internal/*` 也迁到 `/api/internal/*` 统一前缀（破坏性变更，需协调 gateway/authz 调用方）。

### D. 三套 admin 体系并存
- `authed.Group("/admin")` + `systemrole.RequirePlatformAdmin`（DB 查 user_system_roles）
- `middleware.RequirePlatformAdmin()`（JWT claim）
- `authed.Group("/platform/tenants")` + `middleware.RequirePlatformAdmin()`
- `authed.Group("/tenant/*")` + `middleware.RequireTenantAdmin()`

  → 建议把 `/admin/*` 全部迁到 `/platform/*` 或保留一套中间件，避免两套 platform admin 判定逻辑。

### E. Swagger 缺失模块（建议补注解或确认不公开）
- 真正完全缺失：cloud（11 条）、memory（8 条）、channels（9 条，与 notification-channels 是不同模块）、clawagent（20 条）+ `/v1/chat/completions`、kanban、project（含 `/invitations*`）、enterprise、settings、audit、adminuser、deptsync、adminitem、adminimport、authz（`/auth/permissions`、`/auth/dept-scope`、`/admin/permission-grants*`、`/admin/resource-permissions*`、`/admin/permissions/users/:userId/grant`）—— 这些前端 / 客户端在用的接口 swagger 都没生成，外部对接时只能靠口口相传。
- 部分缺失：notification 模块的 `/admin/announcements` 不在 swagger，但 `/admin/notification-channels*` 和用户侧 `/notification-channels*` 都已在 swagger。
- 已在 swagger（无需补）：systemrole（`/admin/system-roles*`、`/auth/system-roles/me` 均已存在）。

---

## 四、统计总览

| 模块 | 端点数 | 备注 |
|---|---|---|
| **Server - swagger.json** | 192 | 含 17 路径 / 24 操作的已废弃 `/team/*` |
| **Server - 模块未入 swagger** | ≈ 130 | cloud/memory/channel/kanban/clawagent/authz/enterprise/settings/audit/adminuser/deptsync/adminitem/adminimport/project（systemrole、notification 主体已在 swagger，仅 `/admin/announcements` 缺） |
| **Server - 内部 service-to-service** | 24 | `/internal/*`(7) + `/api/internal/*`(17) |
| **Server - 第三方 webhook** | 2 | multica、github |
| **Server - 系统令牌** | 1 | `/api/releases` |
| **Gateway** | 5 | 无 swagger |
| **合计（去 swagger 重叠）** | **≈ 320** | — |
