# 修复 Session Prompt 大文本隧道报错

## 状态

Proposed | 2025-05-21

## 问题概述

在 `app-ai-native` 的 session tab 中，当用户输入大文本（尤其是包含文件附件和图片）时，通过 cs-cloud 隧道提交 prompt 会触发"隧道报错"，导致请求失败。

### 影响范围

- **用户影响**：无法提交包含大段代码、长文本或多图片附件的 prompt
- **功能影响**：核心 AI 助手对话功能在复杂场景下不可用
- **触发条件**：
  - Prompt 文本超过约 1-2 万字符
  - 包含文件附件（代码片段等）
  - 包含图片附件（base64 编码）
  - 组合以上多种内容

## 技术背景

### 请求链路

```
浏览器 (app-ai-native)
  → JSON POST /api/v1/conversations/{id}/prompt/async
    → Gateway (云端服务器)
      → WebSocket 连接
        → yamux 多路复用流
          → cs-cloud tunnel (本地设备)
            → handleStream() 解析原始 HTTP
              → proxyHTTP() 转发到本地 HTTP Server
                → handleProxy() → TransformPromptBody → ReverseProxy
                  → Agent 后端 (CS/CSC)
```

### 关键组件

- **yamux**: 基于 WebSocket 的多路复用协议，默认流窗口 256KB
- **WebSocket**: nhooyr.io/websocket v1.8.17
- **cs-cloud tunnel**: 实现于 `internal/tunnel/proxy.go` 和 `connect.go`
- **Agent Proxy**: 实现于 `internal/localserver/proxy.go`

## 根因分析

### 根因 1：`handleStream` 完整缓冲请求体（主要瓶颈）

**文件**: `cs-cloud/internal/tunnel/proxy.go`

```go
buf := make([]byte, 64*1024)
var header []byte

// 阶段1：读 header 直到 \r\n\r\n
for {
    n, err := stream.Read(buf)
    if err != nil { return }     // ← 静默丢弃，无任何错误响应！
    header = append(header, buf[:n]...)
    // ...
}

// 阶段2：根据 Content-Length 把整个 body 读进内存
if contentLength >= 0 {
    for len(body) < contentLength {
        n, err := stream.Read(buf)
        if err != nil { return } // ← 静默丢弃！
        body = append(body, buf[:n]...)
    }
}
```

**问题**：
1. **全量内存缓冲**：整个 body（可能包含 base64 编码的图片、文件附件）被完整读入 `[]byte`
2. **零错误反馈**：任何读取错误都静默 `return`，客户端只能等到连接超时
3. **无大小限制**：没有 `maxBodySize` 保护，可能导致 OOM

### 根因 2：yamux 流窗口限制

**文件**: `cs-cloud/internal/tunnel/connect.go`

```go
yamuxCfg := yamux.DefaultConfig()
yamuxCfg.ConnectionWriteTimeout = 60 * time.Second
// MaxStreamWindowSize 未设置，使用默认值 256KB
```

**问题**：
- yamux v0.1.2 默认 `MaxStreamWindowSize` 为 **256KB**
- Gateway 写入大 body 时，流窗口会耗尽
- 写操作阻塞等待窗口更新
- 超过 60 秒 `ConnectionWriteTimeout` 时整个 yamux session 断开

### 根因 3：请求体被多次完整读入内存

对于 `/conversations/{id}/prompt/async` 端点，请求体被完整读入内存 **3-4 次**：

1. **`handleStream`** (`proxy.go`)：`body = append(body, buf[:n]...)`
2. **`TransformPromptBody`** (`proxy_helpers.go`)：`buf, err := io.ReadAll(body)`
3. **`handleProxy`** (`proxy.go`)：`buf, err := io.ReadAll(r.Body)`
4. **CSC Adapter** (`csc/adapter.go`)：`body, _ := io.ReadAll(r.Body)`

一个 10MB 的 prompt 请求，峰值内存可能达到 **40MB+**。

## 解决方案

### 方案 1：流式代理改造（P0，根治方案）

**目标**：消除 `handleStream` 中的完整 body 缓冲

**实现**：

