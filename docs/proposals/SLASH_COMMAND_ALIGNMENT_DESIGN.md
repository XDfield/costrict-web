# 云端 UI 与 opencode 客户端斜杠命令一致性方案

> 让云端 `app-ai-native` 的斜杠命令(slash commands)与 opencode 客户端 TUI 保持一致 — 通过 cs-cloud 作为单一命令注册中心,云端 UI 与 TUI 都基于同一份命令清单渲染。

## 1. 问题背景

云端 UI 与 opencode TUI 都通过 `/<command>` 形式触发能力,但两端命令清单当前存在显著差异,导致:

- 用户在云端用 `/model` 切模型,在 TUI 上要敲 `/models`,反之亦然
- 云端缺少 `/share` `/rename` `/themes` 等会话管理类命令
- opencode 后端 `/init` `/review` 等 prompt 模板命令通过 `sync.data.command` 自动同步到云端,但 TUI 内置的 UI 操作命令(`/themes` `/help`)没有任何同步通道

## 2. 现状分析

### 2.1 opencode 客户端的两套命令体系

| 体系 | 注册方式 | 性质 | 是否暴露给云端 |
|---|---|---|---|
| **System 1 — 后端 Command** | `Command.Default` + 用户配置 + MCP prompts + skills | LLM prompt 模板,会被展开成提示词发给模型 | **是** — 通过 `/command` 暴露,cs-cloud 透传至云端 |
| **System 2 — TUI UI 命令** | `command.register({ slash })` | TUI 客户端 UI 操作,本地执行(切主题、打开 dialog、退出等) | **否** — 只存在于 TUI 进程内 |

### 2.2 云端 UI 的两类命令

| 类型 | 来源 | 触发方式 |
|---|---|---|
| **builtin** | `use-session-commands.tsx` / `layout.tsx` 中 `command.register()` 注册的 9 个 | 调用 `command.trigger(id, "slash")` 执行本地动作 |
| **custom** | `sync.data.command`(从 cs-cloud `/agents/commands` 拉到的 System 1) | 拼成 `/<name> ` 文本,提交时走 `conversation.sessionCommand()` 发给客户端 |

### 2.3 当前差异

#### 2.3.1 命名不一致(单复数)

| 云端 UI | opencode TUI | 功能 |
|---|---|---|
| `/model` | `/models` | 切换模型 |
| `/mcp` | `/mcps` | MCP 管理 |
| `/agent` | `/agents` | Agent 切换 |
| `/workspace` | `/workspaces` | 工作区管理 |

#### 2.3.2 opencode 独有的 UI 操作命令(System 2)

云端完全缺失以下 28 个命令(含别名):

- **会话管理**: `/sessions`(`/resume` `/continue`)、`/share`、`/unshare`、`/rename`、`/timeline`、`/copy`、`/export`、`/undo`、`/redo`
- **外观/UI**: `/themes`、`/timestamps`(`/toggle-timestamps`)、`/thinking`(`/toggle-thinking`)、`/editor`、`/help`、`/exit`(`/quit` `/q`)
- **模型/服务**: `/variants`、`/connect`、`/status`、`/credit`、`/favorites`(`/fav`)、`/skills`

#### 2.3.3 云端独有的命令(Web 特性)

| 命令 | 说明 |
|---|---|
| `/open` | 打开文件 dialog(`mod+p`) — Web 特有的文件浏览器入口 |
| `/terminal` | toggle 集成终端面板 — Web 特有的视图操作 |

### 2.4 当前数据流

```
opencode (port 4096)
  GET /command  ──────────────►  仅返回 System 1
                                       │
cs-cloud (port 18080)                  ▼
  GET /agents/commands  ◄──── rewriteTo("/command") 透传
                                       │
云端 UI (web)                          ▼
  sync.command.load() ──► 仅拿到 System 1
                          + 本地硬编码 9 个 builtin
```

问题:**TUI 的 System 2 命令没有任何路径暴露给云端**,云端只能在前端硬编码,与 TUI 各走各的。

