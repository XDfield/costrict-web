# Plugin 上传接口设计文档

## 1. 概述

### 目标
新增一个上传 plugin 的接口，能够：
1. 上传 plugin 压缩包（zip 格式）
2. 解析 plugin 内容（`CLAUDE.md` + `cospowers.config.json` + 资产文件）
3. 上传后的 plugin 作为一个整体条目出现在技能商店中

### 关键决策
- **呈现方式**：Plugin 作为一个整体条目出现在技能商店（Option A）
- **Item Type**：新增/复用 `"plugin"` 类型
- **上传方式**：单步直传（用户选择仓库 → 上传 zip → 后端解析并创建/更新条目）
- **目标仓库**：用户可选择有权限的仓库（public 或私有仓库）
- **权限**：仅限组织管理员/仓库成员上传
- **更新**：允许覆盖上传（不保留历史版本）

---

## 2. 架构与数据模型

### 2.1 Plugin 与 CapabilityItem 的映射

一个上传的 plugin 压缩包对应 **一条** `CapabilityItem` 记录：

| 数据库字段 | 来源 |
|---|---|
| `name` | 从 zip 目录名提取，或从 `CLAUDE.md` 的 `# ` 标题提取 |
| `slug` | 从目录名生成（kebab-case） |
| `item_type` | `"plugin"` |
| `description` | 从 `CLAUDE.md` 的第一段非空非标题文本提取 |
| `content` | `CLAUDE.md` 的完整内容 |
| `metadata` | `cospowers.config.json` 的解析结果（JSON） |
| `source_type` | `"archive"` |
| `source_path` | `"CLAUDE.md"` |
| `registry_id` | 用户选择的 target repo 对应的 registry |

### 2.2 Asset 存储

压缩包内除 `CLAUDE.md` 外的所有文件，按相对路径存入 `CapabilityAsset`：

```
skills/solution-design/SKILL.md        →  rel_path: "skills/solution-design/SKILL.md"
rules/business/日志规范.md              →  rel_path: "rules/business/日志规范.md"
cospowers.config.json                  →  rel_path: "cospowers.config.json"
templates/system-design-template.md    →  rel_path: "templates/system-design-template.md"
```

- 文本文件（`.md`, `.json`, `.yaml`, `.txt` 等）且 `< 1MB` → 直接存入 `text_content` 字段
- 其他文件（图片、二进制等）或 `≥ 1MB` → 走 `StorageBackend`，`storage_key` 引用

### 2.3 覆盖更新

通过 `repo_id + item_type("plugin") + slug` 检查是否已存在：
- **存在且用户有权限**（创建者 / 组织管理员 / 平台管理员）→ 覆盖更新内容、元数据、assets
- **存在但用户无权限** → 409 Conflict
- **不存在** → 新建

覆盖更新事务：
```go
db.Transaction(func(tx *gorm.DB) error {
    // 1. 更新 CapabilityItem
    tx.Model(&item).Updates(...)
    // 2. 删除旧 assets
    tx.Where("item_id = ?", item.ID).Delete(&models.CapabilityAsset{})
    // 3. 插入新 assets
    tx.Create(&assets)
    return nil
})
```

---

## 3. API 设计

### 3.1 端点

```http
POST /api/plugins/upload
Content-Type: multipart/form-data
```

### 3.2 请求参数

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `repo_id` | string | 是 | 目标仓库 ID（public 或用户有权限的私有仓库） |
| `file` | file | 是 | plugin 压缩包（.zip），最大 50MB |

### 3.3 响应

**成功（创建新 plugin）→ 201 Created**
```json
{
  "id": "uuid",
  "registryId": "uuid",
  "repoId": "repo_id",
  "slug": "cospowers-solution-design",
  "itemType": "plugin",
  "name": "cospowers-solution-design",
  "description": "This plugin helps an agent turn approved requirements...",
  "content": "# cospowers Solution Design...",
  "metadata": { "templates": {...}, "rules": {...}, "evaluators": {...} },
  "sourceType": "archive",
  "assets": [
    { "relPath": "skills/solution-design/SKILL.md", ... },
    { "relPath": "cospowers.config.json", ... }
  ],
  "createdAt": "...",
  "updatedAt": "..."
}
```

