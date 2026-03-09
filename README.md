# Costrict-Web

AI Agent 平台，整合 Casdoor 的组织管理能力和 buildwithclaude 的技能市场功能。

## 技术栈

- **前端**: Next.js 16 + React 19 + TypeScript + Tailwind CSS
- **后端**: Next.js API Routes (TypeScript)
- **认证**: Casdoor (OAuth 2.0)
- **数据库**: PostgreSQL + Drizzle ORM
- **容器化**: Docker + Docker Compose

## 项目结构

```
costrict-web/
├── app/              # Next.js App Router
├── components/       # React 组件
├── lib/             # 工具库和 API 客户端
├── prisma/          # 数据库 schema
├── docker/          # Docker 配置
└── public/          # 静态资源
```

## 快速开始

### 使用 Docker（推荐）

```bash
# 启动所有服务
docker-compose up -d

# 查看日志
docker-compose logs -f
```

### 本地开发

```bash
# 安装依赖
npm install

# 启动开发服务器
npm run dev
```

## 文档

- [系统设计文档](./SYSTEM_DESIGN.md)
- [开发进度](./todo.md)

## 许可证

MIT
