# MCP Metadata 规范化

## 问题背景

通过 UI 创建 MCP 类型的 capability item 后，`/api/registry/public/index.json` 返回的 `mcp` 字段为空对象 `{}`，导致客户端无法获取 MCP 服务器的配置信息（command、url、headers 等）。

### 根因

MCP item 有三条创建路径，只有 archive 上传路径能正确生成 metadata：

| 创建路径 | metadata 落库内容 | 是否正确 |
|---|---|---|
| JSON API（`CreateItem` / `createItemFromJSON`） | 硬编码 `{}` — 请求体无 `metadata` 字段 | 错误 |
| Archive 上传（`createItemFromArchive`） | 经 `normalizeMCPMetadata` 处理，含完整配置 | 正确 |
| Sync（`SyncRegistry`） | 直接使用 `parsed.Metadata`，未经规范化，且失败时静默回退 | 格式不标准 |

同时，`updateItemFromJSON` 在更新 content 时也不会重新解析 MCP 配置到 metadata。

## 解决思路

### 核心原则：写入时规范化，读取时零转换

1. **`NormalizeMCPMetadata`**：在写入时将任何输入格式转成 Claude MCP 标准格式落库
2. **`buildMCPConfig`**：读取时直接返回 metadata，不做任何转换
3. **`resolveMetadata`**：统一的 metadata 解析入口，MCP item 在无显式 metadata 时自动从 content 解析；无 metadata 也无 content 时拒绝创建

### MCP 标准格式

参照 [Claude MCP 配置标准](https://docs.anthropic.com/en/docs/claude-code/mcp)，落库格式为：

**stdio（本地服务）** — 无 `type` 字段，靠 `command` 识别：
```json
{
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-filesystem"],
  "env": { "KEY": "value" }
}
```

**http（远程服务）**：
```json
{
  "type": "http",
  "url": "https://mcp.example.com/mcp",
  "headers": { "Authorization": "Bearer token" }
}
```

**sse（远程服务，SSE 传输）**：
```json
{
  "type": "sse",
  "url": "https://mcp.example.com/sse",
  "headers": { "X-API-Key": "key" }
}
```

### 输入格式兼容

`NormalizeMCPMetadata` 接受以下输入并统一转换为标准格式：

| 输入格式 | 来源 | 转换结果 |
|---|---|---|
| `{"command":"npx","args":[...]}` | 标准 stdio | 原样保留 |
| `{"type":"http","url":"...","headers":{...}}` | 标准 http | 原样保留 |
| `{"type":"sse","url":"..."}` | 标准 sse | 原样保留 |
| `{"type":"local","command":["npx","-y","@foo"]}` | opencode 特有格式 | `{"command":"npx","args":["-y","@foo"]}` |
| `{"type":"remote","url":"...","headers":{...}}` | opencode 特有格式 | `{"type":"http","url":"...","headers":{...}}` |
| `{"mcpServers":{"name":{...}}}` | `.mcp.json` 标准包裹格式 | 解包后按上述规则处理 |
| `{"url":"..."}` | 无 type 的 url | `{"type":"http","url":"..."}` |
| `{"url":"...","transport":"sse"}` | 无 type 但有 transport 提示 | `{"type":"sse","url":"..."}` |

## 改动点

### 1. `services/archive_service.go` — `NormalizeMCPMetadata`

- 导出为 `NormalizeMCPMetadata`（原 `normalizeMCPMetadata` 未导出）
- 签名从 `func(parsed *ParsedItem)` 改为 `func(meta map[string]any)`，不再绑定 `ParsedItem` 结构体
- 输出 Claude MCP 标准格式：stdio 无 `type` 字段、远程为 `http` 或 `sse`
- 识别 opencode 特有格式（`local` → stdio，`remote` → `http`）并转换
- 识别 `.mcp.json` 的 `mcpServers` 包裹层并解包
- 无 type 但有 url 时，检查 `transport` 字段：`"sse"` 设 `type: "sse"`，否则默认 `"http"`

### 2. `handlers/capability_item.go` — JSON 创建 / 更新路径

**新增 `resolveMetadata(itemType, raw, content)` 辅助函数**：
- 非 MCP item：透传 metadata 或默认 `{}`
- MCP item + 有 metadata：调用 `NormalizeMCPMetadata` 规范化
- MCP item + 无 metadata + 有 content：从 content 解析（当作 `.mcp.json`），再规范化
- MCP item + 无 metadata + 无 content：返回错误，拒绝创建（防止存入无效的空 `{}`）

**`CreateItem` / `createItemFromJSON`**：
- 请求体新增 `Metadata json.RawMessage` 字段
- 使用 `resolveMetadata` 替代硬编码 `{}`

**`updateItemFromJSON`**：
- 当 content 更新且 `itemType == "mcp"` 时，自动从新 content 重新解析 metadata

### 3. `handlers/registry.go` — `buildMCPConfig`

- 简化为直接返回 `si.Metadata`
- 不再做任何格式转换或字段剥离（规范化已在写入时完成）

### 4. `services/sync_service.go` — Sync 路径

- 创建和更新 MCP item 时，调用 `NormalizeMCPMetadata` 规范化
- 规范化失败时计入 `result.Failed`，记录错误信息，跳过该 item（不再静默回退存原始 metadata）
- Version 记录保持原始 metadata 不变（历史快照）

## 影响面

### 向后兼容

- **JSON 创建接口**：`metadata` 字段可选。非 MCP item 不传时默认 `{}`（行为不变）；MCP item 不传 metadata 时从 content 解析，两者都没有时返回 400 错误（**行为变更**）
- **非 MCP item**：完全不受影响
- **Archive 上传**：行为不变，只是 `NormalizeMCPMetadata` 签名调整
- **Sync**：MCP 规范化失败时该 item 被跳过并计入失败数（原先会静默存入非标准格式）

### 存量数据

已有的 MCP item（metadata 为 `{}` 或旧内部格式）不会自动修复。需要：
- 通过 UI 编辑保存一次（触发 `updateItemFromJSON`，从 content 重新解析）
- 或删除重建

### 前端

前端无需修改。MCP item 在文本模式下填写的 content（`.mcp.json` 格式）会被服务端自动解析为 metadata。