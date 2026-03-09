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
├── server/           # Go 后端服务
│   ├── internal/
│   │   ├── config/      # 配置管理
│   │   ├── database/    # 数据库连接
│   │   ├── handlers/    # HTTP 处理器
│   │   └── middleware/  # 中间件
│   ├── main.go       # 应用入口
│   └── Dockerfile    # 后端 Docker 镜像
├── web/             # 前端应用（buildwithclaude）
│   ├── web-ui/       # Next.js 前端应用
│   ├── plugins/       # 插件资源
│   ├── scripts/       # 构建脚本
│   └── tests/         # 测试文件
├── docker-compose.yml # Docker Compose 配置
├── package.json     # 根目录脚本
└── .env.example    # 环境变量示例
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
# 进入前端目录
cd web/web-ui

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

#### 同时启动前后端

```bash
# 安装根目录依赖
npm install

# 同时启动前后端
npm run dev
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