## 3. 解决思路

### 3.1 核心原则:cs-cloud 作为唯一命令注册中心

- **cs-cloud** 维护完整命令清单的真值源:合并 opencode `/command`(System 1) + TUI UI 命令清单(System 2,在 cs-cloud 内本地维护) + Web 独占命令
- **opencode TUI** 不再独占维护 System 2 — 只负责执行,清单从 cs-cloud 拉取(`GET /agents/commands?include=tui`)
- **云端 UI** 不再硬编码 builtin 命令 — 全部从 cs-cloud `/agents/commands` 拉取并渲染
- **命名以 opencode 现有为准**:`/models` `/mcps` `/agents` `/workspaces` 这套复数形式作为标准

### 3.2 命令分类(scope 字段)

cs-cloud 返回的每个命令带 `scope` 字段,标注它在哪些前端可用:

| scope | 含义 | 示例 |
|---|---|---|
| `shared` | TUI 和云端 UI 都实现 | `/models` `/share` `/compact` |
| `tui-only` | 仅 TUI 实现(无 Web 对应) | `/exit` `/editor` |
| `cloud-only` | 仅云端 UI 实现(无 TUI 对应) | `/open` `/terminal` |
| `prompt` | System 1 prompt 模板(原 custom) | `/init` `/review` `/test` |

云端 UI 渲染时过滤 `scope ∈ {shared, cloud-only, prompt}`;TUI 过滤 `scope ∈ {shared, tui-only, prompt}`。

### 3.3 不在本方案范围内的事

- **不**统一两端命令的"实现" — TUI 是终端 dialog,云端是 Web modal,UI 形态不同
- **不**强制让云端实现 100% 的 opencode 命令 — `/exit` `/editor` 在 Web 形态下没有合理对应
- **不**反向把 `/open` `/terminal` 移植到 opencode TUI — CLI 本身就在终端里

## 4. 完整命令清单(目标状态)

| # | 命令 | 别名 | scope | 描述 |
|---|---|---|---|---|
| 1 | `/new` | `/clear` | shared | 新建会话 |
| 2 | `/sessions` | `/resume` `/continue` | shared | 切换会话 |
| 3 | `/workspaces` | — | shared | 工作区管理 |
| 4 | `/models` | — | shared | 切换模型 |
| 5 | `/agents` | — | shared | Agent 切换 |
| 6 | `/mcps` | — | shared | MCP 管理 |
| 7 | `/variants` | — | shared | 模型变体切换 |
| 8 | `/connect` | — | shared | Provider 连接 |
| 9 | `/status` | — | shared | 服务状态 |
| 10 | `/credit` | — | shared | 额度查看 |
| 11 | `/themes` | — | shared | 主题切换 |
| 12 | `/help` | — | shared | 帮助 |
| 13 | `/favorites` | `/fav` | shared | 收藏 skills |
| 14 | `/skills` | — | shared | Skills 选择器 |
| 15 | `/share` | — | shared | 分享会话 |
| 16 | `/unshare` | — | shared | 取消分享 |
| 17 | `/rename` | — | shared | 重命名会话 |
| 18 | `/timeline` | — | shared | 跳转到消息 |
| 19 | `/fork` | — | shared | 会话分叉 |
| 20 | `/compact` | `/summarize` | shared | 压缩会话 |
| 21 | `/undo` | — | shared | 撤销消息 |
| 22 | `/redo` | — | shared | 重做 |
| 23 | `/copy` | — | shared | 复制 transcript |
| 24 | `/export` | — | shared | 导出 transcript |
| 25 | `/timestamps` | `/toggle-timestamps` | shared | 时间戳显示 |
| 26 | `/thinking` | `/toggle-thinking` | shared | 思考过程显示 |
| 27 | `/exit` | `/quit` `/q` | tui-only | 退出 TUI |
| 28 | `/editor` | — | tui-only | 外部编辑器 |
| 29 | `/open` | — | cloud-only | 打开文件 dialog |
| 30 | `/terminal` | — | cloud-only | 切换终端面板 |
| 31 | `/init` | — | prompt | 初始化 AGENTS.md |
| 32 | `/review` | — | prompt | 代码审查 |
| 33 | `/test` | — | prompt | 测试工作流 |
| 34 | `/project-wiki` | — | prompt | 生成项目 wiki |
| 35 | `/security-review` | — | prompt | 安全审查 |
| 36 | `/skills-capture` | — | prompt | 捕获 skill |

