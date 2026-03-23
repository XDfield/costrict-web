# Zip Archive Upload for Item Creation

**Date:** 2026-03-20

## Context

当前通过 `POST /api/items` 创建技能（skill）或 MCP Server 时，`content` 字段以纯文本形式传入 JSON body。对于包含多个文件的场景（如 skill 主描述文件 + 附属资源文件、MCP Server 配置 + 依赖文件），用户希望能以 `.zip` 压缩包的形式一次性上传，由后端解析提取主文件内容和附属文件。

本期目标不是仅把 zip 内容写入数据库，而是让上传后的条目继续符合当前仓库已有的读取约定：

- `POST /api/items` 仍是 zip 上传入口；`POST /api/items` 与 `POST /api/registries/:id/items` 继续都是单 item 创建端点，但本期会收敛到同一套创建内核
- `/registry/{repo}/index.json` 与 `/registry/{repo}/{slug}/{file}` 仍是现有分发/下载路径
- 当前外部可见的 itemType 命名保持为 `skill`、`mcp`、`subagent`、`command`、`hook`

但 zip 上传的支持范围会显式收窄：**本期仅支持 `skill` 与 `mcp` 的压缩包上传；`hook`、`subagent` 与 `command` 继续走现有 JSON / 文本路径，不支持 multipart zip 创建。**

## Discussion

### 压缩包内容结构

本期支持的 zip 内容类型：

- skill：主 Markdown 文件 + 资源文件
- mcp：主配置文件 + 依赖文件

附属文件统一落到 `CapabilityAsset`，并且必须可被现有 registry 下载路径读取；否则 zip 上传只有“写入”而没有“可用”。

### 压缩包格式

仅支持 `.zip`，原因：前端可通过 JSZip 生成，用户最常用，Go 标准库 `archive/zip` 原生支持。

### 上传流程

用户可选择“文本编辑”或“上传压缩包”两种模式，后端根据请求的 `Content-Type` 分别处理：

- `application/json` → 现有 JSON 逻辑
- `multipart/form-data` → zip 上传解析逻辑（仅 `skill` / `mcp`）

### 备选方案对比

| 方案          | 描述                                   | 优点                     | 缺点                             |
| ------------- | -------------------------------------- | ------------------------ | -------------------------------- |
| A             | 新增独立 `POST /api/items/upload` 端点 | 现有端点零侵入           | 两条创建路径并存                 |
| **B（选定）** | `POST /api/items` 支持双 Content-Type  | 不新增端点，API 语义统一 | handler 内部需 Content-Type 分叉 |
| C             | 前端 JSZip 解压 + 多次请求             | 后端零改动               | 非原子操作，可靠性差             |

选定方案 B：不新增端点，`POST /api/items` 根据 Content-Type 分发，现有 JSON 调用方不受影响。

## Approach

`POST /api/items` 的 handler 入口检测 `Content-Type`：

- `application/json` → `createItemFromJSON`
- `multipart/form-data` → `createItemFromZip`

两条路径共享同一个 `persistNewItem` 内部函数，但要明确职责边界：

- `persistNewItem` 只负责 **DB 内真实状态落库**：`CapabilityItem`、首个 `CapabilityVersion`、可选的 `CapabilityAsset`、可选的 `CapabilityArtifact`
- 向量索引、扫描入队、对象存储 best-effort 清理都在事务外执行
- zip 解析逻辑独立为 `services.ParseZip`，不依赖 `gin.Context`，可独立测试并被未来 CLI 上传、导入逻辑复用

### 本期关键设计调整

1. **预分配 item ID**：handler 在任何对象存储写入前先生成 `itemID`，所有 storage key 都基于该 ID 计算，避免“先上传文件、后生成 item ID”的顺序冲突。
2. **将 slug 语义收敛为真实全局唯一**：本期既然继续保留 `/registry/{repo}/{slug}/{file}` 的按 slug 寻址协议，就不能只在个别 handler 做“看起来像全局唯一”的预检查；必须把所有写入口统一到同一套规则，并以数据库唯一约束保证 `slug` 全局唯一。
3. **补齐 asset 读取闭环**：上传 zip 时写入的 `CapabilityAsset` 必须能被 registry 下载接口读取；必要时同步更新 index.json 的 `files` 列表。
4. **收窄 zip 上传支持范围**：multipart zip 创建仅支持 `skill`、`mcp`；`hook`、`subagent` 与 `command` 在 handler 入口直接返回 400。
5. **明确 MCP 元数据必须规范化落库**：`POST /api/items` 一次只创建一个 item，因此 mcp zip 仅允许解析为单个 MCP item；`.mcp.json` 的结构化元数据必须先转换成当前 registry 读取侧可直接消费的规范形态后，再写入 `CapabilityItem.Metadata`，不能把 parser 的原始输出或整份 JSON 直接当成 metadata 落库。

