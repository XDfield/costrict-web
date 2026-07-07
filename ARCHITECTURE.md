# costrict-web 架构文档

> 本文描述 costrict-web 的整体架构、服务边界、关键链路与扩展点。
> 配合 [`README.md`](README.md) 一起阅读。

---

## 整体架构

costrict-web 是 **多服务组成的云端控制面**，由 5 个 Go 模块组成（通过 `go.work` 组织为 workspace）。各服务职责独立，通过 InternalSecret 共享密钥相互调用，对外通过 Gateway 与设备（cs-cloud）通信。

### 顶层架构图

```
                          ┌──────────────────────────┐
                          │       最终用户            │
                          │  (浏览器 / 企业微信)      │
                          └──────────┬───────────────┘
                                     │ HTTPS
                                     ▼
            ┌─────────────────────────────────────────────────────────┐
            │                  costrict-web（云端）                    │
            │                                                         │
            │                  ┌─────────────────┐                    │
            │         ┌────────┤     Server      ├────────┐           │
            │         │        │  (业务 API 中心) │        │           │
            │         │        └────────┬────────┘        │           │
            │         │   InternalSecret│                 │ Internal  │
            │         │                 │ InternalSecret  │ Secret    │
            │         ▼                 ▼                 ▼           │
            │  ┌──────────────┐   ┌──────────┐    ┌──────────────┐   │
            │  │WeCom-Bot-Proxy│   │ Gateway  │    │    Proxy     │   │
            │  │(企微回调 sidecar)│   │(设备隧道)│    │(Session 过滤)│   │
            │  └──────┬───────┘   └─────┬────┘    └──────────────┘   │
            │         │                 │                              │
            │         │  ┌──────────────┘                              │
            │         │  │ Server 通过 Gateway 调度设备                 │
            │         ▼  ▼                                              │
            │     （业务调用 / SSE 流式回传都在 Server ↔ Gateway 之间）│
            │                                                          │
            └──────────────────────────┬──────────────────────────────┘
                                       │ WebSocket + yamux
                                       │ (仅 Gateway 与设备通信)
                                       ▼
                          ┌────────────────────────┐
                          │  用户设备 (cs-cloud)    │
                          │  cs-cloud ──► csc ──►   │
                          │  opencode / 其他 agent  │
                          └────────────────────────┘

依赖服务：
  PostgreSQL 16 (pgvector)  ◄── 持久化 + 向量检索
  Redis 7                   ◄── 缓存 + 队列
  Casdoor                   ◄── OAuth 2.0 + JWKS

关键边界：
  • 只有 Gateway 与设备（cs-cloud）通信，其他服务不直连设备
  • WeCom-Bot-Proxy 只与 Server 交互（HTTP 回调到 /api/wecom/callback）
  • Proxy 是可选的中间过滤层，位于客户端与 Server 之间
  • 所有跨服务调用都通过 InternalSecret 共享密钥鉴权
```

---

## 服务清单与边界

### 1. Server（`server/`）—— 业务核心

**职责：** 所有业务逻辑、API、数据持久化、异步任务的中心。

**端口：** `:8080`

**核心 internal 包（约 30 个）：**

| 分类 | 包 | 职责 |
|------|-----|------|
| **认证授权** | `authidentity` | 多 provider 身份绑定（idtrust/github/phone/casdoor） |
|              | `authz` | 权限决策 |
|              | `casdoor` | Casdoor SDK 封装 |
|              | `middleware` | JWT 校验、INTERNAL_SECRET、CORS、Recovery |
|              | `systemrole` | 系统角色定义 |
| **能力治理** | `adminitem` | capability item CRUD（skill/subagent/command/mcp/plugin） |
|              | `itemdelete` | 软删除与回收站 |
|              | `services` | 业务编排层 |
|              | `storage` | 文件/对象存储抽象 |
|              | `migration` | 数据迁移工具 |
| **用户组织** | `adminuser` | 用户管理（管理员视角） |
|              | `user` | 用户自服务 |
|              | `team` | 团队/组织 |
|              | `enterprise` | 企业实体 |
|              | `deptsync` | 部门同步 |
|              | `leader` | 领导关系 |
| **设备网关** | `cloud` | 设备相关业务 |
|              | `gateway` | 与 Gateway 服务交互的客户端 |
|              | `sessionurl` | session URL 解析（共享 helper） |
|              | `pathutil` | 跨平台路径归一化 |
| **通知** | `channel` | 通道抽象（webhook / bot） |
|          | `notification` | 通知发送与广播 |
|          | `dispatcher` | 权限/问询卡片分发 |
| **AI 能力** | `clawagent` | 个人 AI 助理模块 |
|             | `llm` | LLM 调用封装 |
|             | `memory` | 助理记忆 |
|             | `project` | 项目实体 |
| **运营管理** | `kanban` | 多维度看板视图 |
|              | `audit` | 审计日志 |
|              | `settings` | 系统配置 |
|              | `scheduler` | 定时任务 |
|              | `worker` | sync / channel worker |
| **基础设施** | `config` / `database` / `logger` / `models` | 通用支撑 |

