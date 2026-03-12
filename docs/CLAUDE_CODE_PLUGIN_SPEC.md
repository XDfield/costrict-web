# Claude Code Plugin 规范参考

> 来源：https://code.claude.com/docs/en/plugins-reference  
> 用途：指引技能同步模块（`ParserService` / `SyncService`）对插件仓库结构的解析与处理

---

## 一、插件包目录结构

```
my-plugin/                          ← 插件根目录（即 Git 仓库根或子目录）
├── .claude-plugin/                 ← 元数据目录（可选）
│   └── plugin.json                 ← 插件清单（可选，省略时自动发现）
├── skills/                         ← Agent Skills（推荐，新标准）
│   ├── my-skill/
│   │   ├── SKILL.md                ← 必须，技能入口
│   │   ├── reference.md            ← 可选，详细参考文档
│   │   └── scripts/                ← 可选，脚本文件
│   │       └── helper.sh
│   └── another-skill/
│       └── SKILL.md
├── commands/                       ← 旧式命令（仍兼容，.md 文件）
│   ├── deploy.md
│   └── status.md
├── agents/                         ← 子 Agent 定义
│   ├── security-reviewer.md
│   └── performance-tester.md
├── hooks/                          ← 事件钩子配置
│   └── hooks.json
├── .mcp.json                       ← MCP 服务器配置
├── .lsp.json                       ← LSP 服务器配置
├── settings.json                   ← 插件默认设置
└── scripts/                        ← 钩子和工具脚本
    ├── format-code.sh
    └── deploy.js
```

**重要约束**：
- `commands/`、`agents/`、`skills/` 等组件目录必须在**插件根目录**，不能放在 `.claude-plugin/` 内
- `.claude-plugin/` 只存放 `plugin.json` 清单文件
- 清单文件是**可选的**，省略时 Claude Code 自动按默认路径发现各组件

---

## 二、plugin.json 清单格式

### 完整 Schema

```json
{
  "name": "plugin-name",
  "version": "1.2.0",
  "description": "Brief plugin description",
  "author": {
    "name": "Author Name",
    "email": "author@example.com",
    "url": "https://github.com/author"
  },
  "homepage": "https://docs.example.com/plugin",
  "repository": "https://github.com/author/plugin",
  "license": "MIT",
  "keywords": ["keyword1", "keyword2"],
  "commands": "./custom/commands/special.md",
  "agents": "./custom/agents/",
  "skills": "./custom/skills/",
  "hooks": "./config/hooks.json",
  "mcpServers": "./mcp-config.json",
  "outputStyles": "./styles/",
  "lspServers": "./.lsp.json"
}
```

### 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | **是**（有清单时） | 插件唯一标识，kebab-case，最长 64 字符 |
| `version` | string | 否 | 语义化版本，如 `1.2.0` |
| `description` | string | 否 | 插件用途描述 |
| `author` | object | 否 | 作者信息（name / email / url） |
| `homepage` | string | 否 | 文档地址 |
| `repository` | string | 否 | 源码地址 |
| `license` | string | 否 | 许可证标识，如 `MIT` |
| `keywords` | array | 否 | 检索标签 |
| `commands` | string\|array | 否 | 自定义命令文件路径（补充默认 `commands/` 目录） |
| `agents` | string\|array | 否 | 自定义 Agent 文件路径 |
| `skills` | string\|array | 否 | 自定义 Skills 目录路径 |
| `hooks` | string\|array\|object | 否 | 钩子配置路径或内联配置 |
| `mcpServers` | string\|array\|object | 否 | MCP 服务器配置路径或内联配置 |
| `outputStyles` | string\|array | 否 | 输出样式文件路径 |
| `lspServers` | string\|array\|object | 否 | LSP 服务器配置路径或内联配置 |

**路径规则**：所有自定义路径必须以 `./` 开头，相对于插件根目录；自定义路径是对默认目录的**补充**，不是替换。

---

## 三、SKILL.md 格式

每个 Skill 是一个目录，`SKILL.md` 是必须的入口文件，由 YAML frontmatter 和 Markdown 正文组成。

### 格式结构

```markdown
---
name: skill-name
description: 技能用途描述，Claude 用此判断何时自动加载
argument-hint: "[issue-number]"
disable-model-invocation: false
user-invocable: true
allowed-tools: Read, Grep, Glob
model: claude-opus-4-5
context: fork
agent: Explore
---

技能的 Markdown 正文内容...

支持 $ARGUMENTS 占位符（全部参数）
支持 $ARGUMENTS[0]、$1 等按位置取参数
支持 ${CLAUDE_SESSION_ID} 和 ${CLAUDE_SKILL_DIR}
```

### Frontmatter 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 否 | 技能名称，省略时取目录名；仅允许小写字母、数字、连字符，最长 64 字符 |
| `description` | string | 推荐 | 触发条件描述，省略时取正文第一段 |
| `argument-hint` | string | 否 | 自动补全时显示的参数提示，如 `[filename] [format]` |
| `disable-model-invocation` | boolean | 否 | `true` 时仅允许用户手动调用，Claude 不会自动触发；默认 `false` |
| `user-invocable` | boolean | 否 | `false` 时不出现在 `/` 菜单，仅 Claude 可调用；默认 `true` |
| `allowed-tools` | string | 否 | 技能激活时 Claude 无需确认即可使用的工具列表 |
| `model` | string | 否 | 技能激活时使用的模型 |
| `context` | string | 否 | `fork` 表示在独立子 Agent 中运行 |
| `agent` | string | 否 | `context: fork` 时指定的 Agent 类型（`Explore`、`Plan`、`general-purpose` 或自定义） |
| `hooks` | object | 否 | 技能生命周期钩子配置 |