**成功（覆盖更新）→ 200 OK**
结构同上，但返回 200 表示已覆盖。

### 3.4 错误码

| 状态码 | 场景 |
|---|---|
| 400 | 非 zip 文件 / zip 解压失败 / 缺少 `CLAUDE.md` / 缺少 `cospowers.config.json` |
| 403 | 用户不是目标仓库成员或无管理员权限 |
| 404 | 目标仓库不存在 |
| 413 | 文件超过 50MB |
| 409 | 同名 plugin 存在但用户无更新权限 |

---

## 4. 后端实现

### 4.1 关键发现：已有基础设施

后端已有的 `POST /api/items` 接口（`createItemFromArchive`）已经完整支持 zip 上传、解压、asset 提取、二进制文件存储、artifact 归档。目前仅支持 `skill` 和 `mcp` 两种类型，但基础设施完全可复用。

**已有代码位置**：
- `server/internal/handlers/capability_item.go:2088` — `CreateItemDirect` 分发到 `createItemFromArchive`
- `server/internal/handlers/capability_item.go:2428` — `createItemFromArchive` 处理 multipart 上传
- `server/internal/services/archive_service.go:51` — `ArchiveService.ParseArchive` 解析压缩包
- `server/internal/services/archive_service.go:412` — `resolveMainFile` 解析主文件路径

### 4.2 后端改动点（共 4 处）

#### 改动 1：`services/archive_service.go` — `resolveMainFile`

新增 `plugin` 类型的主文件映射：

```go
func resolveMainFile(itemType string) string {
    switch itemType {
    case "skill":
        return "SKILL.md"
    case "mcp":
        return ".mcp.json"
    case "plugin":
        return "CLAUDE.md"
    default:
        return ""
    }
}
```

#### 改动 2：`services/archive_service.go` — `ParseArchive`

在 `switch itemType` 分支中增加 `plugin` 处理：

```go
case "plugin":
    // Plugin 的主文件 CLAUDE.md 不需要复杂解析，直接读取内容
    parsed = &ParsedItem{
        Content:    string(mainContent),
        SourcePath: mainFile,
        ItemType:   "plugin",
        Version:    "1.0.0",
    }
```

> **注**：`cospowers.config.json` 的解析在 `ParseArchive` 返回后由 `createItemFromArchive` 处理：遍历 `result.Assets` 找到 `cospowers.config.json` 并解析为 metadata，合并到 `parsed.Metadata` 中。

#### 改动 3：`handlers/capability_item.go` — `createItemFromArchive`

放开 `plugin` 类型限制（当前第 2462-2467 行）：

```go
switch itemType {
case "skill", "mcp", "plugin":
default:
    c.JSON(http.StatusBadRequest, gin.H{"error": "itemType must be skill, mcp or plugin"})
    return
}
```

并在创建 item 前增加 plugin 特有的 metadata 提取逻辑：

```go
// 如果是 plugin 类型，从 assets 中提取 cospowers.config.json 作为 metadata
if itemType == "plugin" {
    for _, asset := range result.Assets {
        if asset.Path == "cospowers.config.json" && !asset.Binary {
            var config map[string]any
            if err := json.Unmarshal(asset.Content, &config); err == nil {
                metadataMap = config
            }
            break
        }
    }
    // 如果 description 为空，从 CLAUDE.md 内容提取第一段
    if description == "" {
        description = extractFirstParagraph(result.MainContent)
    }
}
```

#### 改动 4：`handlers/registry.go` — 注册表索引

**`buildRegistryIndex`** 中增加 `plugin` 类型处理：

```go
case "plugin":
    entry.Files = append([]string{"CLAUDE.md"}, assetPaths...)
```

**`contentFilename`** 中增加 `plugin` 类型：

```go
case "plugin":
    return "CLAUDE.md"
```

### 4.3 新增端点 `POST /api/plugins/upload`

