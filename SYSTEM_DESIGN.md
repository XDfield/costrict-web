# Costrict-Web 系统设计文档

## 1. 系统概述

### 1.1 项目目标
构建一个AI Agent平台，整合Casdoor的组织管理能力和buildwithclaude的技能市场功能，实现类似Git/GitLab的团队/仓库结构管理。

### 1.2 核心特性
- **组织管理**: 支持创建团队、邀请成员、角色权限管理
- **仓库管理**: 技能/Agent/命令归属于仓库，支持公开/私有可见性
- **技能市场**: 类似buildwithclaude的技能市场，支持技能发现、安装、使用
- **权限控制**: 基于Casdoor的RBAC权限模型
- **用户管理**: 统一的用户认证和授权

### 1.3 技术栈
- **前端**: Next.js 16 + React 19 + TypeScript + Tailwind CSS
- **后端**: Next.js API Routes (TypeScript)
- **认证**: Casdoor (Go-based IAM平台)
- **数据库**: PostgreSQL + Drizzle ORM
- **状态管理**: React Context + Server Components

---

## 2. 系统架构设计

### 2.1 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                         用户浏览器                            │
│                    (Next.js + React 19)                      │
└────────────────────────┬────────────────────────────────────┘
                         │
                         │ HTTP/HTTPS
                         │
┌────────────────────────┴────────────────────────────────────┐
│                    Costrict-Web 应用层                       │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              Next.js API Routes (TypeScript)           │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐ │ │
│  │  │  Auth API    │  │  Org API     │  │  Skill API    │ │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘ │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐ │ │
│  │  │  User API    │  │  Repo API    │  │  Marketplace  │ │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘ │ │
│  └────────────────────────────────────────────────────────┘ │
│                              │                               │
│                              │                               │
│  ┌───────────────────────────┴───────────────────────────┐ │
│  │              Casdoor SDK / REST API 调用               │ │
│  └───────────────────────────────────────────────────────┘ │
└────────────────────────┬────────────────────────────────────┘
                         │
                         │ REST API / OAuth 2.0
                         │
┌────────────────────────┴────────────────────────────────────┐
│                      Casdoor 服务层                         │
│              (Go-based IAM Platform)                        │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐     │
│  │  Auth Module │  │  Org Module  │  │  User Module │     │
│  └──────────────┘  └──────────────┘  └──────────────┘     │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐     │
│  │  Group Module│  │ Permission   │  │  Role Module │     │
│  │              │  │  Module      │  │              │     │
│  └──────────────┘  └──────────────┘  └──────────────┘     │
└────────────────────────┬────────────────────────────────────┘
                         │
                         │ SQL
                         │
┌────────────────────────┴────────────────────────────────────┐
│                      PostgreSQL 数据库                      │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              Casdoor 数据表                             │ │
│  │  - organization, user, group, permission, role        │ │
│  └────────────────────────────────────────────────────────┘ │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              Costrict-Web 扩展表                        │ │
│  │  - skill_repository, skill, agent, command             │ │
│  │  - marketplace, plugin, mcp_server                    │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 模块划分

#### 2.2.1 认证授权模块 (Auth Module)
- 用户登录/登出
- OAuth 2.0 / OIDC 认证
- JWT Token 管理
- 会话管理

#### 2.2.2 组织管理模块 (Organization Module)
- 创建/编辑/删除组织
- 组织成员管理
- 组织角色管理

#### 2.2.3 仓库管理模块 (Repository Module)
- 创建/编辑/删除仓库
- 仓库可见性控制（公开/私有）
- 仓库成员管理
- 仓库技能/Agent/命令关联

#### 2.2.4 技能市场模块 (Marketplace Module)
- 技能浏览和搜索
- 技能安装和卸载
- 技能评价和评分
- 技能分类管理

#### 2.2.5 权限控制模块 (Permission Module)
- 基于RBAC的权限管理
- 资源访问控制
- 操作权限验证

#### 2.2.6 用户管理模块 (User Module)
- 用户信息管理
- 用户偏好设置
- 用户活动记录

---

## 3. 数据库设计

### 3.1 ER图设计

```
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│  user       │         │ organization│         │  group      │
├─────────────┤         ├─────────────┤         ├─────────────┤
│ id (PK)     │◄────────│ id (PK)     │         │ id (PK)     │
│ name        │         │ name        │         │ name        │
│ email       │         │ display_name│         │ type        │
│ password    │         │ website_url │         │ parent_id   │
│ avatar      │         │ ...         │         │ owner       │
│ ...         │         └─────────────┘         │ is_top_group│
└─────────────┘                                  │ visibility  │
        │                                         │ repo_type   │
        │                                         │ description │
        │                                         │ skill_ids   │
        │                                         │ agent_ids   │
        │                                         │ command_ids │
        │                                         │ mcp_server_ │
        │                                         │   ids       │
        │                                         └─────────────┘
        │                                                 │
        │                                                 │
        │                                                 │
        │          ┌─────────────────────────────────────┘
        │          │
        │          │
        ▼          ▼
┌──────────────────────────────┐
│  skill_repository            │
├──────────────────────────────┤
│ id (PK)                      │
│ name                         │
│ description                  │
│ visibility (public/private)  │
│ owner_id (FK -> user)        │
│ organization_id (FK -> org)  │
│ group_id (FK -> group)       │
│ created_at                   │
│ updated_at                   │
└──────────────────────────────┘
        │
        │
        ├──────────────────────────────┐
        │                              │
        ▼                              ▼
┌─────────────┐              ┌─────────────┐
│  skill      │              │  agent      │
├─────────────┤              ├─────────────┤
│ id (PK)     │              │ id (PK)     │
│ name        │              │ name        │
│ description │              │ description │
│ repo_id (FK)│              │ repo_id (FK)│
│ version     │              │ version     │
│ author      │              │ author      │
│ ...         │              │ ...         │
└─────────────┘              └─────────────┘

┌─────────────┐              ┌─────────────┐
│  command    │              │  mcp_server │
├─────────────┤              ├─────────────┤
│ id (PK)     │              │ id (PK)     │
│ name        │              │ name        │
│ description │              │ description │
│ repo_id (FK)│              │ repo_id (FK)│
│ ...         │              │ ...         │
└─────────────┘              └─────────────┘
```

### 3.2 数据表设计

#### 3.2.1 Casdoor核心表（已有）

