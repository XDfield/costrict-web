# 隧道大 Prompt 传输修复进度

基于 `docs/proposals/TUNNEL_LARGE_PROMPT_FIX.md`，修复 Session Prompt 大文本隧道报错问题的任务跟踪。

---

## 问题概述

在 `app-ai-native` 的 session tab 中，当用户输入大文本（尤其是包含文件附件和图片）时，通过 cs-cloud 隧道提交 prompt 会触发"隧道报错"，导致请求失败。

**核心问题**：
1. `handleStream` 完整缓冲请求体到内存
2. yamux 流窗口限制（默认 256KB）
3. 请求体被多次完整读入内存

---

## 改造范围

**cs-cloud（主要修改）**：
- `internal/tunnel/proxy.go` — `handleStream` 改为流式代理
- `internal/tunnel/connect.go` — 增大 yamux 流窗口配置
- `internal/localserver/proxy.go` — 减少 body 重复读取

**app-ai-native（可选优化）**：
- `src/components/prompt-input/build-request-parts.ts` — 图片上传优化

---

## 任务清单

### Phase 1：快速缓解（1-2 天）

#### 1.1 增大 yamux 流窗口
**文件**: `cs-cloud/internal/tunnel/connect.go`

- [ ] 设置 `yamuxCfg.MaxStreamWindowSize = 4 * 1024 * 1024`（4MB）
- [ ] 设置 `yamuxCfg.ConnectionWriteTimeout = 120 * time.Second`
- [ ] 添加注释说明配置原因
- [ ] 验证 yamux v0.1.2 支持 MaxStreamWindowSize 配置

**预期效果**：减少窗口更新频率，降低超时风险

#### 1.2 添加错误响应机制
**文件**: `cs-cloud/internal/tunnel/proxy.go`

- [ ] 实现 `writeHTTPErrorResponse(conn, statusCode, statusText, body)`
- [ ] 在 `handleStream` 中添加错误响应：
  - [ ] header 解析失败 → 400 Bad Request
  - [ ] body 读取失败 → 400 Bad Request
  - [ ] 本地服务器连接失败 → 502 Bad Gateway
  - [ ] Content-Length 超限 → 413 Request Entity Too Large
- [ ] 在 `proxyHTTP` 和 `proxyWebSocket` 中添加错误处理

**预期效果**：客户端能收到明确错误信息，而非静默失败

#### 1.3 添加请求体大小限制
**文件**: `cs-cloud/internal/tunnel/proxy.go`

- [ ] 定义常量 `const maxBodySize = 50 * 1024 * 1024`（50MB）
- [ ] 在 `handleStream` 中检查 `contentLength > maxBodySize`
- [ ] 超限时返回 413 错误并记录日志

**预期效果**：防止恶意请求导致 OOM

#### 1.4 添加日志和监控
**文件**: `cs-cloud/internal/tunnel/proxy.go`

- [ ] 记录请求大小（Content-Length）
- [ ] 记录处理耗时（从 Accept 到响应完成）
- [ ] 记录错误类型和频率
- [ ] 添加 Prometheus metrics（如果项目使用）

---

### Phase 2：根本解决（3-5 天）

#### 2.1 `handleStream` 流式代理改造
**文件**: `cs-cloud/internal/tunnel/proxy.go`

**核心逻辑变更**：
- [ ] 引入 `bufio.Reader` 替代手动 buffer 管理
- [ ] 使用 `ReadString('\n')` 读取请求行和 headers
- [ ] 使用 `io.CopyN` 流式转发 body
- [ ] 移除 `body = append(body, buf[:n]...)` 的完整缓冲逻辑
- [ ] 确保 response 也使用 `io.Copy` 流式转发

**边界条件**：
- [ ] 缺失 Content-Length 的处理
- [ ] chunked transfer encoding 支持（可选）
- [ ] 连接中断场景测试
- [ ] 超时场景测试

**测试**：
- [ ] 正常大小请求（< 256KB）
- [ ] 中等请求（256KB - 4MB）
- [ ] 大请求（4MB - 50MB）
- [ ] 超大请求（> 50MB，应返回 413）
- [ ] 并发 10 个 5MB 请求

#### 2.2 减少 body 重复读取
**文件**: `cs-cloud/internal/localserver/proxy.go`

- [ ] 移除 `transformFunc` 后的 `io.ReadAll(r.Body)`
- [ ] 直接使用 transformed body，让 ReverseProxy 流式读取
- [ ] 设置 `r.ContentLength = -1` 和 `Transfer-Encoding: chunked`
- [ ] 验证 CSC adapter 不受影响

**预期效果**：减少 1 次完整 body 读取

#### 2.3 单元测试
**文件**: `cs-cloud/internal/tunnel/proxy_test.go`（新建）

- [ ] `TestHandleStream_NormalRequest`：测试正常请求处理
- [ ] `TestHandleStream_LargeRequest`：测试大请求（> 4MB）
- [ ] `TestHandleStream_ExceedLimit`：测试超限请求
- [ ] `TestHandleStream_MissingContentLength`：测试缺失 Content-Length
- [ ] `TestHandleStream_ReadError`：测试读取中断场景
- [ ] `TestWriteHTTPErrorResponse`：测试错误响应格式

#### 2.4 集成测试
**文件**: `cs-cloud/internal/tunnel/integration_test.go`（新建）