**入口：**
- `cmd/api/main.go` —— API 服务
- `cmd/migrate/main.go` —— 数据库迁移
- `cmd/worker/main.go` —— sync worker（异步同步 catalog）
- `cmd/channel-worker/main.go` —— 通知 channel worker
- `cmd/sqlexec/main.go` —— PG 直查调试工具

### 2. Gateway（`gateway/`）—— 设备隧道网关

**职责：** 接入设备的 WebSocket 隧道，作为云端 ↔ 设备的桥。

**端口：** `:8090`

**internal 包：**

| 包 | 职责 |
|-----|------|
| `auth` | InternalSecret 共享密钥校验 |
| `config` | 配置加载 |
| `manager` | 设备连接管理（conn_id + last_heartbeat） |
| `registration` | 设备注册流程 |
| `router` | 路由（区分业务调用 vs SSE 流） |
| `tunnel` | yamux 多路复用封装 |
| `tunnel_handler` | 隧道事件处理 |
| `proxy_handler` | 业务请求代理到设备 |
| `logger` | 结构化日志 |
| `types` | DTO |

**关键流程：**
1. 设备（cs-cloud）发起 WebSocket 握手，携带 `device_token`
2. Gateway 校验 token → 建立连接 → 注册 conn_id
3. 维护 `device_id ↔ conn_id ↔ yamux stream` 映射表
4. Server 调用 Gateway HTTP API，Gateway 通过 yamux 把请求路由到设备
5. SSE 流式响应通过 yamux 流式回传

### 3. Proxy（`proxy/`）—— Session Proxy

**职责：** 在客户端与设备之间的中间层，做内容过滤与审计。

**端口：** `:8091`

**核心能力：**
- **SSE delta 过滤**：根据规则（`filter_rules.yaml`）过滤敏感内容
- **Markdown 代码块过滤**：避免代码泄漏给未经授权的客户端
- **审计日志**：所有经过 proxy 的请求记录留痕
- **大 body 流式转发**：避免内存爆涨

**典型场景：** 企业内部用户通过 Proxy 访问设备 session，所有内容可审计、可过滤。

### 4. WeCom-Bot-Proxy（`wecom-bot-proxy/`）—— 企业微信机器人 sidecar

**职责：** 独立部署的企业微信机器人接入层，桥接企业微信回调与 Server。

**端口：** `:8092`

**核心能力：**
- 基于 `wecom-aibot-go-sdk` 接入企业微信
- 会话隔离（按 userid + deviceUUID + path）
- 群聊守卫、`open_userid` 转换
- 卡片回调直连 Server（`/api/wecom/callback`）
- QR 码配置、欢迎语/错误处理

**配置：** 见 `wecom-bot-proxy/config.yaml.example`

### 5. Client/Go（`client/go/`）—— Go SDK

**职责：** 不部署，供其他服务引用的 Go 客户端 SDK。封装了 Server API 的调用方法。

---

## 关键数据流

### 1. 用户登录与多身份绑定

```
浏览器
  │
  ├─► 重定向到 Casdoor OAuth 登录页
  │
  ├─► Casdoor 回调带 code → Server 交换 access_token
  │
  ├─► Server 校验 JWT（JWKS 公钥，不再用 ParseUnverified）
  │
  ├─► 写入用户 + 绑定 casdoor identity
  │
  └─► 用户可在 Identity 页绑定其他 provider
        ├─► idtrust（企业内部 SSO）
        ├─► github
        └─► phone（短信）

  冲突场景：account merge
  ─► unbindIdentity → startBind → 识别到已有 user → confirmMerge / cancelMerge
```

