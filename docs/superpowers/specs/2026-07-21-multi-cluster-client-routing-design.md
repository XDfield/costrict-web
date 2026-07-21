# 多集群客户端侧集群路由（Client-Side Cluster Routing）设计

**日期**：2026-07-21
**状态**：已确认
**分支**：`feat/multi-cluster-client-routing`

## 背景与问题

交付场景：两个 K8s 集群（A、B），部署相同配置、连接同一个数据库，但对外域名不同。设备通过本集群的 APISIX → nginx-router → Gateway 建立 yamux 隧道，绑定关系（deviceID → gatewayID）落在共享 DB。

当前 Web 操作设备的链路：浏览器 → 当前域名 Server → `registry.GetDeviceGateway(deviceID)` → 按 `GatewayInfo.InternalURL`（Pod IP）直连 Gateway → yamux → 设备。

问题：用户在集群 B 的域名打开 Web，而设备注册在集群 A 时，Server B 拿到的是**集群 A 的 Pod IP**，跨集群不可路由，设备无法控制。

约束（乙方交付环境）：

- 不能修改集群 CoreDNS；CoreDNS 封闭，无法解析公网/对方集群域名。
- 集群间网络不做任何打通假设（不依赖 Cluster Mesh 等）。
- 浏览器侧对两个集群的域名都可达（用户随机从任一域名打开 Web）。

## 方案概述

**客户端侧集群路由**：Server 不做任何跨集群转发；由前端在发起设备相关请求前，把请求路由到设备归属集群的 API 域名。跨集群跳转只发生在浏览器——这是本方案成立的唯一网络前提。

核心不变量：**任何设备只被它归属集群的 Server 操作**。归属集群的 Server 通过 Pod IP 直连本集群 Gateway，全程集群内通信。

## 数据流

```
Gateway 启动 → 从 Nacos（独立 dataId）解析 apiBaseURL，失败回退环境变量
            → 注册/心跳时上报 apiBaseURL → 共享 DB（gateway_registry.api_base_url）

用户在集群B域名打开 Web → GET /api/devices/:id
  → Server B 查绑定 deviceID → Gateway(集群A)
  → 响应附带 clusterAPIURL = 该 Gateway 的 apiBaseURL（集群A的 Server 公网 API 地址）

前端发起设备操作（会话/SSE/终端 WS，全部走 /cloud/device/:id/proxy/...）：
  clusterAPIURL 为空 或 == 当前 origin → 用 env.APP_URL（现状，同源）
  否则                                  → 用 clusterAPIURL 作为 base（跨域到集群A）

集群A Server → registry.GetDeviceGateway → Pod IP 直连集群A Gateway → yamux → 设备
```

## 详细设计

### 1. Gateway 侧：apiBaseURL 解析与上报

**配置优先级**（与现有 endpoint 解析模式完全对称）：

1. Nacos：新增独立 dataId，环境变量 `GATEWAY_NACOS_API_BASE_URL_DATA_ID` 指定；内容为纯文本 URL（如 `https://api-a.example.com`）。复用现有 `NacosConfig` 的 serverAddr/namespace/group/认证/超时配置。
2. 回退：环境变量 `GATEWAY_API_BASE_URL`。
3. 都为空：上报空字符串（单集群部署，向后兼容）。

**实现**：`gateway/internal/endpoint_resolver.go` 的 `resolveFromNacos` 重构为可按 dataId 复用；新增 `APIBaseURLResolver`（或在 `EndpointResolver` 上扩展方法 `ResolveAPIBaseURL`），行为与 `Resolve` 对称：Nacos 404/错误 → 回退 env 并打日志。`Config` 增加 `APIBaseURL string` 与 `Nacos.APIBaseURLDataID string`。

**注册上报**：`gateway/cmd/main.go` 解析后，注册与心跳请求体增加 `apiBaseURL` 字段。

**chart**：`deploy/charts/gateway/values.yaml` 增加 `config.apiBaseURL` 与 `config.nacos.apiBaseURLDataID`，注入 Deployment/DaemonSet/StatefulSet 环境变量。

### 2. Server 侧：存储与设备信息增强

- `GatewayInfo`（`server/internal/gateway/types.go`）增加 `APIBaseURL string`。
- `GatewayRegisterHandler`（`server/internal/gateway/handlers.go:63-92`）body 增加可选 `apiBaseURL`（不做 required 校验，旧版 Gateway 兼容）。心跳处理如已有字段更新逻辑则同步携带。
- 持久化：
  - Postgres：`models.GatewayRegistry` 加 `api_base_url` 列 + migration；`store_postgres.go` upsert 带上。
  - Redis / memory store：同步携带字段。
- 设备信息接口：`deviceToMap`（`server/internal/handlers/device.go:135`）为每个设备附加 `clusterAPIURL`：
  - `registry.GetDeviceGateway(deviceID)` 命中且 `gw.APIBaseURL != ""` → 填该值；
  - 设备离线/未绑定/Gateway 无该字段 → `null`。
  - 列表接口（`ListDevicesHandler`）与详情接口（`GetDeviceHandler`）一致处理。列表场景注意 N+1：`GetDeviceGateway` 当前实现是 `GetDeviceGateway` + `ListGateways` 两次存储调用，列表接口应改为一次 `ListGateways` 构 map 后批量匹配。

