# 重连场景验证计划

当前架构：`Browser → Vite → Server(:8080) → Gateway(:8081) → yamux/WebSocket → cs cloud → opencode`

---

## 场景一（🔴 高）：隧道断连期间请求打到失效绑定

### 问题描述

cs cloud 与 gateway 的 WebSocket 断开后，cs cloud 会指数退避重连（1s→2s→…→60s）。
在此期间：
- server 的 Redis 中 device→gateway 绑定仍然有效
- 浏览器发来的请求会被 server 转发到 gateway
- gateway 的 `TunnelManager` 中已无该设备的 yamux session
- gateway 返回 `503 device tunnel not connected`

### 已修复

`server/internal/gateway/client.go`：
- gateway 整体不可达（网络错误）→ 立即写 `503 + Retry-After: 2`，不再返回 error 由上层二次写 header
- gateway 返回 503 → 透传 body，同时补写 `Retry-After: 2`

### 前端行为（已确认）

**SSE（`/global/event`）**：`serverSentEvents.gen.ts` 的 `createSseClient` 已实现完整自动重连：
- 任何非 2xx 响应或网络错误都会触发 catch → 指数退避重试（默认 3s，最大 30s）
- `sseMaxRetryAttempts` 默认无限重试
- ✅ 无需额外处理

**WebSocket（`/pty/:id/connect`）**：`components/terminal.tsx` 中：
- `handleClose`：code !== 1000 时调用 `onConnectError` → 上层 toast 提示，**不自动重连**
- `handleError`：同样只报错，不重连
- ⚠️ pty WebSocket 断线后需用户手动操作（可接受，pty 本身有状态，自动重连需恢复 cursor 位置）

### 测试用例

```
[TC-1a-1] 隧道断连期间 HTTP 请求返回 503+Retry-After
  前置：cs cloud 已连接，手动 kill gateway 进程
  操作：浏览器发送任意 API 请求（如 GET /global/config）
  预期：返回 503，响应头含 Retry-After: 2

[TC-1a-2] SSE 在隧道断连后自动重连
  前置：SSE 流（/global/event）已建立
  操作：重启 gateway 进程，等待 cs cloud 重连（约 1-5s）
  预期：SSE 客户端在重连成功后自动恢复，无需用户干预，toast 不出现

[TC-1a-3] pty WebSocket 断线提示
  前置：pty WebSocket 已建立
  操作：重启 gateway 进程
  预期：终端显示 toast 错误提示（code != 1000），不自动重连
```

---

## 场景二（🔴 高）：Server 重启后 Gateway 重注册

### 问题描述

Server 重启后，Redis 中的 gateway 注册信息在 60s（`GatewayHeartbeatTimeoutMs`）后过期。
Gateway 的心跳若失败（404 gateway not registered），需要自动重新注册并重通知所有在线设备。

### 现状（已有逻辑，需验证）

`gateway/cmd/main.go` 的 `heartbeatLoop`：
```go
epoch, err := gw.Heartbeat(...)
if err != nil {
    // 心跳失败 → 重注册 → NotifyAllOnline
    gw.Register(...)
    manager.NotifyAllOnline(...)
}
if lastEpoch != 0 && epoch != lastEpoch {
    // epoch 变化 → 重通知（检测 server 重启）
    manager.NotifyAllOnline(...)
}
```

逻辑正确，但 epoch 是进程内变量，多 server 实例下会误触发（见场景二b）。

### 已修复：epoch 多实例问题

**`server/internal/gateway/store.go`**：`Store` 接口新增 `GetOrInitEpoch(initVal int64) (int64, error)`

**`server/internal/gateway/store_redis.go`**：
```go
func (s *RedisStore) GetOrInitEpoch(initVal int64) (int64, error) {
    // SETNX server:epoch initVal → 若已存在则返回已有值
    // 多实例共享同一 epoch，server 重启后新实例写入新值（SETNX 无 TTL）
}
```

**`server/internal/gateway/registry.go`**：`NewGatewayRegistry` 从 store 读取共享 epoch，所有实例返回相同值。

> **注意**：`server:epoch` key 无 TTL，server 重启后新实例会因 SETNX 失败而复用旧 epoch。
> 这意味着 gateway 的 epoch 变化检测**只在 Redis 数据被清空时**才能感知到 server 重启。
> 实际生产中 server 重启后 gateway 依赖心跳失败（404）触发重注册，epoch 机制是辅助手段。

### 测试用例

```
[TC-2a-1] Server 重启后 gateway 自动重注册（单实例）
  前置：gateway 已注册，cs cloud 已连接
  操作：重启 server 进程，等待 60s（heartbeat timeout）
  预期：gateway 心跳收到 404 → 自动重注册 → NotifyAllOnline → 设备绑定恢复
  验证：重启后发送 API 请求，应在 90s 内（60s 超时 + 30s 心跳间隔）恢复正常

[TC-2a-2] Server 重启后 gateway 快速重注册（epoch 变化）
  前置：使用 MemoryStore（非 Redis），gateway 已注册
  操作：重启 server 进程
  预期：gateway 心跳检测到 epoch 变化 → 立即 NotifyAllOnline（无需等 60s）
  注意：Redis 模式下 epoch 不变，此场景仅适用于 MemoryStore 或 Redis 被清空

[TC-2b-1] 多 server 实例下 epoch 稳定
  前置：启动 2 个 server 实例，共享同一 Redis
  操作：gateway 心跳打到不同实例
  预期：所有实例返回相同 epoch，gateway 不会误触发 NotifyAllOnline

[TC-2b-2] 多 server 实例滚动重启
  前置：2 个 server 实例运行中
  操作：重启其中一个实例
  预期：重启实例从 Redis 读取已有 epoch，心跳响应 epoch 不变，gateway 不误触发
```