### 2. 设备 → 云端 → Web 控制台 实时事件链路

```
cs-cloud 内部事件（csc / terminal / gitwatcher）
  │
  ▼
yamux 流式回传到 Gateway
  │
  ▼
Gateway 路由：
  ├─► 业务请求（HTTP） → Server 处理
  └─► SSE 事件流 → 转发给订阅的客户端
        │
        ▼
  （可选）Proxy 过滤 / 审计
        │
        ▼
  Web 控制台（app-ai-native）实时显示
```

### 3. 企业微信权限审批闭环

```
AI Agent 请求权限（cs-cloud → cloud）
  │
  ▼
Server dispatcher 把权限卡片推送到企业微信
  │
  ├─► 通过 WeCom-Bot-Proxy 发送互动卡片
  │
  ▼
员工在企业微信点击"通过"/"拒绝"
  │
  ▼
企业微信回调 → WeCom-Bot-Proxy → Server /api/wecom/callback
  │
  ▼
Server 通过 Gateway 把决策下发给 cs-cloud
  │
  ▼
cs-cloud 继续执行或终止
```

### 4. Capability 同步与安全扫描

```
catalog-bundle 上游（git 仓库）
  │
  ▼
sync worker（cmd/worker）轮询拉取
  │
  ▼
parse plugin.json / hooks.json / .mcp.json / SKILL.md
  │
  ▼
upsert capability_items（5 种 item_type）
  │
  ▼
security_scan（LLM 扫描 + 短路优化）
  │
  ▼
security_status: clean / suspicious / unscanned
  │
  ▼
前端按可见性展示
```

`SECURITY_SCAN_SHORT_CIRCUIT_DISABLED=true` 可强制全量扫描（用于回滚有问题的短路逻辑）。

---

## 鉴权体系

### 三层鉴权

1. **用户鉴权（外部 API）**：JWT 校验（Casdoor JWKS 公钥），解析 user_id 写入 context
2. **服务间鉴权（内部 API）**：InternalSecret 共享密钥（Gateway ↔ Server ↔ Proxy ↔ WeCom-Bot-Proxy）
3. **设备鉴权（隧道层）**：device_token（注册时颁发，绑定到 device_id）

### 关键约束

- 所有跨服务调用必须带 `INTERNAL_SECRET` HTTP 头
- SearchUsers 响应限制为非敏感字段
- Item/Repository mutation 必须验证 ownership
- Tunnel/Proxy/Gateway-assign 接口必须经过 InternalSecret 中间件

---

## 数据库设计

PostgreSQL 16 + pgvector 扩展。核心表：

| 表 | 职责 |
|----|------|
| `users` | 用户主表 |
| `auth_identities` | 多 provider 身份绑定 |
| `organizations` / `teams` | 组织与团队 |
| `repositories` | 仓库（capability 容器） |
| `capability_items` | 5 类 capability（skill/subagent/command/mcp/plugin） |
| `capability_assets` | capability 关联资产 |
| `security_scans` | 安全扫描结果 |
| `devices` | 设备注册表 |
| `workspaces` | 工作区（device + path 绑定） |
| `notifications` / `notification_channels` | 通知与通道配置 |
| `sync_jobs` / `sync_logs` | catalog 同步日志 |
| `audit_logs` | 审计日志 |

迁移文件在 `server/migrations/`，通过 `cmd/migrate/main.go` 执行。

向量检索（pgvector）用于能力项的语义搜索（embedding 字段）。

---

## 异步任务体系

Server 内置 Worker 机制处理异步任务：

| Worker | 入口 | 职责 |
|--------|------|------|
| **sync worker** | `cmd/worker/main.go` | 轮询拉取 catalog-bundle，解析并入库 |
| **channel worker** | `cmd/channel-worker/main.go` | 处理通知通道的发送队列 |
| **scheduler** | `internal/scheduler` | 定时任务（心跳检测、过期清理等） |

Worker 通过 Redis 队列协调，可水平扩展（多副本）。

---

## 扩展点

### 接入新的 Capability 类型

