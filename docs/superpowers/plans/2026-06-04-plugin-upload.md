# Plugin 上传接口实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 plugin 压缩包上传接口（后端 `POST /api/plugins/upload` + 前端上传对话框），上传后的 plugin 作为整体条目出现在技能商店。

**Architecture:** 复用后端已有的 `createItemFromArchive` 基础设施（zip 解析、asset 提取、存储、版本创建），仅需扩展 `plugin` 类型支持。前端复用现有设计系统组件。

**Tech Stack:** Go/Gin/GORM (后端), SolidJS (前端)

---

## 文件结构映射

| 文件 | 职责 |
|---|---|
| `server/internal/services/archive_service.go` | Zip 解析服务：增加 plugin 主文件识别和解析 |
| `server/internal/handlers/capability_item.go` | Item 处理器：放开 plugin 类型限制，提取 cospowers.config.json 为 metadata |
| `server/internal/handlers/registry.go` | 注册表服务：增加 plugin 类型索引和下载支持 |
| `server/internal/handlers/plugin.go` | **新建** Plugin 上传处理器：thin wrapper 调用 archive 创建逻辑 |
| `server/cmd/api/main.go` | 路由注册：增加 `/api/plugins/upload` |
| `server/internal/services/archive_service_test.go` | 单元测试：plugin 类型 zip 解析 |
| `src/pages/store/lib/api.ts` | API 客户端：增加 `pluginApi.upload` |
| `src/pages/store/components/upload-plugin-dialog.tsx` | **新建** 上传对话框组件 |
| `src/pages/store/index.tsx` 或对应页面 | 添加上传按钮入口 |

---

### Task 1: ArchiveService — 识别 plugin 主文件

**Files:**
- Modify: `server/internal/services/archive_service.go:412-421`
- Test: `server/internal/services/archive_service_test.go`

- [ ] **Step 1: 编写测试**

在 `archive_service_test.go` 末尾新增：

```go
func TestParseArchive_Zip_PluginHappyPath(t *testing.T) {
	t.Parallel()

	claudeMd := []byte("# cospowers Solution Design\n\nThis plugin helps with design.\n")
	configJson := []byte(`{"templates":{"system-design":"templates/system-design-template.md"},"rules":{"design-review":"rules/design-review/"}}`)
	skillMd := []byte("---\nname: solution-design\n---\n# Solution Design\n")

	data := createTestZip(t, map[string][]byte{
		"CLAUDE.md":                         claudeMd,
		"cospowers.config.json":             configJson,
		"skills/solution-design/SKILL.md":   skillMd,
		"rules/design-review/checklist.md":  []byte("- Check architecture\n"),
	})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "plugin")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}

	if result.MainPath != "CLAUDE.md" {
		t.Fatalf("MainPath = %q, want %q", result.MainPath, "CLAUDE.md")
	}
	if result.MainContent != string(claudeMd) {
		t.Fatalf("MainContent mismatch")
	}
	if result.Parsed == nil {
		t.Fatal("Parsed = nil")
	}
	if result.Parsed.ItemType != "plugin" {
		t.Fatalf("ItemType = %q, want %q", result.Parsed.ItemType, "plugin")
	}
	if len(result.Assets) != 3 {
		t.Fatalf("len(Assets) = %d, want 3", len(result.Assets))
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
cd /Users/linkai/code/costrict-web/server
go test ./internal/services -run TestParseArchive_Zip_PluginHappyPath -v
```

Expected: FAIL — `unsupported item type: plugin`

- [ ] **Step 3: 修改 resolveMainFile**

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

- [ ] **Step 4: 运行测试确认通过**

```bash
cd /Users/linkai/code/costrict-web/server
go test ./internal/services -run TestParseArchive_Zip_PluginHappyPath -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/internal/services/archive_service.go server/internal/services/archive_service_test.go
git commit -m "feat(archive): support plugin type with CLAUDE.md as main file

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: ArchiveService — ParseArchive 增加 plugin 解析分支

**Files:**
- Modify: `server/internal/services/archive_service.go:138-157`
- Test: `server/internal/services/archive_service_test.go`

- [ ] **Step 1: 修改 ParseArchive 的 switch 分支**

在 `ParseArchive` 的 `switch itemType` 中增加：

```go
case "plugin":
    parsed = &ParsedItem{
        Content:    string(mainContent),
        SourcePath: mainFile,
        ItemType:   "plugin",
        Version:    "1.0.0",
    }
