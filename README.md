# costrict-web

> CoStrict 云端平台 —— 由 Server、Gateway、Proxy、WeCom-Bot-Proxy 等多个 Go 服务组成的云端控制面，负责用户认证、能力治理、设备调度、企业微信通知与外部 API。

---

## 这个项目解决什么问题

CoStrict 是"AI Agent + 设备调度"的 To B 平台。`costrict-web` 是其中"云端"这一侧的合集，承担：

- **用户与组织管理**：通过 Casdoor 完成 OAuth 登录、组织/角色/权限管理
- **能力治理**：Skill / Subagent / Command / MCP / Plugin 五类 capability 的 CRUD、可见性、安全扫描、跨仓库流转
- **设备调度**：通过 Gateway 维护与 cs-cloud 的 WebSocket 隧道，下发指令、回收事件
- **企业微信通知**：互动卡片、权限审批、投票、自动通过、广播
- **Session Proxy**：在客户端与设备之间过滤/审计 SSE 流
- **ClawAgent 个人助理**：基于设备的个人 AI 助理模块

代码不出云端，本地执行由 [cs-cloud](../cs-cloud) 与 [csc](../csc) 完成。

---

## 仓库结构

`costrict-web` 是一个 **Go workspace**，包含 5 个互相独立的模块：

```
costrict-web/
├── server/              # 主 API 后端（业务逻辑核心）
├── gateway/             # 设备隧道网关（WebSocket + yamux）
├── proxy/               # Session Proxy（SSE 过滤 / 审计）
├── wecom-bot-proxy/     # 企业微信机器人 sidecar
├── client/go/           # Go 客户端 SDK
├── casdoor/             # Casdoor 配置
├── deploy/charts/       # Helm Chart
├── docs/                # 设计文档
├── scripts/             # 构建 / 发布脚本
├── docker-compose.yml   # 本地依赖（PostgreSQL + Redis + Casdoor）
├── go.work              # Go workspace 声明
└── init-db.sql          # PostgreSQL 初始化（含 pgvector）
```

| 模块 | 端口 | 说明 |
|------|------|------|
| **server** | 8080 | 业务 API、能力治理、鉴权、Admin 后台、Worker |
| **gateway** | 8090 | 设备 WebSocket 隧道接入、InternalSecret 鉴权 |
| **proxy** | 8091 | Session Proxy（客户端到设备的中间层） |
| **wecom-bot-proxy** | 8092 | 企业微信回调 sidecar（独立部署） |
| **client/go** | — | Go SDK（不部署，供其他服务引用） |

---

## 技术栈

- **后端**：Go 1.25 + Gin + GORM + Swagger
- **数据库**：PostgreSQL 16 + pgvector（向量检索）+ Redis 7（缓存与队列）
- **认证**：Casdoor（OAuth 2.0，JWT JWKS 校验）
- **设备通信**：WebSocket + yamux 多路复用
- **日志**：zap + lumberjack 滚动归档
- **容器化**：Docker + Docker Compose / Podman Compose
- **编排**：Helm Chart（`deploy/charts/`）

---

## 快速开始

### 环境要求

- Go 1.25+
- Node.js 18+ / bun（前端开发）
- Docker 或 Podman
- PostgreSQL 16（含 pgvector 扩展）

### 启动依赖服务

```bash
# 启动 PostgreSQL + Redis + Casdoor
docker-compose up -d
# 或
podman-compose up -d
```

依赖服务端口：
- PostgreSQL: `localhost:5432`（用户 `costrict` / 密码 `costrict_password` / 库 `costrict_db`）
- Redis: `localhost:6379`
- Casdoor: `localhost:8000`

### 启动 Server（主后端）

```bash
cd server
go mod tidy
go run cmd/api/main.go
```

Server 默认监听 `:8080`，Swagger UI 在 `http://localhost:8080/swagger/index.html`。

### 启动 Gateway（设备隧道网关）

