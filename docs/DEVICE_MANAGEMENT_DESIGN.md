# 设备管理模块技术提案

## 目录

- [概述](#概述)
- [背景与动机](#背景与动机)
- [可行性分析](#可行性分析)
- [架构设计](#架构设计)
- [数据模型](#数据模型)
- [模块设计](#模块设计)
- [API 设计](#api-设计)
- [认证集成](#认证集成)
- [与 SSE 模块集成](#与-sse-模块集成)
- [错误处理](#错误处理)
- [实施计划](#实施计划)

---

## 概述

### 背景与动机

`CLOUD_SSE_SERVER_DESIGN.md` 中的 SSE 模块以纯内存方式管理设备连接，deviceID 由设备端自行生成并在连接时传入，无持久化存储。这在初期验证阶段可行，但存在以下问题：

1. **无法验证设备合法性**：任何知道 API 地址的客户端均可伪造 deviceID 建立 SSE 连接
2. **无设备生命周期记录**：重启后无法知道历史上有哪些设备连接过
3. **无法主动管理设备**：用户无法查看、重命名、吊销特定设备的连接权限
4. **无在线状态持久化**：设备在线/离线状态仅存在于内存，无法跨实例查询

本模块在 SSE 设计的基础上，为 opencode CLI 设备实例提供完整的注册、认证、状态追踪和生命周期管理能力。

### 参考来源

参考 openclaw 项目的设备管理设计（`src/infra/device-pairing.ts`、`src/gateway/server-methods/devices.ts`），结合 costrict-web 现有技术栈进行适配：

| 特性 | openclaw | 本模块 |
|------|---------|--------|
| 设备认证 | Ed25519 密钥对 + 签名 | 随机 token（已有 Casdoor JWT 体系，无需额外密钥管理） |
| 配对流程 | 请求 → 管理员审批 | 用户自助注册（JWT 验证即可，降低使用门槛） |
| 存储 | 文件系统 JSON（原子写） | PostgreSQL + GORM（与现有模型一致） |
| 设备 ID | SHA-256(Ed25519 公钥字节) | 客户端自生成 UUID（简化实现） |
| 节点概念 | 独立 Node 类型（执行节点） | 合并到 Device（opencode CLI 即设备即执行节点） |

---

## 可行性分析

### 技术栈匹配度

| 需求 | 现有条件 | 结论 |
|------|---------|------|
| 数据库存储 | GORM + PostgreSQL，`models/models.go` 统一管理 | **直接扩展** |
| 用户身份 | `middleware.UserIDKey` 从 Casdoor JWT 提取 | **直接复用** |
| 工作空间关联 | `Organization.ID` 概念完全一致 | **直接映射** |
| 路由扩展 | 现有 `/api` 路由组 | **零冲突** |
| Token 生成 | Go 标准库 `crypto/rand` | **直接使用** |
| 软删除 | GORM `gorm.DeletedAt` | **直接使用** |

### 需要新增的内容

| 内容 | 工作量 | 说明 |
|------|--------|------|
| `Device` 数据库模型 | 小 | 追加到 `models/models.go` |
| `internal/device/` 模块 | 中 | service + handlers + 初始化 |
| `/api/devices` 路由注册 | 小 | `main.go` 追加约 15 行 |
| SSE 模块集成点 | 小 | `DeviceSSEHandler` 中注入 `DeviceService` |

---

## 架构设计

### 整体定位

```
┌─────────────────────────────────────────────────────────────┐
│                    opencode CLI (设备端)                     │
│  1. POST /api/devices/register  → 注册设备，获取 token       │
│  2. GET  /cloud/device/:id/event → 建立 SSE（携带 JWT）      │
│  3. POST /cloud/event            → 推送执行事件              │
└────────────────────────┬────────────────────────────────────┘
                         │
┌────────────────────────▼────────────────────────────────────┐
│              costrict-web server                             │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  /api/devices 路由组 (device/handlers.go)           │    │
│  │  - POST /register       → RegisterHandler           │    │
│  │  - GET  /               → ListHandler               │    │
│  │  - GET  /:deviceID      → GetHandler                │    │
│  │  - PUT  /:deviceID      → UpdateHandler             │    │
│  │  - DELETE /:deviceID    → DeleteHandler             │    │
│  │  - POST /:deviceID/token/rotate → RotateHandler     │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  DeviceService (device/service.go)                  │    │
│  │  - 设备注册/注销                                     │    │
│  │  - token 生成与轮换                                  │    │
│  │  - 在线状态更新（SetOnline/SetOffline）              │    │
│  │  - 归属权限校验                                      │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  cloud/ConnectionManager (SSE 模块)                 │    │
│  │  - DeviceSSEHandler 中调用 DeviceService 校验归属   │    │
│  │  - 连接建立/断开时更新设备在线状态                   │    │
│  │  - 心跳时更新 LastSeenAt                            │    │
│  └─────────────────────────────────────────────────────┘    │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  PostgreSQL                                         │    │
│  │  - devices 表（持久化设备信息）                      │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

### 新增目录结构

```
server/internal/
├── device/
│   ├── device.go      # 模块初始化、RegisterRoutes
│   ├── handlers.go    # Gin HTTP Handler（6 个端点）
│   └── service.go     # 业务逻辑（token 生成、状态管理）
├── cloud/             # SSE 模块（已有，需集成 DeviceService）
├── models/
│   └── models.go      # 追加 Device 模型
└── ...
```

---

## 数据模型

### Device 表

在 `server/internal/models/models.go` 追加：

```go
// Device 代表一个已注册的 opencode CLI 设备实例
type Device struct {
    ID              string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    DeviceID        string         `gorm:"uniqueIndex;not null"                           json:"deviceId"`
    DisplayName     string         `gorm:"not null"                                       json:"displayName"`
    Platform        string         `gorm:"not null"                                       json:"platform"`
    Version         string         `gorm:"not null"                                       json:"version"`
    UserID          string         `gorm:"not null;index"                                 json:"userId"`
    WorkspaceID     string         `gorm:"index"                                          json:"workspaceId"`
    Status          string         `gorm:"not null;default:'offline'"                     json:"status"`
    Token           string         `gorm:"not null"                                       json:"-"`
    TokenRotatedAt  *time.Time     `                                                      json:"tokenRotatedAt,omitempty"`
    LastConnectedAt *time.Time     `                                                      json:"lastConnectedAt,omitempty"`
    LastSeenAt      *time.Time     `                                                      json:"lastSeenAt,omitempty"`
    CreatedAt       time.Time      `                                                      json:"createdAt"`
    UpdatedAt       time.Time      `                                                      json:"updatedAt"`
    DeletedAt       gorm.DeletedAt `gorm:"index"                                          json:"-"`
}
```

### 字段说明

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| `id` | uuid | PK | 数据库内部主键 |
| `device_id` | varchar | UNIQUE NOT NULL | 客户端自生成 UUID，全局唯一，用于 SSE URL 路径参数 |
| `display_name` | varchar | NOT NULL | 用户自定义设备名称，如 "MacBook Pro - 工作" |
| `platform` | varchar | NOT NULL | 运行平台：`linux` \| `darwin` \| `windows` |
| `version` | varchar | NOT NULL | opencode CLI 版本号，如 `0.1.0` |
| `user_id` | varchar | NOT NULL INDEX | 归属用户（Casdoor JWT sub 字段） |
| `workspace_id` | varchar | INDEX | 关联工作空间（`Organization.ID`），空表示个人设备 |
| `status` | varchar | NOT NULL | 实时在线状态：`online` \| `offline` |
| `token` | varchar | NOT NULL | 设备认证 token（32 字节随机 base64url），不对外暴露 |
| `token_rotated_at` | timestamp | | token 最后轮换时间 |
| `last_connected_at` | timestamp | | 最后一次 SSE 连接建立时间 |
| `last_seen_at` | timestamp | | 最后一次 SSE 心跳时间 |
| `created_at` | timestamp | autoCreateTime | 注册时间 |
| `updated_at` | timestamp | autoUpdateTime | 最后更新时间 |
| `deleted_at` | timestamp | INDEX | 软删除时间（GORM） |

### 索引设计

```sql
CREATE UNIQUE INDEX idx_devices_device_id ON devices(device_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_devices_user_id ON devices(user_id);
CREATE INDEX idx_devices_workspace_id ON devices(workspace_id);
CREATE INDEX idx_devices_deleted_at ON devices(deleted_at);
```

---

## 模块设计

### service.go — 业务逻辑层

```go
package device

import (
    "crypto/rand"
    "encoding/base64"
    "time"

    "gorm.io/gorm"
    "github.com/costrict/costrict-web/server/internal/models"
)

type DeviceService struct {
    db *gorm.DB
}

func NewDeviceService(db *gorm.DB) *DeviceService

// RegisterDevice 注册新设备，返回生成的明文 token（仅此一次返回）
func (s *DeviceService) RegisterDevice(userID string, req RegisterDeviceRequest) (*models.Device, string, error)

// GetDevice 获取设备（校验归属：device.UserID == userID）
func (s *DeviceService) GetDevice(deviceID, userID string) (*models.Device, error)

// ListDevices 列出用户的所有未删除设备
func (s *DeviceService) ListDevices(userID string) ([]models.Device, error)

// ListWorkspaceDevices 列出工作空间内的设备（需校验调用者为成员）
func (s *DeviceService) ListWorkspaceDevices(workspaceID, userID string) ([]models.Device, error)

// UpdateDevice 更新设备元数据（displayName、workspaceID）
func (s *DeviceService) UpdateDevice(deviceID, userID string, req UpdateDeviceRequest) (*models.Device, error)

// DeleteDevice 软删除设备
func (s *DeviceService) DeleteDevice(deviceID, userID string) error

// RotateToken 生成新 token，返回明文（旧 token 立即失效）
func (s *DeviceService) RotateToken(deviceID, userID string) (string, error)

// VerifyDeviceOwnership 验证设备归属（供 SSE 模块调用，不校验 token）
func (s *DeviceService) VerifyDeviceOwnership(deviceID, userID string) (*models.Device, error)

// SetOnline 标记设备在线（SSE 连接建立时调用）
func (s *DeviceService) SetOnline(deviceID string) error

// SetOffline 标记设备离线（SSE 连接断开时调用）
func (s *DeviceService) SetOffline(deviceID string) error

// UpdateLastSeen 更新最后活跃时间（SSE 心跳时调用）
func (s *DeviceService) UpdateLastSeen(deviceID string) error
```

**Token 生成规则**（参考 openclaw `DeviceAuthToken` 设计）：

```go
func generateToken() (string, error) {
    b := make([]byte, 32)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}
```

### handlers.go — HTTP Handler 层

```go
package device

// RegisterHandler 注册设备
// POST /api/devices/register
// 认证：RequireAuth
func RegisterHandler(svc *DeviceService) gin.HandlerFunc

// ListHandler 列出当前用户的设备
// GET /api/devices
// 认证：RequireAuth
func ListHandler(svc *DeviceService) gin.HandlerFunc

// GetHandler 获取设备详情
// GET /api/devices/:deviceID
// 认证：RequireAuth
func GetHandler(svc *DeviceService) gin.HandlerFunc

// UpdateHandler 更新设备信息
// PUT /api/devices/:deviceID
// 认证：RequireAuth
func UpdateHandler(svc *DeviceService) gin.HandlerFunc

// DeleteHandler 注销设备
// DELETE /api/devices/:deviceID
// 认证：RequireAuth
func DeleteHandler(svc *DeviceService) gin.HandlerFunc

// RotateTokenHandler 轮换设备 token
// POST /api/devices/:deviceID/token/rotate
// 认证：RequireAuth
func RotateTokenHandler(svc *DeviceService) gin.HandlerFunc

// ListWorkspaceDevicesHandler 列出工作空间内的设备
// GET /api/workspaces/:workspaceID/devices
// 认证：RequireAuth
func ListWorkspaceDevicesHandler(svc *DeviceService) gin.HandlerFunc
```

### device.go — 模块初始化

```go
package device

type Module struct {
    Service *DeviceService
}

func New(db *gorm.DB) *Module

// RegisterRoutes 注册设备管理路由到 /api 路由组
func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup)
```

---

## API 设计

### 端点汇总

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `POST` | `/api/devices/register` | RequireAuth | 注册设备，返回 token（仅此一次） |
| `GET` | `/api/devices` | RequireAuth | 列出当前用户的所有设备 |
| `GET` | `/api/devices/:deviceID` | RequireAuth | 获取设备详情 |
| `PUT` | `/api/devices/:deviceID` | RequireAuth | 更新设备信息 |
| `DELETE` | `/api/devices/:deviceID` | RequireAuth | 注销设备（软删除） |
| `POST` | `/api/devices/:deviceID/token/rotate` | RequireAuth | 轮换设备认证 token |
| `GET` | `/api/workspaces/:workspaceID/devices` | RequireAuth | 列出工作空间内的设备 |

### 请求/响应格式

#### POST /api/devices/register

```json
// Request
{
  "deviceId": "550e8400-e29b-41d4-a716-446655440000",
  "displayName": "MacBook Pro - 工作",
  "platform": "darwin",
  "version": "0.1.0",
  "workspaceId": "org-uuid-xxx"
}

// Response 201
{
  "device": {
    "id": "db-uuid-xxx",
    "deviceId": "550e8400-e29b-41d4-a716-446655440000",
    "displayName": "MacBook Pro - 工作",
    "platform": "darwin",
    "version": "0.1.0",
    "userId": "casdoor-user-id",
    "workspaceId": "org-uuid-xxx",
    "status": "offline",
    "createdAt": "2026-03-12T00:00:00Z",
    "updatedAt": "2026-03-12T00:00:00Z"
  },
  "token": "base64url-32-bytes-random-token"
}

// Response 409（deviceId 已注册）
{
  "error": "device already registered",
  "deviceId": "550e8400-e29b-41d4-a716-446655440000"
}
```

> **注意**：`token` 字段仅在注册响应中返回一次，后续不再暴露。设备端需妥善保存。

#### GET /api/devices

```json
// Response 200
{
  "devices": [
    {
      "id": "db-uuid-xxx",
      "deviceId": "550e8400-...",
      "displayName": "MacBook Pro - 工作",
      "platform": "darwin",
      "version": "0.1.0",
      "userId": "casdoor-user-id",
      "workspaceId": "org-uuid-xxx",
      "status": "online",
      "lastConnectedAt": "2026-03-12T10:00:00Z",
      "lastSeenAt": "2026-03-12T10:05:00Z",
      "createdAt": "2026-03-12T00:00:00Z",
      "updatedAt": "2026-03-12T10:05:00Z"
    }
  ]
}
```

#### PUT /api/devices/:deviceID

```json
// Request（字段均可选）
{
  "displayName": "MacBook Pro - 个人",
  "workspaceId": "another-org-uuid"
}

// Response 200
{
  "device": { ... }
}
```

#### POST /api/devices/:deviceID/token/rotate

```json
// Response 200
{
  "token": "new-base64url-32-bytes-random-token",
  "rotatedAt": "2026-03-12T10:00:00Z"
}
```

#### GET /api/workspaces/:workspaceID/devices

```json
// Response 200
{
  "devices": [
    {
      "deviceId": "...",
      "displayName": "...",
      "platform": "linux",
      "status": "online",
      "userId": "...",
      "lastSeenAt": "2026-03-12T10:05:00Z"
    }
  ]
}
```

---

## 认证集成

### 复用现有中间件

所有 `/api/devices` 端点统一应用 `middleware.RequireAuth(casdoorEndpoint)`：

```go
// main.go
deviceModule := device.New(db)
deviceModule.RegisterRoutes(apiGroup)
```

```go
// device/device.go
func (m *Module) RegisterRoutes(apiGroup *gin.RouterGroup) {
    devices := apiGroup.Group("/devices")
    devices.POST("/register", handlers.RegisterHandler(m.Service))
    devices.GET("", handlers.ListHandler(m.Service))
    devices.GET("/:deviceID", handlers.GetHandler(m.Service))
    devices.PUT("/:deviceID", handlers.UpdateHandler(m.Service))
    devices.DELETE("/:deviceID", handlers.DeleteHandler(m.Service))
    devices.POST("/:deviceID/token/rotate", handlers.RotateTokenHandler(m.Service))
}
```

### Handler 中提取用户身份

```go
userID := c.GetString(middleware.UserIDKey)  // 复用现有常量
```

### 归属权限校验

所有涉及特定设备的操作，均通过 `DeviceService.GetDevice(deviceID, userID)` 校验归属：

```go
// service.go
func (s *DeviceService) GetDevice(deviceID, userID string) (*models.Device, error) {
    var device models.Device
    result := s.db.Where("device_id = ? AND user_id = ?", deviceID, userID).First(&device)
    if result.Error != nil {
        return nil, result.Error  // gorm.ErrRecordNotFound → 404
    }
    return &device, nil
}
```

---

## 与 SSE 模块集成

### DeviceSSEHandler 集成点

在 `cloud/handlers.go` 的 `DeviceSSEHandler` 中注入 `DeviceService`：

```go
// cloud/handlers.go
func DeviceSSEHandler(manager *ConnectionManager, deviceSvc *device.DeviceService) gin.HandlerFunc {
    return func(c *gin.Context) {
        deviceID := c.Param("deviceID")
        userID := c.GetString(middleware.UserIDKey)

        // 1. 验证设备归属（查 DB，确认 UserID 匹配）
        dev, err := deviceSvc.VerifyDeviceOwnership(deviceID, userID)
        if err != nil {
            c.AbortWithStatusJSON(403, gin.H{"error": "device not found or access denied"})
            return
        }

        // 2. 创建 SSEConnection，注册到 ConnectionManager
        // ...

        // 3. 连接建立后标记在线
        deviceSvc.SetOnline(dev.DeviceID)

        // 4. 连接断开时标记离线
        defer deviceSvc.SetOffline(dev.DeviceID)

        // 5. 进入 SSE 循环...
    }
}
```

### 心跳集成

在 `cloud/connection_manager.go` 的 `startHeartbeat()` goroutine 中：

```go
func (m *ConnectionManager) startHeartbeat(deviceSvc *device.DeviceService) {
    ticker := time.NewTicker(HeartbeatIntervalMs * time.Millisecond)
    for range ticker.C {
        m.mu.RLock()
        for _, conn := range m.connections {
            // 非阻塞发送心跳事件
            select {
            case conn.Send <- Event{Type: EventHeartbeat, ...}:
            default:
            }
            // 更新设备最后活跃时间
            if conn.Type == ConnTypeDevice && conn.DeviceID != "" {
                go deviceSvc.UpdateLastSeen(conn.DeviceID)
            }
        }
        m.mu.RUnlock()
    }
}
```

### 数据流

```
opencode CLI
    │
    │  1. POST /api/devices/register（携带 Casdoor JWT）
    │─────────────────────────────────────────────────► DeviceService.RegisterDevice()
    │                                                         │
    │  ◄─────────────────────────────────────────────── 返回 {device, token}
    │
    │  2. GET /cloud/device/:deviceID/event（携带 Casdoor JWT）
    │─────────────────────────────────────────────────► DeviceSSEHandler
    │                                                         │
    │                                                   DeviceService.VerifyDeviceOwnership()
    │                                                         │
    │                                                   SetOnline(deviceID)
    │  ◄─────────────────────────────────────────────── SSE 连接建立
    │
    │  3. 每 30s 收到 heartbeat 事件
    │  ◄─────────────────────────────────────────────── UpdateLastSeen(deviceID)
    │
    │  4. 连接断开
    │                                                   SetOffline(deviceID)
```

---

## 错误处理

### HTTP 错误响应规范

| 场景 | HTTP 状态码 | 响应体 |
|------|------------|--------|
| 未携带 token | 401 | `{"error": "Authentication required"}` |
| deviceId 已注册 | 409 | `{"error": "device already registered", "deviceId": "..."}` |
| 设备不存在或不属于当前用户 | 404 | `{"error": "device not found"}` |
| 请求体解析失败 | 400 | `{"error": "invalid request body"}` |
| 非工作空间成员 | 403 | `{"error": "not a member of this workspace"}` |

---

## 实施计划

### 阶段一：核心功能（P0，配合 SSE 模块同步实施）

1. **`models/models.go`** — 追加 `Device` 模型，更新 `AutoMigrate`
2. **`internal/device/service.go`** — 实现 `DeviceService`（注册、查询、状态更新、token 管理）
3. **`internal/device/handlers.go`** — 实现 6 个 HTTP Handler
4. **`internal/device/device.go`** — 模块初始化，`RegisterRoutes`
5. **`cmd/api/main.go`** — 注册设备模块路由（约 5 行）
6. **`cloud/handlers.go`** — `DeviceSSEHandler` 接入 `DeviceService` 归属校验

### 阶段二：稳定性增强（P1）

- 设备注册幂等性：相同 `deviceId` 重复注册时返回 409，而非静默覆盖
- 工作空间设备列表的分页支持（`?page=1&pageSize=20`）
- 设备在线状态的定期清理（进程重启时将所有 `status=online` 重置为 `offline`）

### 阶段三：安全增强（P2）

- deviceToken 认证：SSE 连接时支持 `X-Device-Token` 头替代 Casdoor JWT，实现设备级别独立认证
- token 存储加密：数据库中存储 token 的 bcrypt hash，而非明文
- 设备操作审计日志

### main.go 修改点（最小侵入）

```go
// 在现有路由注册之后追加

// Device management module
deviceModule := device.New(database.GetDB())
deviceModule.RegisterRoutes(apiGroup)

// 工作空间设备列表（挂载到 organizations 路由下）
apiGroup.GET("/workspaces/:workspaceID/devices",
    handlers.ListWorkspaceDevicesHandler(deviceModule.Service))
```

---

**文档版本：** 1.0.0
**创建日期：** 2026-03-12
**维护者：** CoStrict Team