- [ ] `TestE2E_PromptSubmission`：端到端 prompt 提交
- [ ] `TestE2E_LargeTextPrompt`：大文本 prompt
- [ ] `TestE2E_ImageAttachment`：包含图片附件
- [ ] `TestE2E_FileAttachment`：包含文件附件
- [ ] `TestE2E_CombinedAttachments`：组合附件场景
- [ ] `TestE2E_ConcurrentRequests`：并发请求测试

#### 2.5 性能测试
**文件**: `cs-cloud/internal/tunnel/benchmark_test.go`（新建）

- [ ] `BenchmarkHandleStream_1MB`
- [ ] `BenchmarkHandleStream_10MB`
- [ ] `BenchmarkHandleStream_50MB`
- [ ] 内存使用分析（使用 pprof）
- [ ] 对比改造前后的性能基准

---

### Phase 3：前端优化（可选，按需实施）

#### 3.1 设计临时图片上传 API
**文件**: `docs/`（新建 API 设计文档）

- [ ] 设计图片上传接口（POST /api/v1/files/upload）
- [ ] 设计图片引用格式（临时 URL 或 file ID）
- [ ] 设计图片清理机制（TTL 或手动触发）
- [ ] 安全性考虑（文件类型校验、大小限制）

#### 3.2 前端实现图片上传逻辑
**文件**: `opencode/packages/app-ai-native/src/components/prompt-input/`

- [ ] 创建图片上传工具函数
- [ ] 修改 `build-request-parts.ts`，base64 → 引用
- [ ] 添加上传进度指示
- [ ] 添加上传失败重试逻辑

#### 3.3 后端实现图片存储服务
**文件**: `cs-cloud/internal/localserver/` 或独立服务

- [ ] 实现图片上传 handler
- [ ] 集成临时存储（本地文件系统或对象存储）
- [ ] 实现 TTL 清理机制
- [ ] 添加图片访问端点

---

## 技术说明

### yamux 流窗口机制

yamux v0.1.2 默认配置：
- `MaxStreamWindowSize`: 256KB（每个流未确认数据的最大值）
- `ConnectionWriteTimeout`: 60s（写操作超时）

当写入数据超过窗口大小时：
1. 写操作阻塞，等待接收方发送 WindowUpdate
2. 如果超过 ConnectionWriteTimeout 未收到确认，连接断开

增大窗口到 4MB 的权衡：
- ✅ 减少窗口更新频率，降低延迟
- ✅ 更适合大 body 传输
- ❌ 增加内存占用（每个流 4MB vs 256KB）
- ❌ 如果连接中断，丢失更多未确认数据

### 流式代理 vs 完整缓冲

**当前实现**（完整缓冲）：
```go
body := []byte{}
for len(body) < contentLength {
    n, _ := stream.Read(buf)
    body = append(body, buf[:n]...)  // 每次可能触发内存重分配
}
```
- 内存：O(contentLength)
- 时间：O(contentLength) + append 开销
- 风险：大请求 OOM

**流式实现**：
```go
io.CopyN(localConn, stream, int64(contentLength))
```
- 内存：O(bufferSize)，buffer 固定 32KB
- 时间：O(contentLength)
- 风险：无额外风险

### 多次 body 读取的内存开销

对于一个 10MB 的 prompt 请求：
1. `handleStream` 缓冲：10MB
2. `TransformPromptBody` 的 `io.ReadAll`：10MB
3. `handleProxy` 的 `io.ReadAll`：10MB
4. CSC adapter 的 `io.ReadAll`：10MB

**峰值内存**：~40MB（不包括 JSON 解析等其他开销）

流式改造后：
1. `handleStream` 流式复制：32KB 固定 buffer
2. `TransformPromptBody`：保持 `io.Pipe` 流式
3. `handleProxy`：移除 `io.ReadAll`，让 ReverseProxy 自行读取
4. CSC adapter：保持现状（adapter 层面的需求）

**峰值内存**：< 15MB

---

## 完成状态

### Phase 1：快速缓解
- [x] 1.1 增大 yamux 流窗口
- [x] 1.2 添加错误响应机制
- [x] 1.3 添加请求体大小限制
- [x] 1.4 添加日志和监控

### Phase 2：根本解决
- [x] 2.1 `handleStream` 流式代理改造
- [ ] 2.2 减少 body 重复读取
- [ ] 2.3 单元测试
- [ ] 2.4 集成测试
- [ ] 2.5 性能测试

### Phase 3：前端优化
- [ ] 3.1 设计临时图片上传 API
- [ ] 3.2 前端实现图片上传逻辑
- [ ] 3.3 后端实现图片存储服务

---

## 进度记录

| 日期 | 内容 | 负责人 |
|------|------|--------|
| 2025-05-21 | 创建进度文档，完成根因分析 | Claude |
| 2025-05-21 | Phase 1 全部完成 + Phase 2.1 流式代理改造完成，编译通过，测试通过 | Claude |
| | | |

---

## 参考文档

- [技术提案](../docs/proposals/TUNNEL_LARGE_PROMPT_FIX.md)
- [HTTP 隧道设计](../docs/proposals/HTTP_TUNNEL_DESIGN.md)
- [yamux 文档](https://github.com/hashicorp/yamux)