## 5. 接口契约

### 5.1 `GET /api/v1/agents/commands`(cs-cloud,扩展)

**Query 参数**:

| 参数 | 含义 | 默认值 |
|---|---|---|
| `include` | 命令分类过滤,逗号分隔 | `shared,prompt,cloud-only` |

**响应**:

```json
{
  "commands": [
    {
      "name": "models",
      "aliases": [],
      "title": "Switch model",
      "description": "Choose model for this session",
      "scope": "shared",
      "category": "model",
      "keybind": "mod+'",
      "source": "command"
    },
    {
      "name": "init",
      "aliases": [],
      "title": "Initialize AGENTS.md",
      "scope": "prompt",
      "source": "command",
      "agent": "build",
      "subtask": false
    }
  ]
}
```

**字段说明**:

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 命令名(无 `/` 前缀) |
| `aliases` | string[] | 别名 |
| `scope` | enum | `shared` / `tui-only` / `cloud-only` / `prompt` |
| `category` | string? | 分组(session/file/model/...),仅 UI 命令有 |
| `keybind` | string? | 推荐快捷键 |
| `source` | enum | `command` / `mcp` / `skill` |
| `agent` `subtask` 等 | string? | prompt 类专有字段(透传 opencode `Command.Info`) |

### 5.2 后向兼容

- 不传 `include` 时,默认返回 `shared+prompt+cloud-only`(现有云端 UI 行为)
- 字段 `scope` 为新增,旧版云端 UI 直接忽略不影响
- 仍兼容旧字段 `name` `description` `source`(原 opencode `Command.Info` 已有)

## 6. cs-cloud 改动

### 6.1 新增命令清单包

在 `cs-cloud/internal/localserver/` 包内新建文件(与现有 `agent_handlers.go` 同包同级,符合现行 `*_handlers.go` 命名约定)。不新建独立包,命令清单与 localserver 生命周期绑定,避免单测与生成工具跨包导入。

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 6.1.1 | `internal/localserver/commands_registry.go` | 新建 — 定义 `Command{Name, Aliases, Title, Description, Scope, Category, Keybind, Source}` 结构体 + `BuiltinCommands` 切片(26 条 UI 命令) + `BuildManifest` 合并函数 | ~280 |
| 6.1.2 | `internal/localserver/commands_registry_test.go` | 新建 — 验证清单完整性、无重复 name/alias、scope 合法、BuildManifest 合并正确 | ~150 |

清单内容(`commands_registry.go` 的核心数据结构):

```go
var BuiltinCommands = []Command{
  {Name: "new", Aliases: []string{"clear"}, Title: "New session", Scope: ScopeShared, Category: "session"},
  {Name: "sessions", Aliases: []string{"resume", "continue"}, Title: "Switch session", Scope: ScopeShared, ...},
  {Name: "models", Title: "Switch model", Scope: ScopeShared, Keybind: "mod+'", ...},
  {Name: "exit", Aliases: []string{"quit", "q"}, Scope: ScopeTuiOnly, ...},
  {Name: "open", Title: "Open file", Scope: ScopeCloudOnly, Keybind: "mod+p", ...},
  // ... 共 26 条
}
```

