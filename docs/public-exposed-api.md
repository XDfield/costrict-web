# 可暴露到公网的接口列表

> 生成日期:2026-08-04
> BasePath `/api`(除非额外标注)

---

## 一、完全公开(无登录)

| 方法 | 路径 |
|---|---|
| GET | `/health`(root) |
| GET | `/api/auth/callback` |
| GET | `/api/auth/login` |
| GET | `/api/auth/bind/callback` |
| GET | `/api/updates/check` |
| GET | `/api/multica/updates/check` |
| GET | `/api/registries/public` |
| GET | `/api/registries/:id` |
| GET | `/api/registries/:id/items` |
| GET | `/api/registry/:repo/access` |
| GET | `/api/registry/:repo/index.json` |
| GET | `/api/registry/:repo/:itemType/:slug/*file` |
| GET | `/api/plugins/:slug/download` |
| GET | `/api/marketplace/:repo/marketplace.json` |
| GET | `/api/items` |
| GET | `/api/items/:id` |
| GET | `/api/items/:id/assets` |
| GET | `/api/items/:id/versions` |
| GET | `/api/items/:id/versions/:version` |
| GET | `/api/items/:id/artifacts` |
| GET | `/api/items/:id/download` |
| GET | `/api/items/:id/scan-status` |
| GET | `/api/items/:id/scan-results` |
| GET | `/api/scan-results/:id` |
| GET | `/api/artifacts/:id/download` |
| GET | `/api/users/names` |
| GET | `/api/users/info` |
| GET | `/api/categories` |
| GET | `/api/categories/:id` |
| GET | `/api/tags` |
| GET | `/api/tags/:id` |
| GET | `/api/items/:id/similar` |
| GET | `/api/items/filter-options` |
| GET | `/api/items/:id/stats` |
| GET | `/api/plugins/builtin` |
| POST | `/api/marketplace/items/search` |
| POST | `/api/marketplace/items/hybrid-search` |
| POST | `/api/marketplace/items/recommend` |
| GET | `/api/marketplace/items/trending` |
| GET | `/api/marketplace/items/new` |
| GET | `/api/webhooks/channels/:type` |
| POST | `/api/webhooks/channels/:type` |

## 二、登录用户(RequireAuth)

### 2.1 认证 / 当前用户

| 方法 | 路径 |
|---|---|
| GET | `/api/auth/resolve` |
| GET | `/api/auth/me` |
| GET | `/api/me` |
| GET | `/api/auth/identities` |
| POST | `/api/auth/identities/:provider/unbind` |
| POST | `/api/auth/bind/start` |
| POST | `/api/auth/bind/confirm-merge` |
| POST | `/api/auth/bind/cancel-merge` |
| POST | `/api/auth/logout` |
| GET | `/api/me/username-available` |
| POST | `/api/me/complete-registration` |
| PATCH | `/api/me/profile` |
| POST | `/api/me/suggest-profile` |
| GET | `/api/auth/system-roles/me` |
| GET | `/api/users/search` |
| GET | `/api/users/me/behavior/summary` |

### 2.2 Items / Plugins / Categories / Tags

| 方法 | 路径 |
|---|---|
| GET | `/api/items/my` |
| POST | `/api/items` |
| POST | `/api/plugins/upload` |
| PUT | `/api/items/:id` |
| POST | `/api/items/:id/check-consistency` |
| POST | `/api/items/:id/fork` |
| DELETE | `/api/items` |
| DELETE | `/api/items/:id` |
| PUT | `/api/items/:id/move` |
| PUT | `/api/items/:id/transfer` |
| POST | `/api/items/:id/distribute` |
| GET | `/api/items/:id/distributions` |
| POST | `/api/items/:id/scan` |
| POST | `/api/scan-jobs/:id/cancel` |
| POST | `/api/items/:id/favorite` |
| DELETE | `/api/items/:id/favorite` |
| PUT | `/api/items/:id/mcp-config` |
| POST | `/api/items/:id/analyze` |
| POST | `/api/items/:id/behavior` |
| POST | `/api/items/:id/improve` |
| POST | `/api/items/:id/tags` |
| POST | `/api/categories` |
| PUT | `/api/categories/:id` |
| DELETE | `/api/categories/:id` |
| POST | `/api/artifacts/upload` |
| DELETE | `/api/artifacts/:id` |

