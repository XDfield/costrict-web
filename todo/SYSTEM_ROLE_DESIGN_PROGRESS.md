# 系统级角色与管理权限实施进度

基于 `docs/proposals/SYSTEM_ROLE_DESIGN.md`，任务跟踪。

---

## 一、数据模型（`server/internal/models/models.go`）

- [x] 追加 `UserSystemRole` 模型
  ```go
  type UserSystemRole struct {
      ID        string         `gorm:"primaryKey;size:36" json:"id"`
      UserID    string         `gorm:"uniqueIndex:uk_user_system_role,priority:1;index;not null;size:191" json:"user_id"`
      Role      string         `gorm:"uniqueIndex:uk_user_system_role,priority:2;index;not null;size:64" json:"role"`
      GrantedBy *string        `gorm:"index;size:191" json:"granted_by"`
      CreatedAt time.Time      `json:"created_at"`
      UpdatedAt time.Time      `json:"updated_at"`
      DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
  }

  func (UserSystemRole) TableName() string {
      return "user_system_roles"
  }
  ```
- [x] 添加唯一索引：`UNIQUE(user_id, role)`
- [x] 添加索引：`user_id`、`role`、`granted_by`
- [x] 角色白名单约束：`platform_admin` / `business_admin`
- [x] `AutoMigrate` 追加 `UserSystemRole`
- [x] 如项目使用 SQL migration，同步新增 `user_system_roles` 建表脚本

---

## 二、系统角色模块（`server/internal/systemrole/`）

### 2.1 目录与基础结构

- [x] 新增目录：`server/internal/systemrole/`
- [x] 新增 `systemrole.go`
- [x] 新增 `service.go`
- [x] 新增 `middleware.go`
- [x] 新增 `types.go`
- [x] 新增 `handlers.go`

### 2.2 `types.go`

- [x] 角色常量：`SystemRolePlatformAdmin`
- [x] 角色常量：`SystemRoleBusinessAdmin`
- [x] 能力常量：`CanManageSystemRoles`
- [x] 能力常量：`CanManageSystemSettings`
- [x] 能力常量：`CanManageSystemChannels`
- [x] 能力常量：`CanViewGlobalBusinessData`
- [x] 能力常量：`CanAccessBusinessDashboard`
- [x] 角色白名单判断工具方法
- [x] 角色到能力映射方法

### 2.3 `service.go`

- [x] `SystemRoleService` 结构体（`db *gorm.DB`）
- [x] `NewSystemRoleService(db *gorm.DB) *SystemRoleService`
- [x] `ListRoles(userID string) ([]string, error)`
- [x] `HasRole(userID, role string) (bool, error)`
- [x] `HasAnyRole(userID string, roles ...string) (bool, error)`
- [x] `GrantRole(userID, role, operatorID string) error`
- [x] `RevokeRole(userID, role, operatorID string) error`
- [x] `ListUsersByRole(role string) ([]models.User, error)`
- [x] `GetCapabilities(userID string) ([]string, error)`

### 2.4 Service 关键规则

- [x] 授予角色前校验目标用户已存在于本地 `users` 表
- [x] 授予角色前校验 `role` 合法
- [x] 重复授予同一角色时保持幂等或返回明确错误
- [x] 撤销角色前校验目标角色记录存在
- [x] 禁止撤销最后一个 `platform_admin`
- [x] `platform_admin` 自动兼容 `business_admin` 能力
- [x] 授予/撤销时正确记录 `granted_by`

### 2.5 `handlers.go`

- [x] `GET /admin/system-roles/users/:userId` — 查询指定用户系统角色
- [x] `POST /admin/system-roles/users/:userId` — 授予系统角色
- [x] `DELETE /admin/system-roles/users/:userId/:role` — 撤销系统角色
- [x] `GET /admin/system-roles?role=business_admin` — 按角色查询成员列表
- [x] `GET /auth/system-roles/me` — 查询当前用户系统角色与能力
- [x] 请求参数校验与统一错误响应
- [x] Swagger 注释补充

### 2.6 `systemrole.go`

- [x] `Module` 结构体
- [x] `New(db *gorm.DB) *Module`
- [x] `RegisterRoutes(...)` 注册系统角色相关路由

---

## 三、鉴权与中间件（`server/internal/systemrole/middleware.go`）

- [x] `RequirePlatformAdmin(roleSvc *SystemRoleService) gin.HandlerFunc`
- [x] `RequireBusinessAdminOrAbove(roleSvc *SystemRoleService) gin.HandlerFunc`
- [x] 从上下文读取 `userId`
- [x] 未登录返回 `401 Unauthorized`
- [x] 角色不足返回 `403 Forbidden`
- [x] 中间件内部复用本地系统角色查询能力
- [x] `platform_admin` 自动满足 `business_admin` 访问要求

---

## 四、现有模块接入改造

### 4.1 通知渠道管理

- [x] 检查 `server/internal/notification/handlers.go` 中当前标记为“admin only”的接口
- [x] 将 `/admin/notification-channels` 等平台级接口切换为 `RequirePlatformAdmin`
- [x] 避免“仅登录即管理员”风险

### 4.2 业务后台与看板类接口

- [ ] 识别全局统计/经营分析/看板相关接口
- [ ] 将只读型后台能力接入 `RequireBusinessAdminOrAbove`
- [ ] 校准只读与可写操作边界

### 4.3 认证态联动

- [x] 复用现有 `RequireAuth` 登录态解析能力
- [x] 保证系统角色中间件运行在认证中间件之后

---

## 五、路由与模块注册

- [x] 在应用启动处初始化 `systemrole` 模块
- [x] 注册 `/admin/system-roles` 路由
- [x] 注册 `/auth/system-roles/me` 路由
- [x] 确保平台管理接口挂载正确鉴权中间件

---

## 六、安全与审计

- [x] 所有系统角色管理接口强制要求 `platform_admin`
- [x] 高权限接口错误响应避免暴露内部细节
- [x] 审核授予/撤销操作审计字段是否满足最小可追踪要求
- [ ] 评估是否需要后续扩展独立审计日志表

---

## 七、测试

### 7.1 Service 测试

- [x] 查询用户角色列表
- [x] 授予 `business_admin` 成功
- [x] 授予 `platform_admin` 成功
- [x] 非法角色授予失败
- [x] 不存在用户授予失败
- [x] 重复授予处理正确
- [x] 撤销角色成功
- [x] 撤销不存在角色处理正确
- [x] 禁止撤销最后一个 `platform_admin`
- [x] `platform_admin` 具备 `business_admin` 能力

### 7.2 Middleware 测试

- [x] 未登录访问 `RequirePlatformAdmin` 返回 401
- [x] 非平台管理员访问 `RequirePlatformAdmin` 返回 403
- [x] `business_admin` 访问 `RequireBusinessAdminOrAbove` 成功
- [x] `platform_admin` 访问 `RequireBusinessAdminOrAbove` 成功

### 7.3 Handler / API 测试

- [x] 查询用户系统角色接口
- [x] 授予系统角色接口
- [x] 撤销系统角色接口
- [ ] 按角色查询成员列表接口
- [x] 查询当前用户角色与能力接口

---

## 进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 数据模型 | 已完成 |
| 二 | 系统角色模块 | 已完成 |
| 三 | 鉴权与中间件 | 已完成 |
| 四 | 现有模块接入改造 | 部分完成 |
| 五 | 路由与模块注册 | 已完成 |
| 六 | 安全与审计 | 部分完成 |
| 七 | 测试 | 部分完成 |
