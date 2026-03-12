# Plugin 规范合规性改造任务

参考规范：`docs/CLAUDE_CODE_PLUGIN_SPEC.md`

## 问题分析

对照规范第七章（对同步模块的影响），梳理现有实现的差距：

### 差距清单

| # | 问题 | 位置 | 规范要求 | 现有实现 |
|---|------|------|---------|---------|
| G1 | includePatterns 默认值不精确 | `sync_service.go:150` | `skills/**/SKILL.md`, `commands/**/*.md`, `agents/**/*.md`, `.claude-plugin/plugin.json`, `hooks/hooks.json`, `.mcp.json` | `**/*.md`, `**/SKILL.md`, `**/plugin.json`, `**/.claude-plugin/plugin.json` |
| G2 | plugin.json 被当成普通 CapabilityItem 存储 | `parser_service.go:101`, `sync_service.go:53` | plugin.json 是仓库级元数据，用于更新 Registry 描述字段，不直接存为 item | `ParsePluginJSON` 返回 `ItemType="skill"` 的 item 被正常入库 |
| G3 | SKILL.md 的 name 从文件名推断而非目录名 | `parser_service.go:192` | `skills/my-tool/SKILL.md` → name = `my-tool`（目录名） | `inferNameFromPath` 取文件名 `SKILL`（去扩展后） |
| G4 | 缺少 hooks/hooks.json 专用解析器 | `parser_service.go`, `sync_service.go:48` | hooks.json 解析为 `item_type: hook` | 无专用解析，会被 `ParseSKILLMD` 处理（JSON 当 Markdown 解析会出错） |
| G5 | 缺少 .mcp.json 专用解析器 | `parser_service.go`, `sync_service.go:48` | .mcp.json 解析为 `item_type: mcp` | 无专用解析 |
| G6 | matchGlob 对 `skills/**/SKILL.md` 的匹配不准确 | `git_service.go:156` | 精确匹配 skills/ 下任意深度的 SKILL.md | `**` 展开后只用 `filepath.Match(suffix, Base(checkName))` 检查文件名，无法校验完整路径 |
| G7 | InferItemType 对 `.mcp.json` 无识别 | `parser_service.go:148` | `.mcp.json` → `mcp` | 无 `.mcp.json` 分支，走 default 返回 `skill` |

---

## 任务清单

### T1 修复 includePatterns 默认值

- **文件**：`server/internal/services/sync_service.go`
- **位置**：`SyncRegistry` 函数中 `cfg.IncludePatterns` 默认赋值（第 150 行）
- **改动**：替换为规范推荐的精确 patterns
- **状态**：✅ 完成

```go
// 改前
cfg.IncludePatterns = []string{"**/*.md", "**/SKILL.md", "**/plugin.json", "**/.claude-plugin/plugin.json"}

// 改后
cfg.IncludePatterns = []string{
    "skills/**/SKILL.md",
    "commands/**/*.md",
    "agents/**/*.md",
    ".claude-plugin/plugin.json",
    "hooks/hooks.json",
    ".mcp.json",
}
```

---

### T2 plugin.json 改为更新 Registry 元数据而非存为 item

- **文件**：`server/internal/services/sync_service.go`
- **位置**：`parseFile` 函数 + 主循环文件处理逻辑
- **改动**：
  1. 主循环中检测到 `plugin.json` 时，调用 `applyPluginJSON` 更新 `CapabilityRegistry` 的 `name`/`description`，然后 `continue`，不写入 `CapabilityItem`
  2. 新增 `applyPluginJSON` 方法
- **状态**：✅ 完成

---

### T3 SKILL.md name 推断改为取目录名

- **文件**：`server/internal/services/parser_service.go`
- **位置**：`inferNameFromPath` 函数（第 192 行）
- **改动**：当文件名为 `SKILL.md`（不区分大小写）时，取其父目录名作为 name
- **状态**：✅ 完成

```go
// 规范示例：skills/my-tool/SKILL.md → name = "my-tool"
func inferNameFromPath(filePath string) string {
    base := strings.ToLower(filepath.Base(filePath))
    if base == "skill.md" {
        // 取父目录名
        dir := filepath.Dir(filePath)
        name := filepath.Base(dir)
        // ...格式化
        return name
    }
    // 原逻辑...
}
```

---

### T4 新增 hooks/hooks.json 解析器

- **文件**：`server/internal/services/parser_service.go`
- **改动**：新增 `ParseHooksJSON`，ItemType=`hook`，Content 存储原始 JSON
- **文件**：`server/internal/services/sync_service.go`
- **改动**：`parseFile` 中识别 `hooks.json` 路由到新解析器
- **状态**：✅ 完成

---

### T5 新增 .mcp.json 解析器

- **文件**：`server/internal/services/parser_service.go`
- **改动**：新增 `ParseMCPJSON`，ItemType=`mcp`，Content 存储原始 JSON
- **文件**：`server/internal/services/sync_service.go`
- **改动**：`parseFile` 中识别 `.mcp.json` 路由到新解析器
- **状态**：✅ 完成

---

### T6 修复 matchGlob 对多级路径的匹配

- **文件**：`server/internal/services/git_service.go`
- **位置**：`matchGlob` 函数
- **改动**：suffix 含 `/` 时改用完整路径后缀匹配，不再只比较 Base
- **状态**：✅ 完成

---

### T7 InferItemType 增加 .mcp.json 识别

- **文件**：`server/internal/services/parser_service.go`
- **位置**：`InferItemType` 函数
- **改动**：增加 `base == ".mcp.json"` 分支，返回 `"mcp"`
- **状态**：✅ 完成

---

## 完成情况

**全部 7 项任务已完成。**

## 执行顺序（已执行）

```
T6（matchGlob 修复）→ T1（includePatterns）→ T3（name 推断）→ T4+T5（新解析器）→ T7（InferItemType）→ T2（plugin.json 元数据化）
```

T6 先行，确保扫描文件列表正确；T2 最后，因为涉及主循环逻辑改动最大。

---

## 验证方法

```bash
cd server
go build ./...
go vet ./...
go test ./internal/services/... -v
```