```bash
cd gateway
go mod tidy
go run cmd/main.go
```

Gateway 默认监听 `:8090`，需要与 Server 共享 InternalSecret。

### 启动 Proxy（Session Proxy）

```bash
cd proxy
go mod tidy
go run cmd/main.go
```

### 启动 WeCom-Bot-Proxy（企业微信机器人 sidecar）

```bash
cd wecom-bot-proxy
cp config.yaml.example config.yaml  # 修改企业微信凭据
go run cmd/proxy/main.go
```

详细配置见 [`wecom-bot-proxy/config.yaml.example`](wecom-bot-proxy/config.yaml.example)。

---

## 常用命令

### Server

| 命令 | 说明 |
|------|------|
| `go run cmd/api/main.go` | 启动 API 服务 |
| `go run cmd/migrate/main.go` | 执行数据库迁移 |
| `go run cmd/worker/main.go` | 启动 sync worker（异步同步 catalog） |
| `go run cmd/channel-worker/main.go` | 启动通知 channel worker |
| `go run cmd/sqlexec/main.go` | 直连 PostgreSQL 调试 CLI |

### 跨服务

```bash
# 构建 Docker 镜像
bash scripts/build-images.sh

# 发布 cs-cloud release（与 cs-cloud 仓库联动）
bash scripts/publish-cs-cloud.sh

# 下载 cs-cloud 二进制
bash scripts/download-cs-cloud.sh
```

---

## 配置

### Server 环境变量（关键项）

| 变量 | 默认 | 说明 |
|------|------|------|
| `DATABASE_URL` | — | PostgreSQL 连接串 |
| `REDIS_URL` | `redis://localhost:6379` | Redis 连接串 |
| `INTERNAL_SECRET` | — | 服务间共享密钥（Gateway ↔ Server） |
| `FRONTEND_URLS` | — | 前端 CORS 白名单（逗号分隔） |
| `JWT_JWKS_URL` | — | Casdoor JWKS 公钥地址 |
| `ARTIFACT_STORAGE_BACKEND` | `local` | 非文本存储模式，只支持 `local` 或 `s3`；两种模式的部署配置见 [非文本制品存储部署](docs/deployment/artifact-storage-local-s3.md) |
| `SECURITY_SCAN_SHORT_CIRCUIT_DISABLED` | 空（启用短路） | 设为 `true` 强制所有 sync/create/update 触发 LLM 扫描 |
| `WORKER_CONCURRENCY` | `3` | sync worker 并发数 |
| `WORKER_POLL_INTERVAL_SECONDS` | `5` | sync worker 轮询间隔 |

完整变量见各模块 `internal/config/`。

---

## 模块职责详解

### Server（`server/internal/`）

包含约 30 个 internal 包，覆盖：

- **认证授权**：`authidentity`、`authz`、`casdoor`、`middleware`、`systemrole`
- **能力治理**：`adminitem`、`itemdelete`、`services`、`storage`、`migration`
- **用户与组织**：`adminuser`、`user`、`team`、`enterprise`、`deptsync`、`leader`
- **设备与网关**：`cloud`、`gateway`、`sessionurl`、`pathutil`
- **通知**：`channel`、`notification`、`dispatcher`
- **AI 能力**：`clawagent`、`llm`、`memory`、`project`
- **运营管理**：`kanban`、`audit`、`settings`、`scheduler`、`worker`

### Gateway（`gateway/internal/`）

- **隧道接入**：WebSocket 握手、yamux 多路复用、设备绑定
- **InternalSecret 鉴权**：内部 API 共享密钥中间件
- **设备注册表**：conn_id 与 last_heartbeat 跟踪
- **代理转发**：业务请求路由到 Server，SSE 流转发到设备

### Proxy（`proxy/internal/`）

- **SSE delta 过滤**：根据规则过滤敏感内容
- **Markdown 代码块过滤**：避免代码泄漏给未经授权的客户端
- **审计日志**：所有经过 proxy 的请求记录

