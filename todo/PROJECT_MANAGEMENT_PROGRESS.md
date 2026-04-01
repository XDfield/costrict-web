# 项目管理模块实施进度

基于 `docs/proposals/PROJECT_MANAGEMENT_DESIGN.md` v1.1.0，任务跟踪。

---

## 一、数据模型（`server/internal/models/models.go`）

### 1.1 Project

- [ ] 追加 `Project` 模型
  ```go
  type Project struct {
      ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      Name        string         `gorm:"not null"                                       json:"name"`
      Description string         `                                                       json:"description,omitempty"`
      CreatorID   string         `gorm:"not null;index"                                 json:"creatorId"`
      Enabled     bool           `gorm:"not null;default:false;index"                   json:"enabled"`
      EnabledAt   *time.Time     `gorm:"index"                                          json:"enabledAt,omitempty"`
      ArchivedAt  *time.Time     `gorm:"index"                                          json:"archivedAt,omitempty"`
      Metadata    datatypes.JSON `                                                       json:"metadata,omitempty"`
      CreatedAt   time.Time      `                                                       json:"createdAt"`
      UpdatedAt   time.Time      `                                                       json:"updatedAt"`
      DeletedAt   gorm.DeletedAt `gorm:"index"                                          json:"-"`
  }
  ```
- [ ] 添加唯一索引：`UNIQUE(creator_id, name)`
- [ ] 添加索引：`enabled`、`enabled_at`、`archived_at`

### 1.2 ProjectMember

- [ ] 追加 `ProjectMember` 模型
  ```go
  type ProjectMember struct {
      ID        string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      ProjectID string         `gorm:"not null;index;uniqueIndex:idx_project_user"    json:"projectId"`
      UserID    string         `gorm:"not null;index;uniqueIndex:idx_project_user"    json:"userId"`
      Role      string         `gorm:"not null;default:'member'"                       json:"role"`
      JoinedAt  time.Time      `gorm:"not null"                                        json:"joinedAt"`
      CreatedAt time.Time      `                                                        json:"createdAt"`
      UpdatedAt time.Time      `                                                        json:"updatedAt"`
      DeletedAt gorm.DeletedAt `gorm:"index"                                           json:"-"`
  }
  ```
- [ ] 约束角色值：`admin` / `member`

### 1.3 ProjectInvitation

- [ ] 追加 `ProjectInvitation` 模型
  ```go
  type ProjectInvitation struct {
      ID          string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      ProjectID   string     `gorm:"not null;index"                                 json:"projectId"`
      InviterID   string     `gorm:"not null;index"                                 json:"inviterId"`
      InviteeID   string     `gorm:"not null;index"                                 json:"inviteeId"`
      Role        string     `gorm:"not null;default:'member'"                      json:"role"`
      Status      string     `gorm:"not null;default:'pending';index"               json:"status"`
      Message     string     `                                                        json:"message,omitempty"`
      RespondedAt *time.Time `                                                        json:"respondedAt,omitempty"`
      ExpiresAt   *time.Time `                                                        json:"expiresAt,omitempty"`
      CreatedAt   time.Time  `                                                        json:"createdAt"`
      UpdatedAt   time.Time  `                                                        json:"updatedAt"`
  }
  ```
- [ ] 添加索引：`(project_id, invitee_id)`、`(invitee_id, status)`、`status`
- [ ] 约束状态值：`pending` / `accepted` / `rejected` / `cancelled`

### 1.4 AutoMigrate / Migration

- [ ] `AutoMigrate` 追加 `Project`、`ProjectMember`、`ProjectInvitation`
- [ ] 或新增 SQL migration：创建三张表与索引
- [ ] 明确 `Project` 创建后自动插入一条创建者 `admin` 成员记录

---

## 二、Project 模块（`server/internal/project/`）

### 2.1 types.go

