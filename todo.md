# Costrict-Web 开发进度

## 项目初始化

- [x] 初始化 Git 仓库
- [x] 创建 .gitignore 文件
- [x] 创建 README.md 文件
- [x] Clone buildwithclaude 参考项目（作为前端）
- [x] 将 buildwithclaude 添加为 submodule 到 web/ 目录（保留更新能力）
- [x] 创建 Go 后端项目结构
- [x] 创建 Go 后端基础文件（main.go, config, database, handlers, middleware）
- [x] 更新 docker-compose.yml（前端使用 web/web-ui）
- [x] 创建 Go 后端 Dockerfile
- [x] 更新根目录 package.json（更新前端路径）
- [x] 更新 .gitignore（添加 web/ 忽略规则）
- [x] 运行 go mod tidy（安装 Go 依赖）
- [x] 创建数据库模型（GORM models）
- [x] 创建 Casdoor 客户端（OAuth 2.0 集成）
- [x] 实现认证逻辑（登录、登出、获取当前用户）
- [x] 实现组织管理 API（CRUD 操作）
- [x] 实现仓库管理 API（CRUD 操作）
- [x] 实现技能管理 API（CRUD 操作）
- [x] 实现 Agent 管理 API（CRUD 操作）
- [x] 实现命令管理 API（CRUD 操作）
- [x] 实现 MCP 服务器管理 API（CRUD 操作）
- [x] 实现技能市场 API（公开技能列表、分类、热门技能）
- [x] 添加数据库迁移（AutoMigrate）
- [ ] 测试 Docker Compose 启动（需要管理员权限）

## 后端开发

- [x] 创建项目结构（internal 目录：config, database, handlers, middleware, models, casdoor）
- [x] 创建配置管理（config）
- [x] 创建数据库连接（database）
- [x] 创建中间件（CORS, Logger, Recovery）
- [x] 创建 HTTP 处理器框架（handlers.go，包含所有 API 端点的实现）
- [x] 创建数据库模型（GORM models：Organization, SkillRepository, Skill, Agent, Command, MCPServer, SkillRating, UserPreference）
- [x] 创建 Casdoor 客户端（OAuth 2.0 集成：登录 URL、token 交换、用户信息获取、组织/用户/组 API 调用）
- [x] 实现认证逻辑（登录、登出、获取当前用户）
- [x] 实现组织管理 API（CRUD 操作）
- [x] 实现仓库管理 API（CRUD 操作）
- [x] 实现技能管理 API（CRUD 操作、安装、评分）
- [x] 实现 Agent/命令/MCP 服务器管理 API（CRUD 操作）
- [x] 实现技能市场 API（公开技能列表、分类、热门技能）
- [x] 添加数据库迁移（AutoMigrate）
- [x] 配置所有 API 路由到 main.go

## 前端开发

- [x] 集成 buildwithclaude 作为前端（submodule，保留更新能力）
- [ ] 修改 buildwithclaude 以集成 Go 后端 API
- [ ] 添加认证流程（Casdoor OAuth）
- [ ] 添加组织管理界面
- [ ] 添加仓库管理界面
- [ ] 添加技能管理界面
- [ ] 添加技能市场界面

## Docker 配置

- [ ] 创建 Dockerfile
- [ ] 创建 docker-compose.yml
- [ ] 配置 PostgreSQL 容器
- [ ] 配置 Casdoor 容器
- [ ] 配置应用容器

## 数据库设计

- [ ] 安装 Drizzle ORM
- [ ] 创建数据库 schema
- [ ] 创建迁移文件
- [ ] 配置数据库连接

## Casdoor 集成

- [ ] 安装 Casdoor SDK
- [ ] 配置 OAuth 2.0
- [ ] 实现登录/登出
- [ ] 实现用户数据同步

## 认证授权模块

- [ ] 创建认证 API 路由
- [ ] 实现登录页面
- [ ] 实现注册页面
- [ ] 实现权限验证中间件

## 组织管理模块

- [ ] 创建组织 API 路由
- [ ] 实现组织列表页面
- [ ] 实现组织详情页面
- [ ] 实现创建组织功能
- [ ] 实现邀请成员功能

## 仓库管理模块

- [ ] 创建仓库 API 路由
- [ ] 实现仓库列表页面
- [ ] 实现仓库详情页面
- [ ] 实现创建仓库功能
- [ ] 实现仓库成员管理

## 技能管理模块

- [ ] 创建技能 API 路由
- [ ] 实现技能列表页面
- [ ] 实现技能详情页面
- [ ] 实现创建技能功能
- [ ] 实现技能评分功能

## Agent 管理模块

- [ ] 创建 Agent API 路由
- [ ] 实现 Agent 列表页面
- [ ] 实现 Agent 详情页面
- [ ] 实现创建 Agent 功能

## 命令管理模块

- [ ] 创建命令 API 路由
- [ ] 实现命令列表页面
- [ ] 实现创建命令功能

## MCP 服务器管理模块

- [ ] 创建 MCP 服务器 API 路由
- [ ] 实现 MCP 服务器列表页面
- [ ] 实现创建 MCP 服务器功能

## 技能市场模块

- [ ] 创建技能市场 API 路由
- [ ] 实现技能市场页面
- [ ] 实现技能搜索功能
- [ ] 实现技能分类功能
- [ ] 实现技能安装功能

## 前端组件

- [ ] 创建布局组件（Header, Sidebar, Footer）
- [ ] 创建通用组件（Button, Input, Modal, Table）
- [ ] 创建组织组件
- [ ] 创建仓库组件
- [ ] 创建技能组件
- [ ] 创建市场组件

## 测试

- [ ] 配置测试环境
- [ ] 编写单元测试
- [ ] 编写集成测试

## 部署

- [ ] 配置生产环境
- [ ] 编写部署脚本
- [ ] 配置 CI/CD

## 文档

- [ ] 编写 API 文档
- [ ] 编写部署文档
- [ ] 编写用户手册