1. 在 `internal/models/capability_item.go` 的 `ItemType` 枚举新增类型
2. 在 `internal/services/` 添加对应解析器（参考已有的 plugin.json / hooks.json 解析）
3. 在 sync worker 中添加 ingest 路径
4. 前端 store 添加新 tab

### 接入新的通知通道

1. 在 `internal/channel/` 实现新的 Sender（参考 webhook / bot）
2. 在 `internal/notification/` 注册通道类型
3. 前端通知配置页添加新通道 UI

### 接入新的 Identity Provider

1. 在 `internal/authidentity/` 添加 provider 实现
2. 在 Identity 页 `PROVIDER_META` 注册 SVG 图标与品牌色
3. 配置 Casdoor 对应 application

---

## 关键设计决策

### 1. 为什么拆成 5 个 Go 模块而不是单服务？

- **独立部署**：Gateway 必须常驻、低延迟；Server 可以频繁迭代；WeCom-Bot-Proxy 可以独立扩缩容
- **故障隔离**：一个服务挂了不影响其他（例如 Proxy 挂了，业务 API 还能用）
- **安全边界**：Gateway 暴露给设备，Server 暴露给前端，访问面收敛
- **复用 SDK**：Client/Go 可被外部项目引用

### 2. 为什么用 go.work 而不是 mono go.mod？

- 各模块依赖版本独立演进
- 跨模块修改可同时编译验证
- 发布时各自打独立镜像

### 3. 为什么 JWT 改用 JWKS 公钥校验？

- 历史代码用 `ParseUnverified`，存在伪造 JWT 风险
- JWKS 公钥从 Casdoor 拉取并缓存，校验在本地完成
- 这是 P0 级安全问题修复，不能回退

### 4. 为什么 WeCom-Bot-Proxy 用独立 sidecar 而不是集成进 Server？

- 企业微信 SDK 依赖较重，会污染 Server 依赖树
- 企业微信回调域名独立配置，sidecar 更灵活
- 切换 SDK（之前是自实现，后切换到官方 wecom-aibot-go-sdk）不影响 Server

### 5. 为什么 Session Proxy 独立部署而不是 Gateway 内嵌？

- Proxy 是可选组件（部分客户不需要审计）
- Proxy 的过滤规则可以按客户定制
- 独立部署便于按需扩缩容

---

## 测试策略

| 类型 | 位置 | 覆盖范围 |
|------|------|---------|
| 单元测试 | `server/internal/**/*_test.go` | auth、middleware、handlers、services、authidentity |
| HTTP 集成测试 | `gateway/internal/*_test.go` | tunnel、auth、manager |
| Swagger 注解测试 | `server/internal/...` | 注解完整性 |
| E2E | 手动 | 完整的"用户登录 → 设备连接 → AI 调度 → 通知"链路 |

**当前不足**：clawagent、dispatcher、notification 模块的测试覆盖偏低，下半年补齐。

---

## 与其他仓库的关系

| 仓库 | 关系 |
|------|------|
| **opencode** | 设备端 AI 内核，提供 `cs cloud` 入口 |
| **cs-cloud** | Gateway 的对端，承载隧道客户端 |
| **csc** | cs-cloud 调度的本地 AI Agent 适配器 |
| **costrict-plugin-marketplace** | Plugin 私有化分发，独立项目 |

详细接口契约见 `docs/SYSTEM_DESIGN.md` 与各模块 Swagger。

---

## 变更日志

架构层面的重要变更请在此处追加：

- **2026-06-30**：WeCom-Bot-Proxy 切换到官方 `wecom-aibot-go-sdk`
- **2026-06-29**：notification 支持延迟定时器与广播 API（all/organization/user）
- **2026-06-24**：gateway 增加 conn_id + last_heartbeat 跟踪
- **2026-06-18**：clawagent 模块上线，persona 系统重构
- **2026-06-17**：account merge 流程（unbind → claim → confirm）
- **2026-06-15**：wecom-bot channel 上线，支持 webhook / bot 分类
- **2026-06-10**：Session Proxy（独立服务）上线
- **2026-05-29**：notification 卡片增强（auto-approve、vote、jump_list）
- **2026-03-23**：安全加固专项（JWT JWKS、InternalSecret、CORS、SearchUsers 字段收敛）
- **2026-03-19**：workspace 管理 CRUD 模块上线
- **2026-03-13**：从空仓库孵化首个 Go 后端骨架