**organization 表**
```sql
CREATE TABLE organization (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    display_name VARCHAR(191),
    website_url VARCHAR(191),
    favicon VARCHAR(191),
    phone VARCHAR(191),
    address VARCHAR(191),
    created_time TIMESTAMP,
    updated_time TIMESTAMP,
    ...
);
```

**user 表**
```sql
CREATE TABLE user (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    email VARCHAR(191) NOT NULL UNIQUE,
    password VARCHAR(191),
    avatar VARCHAR(191),
    created_time TIMESTAMP,
    updated_time TIMESTAMP,
    ...
);
```

**group 表**
```sql
CREATE TABLE group (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    type VARCHAR(191),
    parent_id VARCHAR(191),
    owner VARCHAR(191),
    is_top_group BOOLEAN DEFAULT FALSE,
    created_time TIMESTAMP,
    updated_time TIMESTAMP,
    ...
);
```

**permission 表**
```sql
CREATE TABLE permission (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    users TEXT,
    groups TEXT,
    roles TEXT,
    resources TEXT,
    actions TEXT,
    effect VARCHAR(191),
    created_time TIMESTAMP,
    updated_time TIMESTAMP,
    ...
);
```

**role 表**
```sql
CREATE TABLE role (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    users TEXT,
    groups TEXT,
    roles TEXT,
    created_time TIMESTAMP,
    updated_time TIMESTAMP,
    ...
);
```

#### 3.2.2 Costrict-Web扩展表

**skill_repository 表**
```sql
CREATE TABLE skill_repository (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    description TEXT,
    visibility VARCHAR(50) DEFAULT 'private', -- 'public' or 'private'
    owner_id VARCHAR(191) NOT NULL,
    organization_id VARCHAR(191),
    group_id VARCHAR(191),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (owner_id) REFERENCES user(id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id) REFERENCES organization(id) ON DELETE SET NULL,
    FOREIGN KEY (group_id) REFERENCES group(id) ON DELETE SET NULL
);

CREATE INDEX idx_skill_repo_owner ON skill_repository(owner_id);
CREATE INDEX idx_skill_repo_org ON skill_repository(organization_id);
CREATE INDEX idx_skill_repo_visibility ON skill_repository(visibility);
```

**skill 表**
```sql
CREATE TABLE skill (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    description TEXT,
    version VARCHAR(50),
    author VARCHAR(191),
    repo_id VARCHAR(191) NOT NULL,
    is_public BOOLEAN DEFAULT FALSE,
    install_count INTEGER DEFAULT 0,
    rating DECIMAL(3,2) DEFAULT 0.00,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (repo_id) REFERENCES skill_repository(id) ON DELETE CASCADE,
    FOREIGN KEY (author) REFERENCES user(id) ON DELETE SET NULL
);

CREATE INDEX idx_skill_repo ON skill(repo_id);
CREATE INDEX idx_skill_public ON skill(is_public);
CREATE INDEX idx_skill_author ON skill(author);
```

**agent 表**
```sql
CREATE TABLE agent (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    description TEXT,
    version VARCHAR(50),
    author VARCHAR(191),
    repo_id VARCHAR(191) NOT NULL,
    is_public BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (repo_id) REFERENCES skill_repository(id) ON DELETE CASCADE,
    FOREIGN KEY (author) REFERENCES user(id) ON DELETE SET NULL
);

CREATE INDEX idx_agent_repo ON agent(repo_id);
```

**command 表**
```sql
CREATE TABLE command (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    description TEXT,
    repo_id VARCHAR(191) NOT NULL,
    author VARCHAR(191),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (repo_id) REFERENCES skill_repository(id) ON DELETE CASCADE,
    FOREIGN KEY (author) REFERENCES user(id) ON DELETE SET NULL
);

CREATE INDEX idx_command_repo ON command(repo_id);
```

**mcp_server 表**
```sql
CREATE TABLE mcp_server (
    id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(191) NOT NULL,
    description TEXT,
    repo_id VARCHAR(191) NOT NULL,
    author VARCHAR(191),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (repo_id) REFERENCES skill_repository(id) ON DELETE CASCADE,
    FOREIGN KEY (author) REFERENCES user(id) ON DELETE SET NULL
);

CREATE INDEX idx_mcp_server_repo ON mcp_server(repo_id);
```

**skill_rating 表**
```sql
CREATE TABLE skill_rating (
    id VARCHAR(191) PRIMARY KEY,
    skill_id VARCHAR(191) NOT NULL,
    user_id VARCHAR(191) NOT NULL,
    rating INTEGER CHECK (rating >= 1 AND rating <= 5),
    comment TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (skill_id) REFERENCES skill(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES user(id) ON DELETE CASCADE,
    UNIQUE(skill_id, user_id)
);

CREATE INDEX idx_rating_skill ON skill_rating(skill_id);
CREATE INDEX idx_rating_user ON skill_rating(user_id);
```

**user_preference 表**
```sql
CREATE TABLE user_preference (
    id VARCHAR(191) PRIMARY KEY,
    user_id VARCHAR(191) NOT NULL UNIQUE,
    default_repository_id VARCHAR(191),
    favorite_skills TEXT, -- JSON array
    skill_permissions TEXT, -- JSON object
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES user(id) ON DELETE CASCADE,
    FOREIGN KEY (default_repository_id) REFERENCES skill_repository(id) ON DELETE SET NULL
);
```

### 3.3 数据模型关系说明

1. **Organization → Group**: 一对多关系，一个组织可以包含多个组
2. **Group → User**: 多对多关系（通过Casdoor的users字段），一个组可以包含多个用户
3. **User → SkillRepository**: 一对多关系，一个用户可以创建多个仓库
4. **Organization → SkillRepository**: 一对多关系，一个组织可以包含多个仓库
5. **Group → SkillRepository**: 一对一关系，一个组对应一个仓库（仓库作为组的扩展）
6. **SkillRepository → Skill/Agent/Command/McpServer**: 一对多关系，一个仓库可以包含多个技能/Agent/命令/MCP服务器
7. **User → Skill**: 一对多关系，一个用户可以创建多个技能
8. **Skill → SkillRating**: 一对多关系，一个技能可以有多个评分

---

## 4. API接口设计

### 4.1 认证授权API

