# 1. 概述

## 1.1 背景与动机

costrict-web 当前是设备管控平台：用户通过浏览器/SSE 远程操作 device 上的 opencode agent。平台本身没有 AI 对话能力——`server/internal/llm/client.go` 仅用于 Skill 安全扫描，不支持对话、流式、Tool Calling。

用户希望在平台上拥有一位 **云端个人 AI 助手**，类似 OpenClaw 的体验：

- 通过企微等 IM 渠道与助手对话
- 助手有记忆、有人格（Soul）
- 助手能将任务 **委托** 到用户绑定的 device workspace 执行
- 每个用户有自己的 Provider 配置、Persona 和记忆空间
- **助手能理解自然语言意图，自动处理权限请求和问卷事件**（核心特性）

## 1.2 核心需求

| 编号 | 需求 | 说明 |
|------|------|------|
| R1 | **Soul / Persona** | 每个用户拥有可自定义的 Agent 人格定义 |
| R2 | **Memory** | 每用户一份 memory（TEXT 字段），每轮对话全量注入到 system prompt，每轮结束后 LLM 自动合并更新（无向量、无关键词检索） |
| R3 | **Provider** | 用户自配 LLM Provider，支持 OpenAI / DeepSeek / Ollama 等多模型动态切换 |
| R4 | **Channel** | 通过企微长连接机器人等 IM 渠道接收消息和回复 |
| R5 | **Device Delegation** | 将任务委托到 device workspace 执行，同步等待或异步回调 |
| R6 | **多用户隔离** | Persona / Memory / Session / Provider 按 userID 严格隔离 |
| R7 | **OpenAI 兼容 API** | 暴露 `/v1/chat/completions` 供第三方客户端接入 |
| **R8** | **AI 驱动通知处理** | **核心特性**：将权限请求/问卷的按钮交互升级为自然语言交互，AI 自动理解和处理 |
| ~~R9~~ | **~~Skill~~** | ~~从 costrict Capability Hub 加载和使用 Skill（SKILL.md 格式）~~ **暂时禁用** |

## 1.3 框架选型

经过对 GitHub 上 Go 技术栈 AI Agent 框架的全面调研：

| 框架 | Stars | 语言 | Soul | Memory | Skill | Tool | Channel | 嵌入性 | 结论 |
|------|-------|------|------|--------|-------|------|---------|--------|------|
| **trpc-agent-go** | 1.3k | Go | 代码注入 | TF-IDF 关键词（本方案不用向量） | ~~SKILL.md~~ (暂时禁用) | Function/MCP | 可扩展 | **Go 库，可嵌入** | **首选** |
| openclaw | 378k | TS | SOUL.md | MEMORY.md | ClawHub | Bash/MCP | 25+ 渠道 | 独立进程 | 技术栈不符 |
| fastclaw | ~5k | Go | SOUL.md | MEMORY.md | ClawHub | MCP/插件 | 7 渠道 | 独立进程 | 无法嵌入 |

**选择 trpc-agent-go 的理由**：

1. **Go 库可嵌入**：直接 `import` 进 costrict-web 的 server 进程，零网络开销，共享数据库连接
2. **原生多用户隔离**：`Runner.Run(ctx, userID, sessionID, message)` 以 userID/sessionID 为一等公民
3. **Memory 系统**：支持 PostgreSQL 后端 + TF-IDF 关键词检索（无需 pgvector 扩展）、自动提取、记忆工具（memory_add/search/...）
4. **Skill 系统**：SKILL.md 格式与 OpenClaw 生态兼容，FSRepository 支持多根目录 + 热重载
5. **Provider 抽象**：OpenAI / Anthropic / Gemini / Ollama / DeepSeek 等开箱即用
6. **OpenAI 兼容 Server**：自带 `server/openai/` 包，可直接暴露 `/v1/chat/completions`
7. **生产验证**：腾讯元宝、腾讯视频、QQ音乐等使用

## 1.4 与 OpenClaw 的关键差异

| 维度 | OpenClaw | 本方案 |
|------|---------|--------|
| 定位 | 本地桌面个人助手 | 云端个人助手 |
| 运行方式 | 独立 daemon 进程 | 嵌入 costrict-web server |
| SubAgent 委托 | `sessions_spawn` → 本地子进程 | `device_delegate` → 远程 device workspace |
| 渠道 | 25+ 内置 | 复用 costrict-web 现有渠道适配器（企微等） |
| Skill 来源 | ClawHub / GitHub | costrict Capability Hub |
| 技术栈 | TypeScript (Node.js) | Go (嵌入 server 进程) |