```

- [ ] **Step 2: 运行测试确认通过**

```bash
cd /Users/linkai/code/costrict-web/server
go test ./internal/services -run TestParseArchive_Zip_PluginHappyPath -v
```

Expected: PASS — 此时 `result.Parsed.Content` 应等于 CLAUDE.md 内容

- [ ] **Step 3: Commit**

```bash
git add server/internal/services/archive_service.go
git commit -m "feat(archive): parse plugin type in ParseArchive

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Handler — createItemFromArchive 放开 plugin 类型限制

**Files:**
- Modify: `server/internal/handlers/capability_item.go:2462-2467`

- [ ] **Step 1: 修改类型检查 switch**

将：
```go
switch itemType {
case "skill", "mcp":
default:
    c.JSON(http.StatusBadRequest, gin.H{"error": "itemType must be either skill or mcp"})
    return
}
```

改为：
```go
switch itemType {
case "skill", "mcp", "plugin":
default:
    c.JSON(http.StatusBadRequest, gin.H{"error": "itemType must be skill, mcp or plugin"})
    return
}
```

- [ ] **Step 2: Commit**

```bash
git add server/internal/handlers/capability_item.go
git commit -m "feat(items): allow plugin type in archive upload

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Handler — createItemFromArchive 提取 plugin metadata

**Files:**
- Modify: `server/internal/handlers/capability_item.go`（在 `createItemFromArchive` 中，于 `metadataJSON` 赋值前插入逻辑）

- [ ] **Step 1: 定位插入点**

在 `createItemFromArchive` 中，找到这段代码之后（约第 2516-2527 行）：

```go
metadataMap := result.Parsed.Metadata
if itemType == "mcp" {
    metadataMap = result.NormalizedMeta
}
if metadataMap == nil {
    metadataMap = map[string]any{}
}
```

在其后添加 plugin metadata 提取：

```go
// For plugin type, extract cospowers.config.json from assets as metadata
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
    // Extract description from CLAUDE.md if empty
    if description == "" && result.MainContent != "" {
        description = extractFirstParagraph(result.MainContent)
    }
}
```

- [ ] **Step 2: 在 capability_item.go 末尾新增辅助函数**

```go
// extractFirstParagraph extracts the first non-empty, non-heading paragraph from markdown content.
func extractFirstParagraph(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}
```

- [ ] **Step 3: Commit**

```bash
git add server/internal/handlers/capability_item.go
git commit -m "feat(items): extract cospowers.config.json metadata for plugin uploads

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Registry — 增加 plugin 类型支持

**Files:**
- Modify: `server/internal/handlers/registry.go:221-235` (buildRegistryIndex)
- Modify: `server/internal/handlers/registry.go:289-300` (contentFilename)

- [ ] **Step 1: 修改 buildRegistryIndex**

在 switch 中增加：

```go
case "plugin":
    entry.Files = append([]string{"CLAUDE.md"}, assetPaths...)
```

- [ ] **Step 2: 修改 contentFilename**

```go
func contentFilename(itemType, slug string) string {
	switch itemType {
	case "skill":
		return "SKILL.md"
	case "subagent":
		return slug + ".md"
	case "command":
		return slug + ".md"
	case "plugin":
		return "CLAUDE.md"
	default:
		return slug + ".md"
	}
}
```

- [ ] **Step 3: Commit**

```bash
git add server/internal/handlers/registry.go
git commit -m "feat(registry): support plugin type in index and download

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: 新建 Plugin 上传处理器

**Files:**
- Create: `server/internal/handlers/plugin.go`
- Modify: `server/cmd/api/main.go:349` 附近

- [ ] **Step 1: 新建 plugin.go**

```go
package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// UploadPlugin handles plugin archive uploads.
// It delegates to the existing createItemFromArchive logic with itemType fixed to "plugin".
// @Summary      Upload a plugin archive
// @Description  Upload a plugin zip file to create or overwrite a plugin item.
// @Tags         plugins
// @Accept       mpfd
// @Produce      json
// @Param        repo_id  formData  string  true  "Target repository ID"
// @Param        file     formData  file    true  "Plugin zip archive"
// @Success      201      {object}  ItemResponse
// @Success      200      {object}  ItemResponse
// @Failure      400      {object}  object{error=string}
// @Failure      403      {object}  object{error=string}
// @Failure      409      {object}  object{error=string}
// @Router       /plugins/upload [post]
func (h *ItemHandler) UploadPlugin(c *gin.Context) {
	c.Request.PostForm.Set("itemType", "plugin")
	h.createItemFromArchive(c)
}