## Architecture

### Handler 层（`capability_item.go`）

```go
func (h *ItemHandler) CreateItemDirect(c *gin.Context) {
    if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
        h.createItemFromZip(c)
    } else {
        h.createItemFromJSON(c)
    }
}
```

`POST /api/items` 与 `POST /api/registries/:id/items` 在本期都收敛到共享的创建核心：

- JSON / multipart 只是入参解析不同，落库都走同一个 `persistNewItem`
- slug 冲突检查由共享逻辑处理，最终以数据库唯一约束为准
- `MoveItem` 也复用同一套 slug 冲突语义，避免“创建能过、移动才冲突”或反之的规则分叉

`createItemFromZip` 流程：

1. `c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxZipUploadSize)`（MaxZipUploadSize = 50MB）
2. `c.Request.ParseMultipartForm(MaxMultipartMemory)` 解析 multipart；此步骤用于限制表单解析内存，而**不是**为了强制落盘拿到 `io.ReaderAt`
3. `c.Request.FormFile("file")` 读取 zip 文件；返回的 `multipart.File` 本身满足 `io.Reader` / `io.ReaderAt` / `io.Seeker`
4. 从 form fields 读取元数据：`itemType`、`name`、`slug`、`registryId`、`description`、`category`、`version`、`createdBy`
5. 校验必填字段（`itemType`、`name`）
6. 校验 `itemType` 是否属于允许的 zip 上传集合：`skill`、`mcp`
7. 先确定 `registryID`、`createdBy`、`effectiveSlug`、`itemID`
   - `registryId` 为空时仍回落到 `PublicRegistryID`
   - `createdBy` 不再直接信任表单值：有认证用户时强制使用当前用户；仅匿名创建时才回落到 `anonymous`
   - `slug` 为空时使用现有 `slugify(name)` 规则生成
   - `itemID = uuid.New().String()` 由 handler 预分配
8. 将 `multipart.File` 传入 `services.ParseZip(file, size, itemType)`
9. 基于 `ParseZip` 结果组装 `createItemRequest`
   - `Content` 使用主文件内容
   - `SourcePath` 使用主文件相对路径
   - `SourceSHA` 使用主文件内容哈希
   - `Metadata` 根据 itemType 解析后写入；其中 mcp 必须先做规范化（与当前 registry/index 读取侧期待的字段形状一致），不能统一置空，也不能直接写入未经约束的原始 parser 输出
   - 仅在表单字段缺失时，才用主文件解析结果回填 `description` / `category` / `version`
10. 在事务外写入对象存储
   - 所有二进制 asset 使用预分配的 `itemID` 计算 storage key 后上传
   - `file.Seek(0, io.SeekStart)` 回拨原始 zip，使用 `io.TeeReader` 流式上传 `_package.zip` 并同步计算 `ChecksumSHA256`
11. 开启单个 DB 事务，调用 `persistNewItem` 一次性写入：`CapabilityItem`、首个 `CapabilityVersion`、全部 `CapabilityAsset`、原始 zip 对应的 `CapabilityArtifact`；其中首个 `CapabilityVersion.Metadata` 必须与 `CapabilityItem.Metadata` 保持一致，避免版本记录丢失 MCP 的规范化配置语义
12. 事务提交成功后，再触发索引与扫描；事务失败时 best-effort 清理第 10 步已写入的 storage key
13. 返回 `201 Created + item JSON`

共享内部类型：

