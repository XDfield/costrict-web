# 设备管理模块实施进度

基于 `docs/DEVICE_MANAGEMENT_DESIGN.md` v1.1.0，服务端阶段一（P0）任务跟踪。SSE 集成暂不实施。

## 任务列表

### 1. 数据模型

- [x] `server/internal/models/models.go` — 追加 `Device` 结构体
- [x] `server/internal/models/models.go` — 更新 `AutoMigrate` 注册 `Device` 表

### 2. DeviceService（`server/internal/device/service.go`）

- [x] `NewDeviceService(db *gorm.DB) *DeviceService`
- [x] `RegisterDevice(userID string, req RegisterDeviceRequest) (*models.Device, string, error)` — 注册设备，生成 token，409 幂等处理
- [x] `GetDevice(deviceID, userID string) (*models.Device, error)` — 归属校验
- [x] `ListDevices(userID string) ([]models.Device, error)`
- [x] `ListWorkspaceDevices(workspaceID, userID string) ([]models.Device, error)`
- [x] `UpdateDevice(deviceID, userID string, req UpdateDeviceRequest) (*models.Device, error)`
- [x] `DeleteDevice(deviceID, userID string) error` — 软删除
- [x] `RotateToken(deviceID, userID string) (string, error)` — 轮换 token
- [x] `VerifyDeviceOwnership(deviceID, userID string) (*models.Device, error)` — 供 SSE 模块预留
- [x] `VerifyDeviceToken(token string) (*models.Device, error)` — 供 SSE 模块预留
- [x] `SetOnline(deviceID string) error` — 供 SSE 模块预留
- [x] `SetOffline(deviceID string) error` — 供 SSE 模块预留
- [x] `UpdateLastSeen(deviceID string) error` — 供 SSE 模块预留
- [x] `generateToken() (string, error)` — 32 字节随机 base64url

### 3. HTTP Handlers（`server/internal/device/handlers.go`）

- [x] `RegisterHandler` — `POST /api/devices/register`，返回 device + token（仅一次）
- [x] `ListHandler` — `GET /api/devices`
- [x] `GetHandler` — `GET /api/devices/:deviceID`
- [x] `UpdateHandler` — `PUT /api/devices/:deviceID`
- [x] `DeleteHandler` — `DELETE /api/devices/:deviceID`
- [x] `RotateTokenHandler` — `POST /api/devices/:deviceID/token/rotate`
- [x] `ListWorkspaceDevicesHandler` — `GET /api/workspaces/:workspaceID/devices`

### 4. 模块初始化（`server/internal/device/device.go`）

- [x] `Module` 结构体 + `New(db *gorm.DB) *Module`
- [x] `RegisterRoutes(apiGroup *gin.RouterGroup)` — 注册 `/api/devices` 路由组

### 5. 路由注册（`server/cmd/api/main.go`）

- [x] 初始化 `deviceModule` 并调用 `RegisterRoutes(apiGroup)`
- [x] 注册 `GET /api/workspaces/:workspaceID/devices`

---

## SSE 集成（已完成）

新增 `server/internal/cloud/` 模块，包含：

- [x] `types.go` — `SSEConnection`、`Event`、常量、错误变量
- [x] `connection_manager.go` — 连接注册/注销、心跳（30s）、超时清理（60s）、并发安全
- [x] `event_router.go` — 设备→用户事件路由、用户→设备控制指令路由、16ms 批处理、stale delta 过滤
- [x] `handlers.go` — 7 个 HTTP Handler，`DeviceSSEHandler` 支持 device token 认证（`Authorization: Bearer {token}`），连接建立/断开时调用 `SetOnline`/`SetOffline`
- [x] `cloud.go` — 模块初始化 + `RegisterRoutes`
- [x] `main.go` — 注册 `/cloud` 路由组，应用 `RequireAuth` 中间件

---

## 错误响应规范

| 场景 | 状态码 | 响应体 |
|------|--------|--------|
| deviceId 已注册 | 409 | `{"error": "device already registered", "deviceId": "..."}` |
| 设备不存在或不属于当前用户 | 404 | `{"error": "device not found"}` |
| 请求体解析失败 | 400 | `{"error": "invalid request body"}` |
| 非工作空间成员 | 403 | `{"error": "not a member of this workspace"}` |
| 未携带 token | 401 | `{"error": "Authentication required"}` |

---

## 阶段二 P1（已完成）

- [x] 设备注册幂等性：`deviceId` 重复注册返回 409（阶段一已实现）
- [x] 启动时将所有 `status=online` 设备重置为 `offline`（`main.go` AutoMigrate 后执行）
- [x] `GET /api/workspaces/:workspaceID/devices` 支持分页（`?page=1&pageSize=20`，默认 20，上限 100），响应含 `total`/`page`/`pageSize`/`hasMore`

---

## 进度记录

| 日期 | 内容 |
|------|------|
| 2026-03-13 | 创建进度文档，确认实施范围（SSE 集成暂缓） |
| 2026-03-13 | 完成阶段一全部服务端任务，构建通过 |
| 2026-03-13 | 完成 SSE 集成（cloud 模块全量实现），构建通过 |
| 2026-03-13 | 完成 P1 服务端稳定性增强，构建通过 |