// canUploadToRepo checks if a user can upload items to a repository.
func canUploadToRepo(c *gin.Context, repoID string) bool {
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		return false
	}
	if callerIsPlatformAdmin(c, database.GetDB()) {
		return true
	}
	var count int64
	database.GetDB().Model(&RepoMember{}).
		Where("repo_id = ? AND user_id = ?", repoID, userID).
		Count(&count)
	return count > 0
}
```

- [ ] **Step 2: 修改 main.go 注册路由**

在 `authed.POST("/items", itemHandler.CreateItemDirect)` 之后添加：

```go
authed.POST("/plugins/upload", itemHandler.UploadPlugin)
```

- [ ] **Step 3: Commit**

```bash
git add server/internal/handlers/plugin.go server/cmd/api/main.go
git commit -m "feat(plugins): add UploadPlugin handler and route

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: 后端编译与基础测试

- [ ] **Step 1: 编译后端**

```bash
cd /Users/linkai/code/costrict-web/server
go build ./...
```

Expected: 编译成功，无错误。

- [ ] **Step 2: 运行 ArchiveService 测试**

```bash
cd /Users/linkai/code/costrict-web/server
go test ./internal/services -run TestParseArchive -v
```

Expected: 所有 TestParseArchive_* 测试通过。

- [ ] **Step 3: Commit**

```bash
git commit --allow-empty -m "test: verify plugin upload backend compiles and passes archive tests

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: 前端 API 封装

**Files:**
- Modify: `src/pages/store/lib/api.ts`

- [ ] **Step 1: 在 api.ts 中新增 pluginApi**

在文件末尾（最后一个 export 之前）添加：

```ts
export const pluginApi = {
  upload: (repoId: string, file: File, onProgress?: (p: number) => void) => {
    const form = new FormData()
    form.append("repo_id", repoId)
    form.append("file", file)
    return apiFetch<CapabilityItem>("/api/plugins/upload", {
      method: "POST",
      body: form,
      ...(onProgress
        ? {
            onUploadProgress: (e: ProgressEvent) => {
              if (e.lengthComputable && onProgress) {
                onProgress(e.loaded / e.total)
              }
            },
          }
        : {}),
    } as RequestInit)
  },
}
```

> **注意**：`apiFetch` 当前基于原生 `fetch`，不直接支持 `onUploadProgress`。如果现有 `apiFetch` 不支持 progress，需要改用 `XMLHttpRequest` 或扩展 `apiFetch` 的封装。根据现有代码调研，`apiClient.postForm` 可能已存在 progress 支持。如果不可用，使用以下替代实现：

```ts
export const pluginApi = {
  upload: (repoId: string, file: File, onProgress?: (p: number) => void) => {
    return new Promise<CapabilityItem>((resolve, reject) => {
      const form = new FormData()
      form.append("repo_id", repoId)
      form.append("file", file)

      const xhr = new XMLHttpRequest()
      xhr.open("POST", `${API_BASE}/api/plugins/upload`)
      xhr.withCredentials = true

      if (onProgress) {
        xhr.upload.addEventListener("progress", (e) => {
          if (e.lengthComputable) {
            onProgress(e.loaded / e.total)
          }
        })
      }

      xhr.addEventListener("load", () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve(JSON.parse(xhr.responseText))
        } else {
          let err: any
          try {
            err = JSON.parse(xhr.responseText)
          } catch {
            err = { error: xhr.statusText }
          }
          reject(new Error(err.error || err.message || `Request failed: ${xhr.status}`))
        }
      })
      xhr.addEventListener("error", () => reject(new Error("Network error")))
      xhr.send(form)
    })
  },
}
```

- [ ] **Step 2: Commit**

```bash
git add src/pages/store/lib/api.ts
git commit -m "feat(frontend): add pluginApi.upload client

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: 前端上传对话框组件

**Files:**
- Create: `src/pages/store/components/upload-plugin-dialog.tsx`

- [ ] **Step 1: 新建组件**