### 3. 前端：按设备归属选择 API origin

改动集中在 `portal/packages/app-ai-native`：

- `DeviceResponse` / `normalizeDevice`（`pages/workspace/lib/api.ts:47-87`）透传 `clusterAPIURL`。
- `getProxyUrl`（`pages/workspace/lib/url.ts:3-6`）改为接收 device 对象（或 clusterAPIURL 参数）：
  - `clusterAPIURL` 为空 → `${env.APP_URL}/cloud/device/${deviceId}/proxy`（现状）；
  - 与当前页面 origin 相同 → 同上（同源，避免无谓跨域）；
  - 不同 → `${clusterAPIURL}/cloud/device/${deviceId}/proxy`。
  - 调用点（`layout.tsx:305,311,335,585`、`mobile/layout.tsx`、`mobile/detail.tsx`）本来就有 device 对象在手，直接传入。
- SSE（`device-client.ts:273-328`）、会话（`conversation.create`）、终端（`cloud-terminal-api.ts`，含其 WebSocket）全部基于该 proxied base URL，自动跟随，无需单独修改。
- **失效重试**：对远端 origin 的 proxy 请求若返回"设备未连接"类错误，前端清除该设备缓存的 `clusterAPIURL`，重新拉取设备详情后重试一次（覆盖设备重连后换了集群的场景）。
- `pages/store/lib/api.ts` 中的设备命令调用（`/cloud/device/${deviceId}/proxy/api/v1/commands`）同样改为经 `getProxyUrl` 构造。

### 4. 鉴权与 CORS

- 跨域后请求从同源变跨域。`/cloud/*` 使用 `RequireAuth(casdoor)`，`apiFetch` 不带 credentials（Authorization 头方式），跨域 + CORS allowlist 即可工作。
- **验证点**：`cloud-terminal-api.ts` 使用了 `credentials: "include"`，需确认其鉴权是否依赖 cookie；若依赖，统一改为 Authorization 头（避免 SameSite 跨域问题）。
- 部署配置：两个集群的 `CORS_ALLOWED_ORIGINS` 互相加上对方集群的 Web 域名（中间件已支持 allowlist，纯配置）。
- WebSocket（终端 `input-ws`）跨域不受 CORS 限制，但需确认其鉴权 token 走 query/header 而非 cookie。

### 5. 兼容与边界

- **单集群部署零变化**：不配 `apiBaseURL` → 上报空 → `clusterAPIURL` 为 null → 前端走原逻辑。
- **旧版 Gateway**：无该字段 → 同上，向后兼容。
- **设备换集群**：设备重连（region 变更等）后绑定更新，下次拉取设备信息即得新 `clusterAPIURL`；前端失效重试兜底。
- **多 Gateway 同集群**：同集群所有 Gateway 上报相同的 `apiBaseURL`（同一 chart value/Nacos dataId），天然一致。

### 6. 明确不做（YAGNI）

- Server 不做跨集群转发/中继；不改 CoreDNS、不需要 hostAliases。
- 设备分配逻辑（`Allocate`/region 强制）本次不动。
- `/ws/sessions/:id` 长连接不在本次范围（该端点服务端已废弃，portal 不使用）。
- 集群数量假设为 2 个，但设计本身不限制集群数（每对集群互配 CORS 即可）。

## 错误处理

| 场景 | 行为 |
|---|---|
| Nacos 不可达/404 | Gateway 回退 `GATEWAY_API_BASE_URL` env，打 warn 日志 |
| `apiBaseURL` 非法 URL | Gateway 启动时校验（scheme http/https + host 非空），非法则回退 env 并打日志 |
| 前端跨域请求失败（网络/CORS） | 按现有错误提示展示；不重试（区别于"设备未连接"的路由失效重试） |
| 设备操作中换集群 | proxy 返回设备未连接 → 前端失效重试一次 → 拿到新 clusterAPIURL |

## 测试

- Gateway：resolver 单测（Nacos 命中/404 回退/env 回退/非法 URL 回退），注册请求体含 `apiBaseURL`。
- Server：注册 handler 兼容有无 `apiBaseURL`；store 三实现（postgres/redis/memory）字段往返；`deviceToMap` 附加 `clusterAPIURL` 的三种场景（命中/离线/空值）；列表接口批量匹配无 N+1。
- 前端：`getProxyUrl` 三分支单测；失效重试逻辑单测。
- 端到端：单集群部署回归（不配 apiBaseURL，行为与现状一致）。

## 部署变更清单（交付侧）

1. 每集群 Nacos 发布 `apiBaseURL` dataId（纯文本，值为该集群 Server 公网 API 地址），或配置 `GATEWAY_API_BASE_URL`。
2. 每集群 Server 的 `CORS_ALLOWED_ORIGINS` 增加对方集群 Web 域名。
3. DB migration（`gateway_registry.api_base_url`）。
