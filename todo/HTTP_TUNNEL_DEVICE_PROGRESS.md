# HTTP 隧道 - 设备端改造进度

基于 `docs/HTTP_TUNNEL_DESIGN.md` v1.1.0，设备端（`packages/opencode`）任务跟踪。

---

## 改造范围

**弃用：**
- `src/costrict/device/sse.ts` — SSE 连接逻辑全部废弃

**新增：**
- `src/costrict/device/tunnel.ts` — yamux over WebSocket 隧道客户端

**修改：**
- `src/cli/cmd/cloud.ts` — 将 `connect()` 调用替换为 `tunnel.connect()`

---

## 任务清单

### 1. `src/costrict/device/tunnel.ts`（新建）

- [ ] `wsConn` 适配：使用 Bun 原生 `WebSocket`，封装为 `ReadableStream`/`WritableStream`
- [ ] `yamuxClient`：在 WebSocket 之上建立 yamux client session
  - 连接目标：`ws://<gatewayURL>/device/:deviceID/tunnel`
  - yamux client 模式：等待 Gateway `session.Open()` → `Accept()` 循环
- [ ] `handleStream(stream)`：
  - 从 stream 读取原始 HTTP 请求字节
  - 转发给本地 `http://127.0.0.1:<localPort>`
  - 将响应字节写回 stream
- [ ] `connect(localPort: number): Promise<void>`：
  - 加载 device 信息
  - `assignGateway()` 获取 gatewayURL
  - 建立 WebSocket + yamux session
  - 进入 `Accept()` 循环
  - session 断开后抛出，由外层重连循环处理
- [ ] 重连循环（指数退避，1s → 最大 60s）
- [ ] 日志：连接建立、stream 接收、转发错误、断开重连

### 2. `src/cli/cmd/cloud.ts`（修改）

- [ ] 移除 `import { connect } from "../../costrict/device/sse"`
- [ ] 改为 `import { connect } from "../../costrict/device/tunnel"`
- [ ] 更新 `describe` 字段：`"register device and connect to cloud via WebSocket tunnel"`

### 3. `src/costrict/device/sse.ts`（弃用）

- [ ] 文件保留但标记为废弃（不删除，避免影响其他潜在引用）
- [ ] 或直接删除（确认无其他 import 后）

---

## 技术说明

### yamux 协议（TypeScript 实现）

yamux 是二进制多路复用协议，需要实现以下帧格式：

```
+-------+-------+------+--------+
| version(1) | type(1) | flags(2) |
+-------+-------+------+--------+
| stream_id (4 bytes)            |
+--------------------------------+
| length (4 bytes)               |
+--------------------------------+
| data (length bytes)            |
+--------------------------------+
```

帧类型：
- `0x0` Data — 数据帧
- `0x1` WindowUpdate — 流量控制
- `0x2` Ping
- `0x3` GoAway

client 模式下 stream_id 为奇数（1, 3, 5...），server 模式为偶数。
Gateway 是 yamux server（偶数 ID），cs cloud 是 yamux client（接受 server 发来的 stream）。

### HTTP 转发

stream 中传输的是原始 HTTP/1.1 报文，直接透传给本地 cs serve，无需解析。

```
stream bytes → fetch('http://127.0.0.1:<port>', { ... }) → response bytes → stream
```

由于 Bun/Node 的 `fetch` 不支持直接传原始字节，需要：
1. 解析 stream 中的 HTTP 请求头（method、path、headers）
2. 用 `fetch` 重新构造请求
3. 将 `fetch` 响应序列化为 HTTP/1.1 响应格式写回 stream

### WebSocket 传输

Bun 原生 `WebSocket` 支持 binary message，可直接用于传输 yamux 帧，无需额外依赖。

---

## 完成状态

### 1. `src/costrict/device/tunnel.ts`（新建）
- [x] `YamuxSession`：WebSocket binary message 缓冲 + yamux 帧解析/分发
- [x] `YamuxStream`：chunk 队列 + Promise-based read/write/close
- [x] 帧处理：SYN（新 stream）、DATA、FIN、RST、PING、GO_AWAY
- [x] 流量控制：收到数据后立即发送 WindowUpdate 补充窗口
- [x] `readHTTPRequest`：解析原始 HTTP/1.1 请求（含 Content-Length body 读取）
- [x] `serializeResponse`：普通响应序列化为 HTTP/1.1 格式
- [x] `serializeStreamingResponse`：SSE/流式响应转 chunked 编码持续转发
- [x] `handleStream`：HTTP 转发核心逻辑，自动识别流式/非流式响应
- [x] `runSession`：WebSocket 连接 + yamux Accept 循环
- [x] `connect`：指数退避重连循环（1s → 最大 60s）

### 2. `src/cli/cmd/cloud.ts`（修改）
- [x] import 从 `./sse` 改为 `./tunnel`
- [x] describe 更新为 `"register device and connect to cloud via WebSocket tunnel"`

### 3. `src/costrict/device/sse.ts`（删除）
- [x] 确认无其他引用后删除

---

## 进度记录

| 日期 | 内容 |
|------|------|
| 2026-03-13 | 创建进度文档，确认实施范围 |
| 2026-03-13 | 完成全部设备端改造：tunnel.ts 新建、cloud.ts 修改、sse.ts 删除 |
