# Clawlike Agent — 个人 AI 助手 + Device 委托

> **实现状态：提案中**
>
> - 状态：设计阶段
> - 目标位置：`server/internal/clawagent/`
> - 依赖框架：[trpc-group/trpc-agent-go](https://github.com/trpc-group/trpc-agent-go)（Apache-2.0，腾讯出品）
> - 参考项目：[openclaw](https://github.com/openclaw/openclaw)、[fastclaw](https://github.com/fastclaw-ai/fastclaw)

---

## 定位

在 costrict-web 平台上构建 **云端个人 AI 助手**，具备：

| 能力 | 说明 |
|------|------|
| **Soul（人格）** | 每个用户拥有可自定义的 Agent 人格（SOUL.md 等价物） |
| **Memory（记忆）** | 跨会话持久化记忆（PostgreSQL + TF-IDF 关键词检索，暂不引入向量搜索） |
| **Provider（多模型）** | 用户自配 LLM Provider（OpenAI / DeepSeek / Ollama 等） |
| **Channel（渠道）** | 通过企微等 IM 渠道与用户交互 |
| **Device Delegation（设备委托）** | 将任务以 Workspace 为单位委托到 device 执行（通过 cs-cloud API） |
| **AI 驱动通知处理** | **核心特性**：将权限请求/问卷的按钮交互升级为自然语言交互 |
| **~~Skill（技能）~~** | **暂时禁用**：从 costrict Capability Hub 按需加载到内存（不依赖文件系统） |

**无状态横向扩展**：所有运行时状态（Memory、Session、Persona、Provider）均持久化到 PostgreSQL，服务实例可随时重启或水平扩缩。

与现有 `agent-runtime` 提案的关系：两者独立。`agent-runtime` 面向平台级任务编排，本提案面向个人助手场景。

---

## 重要变更说明

### Skill 机制暂时禁用

**状态**：Skill 相关功能（原 P3 阶段）已暂时禁用，将在首期实施中跳过。

**禁用范围**：
- DBSkillRepository 及相关实现
- 从 Capability Hub 加载 Skill 内容
- skill_load、skill_list_docs 等 Skill 工具
- 与 `models.CapabilityItem` 的集成

**未来启用条件**：
- 核心对话能力、设备委托机制、AI 通知处理验证完成
- 有明确的 Skill 使用场景和需求
- 实施资源充足

**技术保留**：相关设计文档（05-skills-integration.md）保留完整，便于未来实施时参考。

### AI 驱动的企微通知处理（核心特性，已提升为 P5）

**状态**：作为 ClawAgent 的核心特性，必须实施。

**价值**：将现有权限请求/问卷的按钮卡片交互升级为 AI 驱动的自然语言交互，体现 ClawAgent 从"被动响应"升级为"主动理解意图"的能力跃迁。

**实施阶段**：P5（依赖 P4 Workspace 委托完成）

**首期实施总工期**：**21.5 天**（含 P5 核心 AI 通知处理，不含暂时禁用的 P3 Skill 集成）

详细实施计划见 [11-roadmap.md](./11-roadmap.md)，AI 通知处理设计见 [ai-driven-notification-handling.md](./ai-driven-notification-handling.md)。

---

## 文档索引

| 文档 | 说明 |
|------|------|
| [01-overview.md](./01-overview.md) | 背景动机、核心需求、框架选型对比 |
| [02-architecture.md](./02-architecture.md) | 整体架构、模块划分、与现有系统关系 |
| [03-soul-and-memory.md](./03-soul-and-memory.md) | Persona 系统 + Memory 系统设计（postgres 后端） |
| [04-providers.md](./04-providers.md) | 用户 Provider 管理、多模型动态切换 |
| [06-channels.md](./06-channels.md) | 渠道接入（含企微长连接机器人） |
| [ai-driven-notification-handling.md](./ai-driven-notification-handling.md) | **核心特性**：AI 驱动的企微通知处理（权限/问卷自然语言交互） |
| [07-device-delegation.md](./07-device-delegation.md) | Workspace 委托机制（基于 cs-cloud API 契约） |
| [08-device-proxy.md](./08-device-proxy.md) | DeviceProxyClient 接口集（对接 cs-cloud localserver API） |
| [09-api.md](./09-api.md) | REST API 设计 |
| [10-database.md](./10-database.md) | 数据库表结构 DDL + 无状态横向扩展 |
| [11-roadmap.md](./11-roadmap.md) | 分阶段实施计划 |
| [~~05-skills-integration.md~~](./05-skills-integration.md) | ~~Skill 与 Capability Hub 对接（DBSkillRepository）~~ **暂时禁用** |