### WeCom-Bot-Proxy（`wecom-bot-proxy/`）

- 基于 `wecom-aibot-go-sdk` 实现企业微信机器人接入
- 会话隔离、群聊守卫、`open_userid` 转换、QR 码配置
- 卡片回调直连 Server

详细架构见 [`ARCHITECTURE.md`](ARCHITECTURE.md)。

---

## 文档

### 设计文档（`docs/`）

- [`SYSTEM_DESIGN.md`](docs/SYSTEM_DESIGN.md) —— 系统总体设计
- [`DATABASE_DESIGN.md`](docs/DATABASE_DESIGN.md) —— 数据库设计
- [`CATALOG_INGEST.md`](docs/CATALOG_INGEST.md) —— Catalog 同步链路
- [`SKILL_DOWNLOAD_API.md`](docs/SKILL_DOWNLOAD_API.md) —— Skill 主文件与完整文件树下载接口
- [`SCAN_SKILL.md`](docs/SCAN_SKILL.md) —— 安全扫描机制
- [`USAGE_ES_INTEGRATION_REQUIREMENTS.md`](docs/USAGE_ES_INTEGRATION_REQUIREMENTS.md) —— 用量数据 ES 集成
- [`CLAUDE_CODE_PLUGIN_SPEC.md`](docs/CLAUDE_CODE_PLUGIN_SPEC.md) —— Claude Code Plugin 规范

### 提案（`docs/proposals/`）

- [`HTTP_TUNNEL_DESIGN.md`](docs/proposals/HTTP_TUNNEL_DESIGN.md) —— HTTP 隧道分层架构
- [`RESTRICTED_S3_OBJECT_STORAGE_DESIGN.md`](docs/proposals/RESTRICTED_S3_OBJECT_STORAGE_DESIGN.md) —— Local / 受限 S3 最小非文本存储契约与验收边界

### 部署文档（`docs/deployment/`）

- [`artifact-storage-local-s3.md`](docs/deployment/artifact-storage-local-s3.md) —— Local / S3 二选一配置、迁移、验收与回滚

### API 文档

Server 启动后访问 `http://localhost:8080/swagger/index.html`，所有 handler 均带 Swagger 注解。

---

## 关联仓库

| 仓库 | 角色 |
|------|------|
| [opencode](https://github.com/zgsm-ai/opencode) | 设备端 `cs cloud` 入口与 UI 组件 |
| [cs-cloud](../cs-cloud) | 设备端 Cloud Daemon（本仓库 Gateway 的对端） |
| [csc](../csc) | 本地 AI Agent 适配器 |
| [costrict-plugin-marketplace](https://github.com/costrict-plugins-repo/costrict-plugin-marketplace) | Plugin 私有化分发（独立项目） |

---

## 开发规范

- **API 改动必须同步更新 Swagger 注解**（Server 自动从注解生成文档）
- **异步任务用 Worker 机制**（已有 sync worker / channel worker 模板）
- **跨服务调用必须带 `INTERNAL_SECRET` 共享密钥**
- **PR 前运行 `go vet ./...`**

---

## 生产部署

### Helm Chart

```bash
cd deploy/charts
helm install costrict-web . --values values.yaml
```

包含的 Chart：
- `costrict-web`（server + gateway）
- `costrict-proxy`（session proxy）
- `wecom-bot-proxy`

非文本 asset/artifact 的 Local / S3 配置见
[`docs/deployment/artifact-storage-local-s3.md`](docs/deployment/artifact-storage-local-s3.md)。

### CI/CD

GitHub Actions 工作流：
- `build-images` —— 多服务镜像构建
- `release-charts` —— Helm Chart 发布
- `storage-e2e` —— 真实 MinIO 的受限 S3 Put/Get 与服务端下载链路验证；详见 [workflow 文档](.github/workflows/README.md#storage-e2eyaml)

---

## 许可证

MIT