```go
type createItemRequest struct {
    ID          string
    RegistryID  string
    Slug        string
    ItemType    string
    Name        string
    Description string
    Category    string
    Version     string
    Content     string
    Metadata    datatypes.JSON
    SourcePath  string
    SourceSHA   string
    CreatedBy   string
}

type createItemAssets struct {
    Records  []models.CapabilityAsset
    Artifact *models.CapabilityArtifact
}

var ErrSlugConflict = errors.New("slug conflict")

// persistNewItem 只做 DB 事务内写入；不做对象存储 I/O，不做索引，不做扫描入队。
func persistNewItem(
	db *gorm.DB,
	req createItemRequest,
	assets createItemAssets,
) (*models.CapabilityItem, error)
```

### Slug 冲突策略

本期不再把“全局 slug 冲突”仅停留在 `/api/items` 的用户可见行为上，而是把它收敛为**系统真实约束**。

原因不是这个模型最优，而是当前读取侧仍依赖更强的唯一性假设：

- `/registry/{repo}/{slug}/{file}` 目前按“repo 下 registry 集合 + slug”查询，不包含 `itemType`
- `index.json` 与下载路径仍没有切换到 `(registry, itemType, slug)` 寻址协议
- 只靠 handler 预检查无法覆盖并发创建、`POST /api/registries/:id/items`、`MoveItem` 等其他写路径

因此本期采用方案 A：

1. `CapabilityItem.slug` 调整为数据库级全局唯一约束，避免竞态下写入重复 slug
2. `POST /api/items` 与 `POST /api/registries/:id/items` 共用同一套 slug 冲突检查与 `persistNewItem`
3. `MoveItem` 沿用同一套全局 slug 语义，不再只按目标 `(registry_id, item_type, slug)` 判断
4. 继续保持当前 registry 下载协议不变，直到未来单独立项做完整寻址 cutover

也就是说，本期不是「暂不放宽」为止，而是先把当前对外协议所依赖的强唯一性做成真的。

**存量数据迁移保护**：由于旧索引 `idx_item_slug` 是 `(registry_id, item_type, slug)` 复合唯一，存量数据库中可能已存在跨 registry 或跨 type 的同名 slug。服务器启动时在 `runPreMigrations` 末尾自动调用 `deduplicateSlugs`：先 DROP 旧索引，再检测所有重复 slug 并按创建时间排序，最旧的记录保留原 slug，后续重复者依次追加数字后缀（`-2`、`-3`……）直到全局唯一，整个去重过程在 DB 事务内完成。去重成功后 `AutoMigrate` 才建立新的 `idx_item_slug_global`，避免因存量冲突导致服务器启动失败。该函数幂等：若 `idx_item_slug_global` 已存在则直接跳过。

### Zip 解析服务（`services/zip_service.go`，新增）

```go
type ZipParseResult struct {
    MainContent    string
    MainPath       string
    MainSHA        string
    Parsed         *ParsedItem
    Assets         []ZipAsset
    NormalizedMeta map[string]any // MCP 规范化后的 metadata；非 mcp 类型为 nil
}

type ZipAsset struct {
    Path       string // strip 顶层目录后的相对路径
    Content    []byte
    Size       int64
    MimeType   string
    Binary     bool
    ContentSHA string // sha256(Content) hex 编码；填充 CapabilityAsset.ContentSHA
}

// ParseZip 直接接受 multipart.File 对应的 ReaderAt/Seeker 能力；
// 不要求调用方为了 ReaderAt 强制将文件溢写到磁盘。
func (z *ZipService) ParseZip(r io.ReaderAt, size int64, itemType string) (*ZipParseResult, error)
```

解析流程：

1. `archive/zip.NewReader(r, size)` 打开 zip
2. 顶层目录 strip：若所有非目录条目的第一个路径分量相同，则 strip 该前缀
3. 按 `resolveMainFile(itemType)` 规则从全部条目中匹配主文件；此步先于隐藏文件过滤执行，确保 mcp 类型的 `.mcp.json` 不被提前丢弃
4. 解析主文件：
   - `skill` → 复用 `ParserService.ParseSKILLMD`
   - `mcp` → 复用 `ParseMCPJSON` 或等价逻辑，但要求最终只得到 **一个** ParsedItem