- [ ] `CreateProjectRequest`
- [ ] `UpdateProjectRequest`
- [ ] `CreateInvitationRequest`
- [ ] `RespondInvitationRequest`
- [ ] `UpdateMemberRoleRequest`
- [ ] `ProjectResponse` / `InvitationResponse` / `MemberResponse`

### 2.2 service.go

#### 项目管理

- [ ] `NewProjectService(db *gorm.DB, notificationSvc *notification.NotificationService) *ProjectService`
- [ ] `CreateProject(creatorID, name, description string, enabled bool) (*models.Project, error)`
- [ ] `GetProject(projectID string) (*models.Project, error)`
- [ ] `ListProjects(userID string, includeArchived bool) ([]models.Project, error)`
- [ ] `UpdateProject(projectID, userID string, updates map[string]any) error`
- [ ] `DeleteProject(projectID, userID string) error`
- [ ] `EnableProject(projectID, userID string) error`
- [ ] `DisableProject(projectID, userID string) error`
- [ ] `ArchiveProject(projectID, userID string) error`
- [ ] `UnarchiveProject(projectID, userID string) error`

#### 成员管理

- [ ] `ListMembers(projectID string) ([]models.ProjectMember, error)`
- [ ] `GetMember(projectID, userID string) (*models.ProjectMember, error)`
- [ ] `RemoveMember(projectID, operatorID, targetUserID string) error`
- [ ] `UpdateMemberRole(projectID, operatorID, targetUserID, newRole string) error`

#### 邀请管理

- [ ] `CreateInvitation(projectID, inviterID, inviteeID, role, message string) (*models.ProjectInvitation, error)`
- [ ] `RespondInvitation(invitationID, userID string, accept bool) error`
- [ ] `ListInvitations(projectID string) ([]models.ProjectInvitation, error)`
- [ ] `ListMyInvitations(userID string) ([]models.ProjectInvitation, error)`
- [ ] `CancelInvitation(invitationID, operatorID string) error`

#### 权限与状态校验

- [ ] `IsProjectAdmin(projectID, userID string) (bool, error)`
- [ ] `IsProjectMember(projectID, userID string) (bool, error)`
- [ ] `checkPermission(projectID, userID, requiredRole string) error`
- [ ] 校验：项目归档后禁止邀请、修改成员、修改项目信息
- [ ] 校验：禁用项目是否允许查看但禁止部分操作（需按提案约束实现）
- [ ] 校验：防止移除最后一个管理员

### 2.3 handlers.go

#### 项目接口

- [ ] `GET /api/projects` — 列出我的项目
- [ ] `POST /api/projects` — 创建项目
- [ ] `GET /api/projects/:id` — 获取项目详情
- [ ] `PUT /api/projects/:id` — 更新项目信息
- [ ] `DELETE /api/projects/:id` — 删除项目（软删除）
- [ ] `POST /api/projects/:id/enable` — 启用项目
- [ ] `POST /api/projects/:id/disable` — 禁用项目
- [ ] `POST /api/projects/:id/archive` — 归档项目
- [ ] `POST /api/projects/:id/unarchive` — 取消归档

#### 成员接口

- [ ] `GET /api/projects/:id/members` — 列出项目成员
- [ ] `DELETE /api/projects/:id/members/:userId` — 移除成员
- [ ] `PUT /api/projects/:id/members/:userId/role` — 更新成员角色

#### 邀请接口

- [ ] `POST /api/projects/:id/invitations` — 邀请用户加入项目
- [ ] `GET /api/projects/:id/invitations` — 列出项目邀请记录
- [ ] `GET /api/invitations` — 列出我的邀请
- [ ] `POST /api/invitations/:id/respond` — 接受/拒绝邀请
- [ ] `DELETE /api/invitations/:id` — 取消邀请

### 2.4 project.go

- [ ] `Module` 结构体 + `New(db *gorm.DB, notificationSvc *notification.NotificationService) *Module`
- [ ] `RegisterRoutes(apiGroup *gin.RouterGroup)` — 注册 `/api/projects` 与 `/api/invitations` 路由