### 6.2 修改 localserver 路由与 handler

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 6.2.1 | `internal/localserver/server.go:58` | `GET /agents/commands` 从 `s.handleProxy` 改为 `s.handleCommands`(与 `handleListAgents` 同风格,本地构建清单 + 调用 opencode 拿 prompt 类) | ~5 |
| 6.2.2 | `internal/localserver/commands_handler.go` | 新建 — `func (s *Server) handleCommands(w, r)` 方法:解析 `include` 参数 → 调用 `commandsRegistry.BuildManifest()` → 通过现有 `handleProxy` 逻辑拿 opencode `/command` → 合并返回;与 `agent_handlers.go` 同包同级,保持 `(s *Server) handleXxx` 风格 | ~120 |
| 6.2.3 | `internal/localserver/commands_handler_test.go` | 新建 — 测试 include 过滤、scope 合并、opencode 不可达时的降级 | ~150 |
| 6.2.4 | `internal/localserver/commands_registry.go` | 同 6.1.1 的新建文件,合并到这里一行中 | (已在 6.1.1 计行数) |

### 6.3 移除旧的代理路由

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 6.3.1 | `internal/agent/driver_opencode.go:88` | 删除 `{http.MethodGet, "/agents/commands", rewriteTo("/command")}` 这一行(已被本地处理替代) | -1 |

### 6.4 cs-cloud 总量

| 项目 | 数值 |
|---|---|
| 新建文件 | 4 |
| 修改文件 | 2 |
| 估算行数 | ~700(含测试) |

## 7. 云端 UI 改动(`packages/app-ai-native`)

### 7.1 移除硬编码的 builtin 命令注册

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 7.1.1 | `src/pages/session/use-session-commands.tsx` | 删除 `slash: "..."` 字段(7 处:new/open/terminal/model/mcp/agent/compact/fork) — 这些 slash 改由 cs-cloud 清单驱动;命令本身保留(快捷键和命令面板仍用) | -10 |
| 7.1.2 | `src/pages/layout.tsx:981` | 同上,删除 `/workspace` 的 slash 字段 | -1 |

### 7.2 新增基于 cs-cloud 清单的渲染

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 7.2.1 | `src/components/prompt-input/slash-popover.tsx` | `SlashCommand` 类型新增 `scope` 字段,渲染时按 `scope` 加 badge 区分 | ~10 |
| 7.2.2 | `src/components/prompt-input.tsx:559-581` | 重写 `slashCommands` memo:不再合并 `command.options`,直接用 `sync.data.command`(已含完整清单);按 `scope === "cloud-only" \| "shared"` 过滤 | ~30 |
| 7.2.3 | `src/components/prompt-input/submit.ts:279-310` | 提交时区分:`scope === "prompt"` 走 `conversation.sessionCommand()`(原行为);`shared`/`cloud-only` 走本地 action map(7.3 注册) | ~40 |

### 7.3 新增 slash → action 映射

云端 UI 命令的本地实现(28 个 `shared`/`cloud-only` 命令)需要一个映射表:命令 name → 本地 handler。

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 7.3.1 | `src/pages/session/slash-actions.tsx` | 新建 — `SlashActionMap`:`{ "models": () => dialog.show(<DialogSelectModel/>), "share": async () => sdk.session.share(...), ... }`(共 28 个 handler;`shared` 类已存在的命令复用现有 dialog) | ~280 |
| 7.3.2 | `src/components/prompt-input/submit.ts` | 引入 `SlashActionMap`,根据 `cmd.name` 派发执行 | ~10 |

### 7.4 新增缺失的 dialog

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 7.4.1 | `src/components/dialog-session-rename.tsx` | 新建 — 重命名会话 | ~80 |
| 7.4.2 | `src/components/dialog-timeline.tsx` | 新建 — 跳转到消息(参考 opencode `DialogTimeline`) | ~120 |
| 7.4.3 | `src/components/dialog-theme-list.tsx` | 新建 — 主题切换(主题列表已在 settings,抽出独立 dialog) | ~70 |
| 7.4.4 | `src/components/dialog-help.tsx` | 新建 — 快捷键 + 命令清单帮助页 | ~100 |
| 7.4.5 | `src/components/dialog-credit.tsx` | 新建 — 额度展示 | ~60 |
| 7.4.6 | `src/components/dialog-status.tsx` | 新建 — provider/模型/index 状态摘要 | ~80 |
| 7.4.7 | `src/components/dialog-skills.tsx` | 新建 — skills 选择器(若已有 skill 视图,抽公共组件) | ~90 |
| 7.4.8 | `src/components/dialog-favorites.tsx` | 新建 — 收藏 skills 管理 | ~70 |
| 7.4.9 | `src/utils/session-export.ts` | 新建 — transcript 序列化为 markdown | ~60 |