### 2.3 Distributions

| 方法 | 路径 |
|---|---|
| GET | `/api/distributions/my/sent` |
| GET | `/api/distributions/my/received` |
| GET | `/api/distributions/my/authority` |
| GET | `/api/distributions/eligible-users` |
| PUT | `/api/distributions/:id` |
| DELETE | `/api/distributions/:id` |
| POST | `/api/distributions/:id/dismiss` |
| POST | `/api/distributions/:id/read` |

### 2.4 Registries(能力编辑器发布流程间接调用)

> 仓库管理 / 成员 / 邀请 / 同步等接口在前端已无 UI 入口(`/console` 仓库页被侧边栏跳过、`console.repositories` 菜单已注释、`/store/manager` 仓库创建 UI 已注释、`RepoSyncTab` / `invite-dialog.tsx` 为 dead code),已归入"不暴露到公网"。本节仅保留能力编辑器发布能力时间接调用的 5 个接口。

| 方法 | 路径 |
|---|---|
| GET | `/api/repositories/my` |
| GET | `/api/repositories/:id/registry` |
| GET | `/api/registries/my` |
| POST | `/api/registries` |
| POST | `/api/registries/:id/items` |

### 2.5 Projects / Invitations

> 已归入"不暴露到公网"。`/projects` 路由在前端侧边栏无入口(仅直接输入 URL 可访问),前端 `console.projects` 菜单未注册,所有 Projects / Invitations 接口均无 UI 可达路径。

### 2.6 Devices / Workspaces

> Workspace 目录(directory)管理、"设为默认"、按工作空间查设备等 7 个接口,以及设备令牌轮换接口 `POST /api/devices/:deviceID/token/rotate`,共 8 个接口已归入"不暴露到公网"。前端 `workspaceApi`(`pages/workspace/lib/api.ts`)虽定义了 `getDefault` / `setDefault` / `addDirectory` / `updateDirectory` / `deleteDirectory` / `reorderDirectories` / `listByWorkspace` 共 7 个方法,但整个 `app-ai-native` 对这些方法名(及 `/workspaces/default`、`/set-default`、`/directories`、`/directories/reorder`、`/workspaces/:id/devices` 路径)grep 全部仅命中定义本身,无任何 `.method(` 调用;`deviceApi.rotateToken` 同理,grep `rotateToken(` 仅命中 `api.ts` 定义本身,无任何组件调用。前端 workspace 页面目前只做了基础 CRUD(`list/get/create/update/delete`),目录管理、"设为默认"、设备令牌轮换整套功能无 UI 入口。保留的 5 个 workspace CRUD + 设备 CRUD/fingerprint 接口在 mobile/layout、components/layout、multica-page、context/device-workspace 多处活跃调用。
>
> 附:设备令牌轮换还存在路径三方不一致 —— 后端实际路由是 `/api/devices/:deviceID/token/rotate`(`server/cmd/api/main.go:821`),但 Swagger 注释 `device.go:366` 与生成的 `swagger.json` / `docs.go` / `swagger.yaml` 写成 `/devices/{deviceID}/rotate-token`,前端 `deviceApi.rotateToken` 也写成 `/api/devices/:id/rotate-token`。已统一修正为后端的 `/token/rotate`。

| 方法 | 路径 |
|---|---|
| POST | `/api/devices/register` |
| GET | `/api/devices` |
| GET | `/api/devices/:deviceID` |
| PUT | `/api/devices/:deviceID` |
| DELETE | `/api/devices/:deviceID` |
| PUT | `/api/devices/:deviceID/fingerprint` |
| GET | `/api/workspaces` |
| POST | `/api/workspaces` |
| GET | `/api/workspaces/:workspaceID` |
| PUT | `/api/workspaces/:workspaceID` |
| DELETE | `/api/workspaces/:workspaceID` |

### 2.7 Cloud(设备事件 / SSE / 命令)