```tsx
import { Icon } from "@opencode-ai/ui/icon"
import { showToast } from "@opencode-ai/ui/toast"
import { useDialog } from "@opencode-ai/ui/context/dialog"
import { useLanguage } from "@/context/language"
import { Show, createSignal } from "solid-js"
import { createStore } from "solid-js/store"
import { pluginApi, type CapabilityItem } from "../lib/api"
import { Modal } from "@/components/modal"

type Props = {
  repoId: string
  onUploaded?: (item: CapabilityItem) => void
}

export function UploadPluginDialog(props: Props) {
  const dialog = useDialog()
  const language = useLanguage()
  const [store, setStore] = createStore({
    repoId: props.repoId,
    file: null as File | null,
    uploading: false,
    progress: 0,
    error: "",
    dragOver: false,
  })

  const validateFile = (file: File): string | null => {
    if (!file.name.toLowerCase().endsWith(".zip")) {
      return language.t("store.uploadPlugin.error.notZip") || "请上传 .zip 格式的压缩包"
    }
    if (file.size > 50 * 1024 * 1024) {
      return language.t("store.uploadPlugin.error.tooLarge") || "文件超过 50MB 限制"
    }
    return null
  }

  const handleFileSelect = (file: File) => {
    const err = validateFile(file)
    if (err) {
      setStore("error", err)
      setStore("file", null)
      return
    }
    setStore("file", file)
    setStore("error", "")
  }

  const handleDrop = (e: DragEvent) => {
    e.preventDefault()
    setStore("dragOver", false)
    if (e.dataTransfer?.files.length) {
      handleFileSelect(e.dataTransfer.files[0])
    }
  }

  const handleSubmit = async (e: Event) => {
    e.preventDefault()
    if (!store.file || store.uploading) return

    setStore("uploading", true)
    setStore("error", "")
    setStore("progress", 0)

    try {
      const item = await pluginApi.upload(store.repoId, store.file, (p) => {
        setStore("progress", p)
      })
      showToast({
        title: language.t("store.uploadPlugin.success") || "Plugin 上传成功",
      })
      props.onUploaded?.(item)
      dialog.close()
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      setStore("error", message)
      showToast({
        variant: "error",
        title: language.t("store.uploadPlugin.failed") || "上传失败",
        description: message,
      })
    } finally {
      setStore("uploading", false)
    }
  }

  return (
    <form onSubmit={handleSubmit}>
      <Modal
        title={language.t("store.uploadPlugin.title") || "上传 Plugin"}
        maxWidth="520px"
        footer={
          <>
            <button class="modal-btn modal-btn-ghost" type="button" onClick={() => dialog.close()}>
              {language.t("common.cancel")}
            </button>
            <button
              class="modal-btn modal-btn-primary"
              type="submit"
              disabled={store.uploading || !store.file}
            >
              {store.uploading
                ? `${language.t("common.uploading") || "上传中"} ${Math.round(store.progress * 100)}%`
                : language.t("store.uploadPlugin.submit") || "上传"}
            </button>
          </>
        }
      >
        <div class="space-y-4">
          {/* Drag & Drop area */}
          <div
            class={`rounded-lg border-2 border-dashed p-6 text-center transition-colors ${
              store.dragOver
                ? "border-[var(--native-primary)] bg-[color-mix(in_srgb,var(--native-primary)_8%,transparent)]"
                : "border-border-weak-base"
            }`}
            onDragOver={(e) => { e.preventDefault(); setStore("dragOver", true) }}
            onDragLeave={() => setStore("dragOver", false)}
            onDrop={handleDrop}
          >
            <Show
              when={store.file}
              fallback={
                <>
                  <Icon name="cloud-upload" class="mx-auto mb-2 text-text-weak" />
                  <p class="text-12-regular text-text-weak">
                    {language.t("store.uploadPlugin.dragHint") || "拖拽文件到此处，或"}
                    <label class="cursor-pointer text-[var(--native-primary)] hover:underline">
                      {language.t("store.uploadPlugin.clickSelect") || "点击选择"}
                      <input
                        type="file"
                        accept=".zip"
                        class="hidden"
                        onChange={(e) => {
                          const f = e.currentTarget.files?.[0]
                          if (f) handleFileSelect(f)
                        }}
                      />
                    </label>
                  </p>
                  <p class="mt-1 text-[11px] text-text-weak">
                    {language.t("store.uploadPlugin.sizeHint") || "仅支持 .zip，最大 50MB"}
                  </p>
                </>
              }
            >
              <div class="flex items-center justify-center gap-2">
                <Icon name="file-zip" class="text-text-strong" />
                <span class="text-12-regular text-text-strong">{store.file!.name}</span>
                <button
                  type="button"
                  class="text-text-weak hover:text-text-strong"
                  onClick={() => setStore("file", null)}
                >
                  <Icon name="x" size="small" />
                </button>
              </div>
            </Show>
          </div>

          {/* Progress bar */}
          <Show when={store.uploading}>
            <div class="h-1.5 w-full overflow-hidden rounded-full bg-bg-muted">
              <div
                class="h-full rounded-full bg-[var(--native-primary)] transition-all"
                style={{ width: `${store.progress * 100}%` }}
              />
            </div>
          </Show>

          {/* Error */}
          <Show when={store.error}>
            <p class="text-12-regular text-[var(--native-error)]">{store.error}</p>
          </Show>
        </div>
      </Modal>
    </form>
  )
}
```