---

## 三、通知模块集成（`server/internal/notification/`）

### 3.1 事件类型

- [ ] 在 `types.go` 添加 `EventSystemNotification = "system.notification"`
- [ ] 在 `types.go` 添加 `EventProjectInvitationCreated = "project.invitation.created"`
- [ ] 在 `types.go` 添加 `EventProjectInvitationAccepted = "project.invitation.accepted"`
- [ ] 在 `types.go` 添加 `EventProjectInvitationRejected = "project.invitation.rejected"`

### 3.2 消息构建

- [ ] 增加 `ProjectInvitationMessage` 结构
- [ ] 增加 `SystemNotificationMessage` 结构
- [ ] 支持项目邀请消息格式化输出
- [ ] 支持系统通知消息格式化输出

### 3.3 触发逻辑

- [ ] 创建邀请时触发 `project.invitation.created`
- [ ] 创建邀请时同步触发 `system.notification`
- [ ] 接受邀请时触发 `project.invitation.accepted`
- [ ] 拒绝邀请时触发 `project.invitation.rejected`
- [ ] 更新可订阅事件列表，允许用户配置上述事件

### 3.4 待确认项

- [ ] 明确 `system.notification` 是否仅作为站内/系统类通用事件，还是所有业务通知都应复制一份
- [ ] 明确通知模板由项目模块组装还是通知模块统一组装

---

## 四、路由与模块注册

### 4.1 `server/cmd/api/main.go`

- [ ] 初始化 `notificationModule`（若尚未初始化）
- [ ] 初始化 `projectModule := project.New(database.GetDB(), notificationSvc)`
- [ ] 调用 `projectModule.RegisterRoutes(apiGroup)`

### 4.2 中间件与鉴权

- [ ] 所有项目接口接入 `RequireAuth`
- [ ] 项目成员接口校验成员身份
- [ ] 项目管理与邀请接口校验管理员身份

---

## 五、Swagger / API 文档

- [ ] 为项目相关 handler 补充 Swagger 注释
- [ ] 为邀请相关 handler 补充 Swagger 注释
- [ ] 为启用/禁用/归档接口补充 Swagger 注释
- [ ] 更新 OpenAPI 文档生成产物（如项目已有对应流程）

---

## 六、测试

### 6.1 Service 单元测试

- [ ] 创建项目后自动写入创建者 admin 成员
- [ ] 项目名称唯一性校验
- [ ] 项目启用时写入 `enabled_at`
- [ ] 项目禁用时保留 `enabled_at` 历史值
- [ ] 项目归档时写入 `archived_at` 且自动置 `enabled=false`
- [ ] 已归档项目不能重复归档
- [ ] 归档项目取消归档后可重新启用
- [ ] 邀请已存在且未过期时不能重复邀请
- [ ] 接受邀请后自动创建成员记录
- [ ] 拒绝邀请后不创建成员记录
- [ ] 非管理员不能邀请成员
- [ ] 不能移除最后一个管理员

### 6.2 Handler / 集成测试

- [ ] 创建项目接口
- [ ] 查询项目列表接口
- [ ] 邀请与响应流程接口
- [ ] 成员角色变更接口
- [ ] 启用/禁用/归档/取消归档接口
- [ ] 邀请记录查询接口

---

## 七、进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 数据模型 | 未开始 |
| 二 | Project 模块 | 未开始 |
| 三 | 通知模块集成 | 未开始 |
| 四 | 路由与模块注册 | 未开始 |
| 五 | Swagger / API 文档 | 未开始 |
| 六 | 测试 | 未开始 |

---

## 八、进度记录

| 日期 | 内容 |
|------|------|
| 2026-04-01 | 创建进度文档，基于项目管理提案 v1.1.0 初始化任务清单 |