### 字符串替换变量

| 变量 | 说明 |
|------|------|
| `$ARGUMENTS` | 调用时传入的全部参数 |
| `$ARGUMENTS[N]` / `$N` | 按 0-based 索引取第 N 个参数 |
| `${CLAUDE_SESSION_ID}` | 当前会话 ID |
| `${CLAUDE_SKILL_DIR}` | 当前技能目录的绝对路径 |

---

## 四、agents/ 目录格式

每个 Agent 是一个 Markdown 文件：

```markdown
---
name: agent-name
description: Agent 专长描述，Claude 用此判断何时自动调用
---

Agent 的详细系统提示，描述其角色、专长和行为规范。
```

---

## 五、hooks/hooks.json 格式

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write|Edit",
        "hooks": [
          {
            "type": "command",
            "command": "${CLAUDE_PLUGIN_ROOT}/scripts/format-code.sh"
          }
        ]
      }
    ]
  }
}
```

### 支持的事件

| 事件 | 触发时机 |
|------|---------|
| `PreToolUse` | Claude 使用任何工具前 |
| `PostToolUse` | Claude 工具调用成功后 |
| `PostToolUseFailure` | Claude 工具调用失败后 |
| `PermissionRequest` | 权限对话框弹出时 |
| `UserPromptSubmit` | 用户提交提示词时 |
| `Notification` | Claude Code 发送通知时 |
| `Stop` | Claude 尝试停止时 |
| `SubagentStart` | 子 Agent 启动时 |
| `SubagentStop` | 子 Agent 尝试停止时 |
| `SessionStart` | 会话开始时 |
| `SessionEnd` | 会话结束时 |
| `TeammateIdle` | Agent 团队成员即将空闲时 |
| `TaskCompleted` | 任务被标记为完成时 |
| `PreCompact` | 对话历史压缩前 |

### 钩子类型

| 类型 | 说明 |
|------|------|
| `command` | 执行 shell 命令或脚本 |
| `prompt` | 用 LLM 评估提示词（`$ARGUMENTS` 占位符接收上下文） |
| `agent` | 运行带工具的 Agent 做复杂验证 |

---

## 六、.mcp.json 格式

```json
{
  "mcpServers": {
    "plugin-database": {
      "command": "${CLAUDE_PLUGIN_ROOT}/servers/db-server",
      "args": ["--config", "${CLAUDE_PLUGIN_ROOT}/config.json"],
      "env": {
        "DB_PATH": "${CLAUDE_PLUGIN_ROOT}/data"
      }
    }
  }
}
```

---

## 七、对同步模块的影响

### 7.1 文件扫描策略

同步时需要识别以下文件类型并分别处理：

| 文件路径模式 | 对应 `item_type` | 解析方式 |
|------------|-----------------|---------|
| `skills/*/SKILL.md` | `skill` | 解析 YAML frontmatter + Markdown 正文 |
| `commands/*.md` | `command` | 解析 YAML frontmatter + Markdown 正文 |
| `agents/*.md` | `agent` | 解析 YAML frontmatter + Markdown 正文 |
| `.claude-plugin/plugin.json` | 元数据 | 解析 JSON，提取插件级别信息 |
| `hooks/hooks.json` | `hook` | 解析 JSON |
| `.mcp.json` | `mcp` | 解析 JSON |

推荐的 `includePatterns` 默认值：

```json
[
  "skills/**/SKILL.md",
  "commands/**/*.md",
  "agents/**/*.md",
  ".claude-plugin/plugin.json",
  "hooks/hooks.json",
  ".mcp.json"
]
```

### 7.2 ParserService 解析要点

**SKILL.md 解析**：
- frontmatter 使用 `---` 分隔，内容为 YAML
- `name` 字段缺失时，从目录名推断（`skills/my-tool/SKILL.md` → `my-tool`）
- `description` 字段缺失时，取正文第一段文本
- `item_type` 固定为 `skill`

**commands/*.md 解析**：
- 格式与 SKILL.md 相同（frontmatter + 正文）
- `item_type` 固定为 `command`
- slug 从文件名推断（`commands/deploy.md` → `deploy`）

**agents/*.md 解析**：
- 格式与 SKILL.md 相同
- `item_type` 固定为 `agent`

**plugin.json 解析**：
- 提取插件级别的 `name`、`version`、`description`、`author` 等元数据
- 可用于填充 `SkillRegistry` 的描述字段
- 不直接对应 `SkillItem`，作为仓库级别的元数据处理

### 7.3 Slug 生成规则

```
skills/dev-tools/browser/SKILL.md  →  dev-tools-browser
commands/deploy.md                 →  deploy
agents/security-reviewer.md        →  security-reviewer
```

规则：取路径中 `skills/`、`commands/`、`agents/` 之后的部分，去掉 `/SKILL.md` 或 `.md` 后缀，将 `/` 替换为 `-`。

### 7.4 同一仓库多 Skill 的处理

一个 Git 仓库可以包含多个 Skill，每个 Skill 对应一条 `SkillItem` 记录，共享同一个 `SkillRegistry`。同步时按文件逐一处理，互不影响。

### 7.5 plugin.json 缺失的处理

清单文件是可选的。若仓库中没有 `.claude-plugin/plugin.json`，同步模块应：
- 继续正常扫描 `skills/`、`commands/`、`agents/` 等目录
- 不报错，不跳过
- `SkillRegistry` 的描述等字段保持用户手动配置的值不变

---

## 八、参考链接

- Skills 文档：https://code.claude.com/docs/en/skills
- Plugins 参考：https://code.claude.com/docs/en/plugins-reference
- Agent Skills 开放标准（跨工具，兼容 Cursor、Codex 等）