建议实现为一个 thin wrapper，复用 `createItemFromArchive` 的核心逻辑，但：
- 固定 `itemType = "plugin"`
- 增加覆盖更新语义（检查 `repo_id + "plugin" + slug` 是否已存在）

也可直接复用 `POST /api/items`（`CreateItemDirect`）并传入 `itemType=plugin`，由前端调用。根据用户要求，保留独立的 `/api/plugins/upload` 端点作为语义化入口。

### 4.4 权限校验

```go
func canUploadPlugin(db *gorm.DB, repoID, userID string, isPlatformAdmin bool) bool {
    if isPlatformAdmin {
        return true
    }
    var count int64
    db.Model(&models.RepoMember{}).
        Where("repo_id = ? AND user_id = ?", repoID, userID).
        Count(&count)
    return count > 0
}
```

### 4.5 Zip 安全限制

复用已有常量（`server/internal/services/archive_service.go`）：

| 限制项 | 值 | 目的 |
|---|---|---|
| 压缩包大小 | ≤ 50MB (`MaxArchiveUploadSize`) | 防止超大上传 |
| 解压后总大小 | ≤ 50MB (`MaxUncompressedSize`) | 防止 zip bomb |
| 文件数量 | ≤ 500 (`MaxFileCount`) | 防止过多小文件 |
| 单文件大小 | ≤ 10MB (`MaxSingleFileSize`) | 防止单文件过大 |
| 路径检查 | 拒绝 `..` 和绝对路径 (`normalizeArchivePath`) | 防止目录遍历 |

### 4.6 解压到临时文件

根据用户要求，zip 解压应落到临时文件而非内存中。已有 `ArchiveService.ParseArchive` 使用 `zip.NewReader(r, size)` 配合 `io.ReaderAt`，对于大文件可进一步优化为：
- 先将上传文件保存到系统临时目录
- 使用临时文件路径进行 zip 解析
- 处理完成后清理临时文件

---

## 5. 前端 UI 设计

### 5.1 入口位置

在技能商店页面（`src/pages/store/`）的合适位置添加 **"上传 Plugin"** 按钮：

**推荐位置**：现有 **"创建 Skill"** / **"创建 MCP"** 按钮旁，或在仓库详情页的工具栏中。与 `create-capability-dialog.tsx` 并列，新增 `upload-plugin-dialog.tsx`。

### 5.2 UploadPluginDialog 组件

```tsx
// src/pages/store/components/upload-plugin-dialog.tsx
export function UploadPluginDialog(props: {
  repoId: string
  onUploaded?: (item: CapabilityItem) => void
}) {
  const [store, setStore] = createStore({
    repoId: props.repoId,
    file: null as File | null,
    uploading: false,
    progress: 0,
    error: "",
    dragOver: false,
  })
  // ...
}
```

### 5.3 交互流程

1. **点击上传按钮** → 打开对话框
2. **选择目标仓库**（下拉框，默认当前所在仓库；用户可选择自己有权限的其他仓库）
3. **拖放或点击选择 zip 文件**
   - 文件类型校验：仅接受 `.zip`
   - 文件大小校验：≤ 50MB
4. **点击"上传"** → 调用 `POST /api/plugins/upload`
5. **显示上传进度条**（使用 XMLHttpRequest 的 `onprogress`）
6. **成功** → 关闭对话框，刷新列表，显示 Toast 成功提示
7. **失败** → 对话框内显示错误信息，不上传成功关闭

### 5.4 API 封装

在 `src/pages/store/lib/api.ts` 新增：

```ts
export const pluginApi = {
  upload: (repoId: string, file: File, onProgress?: (p: number) => void) => {
    const form = new FormData()
    form.append("repo_id", repoId)
    form.append("file", file)
    return apiClient.postForm<CapabilityItem>("/plugins/upload", form, { onProgress })
  }
}
```

### 5.5 错误提示映射