```go
const maxBodySize = 50 * 1024 * 1024 // 50MB 保护上限

func handleStream(stream net.Conn, localPort int) {
    defer stream.Close()

    br := bufio.NewReaderSize(stream, 64*1024)

    // 读请求行和 headers
    requestLine, err := br.ReadString('\n')
    if err != nil {
        writeErrorResponse(stream, 400, "Bad Request")
        return
    }

    headers := parseHeaders(br)

    contentLength := -1
    if cl, ok := headers["content-length"]; ok {
        fmt.Sscanf(cl, "%d", &contentLength)
    }

    // 大小保护
    if contentLength > maxBodySize {
        writeErrorResponse(stream, 413, "Request Entity Too Large")
        return
    }

    // 连接本地服务器
    localConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
    if err != nil {
        writeErrorResponse(stream, 502, "Bad Gateway")
        return
    }
    defer localConn.Close()

    // 写 header
    writeHeaders(localConn, requestLine, headers)

    // 流式转发 body（不再全量缓冲）
    if contentLength > 0 {
        if _, err := io.CopyN(localConn, br, int64(contentLength)); err != nil {
            return
        }
    }

    // 流式转发响应
    go func() {
        io.Copy(stream, localConn)
        stream.Close()
    }()
    io.Copy(localConn, br)
}

func writeErrorResponse(stream net.Conn, code int, message string) {
    resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", code, message)
    stream.Write([]byte(resp))
    stream.Close()
}
```

**优势**：
- 内存使用从 O(body_size) 降为 O(buffer_size)
- 消除 `append` 带来的内存重分配
- 支持更大的请求体

**风险**：
- 需要充分测试边界条件（超时、中断等）
- 需要确保 yamux 流的正确关闭

### 方案 2：增大 yamux 流窗口（P0，快速缓解）

**文件**: `cs-cloud/internal/tunnel/connect.go`

```go
yamuxCfg := yamux.DefaultConfig()
yamuxCfg.MaxStreamWindowSize = 4 * 1024 * 1024  // 4MB，从默认 256KB 提升
yamuxCfg.ConnectionWriteTimeout = 120 * time.Second // 适当延长超时
```

**优势**：
- 改动极小，风险低
- 立即缓解大 body 传输问题
- 60 秒超时对复杂 prompt 处理可能不够，延长到 120 秒更安全

**权衡**：
- 增加内存占用（每个流 4MB vs 256KB）
- 不解决内存缓冲问题，只是减少窗口更新频率

### 方案 3：减少 body 重复读取（P1）

**文件**: `cs-cloud/internal/localserver/proxy.go`

当前代码：
```go
if transformFunc != nil && r.Body != nil {
    r.Body = transformFunc(r.Body)
    buf, err := io.ReadAll(r.Body)  // ← 不必要！Pipe 已经返回了 reader
    r.Body.Close()
    r.Body = io.NopCloser(bytes.NewReader(buf))
    r.ContentLength = int64(len(buf))
}
```

优化后：
```go
if transformFunc != nil && r.Body != nil {
    r.Body = transformFunc(r.Body)
    // 不再 io.ReadAll，让 ReverseProxy 自行流式读取
    r.ContentLength = -1
    r.Header.Del("Content-Length")
    r.Header.Set("Transfer-Encoding", "chunked")
}
```

**优势**：
- 减少 1 次完整 body 读取
- 配合方案 1 可进一步降低内存占用

### 方案 4：前端图片上传优化（P2，中长期）

**文件**: `opencode/packages/app-ai-native/src/components/prompt-input/build-request-parts.ts`

当前实现：图片以 base64 data URL 内联在 JSON 中
```typescript
const images = input.images.map((attachment) => {
    return {
        id: Identifier.ascending("part"),
        type: "file",
        mime: attachment.mime,
        url: attachment.dataUrl,  // ← base64 data URL
        filename: attachment.filename,
    }
})
```

优化方案：
1. 先通过独立接口上传图片，获取临时 URL
2. Prompt 请求中只发送 URL 引用
3. Body 大小从 O(图片大小) 降为 O(URL 长度)

**优势**：
- 从源头减少 prompt body 体积
- 图片可缓存，避免重复传输

**复杂度**：
- 需要设计临时图片存储和清理机制
- 需要前后端配合改动

### 方案 5：添加错误响应机制（P0，必须配合）

**目标**：让客户端能收到明确的错误信息，而不是静默失败

在 `handleStream` 和 `proxyHTTP` 中添加错误响应：