---

## 场景三（🟡 中）：Gateway 下线时主动清除设备绑定

### 问题描述

Gateway 进程退出（崩溃或 SIGTERM）时：
- 该 gateway 上所有设备的 yamux session 断开
- cs cloud 重连后 `gateway-assign` 会分配到其他 gateway
- 但 server Redis 中的 device→gateway 绑定仍指向旧 gateway
- 在 cs cloud 重连并 `NotifyOnline` 之前，浏览器请求会被路由到已下线的 gateway → 502

**当前清除时机**：依赖 heartbeat timeout（60s）后 `doCleanup` 删除 gateway，进而使 `GetDeviceGateway` 找不到 gateway 返回 404。

### 已实现

**`gateway/internal/registration.go`**：
- 新增 `NotifyAllOffline`：遍历 TunnelManager 中所有设备，逐一调用 `NotifyOffline`
- 新增 `Deregister`：`DELETE /internal/gateway/:gatewayID` 注销自身

**`server/internal/gateway/registry.go`**：新增 `Deregister(gatewayID)` 方法

**`server/internal/gateway/handlers.go`**：
- 新增 `GatewayDeregisterHandler`（`DELETE /internal/gateway/:gatewayID`）
- 路由已注册到 `RegisterInternalRoutes`

**`gateway/cmd/main.go`**：
- 改用 `http.Server` + `srv.Shutdown(ctx)` 替代 `r.Run()`
- 捕获 `SIGTERM`/`SIGINT` → `NotifyAllOffline` → `Deregister` → `Shutdown`

### 测试用例

```
[TC-3a-1] Gateway 优雅退出（SIGTERM）
  前置：gateway 已注册，2 个设备已连接
  操作：向 gateway 发送 SIGTERM
  预期：
    - server 收到 2 次 device/offline 通知，Redis 中设备绑定清除
    - server 收到 gateway 注销请求，Redis 中 gateway 注册清除
    - cs cloud 重连后可正常分配到其他 gateway

[TC-3a-2] Gateway 崩溃（非优雅退出）
  前置：gateway 已注册，1 个设备已连接
  操作：kill -9 gateway 进程
  预期：
    - 60s 内 heartbeat timeout → doCleanup 删除 gateway
    - cs cloud 重连后 gateway-assign 返回其他可用 gateway
    - 60s 内浏览器请求返回 503/502（可接受的降级）

[TC-3a-3] Gateway 下线期间浏览器请求的降级行为
  前置：gateway 已注册，设备已连接
  操作：kill -9 gateway，立即发送 API 请求
  预期：返回 502（gateway unreachable），含 Retry-After: 2
```

---

## 场景四（🟡 中）：前端 SSE / WebSocket 断线重连

### SSE（已确认 ✅）

`packages/sdk/js/src/v2/gen/core/serverSentEvents.gen.ts` 的 `createSseClient`：
- 任何错误（网络、非 2xx 响应）→ 指数退避重试，默认无限次
- 支持 `Last-Event-ID` header，服务端可实现断点续传
- **当前 opencode server 的 `/global/event` 不使用 `id:` 字段**，重连后会重新全量推送

### WebSocket（已确认，不自动重连）

`packages/app/src/components/terminal.tsx`：
- 异常断线（code !== 1000）→ 调用 `onConnectError` → toast 提示
- **不自动重连**，需用户手动重新打开终端
- 这是合理设计：pty 有 cursor 状态，自动重连需要 `cursor` 参数对齐

### 测试用例

```
[TC-4a-1] SSE 断线后自动重连并恢复事件流
  前置：SSE 流已建立，前端显示正常
  操作：重启 server 进程（模拟 SSE 断线）
  预期：
    - SSE 客户端自动重连（约 3s 后）
    - 重连后事件流恢复，前端状态正常
    - 不出现错误 toast

[TC-4a-2] SSE 在 503 期间的退避行为
  前置：SSE 流已建立
  操作：停止 cs cloud（使隧道断连）
  预期：
    - SSE 请求收到 503
    - 客户端指数退避重试（3s, 6s, 12s...）
    - cs cloud 重连后，SSE 在下次重试时恢复

[TC-4b-1] pty WebSocket 异常断线提示
  前置：pty WebSocket 已建立，终端正常使用
  操作：重启 gateway 进程
  预期：
    - 终端显示 toast 错误（"Connection lost"）
    - 不自动重连
    - 用户重新打开终端后可正常连接
```

---

## 优先级汇总与实施状态

| 优先级 | 场景 | 核心问题 | 状态 |
|--------|------|----------|------|
| 🔴 高 | 1a | 隧道断连期间请求 → 503 无 Retry-After | ✅ 已修复 |
| 🔴 高 | 2a | gateway 心跳失败不重注册 | ✅ 逻辑已有，已验证正确 |
| 🔴 高 | 2b | epoch 在多 server 实例下不稳定 | ✅ 已修复（Redis 共享 epoch） |
| 🟡 中 | 1b/3a | gateway 下线未主动清除设备绑定 | ✅ 已实现（SIGTERM 优雅退出） |
| 🟡 中 | 4a | SSE 断线重连 | ✅ SDK 已实现，无需改动 |
| 🟡 中 | 4b | pty WebSocket 断线不自动重连 | ✅ 已确认，当前行为合理 |