| 后端错误 | 前端展示 |
|---|---|
| 400 / 非 zip 或解压失败 | "文件格式错误，请上传有效的 plugin 压缩包" |
| 400 / 缺少 CLAUDE.md | "压缩包中缺少 CLAUDE.md，请检查文件结构" |
| 403 | "你没有该仓库的上传权限" |
| 409 | "该 plugin 已存在且你没有更新权限" |
| 413 | "文件超过 50MB 限制" |

### 5.6 样式

复用现有设计系统：
- 对话框：`@/components/modal.tsx`（与创建/编辑技能保持一致）
- 按钮：`modal-btn modal-btn-primary`
- 文件选择区：拖拽区域 + 点击上传，参考现有表单风格
- 进度条：简洁的蓝色进度条

---

## 6. 注册表与商店集成

### 6.1 后端注册表适配

`server/internal/handlers/registry.go` 需要两处修改：

1. **`buildRegistryIndex` 中增加 `plugin` 类型处理**：
```go
case "plugin":
    entry.Files = append([]string{"CLAUDE.md"}, assetPaths...)
```

2. **`contentFilename` 中增加 `plugin` 类型**：
```go
case "plugin":
    return "CLAUDE.md"
```

### 6.2 前端商店集成

从前端代码调研发现，`plugin` 类型已经在前端有基础支持：

- `TYPE_META` 中已定义 `plugin` 的样式（`item-detail-content.tsx:34`）
- `sourceType === "archive"` 已显示上传图标（`item-detail-content.tsx:462`）
- 列表表格已支持 `itemType` 渲染和 `createdBy !== "system"` 的上传标识

**需要补充的前端工作**：
- 在商店侧边栏或分类中增加 **"Plugins"** 筛选/导航入口
- `upload-plugin-dialog.tsx` 上传成功后触发列表刷新（复用现有 refetch 机制）
- 确保 plugin 类型的安装命令生成正确（复用现有 `getInstallCommand`）

---

## 7. 错误处理、安全与测试

### 7.1 安全考量

| 风险 | 缓解措施 |
|---|---|
| Zip bomb | 已有 `MaxArchiveUploadSize=50MB`、`MaxUncompressedSize=50MB`、`MaxFileCount=500` 限制 |
| 路径遍历 | 已有 `normalizeArchivePath` 过滤 `..` 和绝对路径 |
| 恶意文件 | 异步触发 `enqueueScanAsync` 安全扫描 |
| 权限绕过 | 复用现有 `RepoMember` 权限检查 |

### 7.2 测试策略

1. **单元测试**：在 `archive_service_test.go` 中增加 `TestParseArchive_Zip_PluginHappyPath`
2. **集成测试**：上传一个真实的 `cospowers-solution-design-plugin.zip`，验证：
   - `CapabilityItem` 正确创建（type="plugin", content=CLAUDE.md）
   - `CapabilityAsset` 正确写入（skills/*, rules/*, templates/*, cospowers.config.json）
   - `RegistryIndex` 返回 plugin 条目
3. **前端测试**：验证上传对话框、进度条、错误提示正常

---

## 8. 附录：实现简化说明

由于后端已有的 `createItemFromArchive` 已经完整处理了 zip 上传的大部分逻辑（解析、asset 提取、二进制存储、artifact 归档、版本创建、索引、安全扫描），本功能的实际代码改动量远小于初始预期：

**核心改动清单**：
1. `archive_service.go:412` — `resolveMainFile` 增加 `case "plugin": return "CLAUDE.md"`
2. `archive_service.go:138` — `ParseArchive` 增加 `plugin` 分支（读取 CLAUDE.md 内容）
3. `capability_item.go:2462` — `createItemFromArchive` 放开 `plugin` 类型限制
4. `capability_item.go` — 在 `createItemFromArchive` 中增加 plugin 特有的 metadata 提取（从 assets 中读取 `cospowers.config.json`）
5. `registry.go:221` — `buildRegistryIndex` 增加 `plugin` 分支
6. `registry.go:289` — `contentFilename` 增加 `plugin` 分支
7. 新增 `handlers/plugin.go` — `UploadPlugin` 处理器（thin wrapper，可复用 `createItemFromArchive` 逻辑）
8. 前端新增 `upload-plugin-dialog.tsx` 组件