```go
func writeHTTPErrorResponse(conn net.Conn, statusCode int, statusText string) {
    response := fmt.Sprintf("HTTP/1.1 %d %s\r\n", statusCode, statusText)
    response += "Content-Type: application/json\r\n"
    response += fmt.Sprintf("Content-Length: %d\r\n", len(errorBody))
    response += "\r\n"
    response += errorBody
    conn.Write([]byte(response))
}

// 使用示例
if contentLength > maxBodySize {
    writeHTTPErrorResponse(stream, 413, "Request Entity Too Large",
        `{"error":"REQUEST_TOO_LARGE","message":"request body exceeds 50MB limit"}`)
    return
}
```

## 实施计划

### Phase 1：快速缓解（1-2 天）

| 任务 | 优先级 | 预估工时 |
|------|--------|----------|
| 增大 yamux 流窗口到 4MB | P0 | 0.5h |
| 延长 ConnectionWriteTimeout 到 120s | P0 | 0.5h |
| 添加错误响应机制 | P0 | 2h |
| 添加日志和监控 | P1 | 2h |

**目标**：立即缓解大文本报错问题，让用户能看到明确错误信息。

### Phase 2：根本解决（3-5 天）

| 任务 | 优先级 | 预估工时 |
|------|--------|----------|
| handleStream 流式代理改造 | P0 | 1-2d |
| 减少 body 重复读取 | P1 | 0.5d |
| 单元测试和集成测试 | P0 | 1d |
| 压力测试和性能基准 | P1 | 0.5d |
| 灰度发布和监控 | P0 | 1d |

**目标**：从根本上解决内存和性能问题，支持更大规模的请求。

### Phase 3：前端优化（可选，按需实施）

| 任务 | 优先级 | 预估工时 |
|------|--------|----------|
| 设计临时图片上传 API | P2 | 1d |
| 前端实现图片上传逻辑 | P2 | 1-2d |
| 后端实现图片存储服务 | P2 | 2-3d |
| 图片清理和 GC 机制 | P2 | 1d |

**目标**：从源头减少 prompt body 体积，提升整体性能。

## 测试计划

### 单元测试

1. **`handleStream` 边界条件**：
   - 正常大小请求（< 256KB）
   - 大请求（256KB - 4MB）
   - 超大请求（> 4MB）
   - 缺失 Content-Length
   - 读取中断模拟

2. **流控测试**：
   - yamux 窗口耗尽场景
   - 慢速网络模拟
   - 超时场景

### 集成测试

1. **端到端流程**：
   - 小文本 prompt
   - 大文本 prompt（1MB+）
   - 包含文件附件
   - 包含图片附件
   - 组合场景

2. **压力测试**：
   - 并发请求
   - 大请求 + 小请求混合
   - 长时间运行稳定性

### 性能基准

| 指标 | 改造前 | 改造后目标 |
|------|--------|-----------|
| 10MB prompt 内存峰值 | ~40MB | < 15MB |
| 10MB prompt 处理时间 | ~5s | < 2s |
| 支持 prompt 最大大小 | ~2MB | > 50MB |
| 并发 10 个 5MB 请求 | 经常失败 | 稳定处理 |

## 风险评估

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| 流式代理引入新 bug | 高 | 充分的单元测试和集成测试 |
| 内存泄漏 | 中 | 使用 pprof 进行内存分析 |
| 连接泄漏 | 中 | 确保所有 conn 正确关闭 |
| 向后兼容性 | 低 | WebSocket 协议层面无变化 |
| 性能回归 | 低 | 建立性能基准对比 |

## 监控和告警

### 新增指标

1. **请求大小分布**：P50, P95, P99
2. **处理耗时**：按请求大小分桶
3. **内存使用**：按请求大小分桶
4. **错误率**：按错误类型分类
5. **yamux 流状态**：窗口使用率、超时次数

### 告警规则

1. **大请求错误率 > 1%**：P1 告警
2. **处理耗时 P99 > 30s**：P2 告警
3. **内存使用 > 80%**：P1 告警
4. **yamux 连接断开率 > 0.1%**：P1 告警

## 参考文档

- [HTTP_TUNNEL_DESIGN.md](./HTTP_TUNNEL_DESIGN.md)：cs-cloud 隧道设计文档
- [DEVICE_GATEWAY_DESIGN.md](./DEVICE_GATEWAY_DESIGN.md)：设备网关设计文档
- yamux 文档：https://github.com/hashicorp/yamux
- nhooyr.io/websocket 文档：https://github.com/nhooyr/io/websocket

## 变更历史

| 日期 | 版本 | 变更内容 | 作者 |
|------|------|----------|------|
| 2025-05-21 | 0.1 | 初稿 | Claude |
