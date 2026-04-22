# Skill 内容版本化与一致性校验方案

**实现状态：提案**

- 状态：📝 待实现
- 目标模块：`server/internal/models/models.go`、`server/internal/handlers/capability_item.go`、`server/internal/services/`、相关测试与迁移逻辑
- 关联提案：`SKILL_DATA_DESIGN.md`、`ARCHIVE_UPLOAD_ITEM_CREATE.md`、`SKILL_SYNC_DESIGN.md`

---

## 一、背景

当前项目已经具备基础的 Skill/Capability 数据模型与历史版本表：

- `capability_items` 保存当前最新内容
- `capability_versions` 保存历史快照
- `UpdateItem` 在内容更新时会新增一条 `revision`
- `GET /api/items/:id/versions` 与 `GET /api/items/:id/versions/:version` 已支持拉取历史版本

但现有版本逻辑仍存在几个明显缺口：

1. **缺少内容一致性判断**：只要请求里带了 `content`，就会创建新版本，即使内容完全相同。
2. **缺少稳定内容指纹**：没有 item/version 级别的内容 MD5，无法判断两个版本是否真正一致。
3. **多文件 skill 难以判断是否变化**：archive 上传场景下，不能直接对 zip/tgz 文件本身计算 MD5，因为压缩元数据会导致相同内容产生不同摘要。
4. **版本语义不够清晰**：当前同时存在 `CapabilityItem.Version`（业务版本，如 `1.0.0`）与 `CapabilityVersion.Revision`（内部修订号），但缺少对外统一的 `v1/v2/v3` 内容版本概念。

本提案目标是在不推翻现有模型的前提下，为 Skill/Capability 增加：

- 基于内容的稳定指纹计算
- 内容一致性校验
- 仅在内容真实变化时才升级版本
- 对外明确的 `v1 / v2 / v3` 内容版本标识

---

## 二、设计目标

### 2.1 目标

实现以下能力：

1. 能判断指定能力数据当前内容与输入内容是否一致
2. 能判断某个历史版本与当前版本内容是否一致
3. 单文件与多文件（archive）场景都能得到稳定、可重复的内容 MD5
4. 相同内容重复提交时不创建新版本
5. 保留现有历史版本拉取能力，并对外补充 `v1/v2/v3` 语义

### 2.2 非目标

本提案暂不包含：

1. Git 级别的完整 diff/merge 功能
2. 面向用户的可视化版本对比页面
3. 跨 item 的全局去重存储
4. 将现有 `Version` 业务字段重定义为内容版本号

---

## 三、核心设计原则

### 3.1 区分两类版本

保留现有双版本概念，但明确职责：

#### 1）业务版本 `CapabilityItem.Version`

- 继续用于发布语义
- 示例：`1.0.0`、`1.1.0`、`2025.04`
- 允许由用户或同步流程维护

#### 2）内容版本 `CapabilityVersion.Revision`

- 表示内容修订序号
- 从 1 开始递增
- 对外展示为 `v1`、`v2`、`v3`
- 版本升级的唯一依据是“内容指纹是否变化”

结论：

- **不要**把 `CapabilityItem.Version` 改造成 `v1/v2/v3`
- **应该**将 `Revision` 作为内容版本的主语义

### 3.2 内容一致性优先于请求形式

无论来源是：

- JSON 单文件提交
- multipart archive 上传
- 外部同步

都应该统一沉淀为：

- 当前内容快照
- 当前内容 MD5
- 历史版本快照
- 历史版本 MD5

即：**版本升级依据应是“规范化后的内容是否变化”，而不是“调用了哪个接口”**。

### 3.3 不对压缩包二进制本身计算 MD5

对于多文件 skill，不直接对 `.zip` / `.tgz` 二进制算 MD5，因为以下元数据会导致不稳定：

- 文件顺序
- 压缩级别
- 时间戳
- 打包工具差异
- 目录记录与额外头信息

因此必须基于**解压后的规范化文件集合**计算整体内容 MD5。

---

## 四、数据模型改造

## 4.1 `capability_items` 扩展

建议新增字段：

```sql
ALTER TABLE capability_items
  ADD COLUMN content_md5 VARCHAR(32) DEFAULT '',
  ADD COLUMN current_revision INTEGER NOT NULL DEFAULT 1;
```

字段含义：

| 字段 | 含义 |
|------|------|
| `content_md5` | 当前最新内容的稳定 MD5 指纹 |
| `current_revision` | 当前内容对应的最新修订号，便于快速返回 `vN` |

说明：

- `current_revision` 不是必须字段，但建议增加，避免每次查询 item 都需要实时聚合 `MAX(revision)`。
- 若希望最小改动，也可不加 `current_revision`，改由查询 `capability_versions` 得出；但接口性能和实现复杂度会更差。