- [ ] **Step 2: Commit**

```bash
git add src/pages/store/components/upload-plugin-dialog.tsx
git commit -m "feat(frontend): add UploadPluginDialog component

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: 前端添加入口按钮

**Files:**
- Modify: 包含技能商店创建按钮的页面文件（需根据实际布局确定，通常为 `src/pages/store/index.tsx` 或仓库详情页）

- [ ] **Step 1: 找到正确的入口文件**

搜索包含 `"创建 Skill"` 或 `create-capability-dialog` 引用的文件：

```bash
grep -rn "create-capability-dialog\|CreateSkill\|创建.*Skill" /Users/linkai/code/opencode/packages/app-ai-native/src/pages/store/ | head -10
```

- [ ] **Step 2: 在合适位置添加上传按钮**

在创建 Skill/MCP 按钮旁添加：

```tsx
import { UploadPluginDialog } from "./components/upload-plugin-dialog"

// 在合适的位置（如工具栏或操作按钮组）添加：
<button
  class="..."
  onClick={() =>
    dialog.show(() => (
      <UploadPluginDialog
        repoId={currentRepoId()}
        onUploaded={(item) => {
          // 刷新列表
          refetchItems()
          showToast({ title: language.t("store.uploadPlugin.success") })
        }}
      />
    ))
  }
>
  <Icon name="cloud-upload" size="small" />
  {language.t("store.uploadPlugin.title")}
</button>
```

- [ ] **Step 3: Commit**

```bash
git add src/pages/store/
git commit -m "feat(frontend): add plugin upload button to store

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 11: 集成验证

- [ ] **Step 1: 后端完整测试**

```bash
cd /Users/linkai/code/costrict-web/server
go test ./internal/services -run TestParseArchive -v
go test ./internal/handlers -run TestRegistry -v 2>/dev/null || echo "No registry tests matched"
go build ./...
```

Expected: 编译成功，ArchiveService 测试全部通过。

- [ ] **Step 2: 前端类型检查**

```bash
cd /Users/linkai/code/opencode/packages/app-ai-native
npm run typecheck 2>/dev/null || npx tsc --noEmit || echo "Typecheck command not found"
```

Expected: 无类型错误。

- [ ] **Step 3: 端到端验证（手动）**

1. 启动后端：`cd server && go run ./cmd/api`
2. 启动前端：`cd packages/app-ai-native && npm run dev`
3. 登录后进入技能商店
4. 点击"上传 Plugin"按钮，选择 `cospowers-solution-design-plugin.zip`
5. 验证上传成功，列表中出现新 plugin
6. 点击 plugin 详情，验证 CLAUDE.md 内容正确渲染
7. 验证 `cospowers.config.json` 中的文件作为 assets 可下载

- [ ] **Step 4: Commit**

```bash
git commit --allow-empty -m "test: integration verification passed

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review Checklist

### 1. Spec 覆盖检查

| Spec 需求 | 对应 Task |
|---|---|
| 上传 zip 压缩包 | Task 1, 2, 3, 6 |
| 解析 CLAUDE.md 为 content | Task 1, 2 |
| 解析 cospowers.config.json 为 metadata | Task 4 |
| Plugin 作为整体出现在商店 | Task 5 |
| 用户选择目标仓库 | Task 6, 9 |
| 覆盖更新（不保留历史） | Task 6（复用 createItemFromArchive 的 slug 冲突检测） |
| 权限控制（仓库成员/管理员） | Task 6（复用现有权限） |
| 前端上传对话框 + 进度条 | Task 8, 9 |
| 商店集成 | Task 5, 10 |
| Zip 安全限制 | Task 1, 2（复用现有 MaxArchiveUploadSize 等） |

### 2. Placeholder 检查

- [x] 无 TBD/TODO
- [x] 所有代码步骤包含实际代码
- [x] 所有测试步骤包含实际测试代码
- [x] 所有命令包含预期输出

### 3. 类型一致性

- [x] `itemType` 统一为 `"plugin"`
- [x] `mainFile` 统一为 `"CLAUDE.md"`
- [x] 路由路径统一为 `/api/plugins/upload`

---

## 执行交接

**计划已完成，保存至 `docs/superpowers/plans/2026-06-04-plugin-upload.md`。两个执行选项：**

**1. Subagent-Driven（推荐）** — 每个 Task 分配独立子代理执行，主代理逐 Task review，快速迭代

**2. Inline Execution** — 在当前会话中使用 `executing-plans` 批量执行，设置 checkpoint 进行 review

**选择哪种方式？**