#### 4.1.1 用户登录
```
POST /api/auth/login
Request:
{
  "username": "string",
  "password": "string"
}

Response:
{
  "token": "string",
  "user": {
    "id": "string",
    "name": "string",
    "email": "string",
    "avatar": "string"
  }
}
```

#### 4.1.2 用户登出
```
POST /api/auth/logout
Response:
{
  "success": true
}
```

#### 4.1.3 获取当前用户信息
```
GET /api/auth/me
Response:
{
  "id": "string",
  "name": "string",
  "email": "string",
  "avatar": "string",
  "organizations": [...],
  "groups": [...]
}
```

### 4.2 组织管理API

#### 4.2.1 创建组织
```
POST /api/organizations
Request:
{
  "name": "string",
  "displayName": "string",
  "websiteUrl": "string"
}

Response:
{
  "id": "string",
  "name": "string",
  "displayName": "string",
  "createdAt": "timestamp"
}
```

#### 4.2.2 获取组织列表
```
GET /api/organizations
Response:
{
  "organizations": [
    {
      "id": "string",
      "name": "string",
      "displayName": "string",
      "memberCount": number
    }
  ],
  "total": number
}
```

#### 4.2.3 获取组织详情
```
GET /api/organizations/{id}
Response:
{
  "id": "string",
  "name": "string",
  "displayName": "string",
  "websiteUrl": "string",
  "members": [...],
  "repositories": [...]
}
```

#### 4.2.4 邀请成员到组织
```
POST /api/organizations/{id}/members
Request:
{
  "userId": "string",
  "role": "string"
}

Response:
{
  "success": true
}
```

### 4.3 仓库管理API

#### 4.3.1 创建仓库
```
POST /api/repositories
Request:
{
  "name": "string",
  "description": "string",
  "visibility": "public|private",
  "organizationId": "string"
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "visibility": "public|private",
  "owner": {...},
  "organization": {...},
  "createdAt": "timestamp"
}
```

#### 4.3.2 获取仓库列表
```
GET /api/repositories?visibility=public&organizationId={id}
Response:
{
  "repositories": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "visibility": "public|private",
      "owner": {...},
      "skillCount": number,
      "agentCount": number,
      "commandCount": number
    }
  ],
  "total": number
}
```

#### 4.3.3 获取仓库详情
```
GET /api/repositories/{id}
Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "visibility": "public|private",
  "owner": {...},
  "organization": {...},
  "skills": [...],
  "agents": [...],
  "commands": [...],
  "members": [...],
  "createdAt": "timestamp",
  "updatedAt": "timestamp"
}
```

#### 4.3.4 更新仓库
```
PUT /api/repositories/{id}
Request:
{
  "name": "string",
  "description": "string",
  "visibility": "public|private"
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "visibility": "public|private",
  "updatedAt": "timestamp"
}
```

#### 4.3.5 删除仓库
```
DELETE /api/repositories/{id}
Response:
{
  "success": true
}
```

#### 4.3.6 添加仓库成员
```
POST /api/repositories/{id}/members
Request:
{
  "userId": "string",
  "role": "admin|developer|viewer"
}

Response:
{
  "success": true
}
```

### 4.4 技能管理API

#### 4.4.1 创建技能
```
POST /api/skills
Request:
{
  "name": "string",
  "description": "string",
  "version": "string",
  "repoId": "string",
  "isPublic": false
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "version": "string",
  "repoId": "string",
  "author": {...},
  "createdAt": "timestamp"
}
```

#### 4.4.2 获取技能列表
```
GET /api/skills?repoId={id}&isPublic=true&search={keyword}
Response:
{
  "skills": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "version": "string",
      "author": {...},
      "installCount": number,
      "rating": number,
      "isPublic": boolean
    }
  ],
  "total": number
}
```

#### 4.4.3 获取技能详情
```
GET /api/skills/{id}
Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "version": "string",
  "repoId": "string",
  "author": {...},
  "repository": {...},
  "installCount": number,
  "rating": number,
  "isPublic": boolean,
  "createdAt": "timestamp",
  "updatedAt": "timestamp"
}
```

#### 4.4.4 更新技能
```
PUT /api/skills/{id}
Request:
{
  "name": "string",
  "description": "string",
  "version": "string",
  "isPublic": boolean
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "version": "string",
  "isPublic": boolean,
  "updatedAt": "timestamp"
}
```

#### 4.4.5 删除技能
```
DELETE /api/skills/{id}
Response:
{
  "success": true
}
```

#### 4.4.6 安装技能
```
POST /api/skills/{id}/install
Response:
{
  "success": true,
  "installCount": number
}
```

#### 4.4.7 评分技能
```
POST /api/skills/{id}/rating
Request:
{
  "rating": 5,
  "comment": "string"
}

Response:
{
  "success": true,
  "averageRating": 4.5
}
```

### 4.5 Agent管理API

#### 4.5.1 创建Agent
```
POST /api/agents
Request:
{
  "name": "string",
  "description": "string",
  "version": "string",
  "repoId": "string",
  "isPublic": false
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "version": "string",
  "repoId": "string",
  "author": {...},
  "createdAt": "timestamp"
}
```

#### 4.5.2 获取Agent列表
```
GET /api/agents?repoId={id}&isPublic=true
Response:
{
  "agents": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "version": "string",
      "author": {...},
      "isPublic": boolean
    }
  ],
  "total": number
}
```

#### 4.5.3 获取Agent详情
```
GET /api/agents/{id}
Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "version": "string",
  "repoId": "string",
  "author": {...},
  "repository": {...},
  "isPublic": boolean,
  "createdAt": "timestamp",
  "updatedAt": "timestamp"
}
```

### 4.6 命令管理API

#### 4.6.1 创建命令
```
POST /api/commands
Request:
{
  "name": "string",
  "description": "string",
  "repoId": "string"
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "repoId": "string",
  "author": {...},
  "createdAt": "timestamp"
}
```

#### 4.6.2 获取命令列表
```
GET /api/commands?repoId={id}
Response:
{
  "commands": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "repoId": "string",
      "author": {...}
    }
  ],
  "total": number
}
```

### 4.7 MCP服务器管理API

#### 4.7.1 创建MCP服务器
```
POST /api/mcp-servers
Request:
{
  "name": "string",
  "description": "string",
  "repoId": "string"
}

Response:
{
  "id": "string",
  "name": "string",
  "description": "string",
  "repoId": "string",
  "author": {...},
  "createdAt": "timestamp"
}
```