| 方法 | 路径 |
|---|---|
| GET | `/api/cloud/workspace/:workspaceID/event` |
| POST | `/api/cloud/session/:sessionID/subscribe` |
| POST | `/api/cloud/session/:sessionID/unsubscribe` |
| POST | `/api/cloud/event` |
| POST | `/api/cloud/command` |
| GET | `/api/cloud/stats` |
| POST | `/api/cloud/devices/:deviceID/commands/:commandID/result` |
| Any | `/api/cloud/device/:deviceID/proxy/*path` |
| Any | `/api/cloud/sessions/:sessionID/proxy/*path` |

### 2.8 ClawAgent

> 已归入"不暴露到公网"。ClawAgent 相关接口在前端无 UI 入口,由后端服务或 CLI 直接调用,不暴露到公网。

### 2.9 OpenAI 兼容

> 已归入"不暴露到公网"。`/v1/chat/completions` 为 OpenAI 兼容代理端点,在前端无 UI 入口,仅内网/服务间使用。

### 2.10 Memory

> 已归入"不暴露到公网"。`app-ai-native` 前端无 `memoryApi` / `memoriesApi` 模块,`/api/memories` 在 UI 层零调用,无任何可达入口。

### 2.11 Channels(个人通知渠道)

| 方法 | 路径 |
|---|---|
| GET | `/api/channels/available` |
| GET | `/api/channels` |
| POST | `/api/channels` |
| GET | `/api/channels/:id` |
| PUT | `/api/channels/:id` |
| DELETE | `/api/channels/:id` |
| POST | `/api/channels/:id/test` |

> WeChat 扫码登录接口(`/api/channels/wechat/login/qrcode`、`/api/channels/wechat/login/status`)已归入"不暴露到公网"。`store/components/channels-section.tsx`、`configure-channel-dialog.tsx`、`wechat-login-dialog.tsx`、`add-channel-dialog.tsx` 是 dead code(无页面渲染),而 console 路径下 WeCom 渠道通过 `userId` 直接配置,不需要扫码登录,因此这两个接口在前端无用户可达入口。

### 2.12 Notification Channels

> 已归入"不暴露到公网"。`notificationChannelApi`(用户级单向通知渠道,与 `channelApi` 双向、`adminNotificationChannelApi` 管理员级三个独立 surface)在 `app-ai-native` 前端仅有 `api.ts` 注释引用,无任何页面或组件调用,所有 8 个接口在前端无用户可达入口。真正活跃的是 admin 路径 `/api/admin/notification-channels`(见 §三/四 Admin 段),不在本节。

### 2.13 Kanban / KB / Authz

| 方法 | 路径 |
|---|---|
| GET | `/api/kanban/overview` |
| POST | `/api/kb/ensure` |
| GET | `/api/auth/permissions` |
| GET | `/api/auth/dept-scope` |

## 三、Tenant Admin(登录 + RequireTenantAdmin)

> 已归入"不暴露到公网"。`app-ai-native` 整个前端项目 grep "tenant" 0 匹配,admin 菜单注册表(`admin/lib/menu-registry.ts`)无 tenant 管理入口。7 个 `/api/tenant/*` 接口在前端无任何调用方。注意区分:admin ops 页用的 audit 走 `/api/admin/audit-logs`(在 §四 暴露),不是 `/api/tenant/audit-logs`。

## 四、Platform Admin(登录 + RequirePlatformAdmin)

> `/api/platform/tenants*`(7 个租户 CRUD/suspend/restore/delete 接口)已归入"不暴露到公网"。`app-ai-native` 前端 grep `platform/tenants` / `platformApi` / `/api/platform/` 全部 0 匹配,admin 菜单注册表(`admin/lib/menu-registry.ts`)7 个条目(members/permissions/distributions/content/import/enterprise/ops)无 tenants 管理;admin enterprise 页面调用的是 `/api/enterprise-customers` + `/api/admin/enterprise-customers`(企业客户,与 platform 租户是两个概念)。

