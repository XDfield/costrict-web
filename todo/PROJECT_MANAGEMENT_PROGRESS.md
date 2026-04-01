# 项目管理模块实施进度

基于 `docs/proposals/PROJECT_MANAGEMENT_DESIGN.md` v1.1.0，任务跟踪。

---

## 一、数据模型（`server/internal/models/models.go`）

### 1.1 Project

- [x] 追加 `Project` 模型
  ```go
  type Project struct {
      ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
      Name        string         `gorm:"not null"                                       json:"name"`
      Description string         `                                                       json:"description,omitempty"`
      CreatorID   string         `gorm:"not null;index"                                 json:"creatorId"`
      EnabledAt   *time.Time     `gorm:"index"                                          json:"enabledAt,omitempty"`
      ArchivedAt  *time.Time     `gorm:"index"                                          json:"archivedAt,omitempty"`
      Metadata    datatypes.JSON `                                                       json:"metadata,omitempty"`
      CreatedAt   time.Time      `                                                       json:"createdAt"`
      UpdatedAt   time.Time      `                                                       json:"updatedAt"`
      DeletedAt   gorm.DeletedAt `gorm:"index"                                          json:"-"`
  }
  ```
- [x] 添加唯一索引：`UNIQUE(creator_id, name)`
- [x] 添加索引：`enabled_at`、`archived_at`

### 1.2 ProjectMember

- [x] 追加 `ProjectMember` 模型
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
- [x] 约束角色值：`admin` / `member`（业务层校验）

### 1.3 ProjectInvitation

- [x] 追加 `ProjectInvitation` 模型
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
- [x] 添加索引：`(project_id, invitee_id)`、`(invitee_id, status)`、`status`
- [x] 约束状态值：`pending` / `accepted` / `rejected` / `cancelled`（业务层校验）

### 1.4 AutoMigrate / Migration

- [x] `AutoMigrate` 追加 `Project`、`ProjectMember`、`ProjectInvitation`
- [x] 或新增 SQL migration：创建三张表与索引
- [x] 明确 `Project` 创建后自动插入一条创建者 `admin` 成员记录

---

## 二、Project 模块（`server/internal/project/`）

### 2.1 types.go

- [x] `CreateProjectRequest`
- [x] `UpdateProjectRequest`
- [x] `CreateInvitationRequest`
- [x] `RespondInvitationRequest`
- [x] `UpdateMemberRoleRequest`
- [x] `ProjectResponse` / `InvitationResponse` / `MemberResponse`

### 2.2 service.go

#### 项目管理

- [x] `NewProjectService(db *gorm.DB, notificationSvc *notification.NotificationService) *ProjectService`
- [x] `CreateProject(creatorID, name, description string, enabledAt *time.Time) (*models.Project, error)`
- [x] `GetProject(projectID string) (*models.Project, error)`
- [x] `ListProjects(userID string, includeArchived bool) ([]models.Project, error)`
- [x] `UpdateProject(projectID, userID string, updates map[string]any) error`
- [x] `DeleteProject(projectID, userID string) error`
- [x] `ArchiveProject(projectID, userID string) error`
- [x] `UnarchiveProject(projectID, userID string) error`

#### 成员管理

- [x] `ListMembers(projectID string) ([]models.ProjectMember, error)`
- [x] `GetMember(projectID, userID string) (*models.ProjectMember, error)`
- [x] `RemoveMember(projectID, operatorID, targetUserID string) error`
- [x] `UpdateMemberRole(projectID, operatorID, targetUserID, newRole string) error`

#### 邀请管理

- [x] `CreateInvitation(projectID, inviterID, inviteeID, role, message string) (*models.ProjectInvitation, error)`
- [x] `RespondInvitation(invitationID, userID string, accept bool) error`
- [x] `ListInvitations(projectID string) ([]models.ProjectInvitation, error)`
- [x] `ListMyInvitations(userID string) ([]models.ProjectInvitation, error)`
- [x] `CancelInvitation(invitationID, operatorID string) error`

#### 权限与状态校验

- [x] `IsProjectAdmin(projectID, userID string) (bool, error)`
- [x] `IsProjectMember(projectID, userID string) (bool, error)`
- [x] `checkPermission(projectID, userID, requiredRole string) error`
- [x] 校验：项目归档后禁止邀请、修改成员、修改项目信息
- [x] 校验：不存在禁用项目场景，`enabled_at` 仅作为可选生命周期时间字段
- [x] 校验：防止移除最后一个管理员

### 2.3 handlers.go

#### 项目接口

- [x] `GET /api/projects` — 列出我的项目
- [x] `POST /api/projects` — 创建项目
- [x] `GET /api/projects/:id` — 获取项目详情
- [x] `PUT /api/projects/:id` — 更新项目信息
- [x] `DELETE /api/projects/:id` — 删除项目（软删除）
- [x] `POST /api/projects/:id/archive` — 归档项目
- [x] `POST /api/projects/:id/unarchive` — 取消归档

#### 成员接口

- [x] `GET /api/projects/:id/members` — 列出项目成员
- [x] `DELETE /api/projects/:id/members/:userId` — 移除成员
- [x] `PUT /api/projects/:id/members/:userId/role` — 更新成员角色

#### 邀请接口