### 7.5 i18n 文案

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 7.5.1 | `src/locales/zh-CN.json` | 新增 28 条 `command.*.title` / `command.*.description` | ~60 |
| 7.5.2 | `src/locales/en.json` | 同上(英文) | ~60 |

### 7.6 测试

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 7.6.1 | `src/components/prompt-input.test.tsx` | 验证从 cs-cloud 拉到的清单正确渲染、scope 过滤生效 | ~80 |
| 7.6.2 | `src/pages/session/slash-actions.test.tsx` | 各 slash action 派发正确 | ~120 |

### 7.7 云端 UI 总量

| 项目 | 数值 |
|---|---|
| 新建文件 | 11 |
| 修改文件 | 5 |
| 估算行数 | ~1290(含测试与 i18n) |

## 8. opencode 客户端改动(`packages/opencode`)

opencode 改动较小 — System 2 仍然由 TUI 内部 `command.register({ slash })` 驱动,但需要让 TUI 也通过 cs-cloud 拿到清单。考虑到 TUI 直连 opencode server(非通过 cs-cloud),保留 TUI 现有清单注册不变,**额外**导出一份 manifest 给 cs-cloud 在构建期内嵌即可。

### 8.1 导出 manifest

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 8.1.1 | `src/cli/cmd/tui/component/dialog-command.tsx` | `slashes()` 旁新增 `manifest()` 方法 — 返回所有 `CommandOption` 的 `{ name, aliases, title, scope: "shared" \| "tui-only" }` | ~30 |
| 8.1.2 | `src/server/server.ts`(若存在 commands 路由) | 新增 `GET /tui/manifest` 端点(可选 — 调试用,生产环境 cs-cloud 用静态内嵌清单更稳) | ~20 |

### 8.2 命令名标注 scope

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 8.2.1 | `src/cli/cmd/tui/app.tsx` | 现有 14 个 `command.register({ slash: { name } })` 处增加 `scope: "shared" \| "tui-only"` 字段(`/exit` `/editor` 标 tui-only,其余 shared) | ~20 |
| 8.2.2 | `src/cli/cmd/tui/routes/session/index.tsx` | 现有 12 个 session 命令注册增加 `scope: "shared"` | ~15 |
| 8.2.3 | `src/cli/cmd/tui/component/prompt/index.tsx` | `/editor`(tui-only)、`/skills`(shared)增加 scope | ~5 |

### 8.3 cs-cloud 内嵌清单同步策略

cs-cloud 的 `internal/localserver/commands_registry.go` 中的 26 条记录,通过 build-time 脚本从 opencode `manifest()` 输出生成:

| # | 文件 | 改动点 | 估算行数 |
|---|---|---|---|
| 8.3.1 | `cs-cloud/scripts/sync-commands.sh` | 新建 — 调用 opencode 的导出能力(或读取 `dialog-command.tsx`)生成 `internal/localserver/commands_registry_builtin.gen.go` | ~50 |
| 8.3.2 | `cs-cloud/internal/localserver/commands_registry_builtin.gen.go` | 自动生成,纳入仓库便于审查 | 自动生成 |

CI 中加入 `go generate ./internal/localserver/...` 校验,提交时 builtin 清单与 opencode 不一致则失败。

### 8.4 opencode 总量

| 项目 | 数值 |
|---|---|
| 新建文件 | 1(server 路由可选) |
| 修改文件 | 4 |
| 估算行数 | ~90 |

## 9. 数据流(目标状态)

