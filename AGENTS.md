# AGENTS.md

本文件记录与 costrict-cloud 项目相关的仓库和目录信息，方便开发和调试时快速定位。

## 项目简介

**costrict-cloud** 是一个云端平台，支持用户操作下发任务到设备端。

## 仓库结构

### 当前仓库目录

```
costrict-web/
├── server/          # 服务端代码
├── gateway/         # 网关层（网络分层架构详见 docs/proposals/HTTP_TUNNEL_DESIGN.md）
├── docs/            # 文档目录
│   └── proposals/   # 提案文档
├── casdoor/         # 认证服务
└── deploy/          # 部署相关
```

### 关联仓库

本项目部分组件复用 [opencode](https://github.com/zgsm-ai/opencode) 项目进行改造：

| 组件 | 本地路径 | 启动命令 | 说明 |
|------|----------|----------|------|
| 设备层 | `D:\DEV\opencode\packages\opencode` | `cs cloud` | 设备端服务 |
| UI 层 | `D:\DEV\opencode\packages\app` | - | 复用的 UI 组件 |

### Plugin 私有化分发

Plugin marketplace 的私有化镜像（770+ plugin 打包发布给内网客户）走独立项目 **costrict-plugin-marketplace**，与 costrict-web 解耦：

| 仓库 | 用途 |
|------|------|
| [costrict-plugins-repo/costrict-plugin-marketplace](https://github.com/costrict-plugins-repo/costrict-plugin-marketplace) | build pipeline + 客户 import.sh + bundle 发布渠道 |
| `costrict-plugins-repo/<plugin-id>` × 770 | per-plugin bare repo，build pipeline 自动管理 |
| `costrict-plugins-repo/marketplace` | marketplace 索引 repo |

详见 `openspec/changes/add-plugin-marketplace/` 提案（独立于 `add-plugin-capability-type` /hub 收藏链路，两者零冲突零依赖）。

### Plugin 作为第 5 类 capability（display-only 阶段）

Plugin 已作为第 5 类 `item_type` 接入 `capability_items`（与 skill / subagent / command / mcp 同位），来自 `catalog-download/plugins/<kebab-id>/.plugin.json`（每条 entry 含 `install.marketplace_name` / `marketplace_repo` / `plugin_name` 等字段，`content` 字段为空）。

**当前阶段（display-only）**：
- 前端 store 显示 Plugins 侧边栏 tab + 列表 / 详情 / 搜索
- favorite 按钮在 plugin 类型上不渲染；服务端 `POST /api/items/:id/favorite` 对 plugin 返回 HTTP 409
- 安全扫描跳过 plugin（content 为空），`security_status="unscanned"`，reason `"plugin: content not server-side"`
- 用户**不能**通过 create-capability-dialog 自传 plugin

**未来阶段（follow-up `add-plugin-favorite-csc`）**：csc 客户端接入 `csc plugin marketplace add / install / enable / disable / uninstall` 子命令后，移除 server 端 409 gate + 前端 favorite 按钮恢复，激活完整 favorite / install 链路。

详见 `openspec/changes/add-plugin-display-only/`。

## 包管理器

- **当前项目**: bun
- **opencode 项目**: bun

## 开发规范

### Server 层

- **API 接口改动**: 涉及 API 接口改动时，必须同步修改 Swagger 文档注释
- **Worker 机制**: Server 层采用 Worker 机制处理异步任务

### 环境变量

| 变量 | 默认 | 含义 |
|------|------|------|
| `SECURITY_SCAN_SHORT_CIRCUIT_DISABLED` | 空（启用） | 设为 `true`（不区分大小写）禁用 `ScanJobService.Enqueue` 与 `ScanService.ScanItem` 的 SecurityScan 短路 — 所有 sync/create/update 触发都重新走 LLM。回滚步骤：在 server 容器环境设置该变量并重启进程。 |

## 快速参考

### 启动设备端

```bash
cd D:\DEV\opencode\packages\opencode
cs cloud
```

### 文档索引

- [HTTP 隧道设计提案](./docs/proposals/HTTP_TUNNEL_DESIGN.md) - 网关层网络分层架构说明