| 方法 | 路径 |
|---|---|
| GET | `/api/admin/distributions` |
| GET | `/api/admin/distributions/:id/receipts` |
| GET | `/api/platform/audit-logs` |
| POST | `/api/tags` |
| PUT | `/api/tags/:id` |
| DELETE | `/api/tags/:id` |
| GET | `/api/admin/system-roles` |
| GET | `/api/admin/system-roles/users/:userId` |
| POST | `/api/admin/system-roles/users/:userId` |
| DELETE | `/api/admin/system-roles/users/:userId/:role` |
| POST | `/api/admin/permissions/users/:userId/grant` |
| GET | `/api/admin/resource-permissions` |
| PUT | `/api/admin/resource-permissions/:code` |
| GET | `/api/admin/permission-grants` |
| POST | `/api/admin/permission-grants` |
| DELETE | `/api/admin/permission-grants/:id` |
| GET | `/api/admin/notification-channels` |
| POST | `/api/admin/notification-channels` |
| PUT | `/api/admin/notification-channels/:id` |
| DELETE | `/api/admin/notification-channels/:id` |
| POST | `/api/admin/announcements` |
| GET | `/api/admin/settings` |
| PUT | `/api/admin/settings/:key` |
| GET | `/api/enterprise-customers` |
| GET | `/api/admin/enterprise-customers` |
| POST | `/api/admin/enterprise-customers` |
| PUT | `/api/admin/enterprise-customers/:id` |
| DELETE | `/api/admin/enterprise-customers/:id` |
| GET | `/api/admin/audit-logs` |
| GET | `/api/admin/users` |
| GET | `/api/admin/users/:id/profile` |
| PUT | `/api/admin/users/:id/profile` |
| PUT | `/api/admin/users/:id/status` |
| GET | `/api/admin/organizations` |
| GET | `/api/admin/departments/tree` |
| GET | `/api/admin/departments/children` |
| GET | `/api/admin/departments/:id/users` |
| GET | `/api/admin/items` |
| GET | `/api/admin/items/export.csv` |
| PUT | `/api/admin/items/:id/status` |
| POST | `/api/admin/items/batch-delete` |
| POST | `/api/admin/items/batch-status` |
| DELETE | `/api/admin/items/:id` |
| POST | `/api/admin/import-jobs` |
| GET | `/api/admin/import-jobs` |
| GET | `/api/admin/import-jobs/:id` |
| POST | `/api/admin/import-jobs/:id/confirm` |
| GET | `/api/admin/import-jobs/:id/errors.log` |
| GET | `/api/admin/import-stats` |

## 五、第三方集成 / Webhook(HMAC / 公开回调)

| 方法 | 路径 |
|---|---|
| POST | `/api/integrations/multica/events`(HMAC) |
| POST | `/api/webhooks/github` |

## 六、Gateway(独立部署,仅暴露 health/status)

| 方法 | 路径 |
|---|---|
| GET | `/health` |
| GET | `/status` |

---

## 不暴露到公网(仅内网/服务间)