```
opencode (port 4096)
  GET /command  ─────────► 返回 System 1(prompt 模板)
                             │
cs-cloud (port 18080)        │
  GET /agents/commands       │
    ├── 调用 opencode /command 拿 System 1
    ├── 加载本地内嵌的 26 条 UI 命令(System 2 + Web 独占)
    ├── 按 ?include=... 过滤
    └── 合并返回完整清单
                             │
云端 UI (web)                ▼
  sync.command.load()
    └─► 直接渲染清单(不再硬编码 builtin)
        ├── scope=prompt → 走 sessionCommand()
        ├── scope=shared/cloud-only → 走 SlashActionMap
                             │
opencode TUI (终端)          │
  TUI 仍直连 opencode,内部 command.register() 不变
  cs-cloud 仅用于云端 UI 场景
```

## 10. 影响面

### 10.1 向后兼容

- **命名变更**:云端 `/model` `/mcp` `/agent` `/workspace` 在前端 `SlashActionMap` 中保留为别名,过渡 1 个版本
- **`/agents/commands` 接口**:`include` 参数缺省值与旧行为兼容;新增 `scope` 字段旧客户端忽略
- **opencode 直连场景**:TUI 直连 opencode 不经过 cs-cloud,行为完全不变

### 10.2 部署依赖

cs-cloud 升级先行,云端 UI 依赖新版 cs-cloud(`/agents/commands` 返回完整清单)。云端 UI 需检测 cs-cloud 版本,旧版 cs-cloud 时降级回硬编码 builtin(保留兜底分支 1 个版本)。

### 10.3 性能

`/agents/commands` 由透传变为合并响应,延迟略增(多一次本地清单序列化)。但 cs-cloud 与 opencode 同机进程内通信,影响 < 5ms,可忽略。

### 10.4 文档

- `costrict-web/docs/CLAUDE_CODE_PLUGIN_SPEC.md` 若有命令清单需同步
- cs-cloud `README.md` 增加 `/agents/commands` 接口的 scope 字段说明

## 11. 总量估算

| 项目 | 文件改动 | 估算行数 |
|---|---|---|
| cs-cloud(Go) | 新建 4,修改 2 | ~700 |
| 云端 UI(TypeScript) | 新建 11,修改 5 | ~1290 |
| opencode(TypeScript) | 新建 1,修改 4 | ~90 |
| **合计** | **新建 16,修改 11** | **~2080** |

**预计工作量**:1 名工程师 1.5–2 周(含联调与 i18n)。

## 12. 关键风险

1. **cs-cloud 与 opencode TUI 清单漂移**
   两端都维护 System 2 命令清单,易出现不一致。**缓解**:第 8.3 节的 `sync-commands` 脚本 + CI 校验,任何一方修改命令必须同步另一方,提交时 lint 失败。

2. **云端 UI 启动期 cs-cloud 不可达**
   `sync.command.load()` 失败时云端无任何 builtin 命令,体验崩塌。**缓解**:云端 UI 内置一份 fallback 清单(从构建期 cs-cloud 端拷贝快照),仅命名/title 用于占位,不挂 handler;cs-cloud 恢复后覆盖。

3. **prompt 类命令与 UI 命令命名冲突**
   未来 opencode 后端 Command 新增 `/share` prompt 模板 → 与 TUI `/share` UI 命令冲突。**缓解**:`scope: "prompt"` 优先级最高(走 LLM),`shared/cloud-only` 次之;cs-cloud 合并清单时检测到同 name 不同 scope 直接报错,强制 opencode 上游改名。

4. **scope 标注成本**
   opencode 现有 26 个 TUI 命令需要逐个标 scope,随 opencode 上游 rebase 时易遗漏。**缓解**:opencode `CommandOption` 类型 `scope` 字段设为必填,缺失时 TS 编译失败。

5. **国际化 i18n 同步**
   28 个新命令的 zh-CN/en 文案,工作量集中。**缓解**:i18n key 命名规范(`command.<name>.title` / `command.<name>.description`),对照命令清单批量生成模板,人工补译。