#### 4.7.2 获取MCP服务器列表
```
GET /api/mcp-servers?repoId={id}
Response:
{
  "mcpServers": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "repoId": "string",
      "author": {...}
    }
  ],
  "total": number
}
```

### 4.8 技能市场API

#### 4.8.1 浏览技能市场
```
GET /api/marketplace/skills?category={category}&search={keyword}&sort={sort}&page={page}&limit={limit}
Response:
{
  "skills": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "version": "string",
      "author": {...},
      "repository": {...},
      "installCount": number,
      "rating": number,
      "categories": ["string"]
    }
  ],
  "total": number,
  "page": number,
  "limit": number
}
```

#### 4.8.2 获取技能分类
```
GET /api/marketplace/categories
Response:
{
  "categories": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "skillCount": number
    }
  ]
}
```

#### 4.8.3 获取热门技能
```
GET /api/marketplace/skills/trending
Response:
{
  "skills": [
    {
      "id": "string",
      "name": "string",
      "description": "string",
      "installCount": number,
      "rating": number
    }
  ]
}
```

---

## 5. 前端页面设计

### 5.1 页面结构

```
costrict-web/
├── app/
│   ├── (auth)/
│   │   ├── login/
│   │   │   └── page.tsx
│   │   └── register/
│   │       └── page.tsx
│   ├── (dashboard)/
│   │   ├── layout.tsx
│   │   ├── page.tsx
│   │   ├── organizations/
│   │   │   ├── page.tsx
│   │   │   └── [id]/
│   │   │       └── page.tsx
│   │   ├── repositories/
│   │   │   ├── page.tsx
│   │   │   ├── create/
│   │   │   │   └── page.tsx
│   │   │   └── [id]/
│   │   │       ├── page.tsx
│   │   │       ├── skills/
│   │   │       │   └── page.tsx
│   │   │       ├── agents/
│   │   │       │   └── page.tsx
│   │   │       └── commands/
│   │   │           └── page.tsx
│   │   ├── marketplace/
│   │   │   ├── page.tsx
│   │   │   ├── skills/
│   │   │   │   ├── page.tsx
│   │   │   │   └── [id]/
│   │   │   │       └── page.tsx
│   │   │   └── categories/
│   │   │       └── page.tsx
│   │   └── settings/
│   │       └── page.tsx
│   └── layout.tsx
├── components/
│   ├── layout/
│   │   ├── Header.tsx
│   │   ├── Sidebar.tsx
│   │   └── Footer.tsx
│   ├── auth/
│   │   ├── LoginForm.tsx
│   │   └── RegisterForm.tsx
│   ├── organization/
│   │   ├── OrganizationList.tsx
│   │   ├── OrganizationCard.tsx
│   │   ├── CreateOrganizationModal.tsx
│   │   └── InviteMemberModal.tsx
│   ├── repository/
│   │   ├── RepositoryList.tsx
│   │   ├── RepositoryCard.tsx
│   │   ├── CreateRepositoryModal.tsx
│   │   └── RepositoryDetail.tsx
│   ├── skill/
│   │   ├── SkillList.tsx
│   │   ├── SkillCard.tsx
│   │   ├── CreateSkillModal.tsx
│   │   └── SkillDetail.tsx
│   ├── marketplace/
│   │   ├── MarketplaceGrid.tsx
│   │   ├── SkillCard.tsx
│   │   ├── CategoryFilter.tsx
│   │   └── SearchBar.tsx
│   └── common/
│       ├── Button.tsx
│       ├── Input.tsx
│       ├── Modal.tsx
│       └── Table.tsx
└── lib/
    ├── api/
    │   ├── auth.ts
    │   ├── organization.ts
    │   ├── repository.ts
    │   ├── skill.ts
    │   ├── agent.ts
    │   ├── command.ts
    │   └── marketplace.ts
    └── hooks/
        ├── useAuth.ts
        ├── useOrganization.ts
        ├── useRepository.ts
        └── useSkill.ts
```

### 5.2 主要页面设计

#### 5.2.1 登录页面 (Login Page)
- 用户名/邮箱输入框
- 密码输入框
- 记住我复选框
- 登录按钮
- 注册链接
- OAuth登录按钮（Google, GitHub等）

#### 5.2.2 仪表板页面 (Dashboard Page)
- 欢迎信息
- 快速操作卡片
  - 创建新仓库
  - 浏览技能市场
  - 创建组织
- 统计信息
  - 我的仓库数
  - 我的技能数
  - 组织数
- 最近活动列表

#### 5.2.3 组织列表页面 (Organization List Page)
- 组织列表卡片
  - 组织名称
  - 成员数量
  - 仓库数量
  - 操作按钮（查看、设置）
- 创建组织按钮
- 搜索和过滤功能

#### 5.2.4 组织详情页面 (Organization Detail Page)
- 组织信息卡片
  - 组织名称
  - 组织描述
  - 网站链接
  - 成员列表
  - 仓库列表
- 操作按钮
  - 邀请成员
  - 编辑组织信息
  - 删除组织

#### 5.2.5 仓库列表页面 (Repository List Page)
- 仓库列表卡片
  - 仓库名称
  - 描述
  - 可见性（公开/私有）
  - 所属组织
  - 技能/Agent/命令数量
  - 操作按钮（查看、编辑、删除）
- 创建仓库按钮
- 搜索和过滤功能
  - 按可见性过滤
  - 按组织过滤

#### 5.2.6 仓库详情页面 (Repository Detail Page)
- 仓库信息卡片
  - 仓库名称
  - 描述
  - 可见性
  - 所有者信息
  - 所属组织
  - 创建时间
- 操作按钮
  - 编辑仓库
  - 删除仓库
  - 添加成员
- 标签页
  - 技能列表
  - Agent列表
  - 命令列表
  - MCP服务器列表
  - 成员列表
  - 设置

#### 5.2.7 技能列表页面 (Skill List Page)
- 技能列表卡片
  - 技能名称
  - 描述
  - 版本
  - 作者
  - 安装次数
  - 评分
  - 操作按钮（查看、编辑、删除）
- 创建技能按钮
- 搜索和过滤功能

#### 5.2.8 技能详情页面 (Skill Detail Page)
- 技能信息卡片
  - 技能名称
  - 描述
  - 版本
  - 作者
  - 所属仓库
  - 安装次数
  - 评分
  - 创建时间