- [x] `POST /api/projects/:id/invitations` — 邀请用户加入项目
- [x] `GET /api/projects/:id/invitations` — 列出项目邀请记录
- [x] `GET /api/invitations` — 列出我的邀请
- [x] `POST /api/invitations/:id/respond` — 接受/拒绝邀请
- [x] `DELETE /api/invitations/:id` — 取消邀请

### 2.4 project.go

- [x] `Module` 结构体 + `New(db *gorm.DB, notificationSvc *notification.NotificationService) *Module`
- [x] `RegisterRoutes(apiGroup *gin.RouterGroup)` — 注册 `/api/projects` 与 `/api/invitations` 路由

---

## 三、通知模块集成（`server/internal/notification/`）

### 3.1 事件类型

- [x] 在 `types.go` 添加 `EventSystemNotification = "system.notification"`
- [x] 在 `types.go` 添加 `EventProjectInvitationCreated = "project.invitation.created"`
- [x] 在 `types.go` 添加 `EventProjectInvitationAccepted = "project.invitation.accepted"`
- [x] 在 `types.go` 添加 `EventProjectInvitationRejected = "project.invitation.rejected"`

### 3.2 消息构建

- [x] 增加 `ProjectInvitationMessage` 结构
- [x] 增加 `SystemNotificationMessage` 结构
- [x] 支持项目邀请消息格式化输出
- [x] 支持系统通知消息格式化输出

### 3.3 触发逻辑

- [x] 创建邀请时触发 `project.invitation.created`
- [x] 创建邀请时同步触发 `system.notification`
- [x] 接受邀请时触发 `project.invitation.accepted`（无需实现，已确认不发送通知）
- [x] 拒绝邀请时触发 `project.invitation.rejected`（无需实现，已确认不发送通知）
- [x] 更新可订阅事件列表，允许用户配置上述事件

### 3.4 待确认项

- [x] 明确 `system.notification` 是否仅作为站内/系统类通用事件，还是所有业务通知都应复制一份（已确认：作为通用事件）
- [x] 明确通知模板由项目模块组装还是通知模块统一组装（已确认：由具体业务模块组装）

---

## 四、路由与模块注册

### 4.1 `server/cmd/api/main.go`

- [x] 初始化 `notificationModule`（若尚未初始化）
- [x] 初始化 `projectModule := project.New(database.GetDB(), notificationSvc)`
- [x] 调用 `projectModule.RegisterRoutes(apiGroup)`

### 4.2 中间件与鉴权

- [x] 所有项目接口接入 `RequireAuth`
- [x] 项目成员接口校验成员身份
- [x] 项目管理与邀请接口校验管理员身份

---

## 五、Swagger / API 文档

- [x] 为项目相关 handler 补充 Swagger 注释
- [x] 为邀请相关 handler 补充 Swagger 注释
- [x] 为项目生命周期/归档接口补充 Swagger 注释
- [x] 更新 OpenAPI 文档生成产物（如项目已有对应流程）

---

## 六、测试

### 6.1 Service 单元测试

- [x] 创建项目后自动写入创建者 admin 成员
- [x] 项目名称唯一性校验
- [x] 创建项目时支持写入 `enabled_at`
- [x] 项目允许 `enabled_at` 为空（长期项目/未设置时间）
- [x] 项目归档时写入 `archived_at`
- [x] 已归档项目不能重复归档
- [x] 归档项目取消归档后保留已有 `enabled_at`
- [x] 邀请已存在且未过期时不能重复邀请
- [x] 接受邀请后自动创建成员记录
- [x] 拒绝邀请后不创建成员记录
- [x] 非管理员不能邀请成员
- [x] 不能移除最后一个管理员

### 6.2 Handler / 集成测试

- [x] 创建项目接口
- [x] 查询项目列表接口
- [x] 邀请与响应流程接口
- [x] 成员角色变更接口
- [x] 归档/取消归档接口
- [x] 邀请记录查询接口

---

## 七、进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 数据模型 | 已完成 |
| 二 | Project 模块 | 已完成 |
| 三 | 通知模块集成 | 已完成 |
| 四 | 路由与模块注册 | 已完成 |
| 五 | Swagger / API 文档 | 已完成 |
| 六 | 测试 | 已完成 |

---

## 八、进度记录

| 日期 | 内容 |
|------|------|
| 2026-04-01 | 创建进度文档，基于项目管理提案 v1.1.0 初始化任务清单 |
| 2026-04-01 | 完成项目管理模块首版实现：数据模型、项目服务、成员/邀请接口、路由注册与基础单元测试；通知接受/拒绝事件、Swagger 注释和部分集成测试待补充 |
| 2026-04-01 | 补全项目管理模块 Swagger 注释，并重新生成 docs/swagger.json、docs/swagger.yaml、docs/docs.go |
| 2026-04-01 | 根据确认结果更新通知策略：接受/拒绝邀请无需通知、system.notification 作为通用事件、通知模板由业务模块组装；新增项目管理 SQL migration |
| 2026-04-01 | 根据业务语义重构项目生命周期：移除禁用场景，`enabled_at` 改为可选时间字段，新增移除 `enabled` 列的 migration，并同步更新代码、测试与文档 |
| 2026-04-01 | 补齐项目模块 handler/integration tests，并为通知渠道增加 supportedEvents 返回与 triggerEvents 合法性校验 |