| 类别 | 路径 |
|---|---|
| Swagger UI | `/swagger/*any` |
| Internal(gateway/authz RPC) | `/internal/gateway/*`、`/internal/auth/verify` |
| Internal(team-namespace / git-servers / workflow / user events) | `/api/internal/*` |
| 设备心跳 | `/api/devices/:deviceID/heartbeat` |
| 系统令牌(CI 发布) | `/api/releases` |
| 设备→云通知 | `/api/cloud/device/notify`、`/api/cloud/device/notify/responded` |
| 设备→网关隧道 | `/device/:deviceID/tunnel`、`/internal/device/:deviceID/close`、`/device/:deviceID/proxy/*path` |
| 设备注册(无 JWT) | `/cloud/device/gateway-assign`(root) |
| Repositories 管理接口(前端无 UI 入口) | `/api/repositories`(list/create)、`/api/repositories/:id`(get/put/delete)、`/api/repositories/:id/members`、`/api/repositories/:id/invitations`、`/api/repositories/:id/registries`、`/api/repositories/:id/sync`、`/api/repositories/:id/sync/cancel`、`/api/repositories/:id/sync-status`、`/api/repositories/:id/sync-logs`、`/api/repositories/:id/sync-jobs` |
| Registries 管理接口(前端无 UI 入口) | `/api/registries`(list)、`/api/registries/:id`(put/delete)、`/api/registries/:id/transfer`、`/api/registries/:id/sync`、`/api/registries/:id/sync/cancel`、`/api/registries/:id/sync-status`、`/api/registries/:id/sync-logs`、`/api/registries/:id/sync-jobs` |
| Sync 任务查询(前端无 UI 入口) | `/api/sync-logs/:id`、`/api/sync-jobs/:id` |
| Projects / Invitations(前端无 UI 入口) | `/api/projects`、`/api/projects/:id`(*全部子路径:basic/put/pin/archive-time/delete/archive/unarchive/members/members/:userId/role/invitations/repositories)、`/api/invitations`、`/api/invitations/my`、`/api/invitations/:id`(accept/decline/respond/delete) |
| ClawAgent(前端无 UI 入口) | `/api/clawagent/chat`、`/api/clawagent/sessions`、`/api/clawagent/sessions/:id`、`/api/clawagent/personas`、`/api/clawagent/personas/:id`、`/api/clawagent/personas/:id/default`、`/api/clawagent/providers`、`/api/clawagent/providers/:id`、`/api/clawagent/providers/:id/test`、`/api/clawagent/memory`、`/api/clawagent/workspaces`、`/api/clawagent/workspaces/:id/tasks`、`/api/clawagent/workspaces/:id/tasks/:taskId`、`/api/clawagent/workspaces/:id/tasks/:taskId/abort` |
| OpenAI 兼容代理(前端无 UI 入口) | `/v1/chat/completions`(root) |
| Memory(前端无 UI 入口) | `/api/memories`、`/api/memories/:id`、`/api/memories/:id/content`、`/api/memories/:id/versions`、`/api/memories/:id/versions/:version/content` |
| Channels — WeChat 扫码登录(前端无 UI 入口,store 路径相关组件为 dead code) | `/api/channels/wechat/login/qrcode`、`/api/channels/wechat/login/status` |
| Notification Channels(前端无 UI 入口,`notificationChannelApi` 仅注释引用) | `/api/notification-channels`、`/api/notification-channels/available`、`/api/notification-channels/:id`、`/api/notification-channels/:id/test`、`/api/notification-channels/:id/logs` |
| Tenant Admin(前端无 UI 入口,`app-ai-native` grep "tenant" 0 匹配,admin 菜单无 tenant 入口) | `/api/tenant/users`、`/api/tenant/users/:id/status`、`/api/tenant/config`、`/api/tenant/provider-mapping`、`/api/tenant/audit-logs` |
| Platform Tenants 管理(前端无 UI 入口,前端无 `platformApi` 模块,admin enterprise 页用的是 `/api/enterprise-customers`) | `/api/platform/tenants`、`/api/platform/tenants/:id`、`/api/platform/tenants/:id/suspend`、`/api/platform/tenants/:id/restore`、`/api/platform/tenants/:id/delete` |
| Tenants 邮箱域名建议(前端无 UI 入口,后端为 login picker 设计但前端未实现该 picker) | `/api/tenants/suggest` |
| Admin 平台级 provision / team sync(前端无 UI 入口,`teams/.*sync` / `provision/backfill` / `provisionApi` 在前端 0 匹配) | `/api/admin/tenants/:tenant_id/teams/:team_id/sync`、`/api/admin/users/provision/backfill` |
| Workspaces 目录管理 / 设默认 / 按工作空间查设备(前端无 UI 入口,`workspaceApi` 的 7 个方法仅定义无调用) | `/api/workspaces/default`、`/api/workspaces/:workspaceID/set-default`、`/api/workspaces/:workspaceID/directories`、`/api/workspaces/:workspaceID/directories/reorder`、`/api/workspaces/:workspaceID/directories/:directoryID`、`/api/workspaces/:workspaceID/devices` |
| 设备令牌轮换(前端无 UI 入口,`deviceApi.rotateToken` 仅定义无调用;且前端/Swagger 路径 `/rotate-token` 与后端实际路由 `/token/rotate` 不一致,已统一为 `/token/rotate`) | `/api/devices/:deviceID/token/rotate` |