- 操作按钮
  - 编辑技能
  - 删除技能
  - 安装技能
  - 评分
- 评分列表
- 评论列表

#### 5.2.9 技能市场页面 (Marketplace Page)
- 搜索栏
- 分类过滤器
- 排序选项（热门、最新、评分）
- 技能网格卡片
  - 技能名称
  - 描述
  - 版本
  - 作者
  - 所属仓库
  - 安装次数
  - 评分
  - 安装按钮
- 热门技能推荐
- 分类浏览

#### 5.2.10 设置页面 (Settings Page)
- 用户信息
  - 头像
  - 用户名
  - 邮箱
  - 密码修改
- 偏好设置
  - 默认仓库
  - 收藏的技能
- 安全设置
  - 双因素认证
  - 登录历史
- 通知设置

### 5.3 组件设计

#### 5.3.1 布局组件
- **Header**: 顶部导航栏，包含Logo、搜索、用户菜单
- **Sidebar**: 侧边栏导航，包含主要功能入口
- **Footer**: 页脚，包含版权信息和链接

#### 5.3.2 认证组件
- **LoginForm**: 登录表单组件
- **RegisterForm**: 注册表单组件

#### 5.3.3 组织组件
- **OrganizationList**: 组织列表组件
- **OrganizationCard**: 组织卡片组件
- **CreateOrganizationModal**: 创建组织模态框
- **InviteMemberModal**: 邀请成员模态框

#### 5.3.4 仓库组件
- **RepositoryList**: 仓库列表组件
- **RepositoryCard**: 仓库卡片组件
- **CreateRepositoryModal**: 创建仓库模态框
- **RepositoryDetail**: 仓库详情组件

#### 5.3.5 技能组件
- **SkillList**: 技能列表组件
- **SkillCard**: 技能卡片组件
- **CreateSkillModal**: 创建技能模态框
- **SkillDetail**: 技能详情组件

#### 5.3.6 市场组件
- **MarketplaceGrid**: 市场网格组件
- **SkillCard**: 技能卡片组件（市场用）
- **CategoryFilter**: 分类过滤器
- **SearchBar**: 搜索栏

#### 5.3.7 通用组件
- **Button**: 按钮组件
- **Input**: 输入框组件
- **Modal**: 模态框组件
- **Table**: 表格组件
- **Card**: 卡片组件
- **Badge**: 徽章组件
- **Avatar**: 头像组件

---

## 6. 权限模型设计

### 6.1 RBAC权限模型

#### 6.1.1 角色定义

**组织级别角色**:
- **Owner**: 组织所有者，拥有所有权限
- **Admin**: 组织管理员，可以管理组织和仓库
- **Member**: 组织成员，可以访问组织资源

**仓库级别角色**:
- **Owner**: 仓库所有者，拥有所有权限
- **Admin**: 仓库管理员，可以管理仓库和技能
- **Developer**: 开发者，可以创建和编辑技能
- **Viewer**: 查看者，只能查看仓库和技能

#### 6.1.2 权限定义

**组织权限**:
- `org:read`: 查看组织信息
- `org:update`: 更新组织信息
- `org:delete`: 删除组织
- `org:invite`: 邀请成员
- `org:remove`: 移除成员
- `org:create_repo`: 创建仓库

**仓库权限**:
- `repo:read`: 查看仓库信息
- `repo:update`: 更新仓库信息
- `repo:delete`: 删除仓库
- `repo:invite`: 邀请成员
- `repo:remove`: 移除成员

**技能权限**:
- `skill:read`: 查看技能
- `skill:create`: 创建技能
- `skill:update`: 更新技能
- `skill:delete`: 删除技能
- `skill:install`: 安装技能
- `skill:rate`: 评分技能

**Agent权限**:
- `agent:read`: 查看Agent
- `agent:create`: 创建Agent
- `agent:update`: 更新Agent
- `agent:delete`: 删除Agent

**命令权限**:
- `command:read`: 查看命令
- `command:create`: 创建命令
- `command:update`: 更新命令
- `command:delete`: 删除命令

#### 6.1.3 权限矩阵

| 角色 | 组织权限 | 仓库权限 | 技能权限 | Agent权限 | 命令权限 |
|------|---------|---------|---------|----------|----------|
| Owner (Org) | 全部 | 全部 | 全部 | 全部 | 全部 |
| Admin (Org) | read, update, invite, remove, create_repo | read, update, invite, remove | read, create, update, delete | read, create, update, delete | read, create, update, delete |
| Member (Org) | read | read | read | read | read |
| Owner (Repo) | - | 全部 | 全部 | 全部 | 全部 |
| Admin (Repo) | - | read, update, invite, remove | read, create, update, delete | read, create, update, delete | read, create, update, delete |
| Developer (Repo) | - | read | read, create, update | read, create, update | read, create, update |
| Viewer (Repo) | - | read | read | read | read |

### 6.2 权限验证流程

```
用户请求 → 中间件验证Token → 获取用户角色和权限 → 检查操作权限 → 允许/拒绝
```

#### 6.2.1 权限验证中间件设计

```typescript
// 权限验证中间件伪代码
async function checkPermission(req, res, next) {
  // 1. 从请求中获取用户信息
  const user = await getUserFromToken(req.headers.authorization);

  // 2. 获取请求的资源类型和操作
  const resourceType = req.params.resourceType; // org, repo, skill, etc.
  const resourceId = req.params.id;
  const action = getActionFromMethod(req.method); // read, create, update, delete

  // 3. 获取用户在资源上的角色
  const role = await getUserRole(user.id, resourceType, resourceId);

  // 4. 检查角色是否有执行该操作的权限
  const hasPermission = await checkRolePermission(role, action, resourceType);

  // 5. 根据权限结果决定是否继续
  if (hasPermission) {
    next();
  } else {
    res.status(403).json({ error: 'Permission denied' });
  }
}
```

### 6.3 Casdoor权限集成

#### 6.3.1 使用Casdoor的Permission模型

在Casdoor中创建Permission对象来定义权限规则：

```go
// Casdoor Permission对象示例
permission := &object.Permission{
    Owner:       "app/costrict",
    Name:        "skill:create",
    DisplayName: "Create Skill",
    Users:       []string{},
    Groups:      []string{"developers", "admins"},
    Roles:       []string{"repo-admin"},
    Resources:   []string{"skill-repository/*"},
    Actions:     []string{"skill:create"},
    Effect:      "allow",
}
```

