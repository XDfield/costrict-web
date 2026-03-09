# Costrict-Web

AI Agent 平台，整合 Casdoor 的组织管理能力和 buildwithclaude 的技能市场功能。

## 技术栈

- **前端**: Next.js 16 + React 19 + TypeScript + Tailwind CSS
- **后端**: Go + Gin + GORM
- **认证**: Casdoor (OAuth 2.0)
- **数据库**: PostgreSQL + GORM
- **容器化**: Docker + Docker Compose

## 项目结构

```
costrict-web/
├── app/              # Next.js App Router (前端)
├── components/       # React 组件 (前端)
├── lib/             # 工具库和 API 客户端 (前端)
├── server/           # Go 后端服务
│   ├── internal/
│   │   ├── config/      # 配置管理
│   │   ├── database/    # 数据库连接
│   │   ├── handlers/    # HTTP 处理器
│   │   └── middleware/  # 中间件
│   ├── main.go       # 应用入口
│   └── Dockerfile    # 后端 Docker 镜像
├── docker/          # Docker 配置
├── references/       # 参考项目 (buildwithclaude submodule)
└── public/          # 静态资源 (前端)
```

## 快速开始

### 使用 Docker（推荐）

```bash
# 启动所有服务（PostgreSQL, Casdoor, Go 后端, Next.js 前端）
docker-compose up -d

# 查看日志
docker-compose logs -f

# 停止所有服务
docker-compose down
```

### 本地开发

#### 前端开发

```bash
# 安装依赖
npm install

# 启动开发服务器
npm run dev
```

#### 后端开发

```bash
cd server

# 安装依赖
go mod tidy

# 运行应用
go run main.go
```

## 服务端口

- **前端**: http://localhost:3000
- **后端 API**: http://localhost:8080
- **Casdoor**: http://localhost:8000
- **PostgreSQL**: localhost:5432

## 文档

- [系统设计文档](./SYSTEM_DESIGN.md)
- [开发进度](./todo.md)

## 参考项目

- [buildwithclaude](./references/buildwithclaude) - Claude 技能市场参考实现（submodule）

## 许可证

MIT