5. 将主文件之外的剩余条目过滤为 asset：跳过目录条目、`__MACOSX` 前缀及 `.` 开头的隐藏文件
6. 对每个 asset 计算 MIME、文本/二进制属性与内容哈希

主文件识别规则（`resolveMainFile(itemType)`）：

| itemType | 主文件名约定 |
| --- | --- |
| skill | `SKILL.md` |
| mcp | `.mcp.json` |

本期不再提供 fallback 主文件发现逻辑：

- skill zip 若不存在 `SKILL.md`，即使根目录还有其他 `.md` 文件，也返回主文件缺失错误
- mcp zip 若不存在 `.mcp.json`，即使存在 `mcp.json`、`config.json` 或其他 JSON 文件，也返回主文件缺失错误
- 顶层目录 strip 仅用于去掉统一前缀目录，不改变主文件命名约定

#### MCP 规则

`POST /api/items` 一次只创建一个 item，因此 mcp zip 的规则必须显式收紧：

- `.mcp.json` 若解析出 **多个** `mcpServers` 条目 → 返回 `400 Bad Request`
- `.mcp.json` 若解析出单个 server → 先将该 server 配置规范化为当前 registry 读取侧可直接消费的 metadata 形态，再写入 `CapabilityItem.Metadata`
- `.mcp.json` 若没有 `mcpServers`，但包含单个可直接发布的顶层 MCP 配置 → 仅当它能被规范化到同一 metadata 形态时才允许创建；无法规范化的结构返回 `400 Bad Request`，而不是把整份原始 JSON 直接塞进 `Metadata`
- `Content` 仍保存原始主文件文本，供版本记录与下载使用；但对 registry index 来说，真正的 MCP 配置来源是规范化后的 `Metadata`

`CapabilityItem.Metadata` 的 MCP 规范形态以当前 registry 读取侧为准，至少覆盖：

- command / local 型：`{"hosting_type":"command","command":"npx","args":[...]}`
- remote 型：`{"hosting_type":"remote","server_type":"http|sse","url":"..."}`

也就是说，zip 上传的 mcp 路径需要显式执行一次 normalize，而不是假设 `ParseMCPJSON` 的输出或 `.mcp.json` 原文天然等于最终落库格式。

找不到主文件时返回明确错误，直接告知期望的 canonical 文件名；例如 skill 必须是 `SKILL.md`，mcp 必须是 `.mcp.json`。

安全约束（可配置常量）：

- 压缩包上传大小上限：50MB（HTTP 层通过 `MaxBytesReader` 强制）
- 总解压大小上限：50MB（防 zip bomb）；实现要求：每个条目读取前先将 `entry.UncompressedSize64` 累加至运行总量并检查超限，每个条目通过 `io.LimitReader(rc, maxSingleFileSize+1)` 读取——禁止先 `io.ReadAll` 后再检查大小
- 单文件大小上限：10MB
- 文件数量上限：500
- 路径遍历校验：reject 包含 `..` 的路径

### 附属文件存储与读取

zip 解压出的附属文件存为 `CapabilityAsset` 记录，复用 `syncAssets` 同一模型：

- **文本文件**（`.md`、`.json`、`.yaml` 等）→ 内容存入 `TextContent`，无需对象存储
- **二进制文件** → 通过 `StorageBackend.Put` 存入对象存储，记录 `StorageKey`、`StorageBackend`
- storage key 格式：`{itemID}/assets/{relativePath}`

原始 zip 包本身单独作为 `CapabilityArtifact` 存储：

- `filename = _package.zip`
- `artifactVersion = item.Version`
- `isLatest = true`

#### 读取闭环

为了让 zip 上传后的资产真正可用，需要同步更新现有读取路径：

1. **`RegistryIndex`**
   - `files` 列表不再只返回主文件名
   - 对非 MCP item，返回 `主文件名 + 所有 CapabilityAsset.RelPath`
2. **`DownloadRegistryFile`**
   - 现有路由 `/registry/{repo}/{slug}/{file}` 只能匹配单段文件名，无法覆盖 `scripts/setup.sh` 这类带 `/` 的嵌套 asset 路径
   - 因此需将路由调整为支持多段路径的通配形式（如 `/registry/{repo}/{slug}/*file`），handler 内再去掉前导 `/` 并做路径合法性校验
   - 若请求文件名等于主文件名 → 返回 `item.Content`
   - 否则按 `item_id + rel_path` 查询 `CapabilityAsset`
   - `TextContent != nil` → 直接返回文本内容
   - 二进制 asset → `StorageBackend.Get` 流式返回对象存储内容
   - 这样 `index.json` 中列出的嵌套 asset 路径才真正可下载