#### 6.3.2 权限验证API调用

```typescript
// 调用Casdoor API验证权限
async function verifyPermission(userId, resource, action) {
  const response = await fetch('http://localhost:8000/api/verify-permission', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      userId: userId,
      resource: resource,
      action: action,
    }),
  });

  const result = await response.json();
  return result.allowed;
}
```

---

## 7. 集成方案设计

### 7.1 Casdoor集成架构

#### 7.1.1 部署架构

```
┌─────────────────────────────────────────────────────────────┐
│                    Costrict-Web 应用                        │
│                  (Next.js + TypeScript)                     │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              Casdoor SDK / HTTP Client                 │ │
│  └────────────────────────────────────────────────────────┘ │
└────────────────────────┬────────────────────────────────────┘
                         │
                         │ HTTP / OAuth 2.0
                         │
┌────────────────────────┴────────────────────────────────────┐
│                    Casdoor 服务                             │
│                    (Go Backend)                              │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              REST API                                   │ │
│  │  - /api/login                                          │ │
│  │  - /api/organizations                                  │ │
│  │  - /api/groups                                        │ │
│  │  - /api/users                                         │ │
│  │  - /api/permissions                                   │ │
│  └────────────────────────────────────────────────────────┘ │
└────────────────────────┬────────────────────────────────────┘
                         │
                         │ SQL
                         │
┌────────────────────────┴────────────────────────────────────┐
│                    PostgreSQL 数据库                        │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              Casdoor Tables                             │ │
│  │  - organization                                        │ │
│  │  - user                                                │ │
│  │  - group                                               │ │
│  │  - permission                                          │ │
│  │  - role                                                │ │
│  └────────────────────────────────────────────────────────┘ │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              Costrict-Web Tables                        │ │
│  │  - skill_repository                                    │ │
│  │  - skill                                               │ │
│  │  - agent                                               │ │
│  │  - command                                             │ │
│  │  - mcp_server                                          │ │
│  │  - skill_rating                                        │ │
│  │  - user_preference                                     │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

#### 7.1.2 集成方式

**方式1: OAuth 2.0 集成（推荐）**
- Costrict-Web使用OAuth 2.0与Casdoor进行认证
- 用户登录时重定向到Casdoor的登录页面
- Casdoor返回access_token和id_token
- Costrict-Web使用access_token调用Casdoor的API

**方式2: REST API 集成**
- Costrict-Web直接调用Casdoor的REST API
- 需要在Casdoor中创建应用（Application）
- 使用应用的Client ID和Client Secret进行认证

**方式3: Casdoor SDK集成**
- 使用Casdoor的JavaScript/TypeScript SDK
- SDK封装了API调用逻辑
- 简化开发工作

### 7.2 数据同步策略

#### 7.2.1 用户数据同步

**同步时机**:
- 用户首次登录时
- 用户信息更新时

**同步内容**:
- 用户基本信息（id, name, email, avatar）
- 用户所属组织
- 用户所属组

**同步方式**:
```typescript
// 用户登录时同步数据
async function syncUserFromCasdoor(casdoorUser) {
  // 1. 检查本地数据库是否存在该用户
  let localUser = await db.user.findUnique({
    where: { id: casdoorUser.id }
  });

  // 2. 如果不存在，创建新用户
  if (!localUser) {
    localUser = await db.user.create({
      data: {
        id: casdoorUser.id,
        name: casdoorUser.name,
        email: casdoorUser.email,
        avatar: casdoorUser.avatar,
      }
    });
  } else {
    // 3. 如果存在，更新用户信息
    localUser = await db.user.update({
      where: { id: casdoorUser.id },
      data: {
        name: casdoorUser.name,
        email: casdoorUser.email,
        avatar: casdoorUser.avatar,
      }
    });
  }

  // 4. 同步用户的组织和组
  await syncUserOrganizations(casdoorUser.id);
  await syncUserGroups(casdoorUser.id);

  return localUser;
}
```

#### 7.2.2 组织数据同步

**同步时机**:
- 组织创建时
- 组织信息更新时

**同步方式**:
```typescript
// 同步组织数据
async function syncOrganizationFromCasdoor(casdoorOrg) {
  let localOrg = await db.organization.findUnique({
    where: { id: casdoorOrg.id }
  });

  if (!localOrg) {
    localOrg = await db.organization.create({
      data: {
        id: casdoorOrg.id,
        name: casdoorOrg.name,
        displayName: casdoorOrg.displayName,
        websiteUrl: casdoorOrg.websiteUrl,
      }
    });
  } else {
    localOrg = await db.organization.update({
      where: { id: casdoorOrg.id },
      data: {
        name: casdoorOrg.name,
        displayName: casdoorOrg.displayName,
        websiteUrl: casdoorOrg.websiteUrl,
      }
    });
  }

  return localOrg;
}
```

#### 7.2.3 组数据同步

**同步时机**:
- 组创建时
- 组信息更新时

**同步方式**:
```typescript
// 同步组数据（组对应仓库）
async function syncGroupFromCasdoor(casdoorGroup) {
  let localGroup = await db.group.findUnique({
    where: { id: casdoorGroup.id }
  });

  if (!localGroup) {
    localGroup = await db.group.create({
      data: {
        id: casdoorGroup.id,
        name: casdoorGroup.name,
        type: casdoorGroup.type,
        parentId: casdoorGroup.parentId,
        owner: casdoorGroup.owner,
        isTopGroup: casdoorGroup.isTopGroup,
      }
    });
  } else {
    localGroup = await db.group.update({
      where: { id: casdoorGroup.id },
      data: {
        name: casdoorGroup.name,
        type: casdoorGroup.type,
        parentId: casdoorGroup.parentId,
        owner: casdoorGroup.owner,
        isTopGroup: casdoorGroup.isTopGroup,
      }
    });
  }

  return localGroup;
}
```

### 7.3 API调用策略

#### 7.3.1 认证流程

```
1. 用户点击登录
2. 重定向到Casdoor登录页面
3. 用户输入用户名密码
4. Casdoor验证成功，重定向回Costrict-Web，带上code
5. Costrict-Web使用code换取access_token
6. Costrict-Web使用access_token获取用户信息
7. Costrict-Web创建本地会话
8. 用户登录成功
```

#### 7.3.2 API调用示例

**登录流程**:
```typescript
// 1. 构建Casdoor登录URL
const loginUrl = `https://your-casdoor.com/login/oauth/authorize?` +
  `client_id=${clientId}&` +
  `redirect_uri=${encodeURIComponent(redirectUri)}&` +
  `response_type=code&` +
  `scope=openid profile email&` +
  `state=${state}`;