## 4.2 `capability_versions` 扩展

建议新增字段：

```sql
ALTER TABLE capability_versions
  ADD COLUMN content_md5 VARCHAR(32) DEFAULT '';
```

字段含义：

| 字段 | 含义 |
|------|------|
| `content_md5` | 该历史版本快照的稳定内容 MD5 |

## 4.3 可选扩展

如后续需要更强审计能力，可继续扩展：

```sql
ALTER TABLE capability_versions
  ADD COLUMN version_label VARCHAR(16) DEFAULT '',
  ADD COLUMN content_manifest JSONB DEFAULT '{}'::jsonb;
```

但本期不建议存 `version_label`，因为它完全可由 `revision` 派生：

```text
revision=1 -> v1
revision=2 -> v2
```

`content_manifest` 也建议先不落库，只在需要排查多文件差异时再考虑。

---

## 五、内容 MD5 计算方案

## 5.1 单文件 item

对于 JSON 直传内容的 skill / command / subagent / hook / mcp：

### 计算规则

1. 读取 `content`
2. 执行最小规范化
3. 对规范化结果计算 MD5

### 推荐规范化策略

#### 文本内容

- 统一换行：`
- 保留正文空格与缩进
- 不做语义重写

#### MCP JSON 内容

若 `itemType == "mcp"` 且内容本身是 JSON：

- 先解析 JSON
- 对 key 做稳定排序
- 重新序列化为 canonical JSON
- 再计算 MD5

这样可以避免仅因 JSON key 顺序不同而误判为内容变化。

### 结果

```text
contentMd5 = md5(normalizedContent)
```

## 5.2 多文件 archive item

对 archive 上传的 skill / mcp，整体内容 MD5 不能基于压缩包字节流，而必须基于**解压后的规范化文件集合**。

### 计算思路

对每个文件先生成稳定文件哈希，再按路径排序构造 manifest，最后对 manifest 计算整体 MD5。

### 步骤

#### 第 1 步：解压并得到全部文件条目

沿用现有 `ArchiveService.ParseArchive` 结果：

- 主文件内容
- 资源文件列表 `ArchiveAsset`

#### 第 2 步：路径规范化

统一规则：

- 使用 `/` 作为分隔符
- strip 顶层公共目录
- 禁止 `../` 路径穿越
- 过滤空目录

#### 第 3 步：过滤无关文件

以下内容不参与整体内容 MD5：

- `__MACOSX/`
- `.DS_Store`
- `Thumbs.db`
- 纯目录条目

隐藏文件是否参与，沿用现有解析规则：

- 若它是主文件（如 `.mcp.json`），必须参与
- 普通隐藏资源文件默认可过滤

#### 第 4 步：单文件哈希

每个文件生成稳定哈希：

- 文本文件：统一换行后再算 MD5 或 SHA
- JSON 文件：canonical JSON 后再算
- 二进制文件：原始字节直接算

当前项目里 `CapabilityAsset.ContentSHA` 已存在，archive 解析过程中也会计算 `ContentSHA`。因此整体内容 MD5 建议直接复用：

- `rel_path`
- `content_sha`

#### 第 5 步：生成稳定 manifest

按路径排序后拼接：

```text
SKILL.md:<main-file-hash>
assets/icon.png:<asset-hash>
prompts/system.md:<asset-hash>
```

#### 第 6 步：计算整体 MD5

```text
contentMd5 = md5(join("\n", sortedManifestLines))
```

### 关键结论

archive item 的整体内容 MD5 = **主文件 + 所有参与版本语义的资产文件** 的规范化摘要。

而不是：

```text
md5(zip bytes)
```

## 5.3 为什么主文件也要参与 manifest

虽然 `CapabilityItem.Content` 已保存主文件正文，但 archive skill 的完整语义不只包含主文件，还包括：

- 引用的 prompt 文件
- 资源文件
- `.mcp.json` 对应配置

因此整体版本判断必须覆盖全部版本化文件，而不是只看 `CapabilityItem.Content`。

---

## 六、版本升级规则

## 6.1 创建 item

### JSON 创建

1. 计算 `content_md5`
2. 写入 `capability_items.content_md5`
3. 写入 `capability_items.current_revision = 1`
4. 创建首条 `capability_versions`：
   - `revision = 1`
   - `content_md5 = 当前内容 md5`

### Archive 创建

1. 解析 archive
2. 生成整体内容 `content_md5`
3. 写入 item 当前记录
4. 创建首条版本记录 `revision = 1`

## 6.2 更新 item

### 核心规则

只有当“新内容 MD5 != 当前 `content_md5`”时，才创建新版本。

### 更新流程

1. 读取当前 item
2. 计算新内容 `newContentMD5`
3. 与 `item.content_md5` 比较

#### 情况 A：内容未变化

- 不创建新 `CapabilityVersion`
- 不增加 `current_revision`
- 允许更新非内容字段：
  - `name`
  - `description`
  - `category`
  - `status`
  - `version`（业务版本）

#### 情况 B：内容已变化

- 更新 `CapabilityItem.Content`
- 更新 `CapabilityItem.Metadata`（如需）
- 更新 `CapabilityItem.ContentMD5`
- `current_revision = current_revision + 1`
- 新建一条 `CapabilityVersion`

### 版本标签

对外展示：

```text
versionLabel = "v" + strconv.Itoa(revision)
```

示例：

| revision | versionLabel |
|----------|--------------|
| 1 | v1 |
| 2 | v2 |
| 3 | v3 |

---

## 七、接口改造建议

## 7.1 `GET /api/items/:id`

建议在现有 item 响应中增加：

```json
{
  "id": "...",
  "version": "1.0.0",
  "currentRevision": 3,
  "currentVersionLabel": "v3",
  "contentMd5": "d41d8cd98f00b204e9800998ecf8427e"
}
```

说明：

- `version` 继续表示业务版本
- `currentRevision` / `currentVersionLabel` 表示内容版本

## 7.2 `GET /api/items/:id/versions`

建议为每个历史版本增加：

```json
{
  "revision": 2,
  "versionLabel": "v2",
  "contentMd5": "...",
  "commitMsg": "..."
}
```

## 7.3 `GET /api/items/:id/versions/:version`

当前 `:version` 实际上传的是 `revision` 整数，建议保持兼容，但在响应中补充：

```json
{
  "revision": 2,
  "versionLabel": "v2",
  "contentMd5": "...",
  "content": "..."
}
```

## 7.4 新增一致性校验接口（建议）

### 方案 A：按内容校验

```http
POST /api/items/:id/check-consistency
```

请求：

```json
{
  "content": "..."
}
```

返回：

```json
{
  "matched": true,
  "contentMd5": "...",
  "matchedCurrent": true,
  "matchedRevision": 3,
  "matchedVersionLabel": "v3"
}
```

### 方案 B：按 MD5 校验

```http
POST /api/items/:id/check-consistency
```

请求：

```json
{
  "md5": "..."
}
```

返回：

```json
{
  "matchedCurrent": false,
  "matchedRevision": 2,
  "matchedVersionLabel": "v2"
}
```

### 方案选择

建议一个接口同时支持两种输入：

- `content`
- `md5`

若同时传入，以 `md5` 优先或直接报错，避免歧义。

---

## 八、后端实现建议

## 8.1 新增服务层能力

建议新增 `server/internal/services/content_hash_service.go`：

```go
type ContentHashService struct {}