> 注意：若不修改路由形状，zip 上传虽然可以写入 `CapabilityAsset.RelPath = scripts/setup.sh`，但 HTTP 层会在进入 handler 前就因路由不匹配而返回 404。

### 操作顺序与事务性保证

与 `UploadArtifact` 的已有模式一致：**所有 I/O 在 DB 事务外完成，事务仅包含短暂的 DB 写操作**。

1. handler 预分配 `itemID`
2. 将二进制 asset 逐一调用 `StorageBackend.Put` 写入对象存储，记录已写入 key 列表
3. `file.Seek(0, io.SeekStart)` 重置文件位置；将原始 zip 通过 `io.TeeReader` 流式写入对象存储，同步计算 `ChecksumSHA256`
4. 开启短暂 DB 事务：创建 `Item + Version + 所有 CapabilityAsset + CapabilityArtifact`；`Version.Metadata` 与当次落库的 `Item.Metadata` 保持一致
5. 若 DB 事务失败 → best-effort 清理步骤 2–3 已写入的全部 storage key（Delete 失败仅记录日志，不阻塞返回）
6. 事务提交成功 → 事务外触发索引与扫描

> 已知故障模式：存储写入成功、但 DB 提交失败时，对象存储文件可能成为孤儿。缓解策略保持不变：使用确定性 key 命名，后续由后台 GC 任务扫描对象存储并比对 DB 记录清理孤儿文件。

> JSON 路径同样受益：`persistNewItem` 的事务化改造会同时修复现有 JSON 创建路径中 item + version 不在同一事务内的问题。

## Error Handling

| 场景                         | HTTP 状态码 | 错误信息                                                                                               |
| ---------------------------- | ----------- | ------------------------------------------------------------------------------------------------------ |
| 上传体超过压缩包大小限制     | 413         | `"Zip upload exceeds maximum size"`                                                                  |
| 不支持的 zip 上传 itemType   | 400         | `"itemType must be either skill or mcp"`                                                             |
| zip 损坏或非 zip 格式        | 400         | zip 解析错误原文透传                                                                                   |
| 找不到主文件                 | 400         | skill: `"zip archive must include SKILL.md"`; mcp: `"zip archive must include .mcp.json"`          |
| mcp zip 解析出多个 server    | 400         | `".mcp.json must contain exactly 1 server entry"`                                                    |
| 超过解压大小限制             | 400         | `"zip archive exceeds maximum uncompressed size of 52428800 bytes"`                                  |
| 文件数量超限                 | 400         | `"zip archive contains more than 500 files"`                                                         |
| 路径遍历检测                 | 400         | `"zip entry \"<name>\" contains path traversal"`                                                 |
| 缺少必填 form field          | 400         | `"itemType and name are required"`                                                                    |
| slug 冲突                    | 409         | `"An item with this slug already exists"`（全局唯一，所有创建/移动入口一致）                         |
| 文件存储失败                 | 500         | `"Failed to store zip assets"` / `"Failed to store uploaded zip"`                                  |
| asset 下载缺失               | 404         | `"File not found"`                                                                                   |

## Test Strategy

### `services/zip_service_test.go`（单元测试）

- 正常 zip：含主文件 + 附属文件 → 正确拆分
- 仅含主文件、无附属文件 → `Assets` 为空，`MainContent` 正确
- 单顶层目录 strip
- 缺少主文件 → 明确错误
- skill 只有非 `SKILL.md` 的 Markdown 文件 → 仍返回主文件缺失，不做 fallback
- mcp 只有 `mcp.json` / `config.json` 等非 `.mcp.json` 文件 → 仍返回主文件缺失，不做 fallback
- zip bomb 防御（构造超大解压比）
- 路径遍历 `../` → reject
- `__MACOSX` / 隐藏文件过滤
- 支持的 itemType 主文件识别（skill、mcp）
- mcp 单 server 成功、多 server 返回错误

