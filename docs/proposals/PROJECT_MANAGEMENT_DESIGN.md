> **实现状态：待实现**
>
> - 状态：📋 待实现
> - 实现位置：`server/internal/project/`（待创建）
> - 数据模型：`Project`、`ProjectMember`、`ProjectInvitation` 待在 `server/internal/models/models.go` 中添加
> - 说明：项目模块待实现，包括项目创建、成员管理、邀请机制、通知集成等功能。

---

# 项目管理模块技术提案

## 目录

- [概述](#概述)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [模块设计](#模块设计)
- [API 设计](#api-设计)
- [通知集成](#通知集成)
- [错误处理](#错误处理)
- [实施计划](#实施计划)

---

## 概述

### 背景与动机

当前系统以设备、仓库、工作空间为核心组织单元，缺乏更高层级的"项目"概念。用户无法将相关资源（设备、仓库、工作空间）组织到统一的项目中，也无法在项目维度进行协作管理。

引入"项目"概念后，可以实现：
- **资源聚合**：将设备、仓库等资源归属到项目下统一管理
- **团队协作**：支持邀请用户加入项目，区分管理员与成员角色
- **权限隔离**：项目级别的资源访问控制
- **生命周期管理**：支持项目启用/归档，控制项目可用性

### 设计原则

- **Casdoor 用户体系**：复用现有 Casdoor OAuth 认证，不创建本地用户表
- **角色分离**：区分项目管理员（admin）与普通成员（member）
- **邀请机制**：通过邀请链接邀请用户，支持接受/拒绝
- **通知集成**：复用现有通知渠道模块，发送项目邀请通知
- **软删除**：项目支持软删除，归档后可恢复
- **审计日志**：记录邀请历史，便于追溯

---

## 架构设计

### 整体定位

```
┌─────────────────────────────────────────────────────────┐
│                      Casdoor OAuth                       │
│                    (用户认证与授权)                       │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│                    Project Service                        │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Project CRUD │  │  Member Mgmt │  │ Invitation   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
                            │
            ┌───────────────┼───────────────┐
            ▼               ▼               ▼
    ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
    │   Project    │ │ProjectMember │ │ProjectInvit. │
    │   (资源聚合)  │ │  (角色管理)  │ │  (邀请机制)  │
    └──────────────┘ └──────────────┘ └──────────────┘
            │
            └───────────────────────────────────────┐
                                                    │
                                                    ▼
                                    ┌──────────────────────────┐
                                    │  Notification Service   │
                                    │  (邀请通知发送)         │
                                    └──────────────────────────┘
```

### 目录结构

```
server/internal/project/
├── project.go          # 模块初始化、RegisterRoutes
├── service.go          # ProjectService（业务逻辑）
├── handlers.go         # Gin HTTP Handler
└── types.go            # 请求/响应结构体定义
```

---

## 数据模型

### Project 表

```go
type Project struct {
    ID          string    // uuid PK
    Name        string    // 项目名称，UNIQUE(creator_id, name)
    Description string    // 项目描述
    CreatorID   string    // 创建者 ID（Casdoor user_id）
    Enabled     bool      // 是否启用
    EnabledAt   *time.Time // 启用时间，非空表示已启用
    ArchivedAt  *time.Time // 归档时间，非空表示已归档
    Metadata    datatypes.JSON // 扩展元数据
    CreatedAt   time.Time
    UpdatedAt   time.Time
    DeletedAt   gorm.DeletedAt
}
```

**索引：**
- `idx_creator_name`: `(creator_id, name)` UNIQUE
- `idx_enabled`: `enabled`
- `idx_enabled_at`: `enabled_at`
- `idx_archived_at`: `archived_at`

**项目状态说明：**
| Enabled | EnabledAt | ArchivedAt | 状态 |
|---------|-----------|------------|------|
| `false` | `null` | `null` | 未启用（草稿） |
| `true` | `非空` | `null` | 已启用（活跃） |
| `false` | `任意` | `非空` | 已归档 |

**状态转换规则：**
- 未启用 → 已启用：设置 `enabled=true`，`enabled_at=NOW()`
- 已启用 → 已归档：设置 `enabled=false`，`archived_at=NOW()`
- 已归档 → 已启用：设置 `enabled=true`，`archived_at=null`

### ProjectMember 表

```go
type ProjectMember struct {
    ID        string    // uuid PK
    ProjectID string    // 关联 Project
    UserID    string    // Casdoor user_id
    Role      string    // "admin" | "member"
    JoinedAt  time.Time // 加入时间
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt
}
```

**索引：**
- `idx_project_user`: `(project_id, user_id)` UNIQUE
- `idx_user`: `user_id`

**角色权限：**
| 角色 | 权限 |
|------|------|
| `admin` | 创建/编辑/删除项目、邀请成员、移除成员、归档项目 |
| `member` | 查看项目、查看项目资源、接受/拒绝邀请 |

### ProjectInvitation 表

```go
type ProjectInvitation struct {
    ID          string    // uuid PK
    ProjectID   string    // 关联 Project
    InviterID   string    // 邀请人 ID
    InviteeID   string    // 被邀请人 ID（Casdoor user_id）
    Role        string    // 邀请角色 "admin" | "member"
    Status      string    // "pending" | "accepted" | "rejected" | "cancelled"
    Message     string    // 邀请附言
    RespondedAt *time.Time // 响应时间
    ExpiresAt   *time.Time // 过期时间（可选）
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

**索引：**
- `idx_project_invitee`: `(project_id, invitee_id)`
- `idx_invitee_status`: `(invitee_id, status)`
- `idx_status`: `status`

**状态流转：**
```
pending → accepted
   ↓
rejected
   ↓
cancelled
```

---

## 模块设计

### ProjectService

```go
type ProjectService struct {
    db           *gorm.DB
    notificationSvc *NotificationService
}

// 项目管理
func (s *ProjectService) CreateProject(creatorID, name, description string) (*models.Project, error)
func (s *ProjectService) GetProject(projectID string) (*models.Project, error)
func (s *ProjectService) ListProjects(userID string, includeArchived bool) ([]models.Project, error)
func (s *ProjectService) UpdateProject(projectID, userID string, updates map[string]any) error
func (s *ProjectService) DeleteProject(projectID, userID string) error
func (s *ProjectService) EnableProject(projectID, userID string) error
func (s *ProjectService) DisableProject(projectID, userID string) error
func (s *ProjectService) ArchiveProject(projectID, userID string) error
func (s *ProjectService) UnarchiveProject(projectID, userID string) error

// 成员管理
func (s *ProjectService) ListMembers(projectID string) ([]models.ProjectMember, error)
func (s *ProjectService) RemoveMember(projectID, operatorID, targetUserID string) error
func (s *ProjectService) UpdateMemberRole(projectID, operatorID, targetUserID, newRole string) error

// 邀请管理
func (s *ProjectService) CreateInvitation(projectID, inviterID, inviteeID, role, message string) (*models.ProjectInvitation, error)
func (s *ProjectService) RespondInvitation(invitationID, userID string, accept bool) error
func (s *ProjectService) ListInvitations(projectID string) ([]models.ProjectInvitation, error)
func (s *ProjectService) ListMyInvitations(userID string) ([]models.ProjectInvitation, error)
func (s *ProjectService) CancelInvitation(invitationID, operatorID string) error

// 权限校验
func (s *ProjectService) IsProjectAdmin(projectID, userID string) (bool, error)
func (s *ProjectService) IsProjectMember(projectID, userID string) (bool, error)
```

### 权限校验逻辑

```go
func (s *ProjectService) checkPermission(projectID, userID string, requiredRole string) error {
    member, err := s.GetMember(projectID, userID)
    if err != nil {
        return ErrNotMember
    }
    
    if requiredRole == "admin" && member.Role != "admin" {
        return ErrPermissionDenied
    }
    
    return nil
}
```

---

## API 设计

### 端点汇总

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| `GET` | `/api/projects` | 用户 | 列出我的项目 |
| `POST` | `/api/projects` | 用户 | 创建项目 |
| `GET` | `/api/projects/:id` | 项目成员 | 获取项目详情 |
| `PUT` | `/api/projects/:id` | 项目管理员 | 更新项目信息 |
| `DELETE` | `/api/projects/:id` | 项目管理员 | 删除项目（软删除） |
| `POST` | `/api/projects/:id/enable` | 项目管理员 | 启用项目 |
| `POST` | `/api/projects/:id/disable` | 项目管理员 | 禁用项目 |
| `POST` | `/api/projects/:id/archive` | 项目管理员 | 归档项目 |
| `POST` | `/api/projects/:id/unarchive` | 项目管理员 | 取消归档 |
| `GET` | `/api/projects/:id/members` | 项目成员 | 列出项目成员 |
| `DELETE` | `/api/projects/:id/members/:userId` | 项目管理员 | 移除成员 |
| `PUT` | `/api/projects/:id/members/:userId/role` | 项目管理员 | 更新成员角色 |
| `POST` | `/api/projects/:id/invitations` | 项目管理员 | 邀请用户加入项目 |
| `GET` | `/api/projects/:id/invitations` | 项目管理员 | 列出项目邀请记录 |
| `GET` | `/api/invitations` | 用户 | 列出我的邀请 |
| `POST` | `/api/invitations/:id/respond` | 被邀请用户 | 接受/拒绝邀请 |
| `DELETE` | `/api/invitations/:id` | 邀请人 | 取消邀请 |

### 关键请求/响应

#### POST /api/projects

```json
// Request
{
  "name": "AI 能力平台",
  "description": "企业级 AI 能力管理平台",
  "enabled": true
}

// Response 201
{
  "project": {
    "id": "uuid-xxx",
    "name": "AI 能力平台",
    "description": "企业级 AI 能力管理平台",
    "creatorId": "user-id",
    "enabled": true,
    "enabledAt": "2026-04-01T00:00:00Z",
    "archivedAt": null,
    "createdAt": "2026-04-01T00:00:00Z"
  }
}
```

#### POST /api/projects/:id/invitations

```json
// Request
{
  "inviteeId": "target-user-id",
  "role": "member",
  "message": "欢迎加入我们的项目"
}

// Response 201
{
  "invitation": {
    "id": "uuid-yyy",
    "projectId": "uuid-xxx",
    "inviterId": "user-id",
    "inviteeId": "target-user-id",
    "role": "member",
    "status": "pending",
    "message": "欢迎加入我们的项目",
    "expiresAt": "2026-04-08T00:00:00Z",
    "createdAt": "2026-04-01T00:00:00Z"
  }
}
```

#### POST /api/projects/:id/enable

```json
// Response 200
{
  "project": {
    "id": "uuid-xxx",
    "enabled": true,
    "enabledAt": "2026-04-01T02:00:00Z",
    "archivedAt": null
  }
}
```

#### POST /api/projects/:id/disable

```json
// Response 200
{
  "project": {
    "id": "uuid-xxx",
    "enabled": false,
    "enabledAt": "2026-04-01T00:00:00Z",
    "archivedAt": null
  }
}
```

#### POST /api/projects/:id/archive

```json
// Response 200
{
  "project": {
    "id": "uuid-xxx",
    "enabled": false,
    "enabledAt": "2026-04-01T00:00:00Z",
    "archivedAt": "2026-04-01T02:00:00Z"
  }
}
```

#### POST /api/invitations/:id/respond

```json
// Request
{
  "accept": true
}

// Response 200
{
  "invitation": {
    "id": "uuid-yyy",
    "status": "accepted",
    "respondedAt": "2026-04-01T01:00:00Z"
  }
}
```

#### GET /api/projects/:id/members

```json
// Response 200
{
  "members": [
    {
      "id": "uuid-aaa",
      "projectId": "uuid-xxx",
      "userId": "user-id",
      "role": "admin",
      "joinedAt": "2026-04-01T00:00:00Z"
    },
    {
      "id": "uuid-bbb",
      "projectId": "uuid-xxx",
      "userId": "member-id",
      "role": "member",
      "joinedAt": "2026-04-01T01:00:00Z"
    }
  ]
}
```

---

## 通知集成

### 新增通知事件类型

在 `server/internal/notification/types.go` 中添加系统通知事件：

```go
const (
    // ... 现有事件
    
    // 系统通知事件（通用系统消息）
    EventSystemNotification = "system.notification"
    
    // 项目相关事件
    EventProjectInvitationCreated = "project.invitation.created"
    EventProjectInvitationAccepted = "project.invitation.accepted"
    EventProjectInvitationRejected = "project.invitation.rejected"
)
```

### 邀请通知触发

当创建项目邀请时，触发通知：

```go
func (s *ProjectService) CreateInvitation(projectID, inviterID, inviteeID, role, message string) (*models.ProjectInvitation, error) {
    // 1. 创建邀请记录
    invitation := &models.ProjectInvitation{
        ProjectID: projectID,
        InviterID: inviterID,
        InviteeID: inviteeID,
        Role:      role,
        Status:    "pending",
        Message:   message,
        ExpiresAt: time.Now().Add(7 * 24 * time.Hour), // 默认 7 天过期
    }
    
    if err := s.db.Create(invitation).Error; err != nil {
        return nil, err
    }
    
    // 2. 触发邀请通知
    if s.notificationSvc != nil {
        s.notificationSvc.TriggerNotifications(
            inviteeID,
            "project.invitation.created",
            invitation.ID,
            "",
        )
    }
    
    // 3. 触发系统通知（通用系统消息）
    if s.notificationSvc != nil {
        s.notificationSvc.TriggerNotifications(
            inviteeID,
            "system.notification",
            invitation.ID,
            "",
        )
    }
    
    return invitation, nil
}
```

### 通知消息格式

#### 项目邀请消息

```go
type ProjectInvitationMessage struct {
    InvitationID string `json:"invitationId"`
    ProjectID    string `json:"projectId"`
    ProjectName  string `json:"projectName"`
    InviterName  string `json:"inviterName"`
    Role         string `json:"role"`
    Message      string `json:"message"`
    ExpiresAt    string `json:"expiresAt"`
}
```

#### 系统通知消息

```go
type SystemNotificationMessage struct {
    Type        string `json:"type"`        // "project.invitation" | "project.member_added" 等
    Title       string `json:"title"`       // 通知标题
    Content     string `json:"content"`     // 通知内容
    RelatedID   string `json:"relatedId"`   // 关联资源 ID（如邀请 ID、项目 ID）
    RelatedType string `json:"relatedType"` // 关联资源类型（如 "invitation", "project"）
    ActionURL   string `json:"actionUrl"`   // 操作链接（可选）
}
```

### 通知使用场景

| 事件类型 | 触发时机 | 通知内容 |
|---------|---------|---------|
| `project.invitation.created` | 创建项目邀请 | 邀请详情、项目信息 |
| `project.invitation.accepted` | 用户接受邀请 | 通知邀请人成员已加入 |
| `project.invitation.rejected` | 用户拒绝邀请 | 通知邀请人成员已拒绝 |
| `system.notification` | 系统通用通知 | 系统消息、提醒等 |

### 通知渠道配置

用户可在通知渠道配置中订阅以下事件：

```json
{
  "triggerEvents": [
    "session.completed",
    "session.failed",
    "session.aborted",
    "device.offline",
    "project.invitation.created",
    "system.notification"
  ]
}
```

---

## 错误处理

| 场景 | HTTP 状态码 | 响应体 |
|------|------------|--------|
| 未携带 token | 401 | `{"error": "Authentication required"}` |
| 项目不存在 | 404 | `{"error": "project not found"}` |
| 无权限访问项目 | 403 | `{"error": "permission denied"}` |
| 项目名称重复 | 400 | `{"error": "project name already exists"}` |
| 邀请自己 | 400 | `{"error": "cannot invite yourself"}` |
| 用户已在项目中 | 400 | `{"error": "user already in project"}` |
| 邀请已存在且未过期 | 400 | `{"error": "invitation already exists"}` |
| 邀请不存在 | 404 | `{"error": "invitation not found"}` |
| 邀请已过期 | 400 | `{"error": "invitation expired"}` |
| 邀请已处理 | 400 | `{"error": "invitation already responded"}` |
| 非邀请人操作 | 403 | `{"error": "only inviter can cancel invitation"}` |

---

## 实施计划

### 阶段一：数据模型与基础 CRUD（预计 2 天）

1. **数据库迁移**
   - 创建 `projects` 表
   - 创建 `project_members` 表
   - 创建 `project_invitations` 表
   - 添加索引

2. **基础 Service 实现**
   - `CreateProject`
   - `GetProject`
   - `ListProjects`
   - `UpdateProject`
   - `DeleteProject`

3. **API Handler 实现**
   - 注册路由
   - 实现 HTTP Handler
   - 添加 Swagger 注释

### 阶段二：成员管理（预计 1 天）

1. **Service 扩展**
   - `ListMembers`
   - `RemoveMember`
   - `UpdateMemberRole`
   - 权限校验逻辑

2. **API Handler 实现**
   - 成员相关端点

### 阶段三：邀请机制（预计 1.5 天）

1. **Service 扩展**
   - `CreateInvitation`
   - `RespondInvitation`
   - `ListInvitations`
   - `ListMyInvitations`
   - `CancelInvitation`

2. **API Handler 实现**
   - 邀请相关端点

3. **通知集成**
   - 添加项目相关通知事件
   - 实现邀请通知触发逻辑

### 阶段四：启用/归档功能（预计 1 天）

1. **Service 扩展**
   - `EnableProject`
   - `DisableProject`
   - `ArchiveProject`
   - `UnarchiveProject`

2. **API Handler 实现**
   - 启用/禁用相关端点
   - 归档相关端点

3. **状态校验**
   - 已归档项目不能再次归档
   - 已启用项目不能重复启用
   - 归档项目需先取消归档才能启用

### 阶段五：测试与文档（预计 1 天）

1. **单元测试**
   - Service 层测试
   - Handler 层测试

2. **集成测试**
   - 完整流程测试

3. **文档更新**
   - 更新 API 文档
   - 添加使用示例

---

**文档版本：** 1.1.0
**创建日期：** 2026-04-01
**更新日期：** 2026-04-01
**维护者：** CoStrict Team