func (s *ContentHashService) HashTextContent(itemType string, content string) (string, error)
func (s *ContentHashService) HashArchiveContent(mainPath string, mainContent []byte, assets []ArchiveAsset) (string, error)
func (s *ContentHashService) BuildVersionLabel(revision int) string
```

### 设计要点

1. 单文件与多文件统一从该服务计算内容 MD5
2. Handler 不直接拼装 hash 规则
3. 后续若要从 MD5 升级为 SHA256，也只需改服务内部实现

## 8.2 `persistNewItem` 改造

当前 `persistNewItem` 已负责：

- 创建 `CapabilityItem`
- 创建首个 `CapabilityVersion`
- 创建 assets / artifact

建议扩展 `createItemRequest`：

```go
type createItemRequest struct {
    ...
    ContentMD5 string
}
```

同时将首个版本写入：

```go
CapabilityVersion{
    Revision:   1,
    ContentMD5: req.ContentMD5,
}
```

并同步写 `CapabilityItem.ContentMD5` 与 `CurrentRevision = 1`。

## 8.3 `UpdateItem` JSON 路径改造

当前 `updateItemFromJSON` 的关键问题是：

- 只要 `req.Content != ""` 就创建新版本

建议改为：

1. 判断是否提交了内容字段
2. 计算 `newContentMD5`
3. 与当前 `item.ContentMD5` 比较
4. 仅当内容变化时才：
   - 落库新内容
   - 插入新版本

同时注意：

- 若 `itemType == "mcp"`，应对 metadata 使用规范化后的内容进行哈希，避免 metadata 与内容计算口径不一致。

## 8.4 `updateItemFromArchive` 路径改造

archive 更新时：

1. `ParseArchive`
2. 生成整体 `ContentMD5`
3. 与当前 item 的 `ContentMD5` 比较
4. 相同则不升版；不同则升版

同时需要明确：

- archive 更新的版本判断依据应基于**主文件 + assets** 的整体内容，而不是仅比较主文件正文。

---

## 九、迁移方案

## 9.1 表结构迁移

新增字段：

1. `capability_items.content_md5`
2. `capability_items.current_revision`
3. `capability_versions.content_md5`

## 9.2 存量数据回填

### 回填策略

#### 对 JSON/单文件 item

- 基于当前 `content` 重新计算 `content_md5`

#### 对历史版本

- 基于 `capability_versions.content` 重新计算 `content_md5`

#### 对 archive item

存在两种情况：

1. 若当前系统能从 `CapabilityAsset` 恢复参与版本判断的资产集合，则按新规则重新计算整体 MD5
2. 若历史数据无法可靠还原完整资产语义，则先降级回填为“仅主内容 hash”，并对这类数据打迁移日志

### 兼容建议

考虑到 archive item 的历史数据可能并不完整，建议：

1. **新规则自迁移后生效**
2. 存量 archive 数据以“尽力回填”为原则
3. 必要时允许 `content_md5` 为空，首次更新后再进入新规则

---

## 十、测试方案

## 10.1 单元测试

新增覆盖：

1. 相同文本内容 -> MD5 相同
2. 仅换行差异 -> 规范化后 MD5 相同
3. JSON key 顺序不同 -> canonical 后 MD5 相同
4. archive 相同内容、不同打包顺序 -> 整体 MD5 相同
5. archive 相同内容、不同时间戳 -> 整体 MD5 相同
6. archive 某个 asset 改动 -> 整体 MD5 变化

## 10.2 Handler 测试

新增或改造以下场景：

1. `CreateItem` 创建后写入 `content_md5`
2. `UpdateItem` 内容不变 -> 不新增 `CapabilityVersion`
3. `UpdateItem` 内容变化 -> revision + 1
4. archive 更新主文件未变但 asset 变化 -> 应新增版本
5. `GET /items/:id/versions` 返回 `versionLabel` 与 `contentMd5`
6. 一致性校验接口返回匹配的 revision

---

## 十一、风险与权衡

## 11.1 MD5 冲突风险

MD5 存在理论冲突可能，但本场景主要用于：

- 内容一致性判断
- 版本去重
- 快速查重

对于业务场景通常足够。

若后续对安全性要求更高，可演进为：

- 存储字段名仍保留 `contentMd5` 以兼容前端
- 内部新增 `content_sha256`
- 版本比较逐步切换到 SHA256

## 11.2 archive 版本语义边界

需要明确哪些文件参与“内容版本”计算。

建议规则：

- 主文件必须参与
- 所有可对外分发、可影响运行结果的 asset 必须参与
- 明确过滤系统临时文件

否则会出现：

- 某资源文件变了，但系统没升版
- 或仅打包元数据变化却错误升版

## 11.3 与 `SourceSHA` 的关系

当前已有 `SourceSHA` 字段，但它更接近：

- 来源文件哈希
- 同步文件 blob 摘要

不等价于“当前 item 整体内容版本摘要”。

因此两者应并存：

| 字段 | 作用 |
|------|------|
| `SourceSHA` | 标识来源文件/主文件摘要 |
| `ContentMD5` | 标识版本语义下的整体内容摘要 |

---

## 十二、最终建议

推荐采用以下落地策略：

1. 保留 `CapabilityItem.Version` 作为业务版本
2. 将 `CapabilityVersion.Revision` 作为内容版本主语义
3. 对外统一返回 `v1 / v2 / v3` 形式的 `versionLabel`
4. 为 `CapabilityItem` 与 `CapabilityVersion` 增加 `content_md5`
5. archive 场景基于“规范化文件集合”计算整体内容 MD5
6. 更新时只有内容 MD5 变化才创建新版本
7. 补充一致性校验接口，支持按 `content` 或 `md5` 查询匹配版本

该方案具备以下优点：

- 最大程度复用现有 `CapabilityVersion` 机制
- 保持与当前历史版本接口兼容
- 同时覆盖单文件与多文件 skill 场景
- 为后续 diff、回滚、查重、同步幂等提供稳定基础

---

## 十三、建议实施顺序

建议按以下顺序分阶段实施：

### Phase 1：基础字段与规则

1. 增加 `content_md5` / `current_revision`
2. 创建与更新路径写入内容 MD5
3. 相同内容不再新建版本

### Phase 2：接口增强

1. `GET /items/:id` 返回 `currentRevision` / `currentVersionLabel`
2. 版本列表与详情返回 `contentMd5` / `versionLabel`

### Phase 3：一致性校验与回填

1. 新增 `check-consistency` 接口
2. 回填存量数据的 `content_md5`
3. 补齐 archive 历史数据的迁移策略

### Phase 4：后续增强（可选）

1. 版本 diff 接口
2. 按 MD5 快速查重
3. 支持 SHA256 双轨摘要