### `handlers/capability_item_test.go`（handler 集成测试）

- multipart zip 上传成功 → 返回 item，`content` 来自主文件，`CapabilityAsset` 记录数正确
- 仅含主文件的 zip 上传 → 无 asset 记录，item 创建成功
- zip 上传后 `metadata` 正确落库（尤其 mcp：验证写入的是规范化后的 registry-facing metadata，而不是原始 parser 输出），且首个 version metadata 与 item metadata 一致
- JSON 提交回归测试（确认不受影响）
- 损坏 zip → 400
- zip 缺主文件 → 400
- mcp 多 server → 400
- `itemType=hook` / `itemType=subagent` / `itemType=command` 的 multipart 请求 → 400
- 缺必填字段 → 400
- 超过大小限制 → 413
- 事务失败时触发对象存储 best-effort 清理

### `handlers/registry_test.go`（读取侧回归测试，追加用例）

- `index.json` 包含主文件与 asset 文件列表
- 下载主文件仍走 `item.Content`
- MCP 主文件下载使用 `.mcp.json`，返回 `item.Content`
- 下载文本 asset 返回 `TextContent`
- 下载二进制 asset 走 `StorageBackend.Get`
- 嵌套 asset 路径（如 `scripts/setup.sh`）可通过 registry 下载路由成功访问
- 无权访问时仍按现有 visibility 规则返回 403/401

## Change Scope

变更文件：

- `server/internal/handlers/capability_item.go`
  - `CreateItemDirect` 按 Content-Type 分流
  - `CreateItem` / `CreateItemDirect` 共享 `createItemFromJSON` / `createItemFromZip` / `persistNewItem`
  - 预分配 `itemID`
  - zip 路径增加 itemType 白名单校验（仅 `skill` / `mcp`）
  - 统一 slug 冲突检查，并让 `MoveItem` 复用同一套全局 slug 语义
  - Swagger 注释增加 `multipart/form-data`
- `server/internal/models/models.go`
  - 将 `CapabilityItem.slug` 调整为数据库级全局唯一约束（不新增列，仅调整唯一索引）
- `server/internal/services/parser_service.go` 或等价 MCP 规范化逻辑
  - 将单 server / 顶层 MCP 配置转换为当前 registry 读取侧可直接消费的 metadata 形态
- `server/internal/services/zip_service.go`（新增）
- `server/internal/services/zip_service_test.go`（新增）
- `server/cmd/api/main.go`
  - 将 registry 文件下载路由从 `/registry/:repo/:slug/:file` 调整为支持多段路径的通配形式（如 `/registry/:repo/:slug/*file`）
- `server/internal/handlers/registry.go`
  - `RegistryIndex` 补齐 assets 文件列表
  - `DownloadRegistryFile` 支持从 `CapabilityAsset` 读取文本/二进制附属文件
- `server/internal/handlers/registry_test.go`
  - 增加 assets 读取回归测试
- `server/internal/handlers/capability_item_test.go`
  - 新增 multipart zip、全局 slug 冲突与双创建入口共用规则的用例

不变：

- `POST /api/items` 的 JSON 请求结构
- `CapabilityAsset / CapabilityArtifact` 模型字段（本期不新增字段）
- `subagent` / `command` 的现有 JSON 创建与下载行为
- 前端上传模式切换之外的交互设计

## Explicit Non-goals

本期**不做**以下改动：

- 不支持 `hook` / `subagent` / `command` 的压缩包上传
- 不重构 `/registry/{repo}/{slug}/{file}` 为 `(registry, itemType, slug)` 寻址协议
- 不把 slug 唯一性规则放宽到 `(registry_id, item_type, slug)`
- 不新增专门的 zip-upload 专用端点
- 不一次创建多个 item；特别是 mcp zip 不支持多 server fan-out 创建

## Follow-up

- 单独设计 `hook` / `subagent` / `command` 是否需要 zip 上传，以及若支持时的主文件命名与下载协议
- 单独设计 registry 下载寻址协议的完整 cutover；届时再评估是否可以从全局 slug 唯一收敛回更宽的复合唯一索引语义
- 评估是否为 zip / asset 孤儿对象引入后台 GC 任务