// 2. 用户登录后，Casdoor重定向回redirect_uri，带上code
const code = req.query.code;

// 3. 使用code换取access_token
const tokenResponse = await fetch('https://your-casdoor.com/api/login/oauth/access_token', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/x-www-form-urlencoded',
  },
  body: new URLSearchParams({
    grant_type: 'authorization_code',
    client_id: clientId,
    client_secret: clientSecret,
    code: code,
    redirect_uri: redirectUri,
  }),
});

const tokenData = await tokenResponse.json();
const accessToken = tokenData.access_token;

// 4. 使用access_token获取用户信息
const userResponse = await fetch('https://your-casdoor.com/api/userinfo', {
  headers: {
    'Authorization': `Bearer ${accessToken}`,
  },
});

const userData = await userResponse.json();

// 5. 同步用户数据到本地数据库
const localUser = await syncUserFromCasdoor(userData);

// 6. 创建本地会话
req.session.userId = localUser.id;
```

**调用Casdoor API**:
```typescript
// 调用Casdoor的API
async function callCasdoorApi(endpoint, method = 'GET', data = null) {
  const response = await fetch(`https://your-casdoor.com/api/${endpoint}`, {
    method: method,
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${getAccessToken()}`,
    },
    body: data ? JSON.stringify(data) : null,
  });

  return await response.json();
}

// 获取组织列表
const organizations = await callCasdoorApi('organizations');

// 获取用户组
const groups = await callCasdoorApi(`users/${userId}/groups`);

// 验证权限
const permission = await callCasdoorApi('verify-permission', 'POST', {
  userId: userId,
  resource: resource,
  action: action,
});
```

---

## 8. 安全设计

### 8.1 认证安全

#### 8.1.1 OAuth 2.0安全
- 使用HTTPS进行所有通信
- 使用state参数防止CSRF攻击
- 使用PKCE（Proof Key for Code Exchange）增强安全性
- 设置合理的token过期时间
- 实现token刷新机制

#### 8.1.2 会话管理
- 使用HttpOnly Cookie存储session
- 设置Cookie的Secure和SameSite属性
- 实现会话超时机制
- 提供登出功能清除session

### 8.2 授权安全

#### 8.2.1 权限验证
- 所有API端点都需要权限验证
- 使用中间件统一处理权限验证
- 记录所有权限拒绝的日志
- 实现最小权限原则

#### 8.2.2 数据隔离
- 私有仓库只能被授权用户访问
- 公开仓库可以被所有用户查看
- 实现数据查询时的权限过滤

### 8.3 数据安全

#### 8.3.1 数据加密
- 敏感数据（如密码）使用bcrypt加密存储
- 使用TLS加密数据库连接
- 使用环境变量存储敏感配置

#### 8.3.2 SQL注入防护
- 使用参数化查询
- 使用ORM（Drizzle）防止SQL注入
- 对用户输入进行验证和过滤

#### 8.3.3 XSS防护
- 对用户输入进行HTML转义
- 使用Content Security Policy (CSP)
- 使用React的自动XSS防护

### 8.4 API安全

#### 8.4.1 速率限制
- 实现API速率限制
- 防止暴力破解
- 防止DDoS攻击

#### 8.4.2 CORS配置
- 配置合理的CORS策略
- 只允许可信的域名访问

#### 8.4.3 输入验证
- 对所有API输入进行验证
- 使用schema验证库（如Zod）
- 返回清晰的错误信息

---

## 9. 性能优化设计

### 9.1 数据库优化

#### 9.1.1 索引优化
- 为常用查询字段创建索引
- 为外键字段创建索引
- 为过滤字段创建索引
- 定期分析和优化索引

#### 9.1.2 查询优化
- 使用分页查询避免大量数据加载
- 使用select只查询需要的字段
- 使用join减少查询次数
- 使用缓存减少数据库访问

### 9.2 API优化

#### 9.2.1 响应优化
- 使用HTTP缓存（ETag, Cache-Control）
- 实现数据压缩（Gzip）
- 使用GraphQL减少over-fetching
- 实现API响应分页

#### 9.2.2 并发处理
- 使用异步处理提高并发能力
- 使用连接池管理数据库连接
- 使用队列处理耗时任务

### 9.3 前端优化

#### 9.3.1 加载优化
- 使用代码分割（Code Splitting）
- 使用懒加载（Lazy Loading）
- 使用图片优化
- 使用CDN加速静态资源

#### 9.3.2 渲染优化
- 使用React Server Components
- 使用React.memo避免不必要的重渲染
- 使用虚拟列表处理长列表
- 使用SWR/React Query管理数据缓存

---

## 10. 测试策略设计

### 10.1 单元测试

#### 10.1.1 后端测试
- 测试API端点
- 测试业务逻辑
- 测试数据模型
- 测试权限验证

#### 10.1.2 前端测试
- 测试组件渲染
- 测试用户交互
- 测试表单验证
- 测试状态管理

### 10.2 集成测试

#### 10.2.1 API集成测试
- 测试API调用流程
- 测试数据同步
- 测试权限验证
- 测试错误处理

#### 10.2.2 系统集成测试
- 测试OAuth流程
- 测试Casdoor集成
- 测试数据库操作
- 测试端到端流程

### 10.3 E2E测试

#### 10.3.1 用户流程测试
- 测试用户注册登录流程
- 测试创建组织流程
- 测试创建仓库流程
- 测试创建技能流程
- 测试技能市场浏览流程

### 10.4 性能测试

#### 10.4.1 负载测试
- 测试系统在高并发下的表现
- 测试数据库性能
- 测试API响应时间

#### 10.4.2 压力测试
- 测试系统的极限承载能力
- 测试系统的稳定性

---

## 11. 部署方案设计

### 11.1 开发环境

#### 11.1.1 本地开发
- 使用Docker Compose启动Casdoor和PostgreSQL
- 使用npm run dev启动Next.js开发服务器
- 使用热重载提高开发效率

#### 11.1.2 Docker Compose配置
```yaml
version: '3.8'
services:
  casdoor:
    image: casbin/casdoor:latest
    ports:
      - "8000:8000"
    environment:
      - RUNNING_IN_DOCKER=true
    depends_on:
      - postgres
    volumes:
      - ./conf:/conf

  postgres:
    image: postgres:15
    ports:
      - "5432:5432"
    environment:
      - POSTGRES_USER=postgres
      - POSTGRES_PASSWORD=postgres
      - POSTGRES_DB=casdoor
    volumes:
      - postgres-data:/var/lib/postgresql/data

  costrict-web:
    build: .
    ports:
      - "3000:3000"
    environment:
      - DATABASE_URL=postgresql://postgres:postgres@postgres:5432/casdoor
      - CASDOOR_ENDPOINT=http://casdoor:8000
      - CASDOOR_CLIENT_ID=your-client-id
      - CASDOOR_CLIENT_SECRET=your-client-secret
    depends_on:
      - casdoor
      - postgres

volumes:
  postgres-data:
```

### 11.2 生产环境

#### 11.2.1 部署架构
```
┌─────────────────────────────────────────────────────────────┐
│                        负载均衡器                            │
│                      (Nginx / AWS ELB)                      │
└────────────────────────┬────────────────────────────────────┘
                         │
        ┌────────────────┼────────────────┐
        │                │                │
        ▼                ▼                ▼
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│  Next.js 1  │  │  Next.js 2  │  │  Next.js 3  │
│  (PM2)      │  │  (PM2)      │  │  (PM2)      │
└─────────────┘  └─────────────┘  └─────────────┘
        │                │                │
        └────────────────┼────────────────┘
                         │
        ┌────────────────┼────────────────┐
        │                │                │
        ▼                ▼                ▼
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│  Casdoor 1  │  │  Casdoor 2  │  │  Casdoor 3  │
│  (Go)       │  │  (Go)       │  │  (Go)       │
└─────────────┘  └─────────────┘  └─────────────┘
        │                │                │
        └────────────────┼────────────────┘
                         │
                         ▼
              ┌─────────────────────┐
              │   PostgreSQL        │
              │   (Primary + Replicas) │
              └─────────────────────┘
```

#### 11.2.2 部署工具
- 使用Docker容器化应用
- 使用Kubernetes编排容器
- 使用CI/CD自动化部署
- 使用监控工具监控系统状态

#### 11.2.3 监控和日志
- 使用Prometheus监控指标
- 使用Grafana可视化监控
- 使用ELK Stack收集日志
- 使用Sentry收集错误

---

## 12. 实施计划

### 12.1 阶段划分

#### 阶段1: 基础集成（2-3周）
- 搭建开发环境
- 部署Casdoor和PostgreSQL
- 实现OAuth 2.0认证
- 实现用户数据同步
- 创建基础页面框架

#### 阶段2: 数据模型扩展（2-3周）
- 设计并创建数据库表
- 实现数据模型
- 实现数据同步逻辑
- 编写单元测试

#### 阶段3: 核心功能开发（4-5周）
- 实现组织管理功能
- 实现仓库管理功能
- 实现技能管理功能
- 实现Agent管理功能
- 实现命令管理功能

#### 阶段4: 权限控制（2-3周）
- 实现RBAC权限模型
- 实现权限验证中间件
- 集成Casdoor权限系统
- 编写权限测试

#### 阶段5: 技能市场（3-4周）
- 实现技能市场页面
- 实现技能搜索和过滤
- 实现技能安装功能
- 实现技能评分功能

#### 阶段6: UI优化（2-3周）
- 优化页面布局
- 优化用户体验
- 实现响应式设计
- 性能优化

#### 阶段7: 测试和部署（2-3周）
- 编写集成测试
- 编写E2E测试
- 性能测试
- 部署到生产环境

### 12.2 里程碑

- **M1 (第3周)**: 完成基础集成，用户可以登录
- **M2 (第6周)**: 完成数据模型，可以创建组织和仓库
- **M3 (第11周)**: 完成核心功能，可以管理技能和Agent
- **M4 (第14周)**: 完成权限控制，实现RBAC
- **M5 (第18周)**: 完成技能市场，可以浏览和安装技能
- **M6 (第21周)**: 完成UI优化，提升用户体验
- **M7 (第24周)**: 完成测试和部署，系统上线

---

## 13. 风险和挑战

### 13.1 技术风险

#### 13.1.1 Casdoor集成复杂度
- **风险**: Casdoor的API可能比较复杂，集成难度大
- **缓解**: 充分研究Casdoor文档，使用官方SDK，编写集成测试

#### 13.1.2 数据同步问题
- **风险**: Casdoor和本地数据库的数据可能不一致
- **缓解**: 实现数据同步机制，定期检查数据一致性，实现数据修复功能

#### 13.1.3 性能问题
- **风险**: 系统在大量用户和数据时可能出现性能问题
- **缓解**: 优化数据库查询，实现缓存，使用分页，进行性能测试

### 13.2 业务风险

#### 13.2.1 需求变更
- **风险**: 需求可能在开发过程中发生变化
- **缓解**: 采用敏捷开发，快速迭代，保持代码灵活性

#### 13.2.2 用户体验
- **风险**: 用户可能觉得系统复杂难用
- **缓解**: 进行用户调研，设计简洁的UI，提供帮助文档

### 13.3 安全风险

#### 13.3.1 数据泄露
- **风险**: 用户数据可能被泄露
- **缓解**: 实现严格的安全措施，定期进行安全审计，使用HTTPS

#### 13.3.2 权限漏洞
- **风险**: 可能存在权限绕过漏洞
- **缓解**: 实现严格的权限验证，编写权限测试，进行安全测试

---

## 14. 总结

本设计文档详细描述了Costrict-Web系统的架构、数据库设计、API设计、前端设计、权限模型、集成方案、安全设计、性能优化、测试策略、部署方案和实施计划。

系统采用Next.js + React 19 + TypeScript作为前端技术栈，Casdoor作为认证授权平台，PostgreSQL作为数据库，实现了类似Git/GitLab的团队/仓库结构管理，整合了技能市场功能。

通过本设计文档，开发团队可以清晰地了解系统的整体架构和各个模块的设计，为后续的开发工作提供指导。
