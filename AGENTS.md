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

## 包管理器

- **当前项目**: bun
- **opencode 项目**: bun

## 开发规范

### Server 层

- **API 接口改动**: 涉及 API 接口改动时，必须同步修改 Swagger 文档注释
- **Worker 机制**: Server 层采用 Worker 机制处理异步任务

## 快速参考

### 启动设备端

```bash
cd D:\DEV\opencode\packages\opencode
cs cloud
```

### 文档索引

- [HTTP 隧道设计提案](./docs/proposals/HTTP_TUNNEL_DESIGN.md) - 网关层网络分层架构说明
