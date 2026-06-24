> **实现状态：已完成**
>
> - 状态：✅ 已完成
> - 涉及仓库：
>   - `cs-cloud`：`internal/provider/machine.go`，`internal/provider/machine_test.go`，`internal/device/client.go`，`internal/device/storage.go`
>   - `costrict-web/gateway`：`gateway/internal/manager.go`，`gateway/internal/tunnel_handler.go`，`gateway/internal/router.go`，`gateway/internal/registration.go`
>   - `costrict-web/server`：`server/internal/gateway/store.go`，`server/internal/gateway/store_redis.go`，`server/internal/gateway/registry.go`，`server/internal/gateway/handlers.go`，`server/internal/gateway/client.go`，`server/cmd/api/main.go`
> - 说明：克隆设备处理方案已完整实现并验证通过。

---

# 克隆设备处理方案

## 目录

- [背景与问题](#背景与问题)
- [架构概览](#架构概览)
- [机制一：随机 device_id 生成](#机制一随机-device_id-生成)
- [机制二：跨 Gateway 设备接管](#机制二跨-gateway-设备接管)
- [组件改动范围](#组件改动范围)
- [API 设计](#api-设计)
- [数据流设计](#数据流设计)
- [场景覆盖分析](#场景覆盖分析)
- [被否决的方案](#被否决的方案)
- [验证](#验证)

---

## 背景与问题

### 问题描述

cs-cloud 设备端通过 `device_id` 标识设备身份，`device_id` 存储在 `device.json` 文件中。以下场景会导致设备身份冲突：

1. **MAC 地址哈希碰撞**：原 `GenerateMachineID()` 使用 `SHA256(platform + 首个排序后 MAC + username)` 生成设备 ID。当多台机器具有相同的硬件配置（VM 模板）、相同用户时，会生成完全相同的 `device_id`，导致设备注册冲突。

2. **VM 镜像克隆**：从模板克隆出的虚拟机共享完全相同的 `device.json`（包括 `device_id` 和 `device_token`），克隆机器间相互干扰。

3. **磁盘快照恢复**：恢复到历史快照的设备与当前运行的设备持有相同的 `device.json`，出现身份争抢。

### 核心挑战

| 挑战 | 说明 |
|------|------|
| 同设备克隆 | 克隆 B 与克隆 A 持有完全相同 credentials，**服务器在隧道层无法区分** |
| 数据一致性 | 多设备同时写入同一设备数据可能导致覆盖或状态异步 |
| 最小化误判 | 重连、重建、接管等合法场景不应被误判为克隆 |

---

## 架构概览

解决方案由两个**独立且互补**的机制组成：

```
┌──────────────────────────────────────────────────────────────┐
│                    克隆设备处理方案                            │
├──────────────────────────────┬───────────────────────────────┤
│      机制一：身份生成层       │      机制二：隧道连接层         │
│                              │                               │
│  GenerateMachineID()         │  DeviceOnline → 服务器发现     │
│  └─ crypto/rand 32 字节      │    旧 gateway → 通知关闭 session│
│                              │                               │
│  GenerateOldMachineID()      │  UnregisterIf 返回 bool       │
│  └─ 复刻旧哈希算法            │  └─ 仅活跃 session 发离线通知   │
│     用于新注册的 legacyDeviceId│                               │
│                              │  connID 连接身份链             │
│  device_v2.json 分文件管理     │  └─ Register 生成唯一 connID   │
│  └─ 新设备 → device_v2.json   │  └─ NotifyOnline 携带 connID   │
│  └─ 旧设备 → device.json     │  └─ BindDevice 存储/返回 connID │
│      + 迁移到 device_v2.json  │  └─ CloseIfConnID 精准关闭     │
│  └─ 文件存在性 → 迁移检测     │                               │
│                              │  BindDevice Lua 原子操作       │
│  Register() auth 前置校验     │  └─ read+write 单次 EVAL       │
│  └─ 已迁移设备也检查用户凭证   │                               │
│                              │  DeviceOffline 归属校验        │
│  适用：所有设备                │  └─ unbind 前验证 gateway 归属 │
│  效果：统一随机 ID，消除碰撞    │                               │
│                              │  适用：共享 device.json 的克隆   │
│                              │  效果：后启动踢先启动，单点在线    │
└──────────────────────────────┴───────────────────────────────┘
```

### 场景覆盖

| 场景 | 触发机制 | 效果 |
|------|---------|------|
| 新克隆 VM（首次启动，无 device.json） | 机制一 | 随机 ID，互不干扰 |
| 旧克隆（共享 device.json，不同 gateway） | 机制二 | 后启动踢先启动，单点在线 |
| 旧克隆（共享 device.json，同 gateway） | Gateway TunnelManager 内置 | 直接替换 session |
| 正常设备崩溃重启 | 不触发（同一 gateway，session 已死） | Normalize offline 通知 |
| Gateway 宕机切换 | 不触发（旧 gateway 心跳超时，已被清理） | 正常连接 |

---

## 机制一：所有设备统一随机 ID + Server 端迁移

### 问题

原 `GenerateMachineID()` 使用 `SHA256(platform + 首个排序后 MAC + username)` 生成设备 ID，存在两个核心问题：

1. **碰撞**：相同硬件配置的 VM 生成完全相同的 `device_id`，注册冲突
2. **不稳定**：笔记本网卡变化导致 `device_id` 变化，同一设备在 Server 上注册多条记录

旧方案（信任 device.json 保留旧哈希）不足以解决这两个问题——**碰撞已经在生产环境中发生**，必须主动迁移。

### 方案

所有设备统一使用 `crypto/rand` 生成的随机 ID。旧设备通过 Server 已有的 `legacyDeviceId` 迁移路径将旧记录更新为新 ID。

### 改动

#### `internal/provider/machine.go`

**`GenerateMachineID()`**：改用 `crypto/rand` 生成 32 字节随机数（先前已改动）：

```go
func GenerateMachineID() string {
    b := make([]byte, 32)
    if _, err := rand.Read(b); err != nil {
        // Fallback: hash machine characteristics
        platform, macAddrs, hostname, username := MachineIDParts()
        raw := fmt.Sprintf("%s-%s-%s-%s", platform, macAddrs, hostname, username)
        h := sha256.Sum256([]byte(raw))
        return fmt.Sprintf("%x", h)
    }
    return hex.EncodeToString(b)
}
```

**`GenerateOldMachineID()`**（新增）：完整复刻旧的确定性哈希算法（含原始的单 MAC 逻辑），仅用于新注册时作为 `legacyDeviceId` 供 Server 端查找碰撞记录：

```go
func GenerateOldMachineID() string {
    platform := jsPlatform()
    mac := getFirstMACAddress()   // 旧行为：排序后取第一个 MAC
    username := "unknown"
    if u, err := user.Current(); err == nil && u.Username != "" {
        username = stripDomain(u.Username)
    }
    raw := fmt.Sprintf("%s-%s-%s", platform, mac, username)
    h := sha256.Sum256([]byte(raw))
    return fmt.Sprintf("%x", h)
}
```

**`getFirstMACAddress()`**（新增）：复刻原始单 MAC 选择逻辑（排序后取第一个）：

```go
func getFirstMACAddress() string {
    interfaces, err := net.Interfaces()
    if err != nil { return "unknown" }
    var addrs []string
    for _, iface := range interfaces {
        if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 || len(iface.HardwareAddr) == 0 {
            continue
        }
        addrs = append(addrs, iface.HardwareAddr.String())
    }
    if len(addrs) == 0 { return "unknown" }
    sort.Strings(addrs)
    return strings.ToLower(strings.ReplaceAll(addrs[0], ":", ""))
}
```

**`MachineIDParts()`**：返回 4 个值（含 `hostname`），用于 `cs cloud status` 展示。不再用于 device_id 生成。

#### `internal/device/storage.go` — `device_v2.json` 分文件管理

新增 `device_v2.json` 作为新的设备身份文件。旧设备保留 `device.json`，迁移后写入 `device_v2.json` 并删除 `device.json`。

**新增函数：**

| 函数 | 作用 |
|------|------|
| `DeviceV2Path()` | 返回 `device_v2.json` 路径 |
| `deviceV2FileExists()` | 判断 `device_v2.json` 是否存在 |
| `ClearDeviceV2()` | 删除 `device_v2.json`（owner 变更时） |
| `loadDeviceFrom()` | 内部辅助，从指定路径加载设备信息 |

**`LoadDevice()`**：先检查 `device_v2.json`，不存在则回退到 `device.json`。

**`SaveDevice()`**：始终写入 `device_v2.json`（新设备写入、迁移目的文件）。

**`GetDeviceID()`**：`loadStoredDeviceID()` 先查 `device_v2.json`，再回退到 `device.json`。

#### `internal/device/client.go` — `Register()` 流程重构

核心逻辑：用 `deviceV2FileExists()` 判断设备状态——有 v2 文件 = 已迁移/新设备，只有旧 device.json = 需迁移。

```
Register():
│
├─ LoadDevice() (先查 v2，回退 device.json)
│   ├─ 非 nil → device_v2.json 存在?
│   │   ├─ 是 → **已迁移/新设备**
│   │   │      └─ owner 变更?
│   │   │          ├─ 是 → ClearDeviceV2() → 继续到首次注册
│   │   │          └─ 否 → auth() 校验用户凭证
│   │   │                 ├─ 成功 → 返回 existing ✅
│   │   │                 └─ 失败 → 返回 error（触发重新登录）
│   │   │
│   │   └─ 否 → **旧设备，需迁移**
│   │          ├─ auth()（失败 → 返回 error，触发重新登录）
│   │          ├─ newID = GenerateMachineID()
│   │          ├─ enroll(deviceId=newID, legacyDeviceId=existing.DeviceID)
│   │          │   └─ Server: migrateFromLegacyDeviceID → 更新记录
│   │          ├─ 成功 → SaveDevice(info) → 写入 device_v2.json
│   │          │      → ClearDevice() → 删除旧 device.json
│   │          │      → 返回
│   │          └─ 失败 → auth 错误? → 返回 existing + error（触发登录）
│   │                   瞬态错误? → log warning → 返回 existing（降级）
│   │
│   └─ nil → **首次注册**
│          ├─ auth()
│          ├─ newID = GenerateMachineID()          // crypto/rand
│          ├─ oldHash = GenerateOldMachineID()     // 旧哈希（供 Server 查找碰撞记录）
│          ├─ enroll(deviceId=newID, legacyDeviceId=oldHash)
│          ├─ SaveDevice(info) → 写入 device_v2.json
│          └─ 返回
```

**关键设计决策：**

1. **已迁移设备增加 auth 前置校验**：device_v2.json 存在且 owner 匹配时，不再直接返回缓存信息，而是先调用 `auth()` 验证用户凭证。若凭证无效（token 过期且 refresh 失败），返回 error 触发重新登录，而非静默继续。

2. **迁移失败区分错误类型**：
   - **auth 错误**（401/403/token refresh failed）：上抛 error，上层捕获后触发重新登录。
   - **瞬态错误**（Server 500、网络超时）：`logger.Warn` + 返回 existing，设备继续工作，下次启动重试。

关键变化：**不再使用 `GenerateOldMachineID()` 做迁移判断**。迁移检测改为文件存在性检查——`device_v2.json` 是否存在。旧 ID 从 `device.json` 文件中直接读取（而非重新计算），因此 MAC 变化不影响迁移：

| 状态 | 文件状态 | 检测方式 | 行为 |
|------|---------|---------|------|
| 迁移前 | 仅 `device.json`（旧哈希） | `deviceV2FileExists() = false` | **触发迁移** |
| 已迁移 | `device_v2.json`（随机 ID） | `deviceV2FileExists() = true` | 跳过迁移 |
| 笔记本 MAC 变化 | 仅 `device.json`（旧哈希） | `deviceV2FileExists() = false` | **正常迁移**，从文件读出旧哈希 ✅ |

**`migrateDeviceID()`**：将旧设备迁移到新随机 ID，写入 `device_v2.json` 并清理旧文件。

```go
func (c *Client) migrateDeviceID(ctx context.Context, existing *DeviceInfo) (*DeviceInfo, error) {
    creds, err := auth(ctx, c.cloud)
    if err != nil {
        return nil, err
    }

    base := c.cloud.CloudBaseURL(creds.BaseURL)
    newID := provider.GenerateMachineID()

    info, err := enroll(ctx, c.cloud, creds, base, newID, existing.DeviceID)
    if err != nil {
        if IsAuthError(err) {
            return existing, err
        }
        logger.Warn("[device] migration failed (transient), using existing device: %v", err)
        return existing, nil
    }

    // Migration succeeded — save went to device_v2.json, clean up old device.json
    if err := ClearDevice(); err != nil {
        logger.Warn("[device] failed to remove old device.json: %v", err)
    }

    logger.Info("[device] migrated device_id from %q to %q in device_v2.json", existing.DeviceID, info.DeviceID)
    return info, nil
}
```

迁移失败时根据错误类型采取不同策略：auth 错误上抛触发重新登录，瞬态错误保持现有 device.json 不变（下次启动重试）。迁移成功后自动删除旧 device.json，此后 `LoadDevice()` 直接读取 `device_v2.json`。

### Server 侧迁移链路

Server 侧**零改动**，复用现有接口能力：

```
POST /devices { deviceId: "rand_xyz", legacyDeviceId: "sha256_abc" }
       ↓
RegisterDevice() → 未找到 deviceId "rand_xyz"
                 → migrateFromLegacyDeviceID("sha256_abc")
                 → 找到旧记录（device_id = "sha256_abc"）
                 → migrateDeviceID(): UPDATE device_id = "rand_xyz" WHERE device_id = "sha256_abc"
                 → 生成新 token
                 → 返回 { device: { deviceId: "rand_xyz" }, token: "tok_new" }
       ↓
客户端保存: device_v2.json → { device_id: "rand_xyz", device_token: "tok_new", ... }
                     + 删除 device.json（旧格式清理）
```

### 迁移示例

```
升级前：
  device.json: { device_id: "sha256_abc...", device_token: "tok_old", ... }
  Server DB:   device (device_id="sha256_abc...", token="tok_old", workspace关联...)

升级后首次启动：
  1. LoadDevice → 查 device_v2.json（不存在）→ 查 device.json → 得到旧哈希 "sha256_abc..."
  2. deviceV2FileExists() = false → 需迁移
  3. GenerateMachineID() = "rand_xyz..." (crypto/rand)
  4. POST /devices { deviceId: "rand_xyz", legacyDeviceId: "sha256_abc" }
  5. Server: migrateDeviceID → UPDATE device SET device_id="rand_xyz", token="tok_new"
  6. 返回 { device: { deviceId: "rand_xyz" }, token: "tok_new" }
  7. SaveDevice → device_v2.json: { device_id: "rand_xyz", device_token: "tok_new", ... }
  8. ClearDevice → 删除旧 device.json

第二次启动（及以后）：
  1. LoadDevice → 查 device_v2.json（存在）→ 直接返回随机 ID
  2. auth() 校验用户凭证 → 通过
  3. 返回 existing
  4. 正常使用
```

### 兼容性

| 场景 | 行为 | 影响 |
|------|------|------|
| 稳定机器升级（旧 device.json） | 一次迁移到 device_v2.json，获得新随机 ID | ✅ 消除碰撞 |
| 笔记本电脑升级（MAC 变化） | v2 文件检测 → 从 device.json 读取旧哈希 → 正常迁移 | ✅ 设备被正确迁移 ✅ |
| 已迁移设备再次启动 | device_v2.json 存在 → auth 校验通过 → 返回 | ✅ |
| 已迁移设备凭证过期 | device_v2.json 存在 → auth 校验失败 → 返回 error → 触发重新登录 | ✅ 身份安全 |
| 全新安装（无任何设备文件） | 随机 ID 写入 device_v2.json + 旧哈希作为 legacyDeviceId | ✅ 新设备附加上一次迁移能力 |
| Server 上旧记录属不同用户 | `createDeviceFromLegacyConflict` → 创建新设备 | ✅ 安全隔离 |
| Server 上旧记录属同一用户 | `migrateDeviceID` → 更新 device_id | ✅ |
| Workspace 关联 | 通过 Device UUID 主键关联，迁移不影响 | ✅ |

---

## 机制二：跨 Gateway 设备接管

### 问题

克隆 A 和克隆 B 持有相同 `device.json`（相同 `device_id` 和 `device_token`）。当 B 上线时，服务器需要：

1. 确保 A 不再处理设备数据（单点写入）
2. 不产生错误的状态转换（如误将 B 的连接标记为 A 离线）

### 改动

#### 1. Gateway 侧 — TunnelManager

**`gateway/internal/manager.go`**：引入 `managedSession` 结构体，`Register()` 返回 `connID`，新增 `CloseIfConnID()` 方法。

```go
type managedSession struct {
    session *yamux.Session
    connID  string
}

type TunnelManager struct {
    mu       sync.RWMutex
    sessions map[string]*managedSession
}

// Register 创建新隧道 session，生成唯一 connID。
// 如果已有旧 session 存在，关闭旧 session（同 gateway 替换）。
// 返回 connID 供调用方传递给 NotifyOnline。
func (m *TunnelManager) Register(deviceID string, session *yamux.Session) string {
    connID := nextConnID()
    m.mu.Lock()
    defer m.mu.Unlock()
    if old, ok := m.sessions[deviceID]; ok {
        old.session.Close()
    }
    m.sessions[deviceID] = &managedSession{session: session, connID: connID}
    return connID
}

// CloseIfConnID 仅当 connID 匹配时关闭 session。
// connID 为空时无条件关闭（向后兼容）。
func (m *TunnelManager) CloseIfConnID(deviceID, connID string) bool {
    m.mu.Lock()
    defer m.mu.Unlock()
    ms, ok := m.sessions[deviceID]
    if !ok {
        return false
    }
    if connID != "" && ms.connID != connID {
        return false
    }
    ms.session.Close()
    delete(m.sessions, deviceID)
    return true
}

// UnregisterIf 仅当 session 匹配时移除并返回 true。
func (m *TunnelManager) UnregisterIf(deviceID string, session *yamux.Session) bool {
    m.mu.Lock()
    defer m.mu.Unlock()
    if ms, ok := m.sessions[deviceID]; ok && ms.session == session {
        ms.session.Close()
        delete(m.sessions, deviceID)
        return true
    }
    return false
}
```

**意义**：

- **`UnregisterIf`**：当 session 关闭时，只有**当前活跃的 session** 才发送 `NotifyOffline`。如果 session 已被新连接替换（`Register` 中关闭旧 session），旧 session 的 close 事件不会触发离线通知。
- **`CloseIfConnID`**：跨 gateway close 请求携带 connID，只有 connID 匹配的 session 才被关闭。防止"close 请求到达前设备已闪断重连，误杀新 session"的竞态。
- **`connID`**：通过原子递增计数器生成，每次 `Register` 生成唯一值，贯穿 Register → NotifyOnline → BindDevice → close 请求 → CloseIfConnID 完整链路。

#### 2. Gateway 侧 — TunnelHandler

**`gateway/internal/tunnel_handler.go`**：

```go
// DeviceTunnelHandler — 连接建立时
connID := manager.Register(deviceID, session)

go func() {
    NotifyOnline(cfg.ServerURL, cfg.GatewayID, deviceID, connID, cfg.InternalSecret)
}()

// 连接结束时
if manager.UnregisterIf(deviceID, session) {
    // 只有当前活跃 session 关闭才发离线通知
    go NotifyOffline(...)
}

// DeviceCloseHandler — 由 API Server 调用，关闭指定设备的 session
// 请求体可包含 connID，确保只关闭匹配的 session（防止误杀重连后的新 session）
func DeviceCloseHandler(manager *TunnelManager) gin.HandlerFunc {
    return func(c *gin.Context) {
        deviceID := c.Param("deviceID")
        var body struct {
            ConnID string `json:"connID"`
        }
        _ = c.ShouldBindJSON(&body)
        manager.CloseIfConnID(deviceID, body.ConnID)
        c.JSON(http.StatusOK, gin.H{"success": true})
    }
}
```

#### 3. Gateway 侧 — Router

**`gateway/internal/router.go`**：新增路由

```go
r.POST("/internal/device/:deviceID/close",
    InternalSecretAuth(cfg.InternalSecret),
    DeviceCloseHandler(manager))
```

#### 4. Server 侧 — Store 接口

**`server/internal/gateway/store.go`**：`BindDevice()` 新增 `connID` 参数，返回旧 gateway ID 和旧 connID。新增 `GetGateway()` 方法。

```go
type Store interface {
    // ... 新增 GetGateway，O(1) 单条查询替代 ListGateways 遍历
    GetGateway(gatewayID string) (*GatewayInfo, error)

    // connID 参数 + 返回旧 connID，用于跨 gateway close 请求的身份链
    BindDevice(deviceID, gatewayID, connID string) (oldGatewayID, oldConnID string, err error)
    // ...
}
```

**`server/internal/gateway/store_redis.go`**：Redis 实现使用 **Lua 脚本**原子完成 read-then-write，消除 HGet+HSet 之间的竞态窗口。新增 `device:connID` Redis hash key 存储 connID 映射。

```go
var bindDeviceScript = redis.NewScript(`
local oldGw = redis.call('HGET', KEYS[1], ARGV[1])
if oldGw == false then oldGw = '' end
local oldConn = redis.call('HGET', KEYS[2], ARGV[1])
if oldConn == false then oldConn = '' end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return {oldGw, oldConn}
`)
```

**`server/internal/gateway/store.go`（MemoryStore）**：在覆盖前读取已有映射，同步维护 `deviceConnID` map。

#### 5. Server 侧 — GatewayRegistry

**`server/internal/gateway/registry.go`**：

```go
// BindDevice 返回旧 gateway ID 和旧 connID
func (r *GatewayRegistry) BindDevice(deviceID, gatewayID, connID string) (string, string) {
    oldGw, oldConn, err := r.store.BindDevice(deviceID, gatewayID, connID)
    if err != nil {
        logger.Error("[GatewayRegistry] BindDevice failed: %v", err)
        return "", ""
    }
    return oldGw, oldConn
}

// GetGatewayInfo 使用 store.GetGateway() O(1) 查询，替代 O(n) 遍历
func (r *GatewayRegistry) GetGatewayInfo(gatewayID string) *GatewayInfo {
    gw, err := r.store.GetGateway(gatewayID)
    if err != nil {
        return nil
    }
    return gw
}

// GetDeviceGatewayID 返回设备当前绑定的 gateway ID（轻量查询，用于 Offline 归属校验）
func (r *GatewayRegistry) GetDeviceGatewayID(deviceID string) string {
    gwID, err := r.store.GetDeviceGateway(deviceID)
    if err != nil {
        return ""
    }
    return gwID
}
```

#### 6. Server 侧 — DeviceOnlineHandler

**`server/internal/gateway/handlers.go`**：接收 connID，绑定后通知旧 gateway 关闭时携带旧 connID。使用带 5s 超时的 `closeHTTPClient` 替代 `http.DefaultClient`。

```go
var closeHTTPClient = &http.Client{Timeout: 5 * time.Second}

func DeviceOnlineHandler(registry *GatewayRegistry, client *Client, deviceSvc *services.DeviceService) gin.HandlerFunc {
    return func(c *gin.Context) {
        var body struct {
            DeviceID  string `json:"deviceID" binding:"required"`
            GatewayID string `json:"gatewayID" binding:"required"`
            ConnID    string `json:"connID"`
        }
        // ...

        oldGwID, oldConnID := registry.BindDevice(body.DeviceID, body.GatewayID, body.ConnID)
        _ = deviceSvc.SetOnline(body.DeviceID)

        if oldGwID != "" && oldGwID != body.GatewayID {
            if oldGw := registry.GetGatewayInfo(oldGwID); oldGw != nil {
                go func() {
                    closeURL := fmt.Sprintf("%s/internal/device/%s/close", oldGw.InternalURL, body.DeviceID)
                    closeBody, _ := json.Marshal(map[string]string{"connID": oldConnID})
                    req, _ := http.NewRequest(http.MethodPost, closeURL, bytes.NewReader(closeBody))
                    req.Header.Set("Content-Type", "application/json")
                    req.Header.Set("X-Internal-Secret", client.InternalSecret())
                    resp, err := closeHTTPClient.Do(req)
                    // ...
                }()
            }
        }
        // ...
    }
}
```

#### 7. Server 侧 — DeviceOfflineHandler

**`server/internal/gateway/handlers.go`**：unbind 前校验设备当前是否绑定在本 gateway，防止行为异常的 gateway 恶意下线不属于它的设备。

```go
func DeviceOfflineHandler(registry *GatewayRegistry, deviceSvc *services.DeviceService) gin.HandlerFunc {
    return func(c *gin.Context) {
        // ...
        // 只有设备当前绑定在本 gateway 时才 unbind
        currentGwID := registry.GetDeviceGatewayID(body.DeviceID)
        if currentGwID != body.GatewayID {
            c.JSON(http.StatusOK, gin.H{"success": true})
            return
        }
        registry.UnbindDevice(body.DeviceID)
        _ = deviceSvc.SetOffline(body.DeviceID)
        // ...
    }
}
```

#### 8. Server 侧 — Client & main.go

**`server/internal/gateway/client.go`**：新增 `InternalSecret()` 公开方法。

**`server/cmd/api/main.go`**：更新 `RegisterInternalRoutes` 调用签名，传递 `gatewayClient`。

### 接管流程

```
克隆 A (已连接)
  └─ GatewayA:   yamux session (deviceX, connID=1)
  └─ Server:     device:gateway → { deviceX: GatewayA }, device:connID → { deviceX: 1 }

克隆 B (启动)
  └─ GatewayAssign → 分配到 GatewayB
  └─ WebSocket 连接 → GatewayB

GatewayB:
  └─ DeviceTunnelHandler
     └─ 验证 deviceX 的 token → 有效
     └─ 创建 yamux session
     └─ manager.Register(deviceX, session) → connID=2
     └─ POST /internal/gateway/device/online
        Body: { deviceID: "deviceX", gatewayID: "GatewayB", connID: "2" }

Server:
  └─ DeviceOnlineHandler
     └─ BindDevice("deviceX", "GatewayB", "2")
        └─ 返回 oldGwID = "GatewayA", oldConnID = "1"
     └─ oldGwID ≠ "" 且 oldGwID ≠ GatewayB
        └─ GetGatewayInfo("GatewayA") → { InternalURL: "http://gateway-a:8081" }
        └─ POST http://gateway-a:8081/internal/device/deviceX/close
           Header: X-Internal-Secret: xxx
           Body: { "connID": "1" }

GatewayA:
  └─ DeviceCloseHandler
     └─ manager.CloseIfConnID("deviceX", "1")
        └─ sessions["deviceX"].connID == "1"? → YES → 关闭 session
        └─ （若 A 已闪断重连为 connID=3，则 1 ≠ 3 → 不关闭 ✅）

克隆 A:
  └─ yamux session 断开
  └─ 尝试重连
      └─ 重新走 GatewayAssign → 可能分配到 GatewayA 或 GatewayB
      └─ 结果：新连接会再次踢掉旧连接（如果克隆 B 还在线）
```

---

## 场景覆盖分析

### 新克隆机器（无 device.json）

| 步骤 | 事件 | 结果 |
|------|------|------|
| 1 | 克隆机器首次启动 | 无 device.json |
| 2 | `Register()` → `GetDeviceID()` 回退到 `GenerateMachineID()` | `crypto/rand` 生成 64 位随机 hex |
| 3 | `SaveDevice()` 写入 device_v2.json | 持久化随机 ID |
| 4 | `Register()` → `POST /devices` | 新 `device_id`，无冲突 |
| **结论** | 不同克隆自动获得不同 ID | ✅ |

### 共享 device.json 的克隆（同时在线）

| 步骤 | 事件 | 结果 |
|------|------|------|
| 1 | Clone A 持有 device.json，已连接 | session(deviceX) on GatewayA |
| 2 | Clone B 启动（共享磁盘/cow 快照） | 读取相同 device.json → 相同 device_id |
| 3 | Clone B 连接 GatewayB | WebSocket 升级成功（token 有效） |
| 4 | GatewayB 发 NotifyOnline（携带 connID） | 服务器绑定 deviceX → GatewayB |
| 5 | Server 发现旧绑定 GatewayA | 通知 GatewayA 关闭 session（携带旧 connID） |
| 6 | Clone A session 断开 | 数据写入停止，触发重连 |
| **结论** | 后启动的克隆踢掉先启动的，单点在线 | ✅ |

### 共享 device.json 的克隆（错开时间）

| 步骤 | 事件 | 结果 |
|------|------|------|
| 1 | Clone A 持有 device.json，工作 2 小时后关机 | NotifyOffline → device 标记离线 |
| 2 | Clone B 启动 | 读取相同 device.json |
| 3 | Clone B 注册/连接 | BindDevice → 无旧绑定 → 正常上线 |
| **结论** | 服务器无法区分"同一设备重新上线"和"克隆机器上线"，但**数据一致性无影响**（无并行写入） | ⚠️ 接受 |

### 正常设备崩溃重启

| 步骤 | 事件 | 结果 |
|------|------|------|
| 1 | 设备在线，WebSocket 意外断开 | Gateway 检测到连接关闭 |
| 2 | Gateway: `UnregisterIf(deviceX, session)` | 返回 true（仍是活跃 session） |
| 3 | Gateway: `NotifyOffline` | 设备标记离线 |
| 4 | 设备快速重启，重连（可能连到不同 gateway） | 新 NotifyOnline → 正常绑定 |
| 5 | 旧 session 已关闭 | 服务器发现旧 gateway 无此 session（Close 是幂等的） |
| **结论** | 正常流程，无副作用 | ✅ |

### Gateway 宕机

| 步骤 | 事件 | 结果 |
|------|------|------|
| 1 | GatewayA 宕机 | 心跳超时 |
| 2 | Server 清理：`RemoveGatewayWithDevices("GatewayA")` | 解除所有设备绑定 + 发 `onDevicesOffline` |
| 3 | 设备检测到 WebSocket 断开 | 重试 `GatewayAssign` |
| 4 | 设备重新连接 GatewayB | 新 NotifyOnline → BindDevice：无旧绑定（已清理） |
| 5 | 旧 GatewayA 已宕机 | 不会误触发 `/close` 请求（GetGatewayInfo 返回 nil） |
| **结论** | 宕机场景安全 | ✅ |

---

## 迁移分析

**本次重构涉及数据迁移。** 旧设备的确定性哈希 ID 需要通过 Server 端的 `migrateFromLegacyDeviceID` 路径迁移为随机 ID。

### 迁移触发条件

迁移由 cs-cloud 客户端的 `Register()` 方法通过文件存在性判断：

```
LoadDevice() 返回非 nil 且 deviceV2FileExists() = false?
  ├─ 是 → 触发 migrateDeviceID()
  │      ├─ auth()（失败 → 返回 error，触发重新登录）
  │      ├─ 从 device.json 读出旧 ID（文件原始值，非重算）
  │      ├─ 用 crypto/rand 生成新 ID
  │      ├─ enroll(deviceId=newID, legacyDeviceId=oldID)
  │      ├─ 成功 → SaveDevice 到 device_v2.json
  │      │      → ClearDevice() 删除旧 device.json
  │      └─ 失败 → auth 错误? → 返回 existing + error（触发登录）
  │               瞬态错误? → log warning → 返回 existing（降级，下次重试）
  │
  └─ 否（v2 存在或设备不存在）→ 跳过迁移
```

关键变化：**不再需要 `GenerateOldMachineID()` 比对。** 只要 `device_v2.json` 不存在而 `device.json` 存在，就说明这是一台需要迁移的旧设备。旧 ID 直接从 `device.json` 读取，不受 MAC 变化影响。

### 迁移流程

```
客户端                                Server
  │                                    │
  ├─ LoadDevice() → 从 device.json      │
  │  读到旧哈希 ID                       │
  ├─ deviceV2FileExists() = false → 需迁移 │
  ├─ auth() 校验用户凭证                 │
  ├─ 生成新随机 ID                     │
  ├─ POST /devices ──────────────────► │
  │  { deviceId: "rand_new",          │
  │    legacyDeviceId: "sha256_old" }  │
  │                                    ├─ RegisterDevice()
  │                                    ├─ deviceId "rand_new" 不存在
  │                                    ├─ legacyDeviceId "sha256_old" → 找到旧记录
  │                                    ├─ migrateDeviceID: UPDATE device_id
  │                                    ├─ 生成新 token
  │                                    ├─ 返回新 device 信息 ──────────►
  │◄───────────────────────────────────┤
  ├─ SaveDevice → device_v2.json       │
  ├─ ClearDevice → 删除 device.json    │
  │                                    │
  此后 LoadDevice() 从 device_v2.json   │
  读取，迁移完成                         │
```

### 迁移的不可逆性

一旦迁移，`device_v2.json` 存储随机 ID，`device.json` 被删除。Server 上的旧 `device_id` 被 UPDATE 为新值。**cs-cloud 二进制回滚后**，旧代码读取 `device.json`（已不存在）→ 重新用哈希计算 ID → 与 Server 上的新 `device_id` 不匹配 → 设备无法通过 token 验证 → 需重新注册 → 产生新设备记录。回滚后 workspace 关联不受影响（通过 Device UUID 关联）。

### 迁移安全边界

| 场景 | 行为 | 数据安全 |
|------|------|---------|
| 迁移成功 | device_v2.json 写入新 ID + 删除旧 device.json + Server 数据更新 | ✅ |
| 迁移失败（auth 失败） | 返回 error → 上层触发重新登录 → 登录后重试迁移 | ⚠️ 设备启动暂停，需重新登录 |
| 迁移失败（enroll auth 错误） | 返回 existing + error → 触发重新登录 | ⚠️ 设备启动暂停，需重新登录 |
| 迁移失败（瞬态错误，如 Server 500） | 返回现有 device.json 数据，下次重试 | ✅ 设备继续工作 |
| 迁移中 Server 旧记录属不同用户 | `createDeviceFromLegacyConflict` 创建新设备 | ✅ 数据隔离 |
| Workspace 关联 | 通过 Device UUID 主键关联 | ✅ 不受影响 |
| 回滚二进制 | 旧代码读不到 device_v2.json → 用哈希注册 → 新设备记录 | ⚠️ 需重新注册 |

---

### 新增端点

#### Gateway 侧

| 方法 | 路径 | 认证 | 描述 |
|------|------|------|------|
| POST | `/internal/device/:deviceID/close` | `X-Internal-Secret` | 关闭指定设备的隧道 session |

请求示例：

```
POST /internal/device/deviceX/close HTTP/1.1
X-Internal-Secret: shared-secret
Content-Type: application/json

{"connID": "1"}
```

响应：

```json
{"success": true}
```

### 内部接口变更

#### Store.BindDevice

| 版本 | 签名 | 说明 |
|------|------|------|
| 改前 | `BindDevice(deviceID, gatewayID string) error` | 覆盖旧绑定，丢失旧 gateway 信息 |
| 改后 | `BindDevice(deviceID, gatewayID, connID string) (oldGatewayID, oldConnID string, err error)` | 原子操作返回旧 gateway ID 和旧 connID |

#### Store.GetGateway（新增）

| 签名 | 说明 |
|------|------|
| `GetGateway(gatewayID string) (*GatewayInfo, error)` | O(1) 单条查询，替代 ListGateways 遍历 |

---

## 数据流设计

### 跨 Gateway 接管时序

```
Clone B                 GatewayB                Server                  GatewayA                Clone A
  │                       │                       │                       │                       │
  ├─ ws tunnel ──────────►│                       │                       │                       │
  │                       ├─ Register → connID=2  │                       │                       │
  │                       ├─ NotifyOnline ───────►│                       │                       │
  │                       │  {deviceID, gwID,     │                       │                       │
  │                       │   connID:"2"}         │                       │                       │
  │                       │                       ├─ BindDevice ──┐       │                       │
  │                       │                       │  (Lua atomic) │       │                       │
  │                       │                       │◄─ old=GwA,    ┘       │                       │
  │                       │                       │   oldConn=1          │                       │
  │                       │                       │                       │                       │
  │                       │                       ├─ GetGateway(GwA)     │                       │
  │                       │                       │◄─ {InternalURL}       │                       │
  │                       │                       │                       │                       │
  │                       │                       ├─ POST /device/close ──►│                       │
  │                       │                       │  Body: {connID:"1"}   │                       │
  │                       │                       │  (5s timeout)         ├─ CloseIfConnID ──────►│
  │                       │                       │                       │  connID 1==1? YES     │
  │                       │                       │                       │                 session 断开
  │                       │◄── {success} ─────────┤                       │◄───── 断开确认 ───────┤
  │◄── tunnel ok ─────────┤                       │                       │                       │
```

### NotifyOffline 防误报

```
时序：             Register(deviceX, sessionB, connID=2)
                    │
sessionA close ─────┤
                    │
                    ▼
              sessions["deviceX"] = {sessionB, connID=2}

sessionA 的 goroutine:
  ←CloseChan()
  UnregisterIf("deviceX", sessionA)
    └─ sessions["deviceX"].session == sessionA?  →  NO (现在是 sessionB)
    └─ return false
  → 不发送 NotifyOffline ✅
```

### DeviceOfflineHandler 防误下线

```
时序：             Device 已从 GatewayA 重连到 GatewayB
                  device:gateway → { deviceX: GatewayB }

GatewayA 残留的 NotifyOffline:
  DeviceOfflineHandler
    ├─ GetDeviceGatewayID("deviceX") → "GatewayB"
    ├─ "GatewayB" != body.GatewayID("GatewayA") → 不匹配
    └─ 直接返回 success，不 unbind ✅
```

---

## 被否决的方案

### 方案一：基于用户身份的克隆识别

在 `GatewayAssignHandler` 中增加用户 session 认证，比对 `device_token` 对应的注册用户和当前登录用户：

```
POST /cloud/device/gateway-assign
Authorization: Bearer <用户 B 的 session token>
Body: { deviceID: "abc", token: "device_token" }

Server:
  device_token 查到的 user_id = "A"
  session 中的 user_id = "B"
  A ≠ B → 判定克隆 → 通知客户端重新生成 device_id
```

**否决原因**：

1. **有盲区**：只覆盖"克隆机器上登录了不同用户"的子集，同用户克隆场景无法检测
2. **信息论上不可区分**：当克隆 A 离线后克隆 B 上线，服务器没有"这是克隆而不是同一台机器重启"的任何依据
3. **误判代价高**：设备崩溃快速重启场景，如果旧 gateway session 还没清理，服务器误判为克隆→通知客户端重新生成 device_id→丢失所有 workspace 关联

### 方案二：客户端侧重试 + 用户确认

设备被踢后不自动重连，弹出提示让用户确认是否生成新身份。

**否决原因**：这是体验优化而非数据安全方案，且在无人值守场景（device agent）下不可行。可以作为未来迭代，但不替代核心互踢逻辑。

---

## 验证

### 验证方法

1. **单元测试**（cs-cloud）：

```bash
cd D:\DEV\cs-cloud && go test ./internal/device/ -v
```

覆盖用例：
- `TestGetDeviceID_ReadsFromStoredDeviceJson` — device_v2.json 优先，回退到 device.json
- `TestLoadDevice_TrustsStoredID` — LoadDevice 信任已存储的 ID
- `TestSaveDevice_PreservesDeviceID` — SaveDevice 不覆写 ID
- `TestSaveDevice_LoadDevice_Roundtrip` — 读写回环一致性
- `TestGenerateOldMachineID_Deterministic` — 旧哈希函数确定性
- `TestGenerateOldMachineID_DifferentFromRandom` — 旧哈希与新随机 ID 不同

2. **单元测试**（server + gateway）：

```bash
cd D:\DEV\costrict-web && go test ./server/internal/gateway/...
cd D:\DEV\costrict-web/gateway && go test ./...
```

3. **编译验证**：

```bash
cd D:\DEV\costrict-web && go build ./server/cmd/api/
cd D:\DEV\costrict-web/gateway && go build ./...
cd D:\DEV\cs-cloud && go build ./...
```

4. **代码静态分析**：

```bash
go vet ./server/internal/gateway/...
go vet ./gateway/internal/...
```

### 验证结果

- 构建：全部通过
- 测试：全部通过
- Vet：无警告
- 调用链分析：所有调用方签名匹配
- 并发场景探测：`BindDevice`（Lua 原子）/`UnregisterIf`/`CloseIfConnID` 在竞态下的行为符合预期

### 场景验证矩阵

以下场景应作为集成测试用例覆盖，按风险等级排序。

#### P0 — 核心正确性（必须验证）

| 编号 | 场景 | 前置条件 | 操作 | 预期结果 | 风险点 |
|------|------|---------|------|---------|--------|
| V001 | 跨 gateway 接管 | 设备 A 在 GatewayA 在线 | 设备 B（同 device.json）连 GatewayB | A 的 session 被关闭（CloseIfConnID 匹配），B 正常在线 | 接管链路完整 |
| V002 | 同 gateway 接管 | 设备 A 在 GatewayA 在线 | 设备 B（同 device.json）连 GatewayA | Gateway 的 `Register` 关闭旧 session，两个 NotifyOnline 合并成一次 BindDevice | 同一 gateway 内不触发跨 gateway 关闭 |
| V003 | 接管后 NotifyOffline 不误报 | 设备 A 在 GatewayA，被 B 接管 | A 的旧 session 关闭 goroutine 执行 `UnregisterIf` | `UnregisterIf` 返回 false，不发送 NotifyOffline | 数据流中的防误报逻辑 |
| V004 | device_v2.json 权威性 | device_v2.json 已存在有 `device_id="X"` | 调用 `GetDeviceID()` | 返回 `"X"`，不调用 `GenerateMachineID()` | 身份持久化 |
| V005 | 首次启动生成随机 ID | 无 device.json | 调用 `GetDeviceID()` | 返回 64 位 hex，每次运行不同 | 随机性 |

#### P1 — 边界条件（重要）

| 编号 | 场景 | 前置条件 | 操作 | 预期结果 | 风险点 |
|------|------|---------|------|---------|--------|
| V006 | 旧 gateway 关闭请求超时 | A 在 GatewayA，B 连 GatewayB，GatewayA 已宕机 | Server 向 GatewayA 发 `/close` | `closeHTTPClient`（5s timeout）超时，但 Server 已绑定到 B，B 不受影响 | 旧 gateway 不可达时不阻塞 |
| V007 | 旧 gateway 的 close 返回非 200 | A 在 GatewayA，B 连 GatewayB，GatewayA 异常 | Server 通知 GatewayA 关闭，返回 500 | Server log warning，B 不受影响 | 非 200 响应不中断流程 |
| V008 | 旧 gateway 的 InternalURL 不可达（网络分区） | A 在 GatewayA，B 连 GatewayB，GatewayA 的网络隔离 | Server 通知 GatewayA 关闭 | DNS 解析失败或 TCP 连接失败，Server log warning，B 不受影响 | 网络分区不阻塞接管 |
| V009 | 并发同时接管（双克隆同时上线） | 克隆 A、B 同时启动，同时连不同 gateway | 两个 NotifyOnline 同时到达 Server | `BindDevice` Lua 脚本原子执行，最终只有一个 session 存活 | 并发安全 |
| V010 | 同一设备快速重连同 gateway | 设备 A 在线，网络闪断（< 1s） | A 重新连同一个 Gateway | Gateway 的 `Register` 关闭旧 session，`UnregisterIf` 返回 false（新 session 已替换），不发送 NotifyOffline | 闪断重连的数据连续性 |
| V030 | device_v2.json 损坏 | device_v2.json 内容为无效 JSON，旧 device.json 已删除 | `LoadDevice()` | 解析失败返回 nil → 走首次注册流程（生成新随机 ID + legacyDeviceId 旧哈希）。**注意：旧 device_id 丢失，Server 上旧记录成为孤儿** | 容错但数据孤儿 |
| V031 | 迁移部分写入（device_v2.json 写成功，device.json 删除失败） | 迁移过程中 `ClearDevice()` 失败（权限/锁） | 下次启动 | `LoadDevice()` 优先读 device_v2.json → 跳过迁移，旧 device.json 残留无害 | 残留文件清理 |
| V032 | 迁移后克隆用旧 token 连接 | A、B 共享同一 device.json；A 升级后迁移成功（获得新 device_id + 新 token），B 仍持旧 device.json（旧 device_id + 旧 token） | B 用旧 device.json 连接 gateway | Server 迁移后旧 device_id 已 UPDATE 为新值 → B 的旧 device_id 在 Server 上不存在 → token 验证失败 → B 需重新注册（注册时 legacyDeviceId 查不到旧记录 → 创建全新设备） | 迁移后克隆隔离 |
| V033 | 活跃 workspace 会话期间迁移 | 设备有活跃的 tunnel/workspace 连接 | `Register()` 触发迁移，device_id 变更 | 迁移后旧 device_id 的路由绑定失效 → 活跃 session 断开或路由错误 | 迁移时机安全性 |
| V034 | MAC 碰撞迁移（原始问题复现） | 两台**不同物理机**因相同 MAC + username 生成相同旧哈希，各自有独立 device.json（相同 device_id） | 两台机器同时从旧 device.json 迁移，legacyDeviceId 相同 | A 先迁移 → Server UPDATE device_id；B 后到 → legacyDeviceId 查不到旧记录 → `createDeviceFromLegacyConflict` 创建新设备 | 碰撞场景的迁移正确性 |

#### P2 — 边缘情况（应覆盖）

| 编号 | 场景 | 前置条件 | 操作 | 预期结果 | 风险点 |
|------|------|---------|------|---------|--------|
| V011 | GatewayA 宕机后设备 A 连 GatewayB | GatewayA 进程崩溃，心跳超时，Server 清理绑定 | 设备 A 重新 AssignGateway 得到 GatewayB，发 NotifyOnline | Server 发现无旧绑定（`BindDevice` 返回空字符串），正常绑定 | Gateway 宕机恢复后无残留 |
| V012 | 接管链环（A→B→C→A） | 三台克隆，三台 gateway | A 连 Gw1→B 连 Gw2→C 连 Gw3→A 再次连 Gw1 | 每次接管正确关闭旧 session，最终只有 A 在线 | 环形切换不产生死锁 |
| V013 | 接管后旧设备重连到旧 gateway | A 被 B 接管，A 自动重连分配回 GatewayA | A 连 GatewayA，发 NotifyOnline | Server 发现 GatewayA 的绑定，但 device 当前绑定 GatewayB → 关闭 GatewayB 上的 B 的 session | 交替接管 |
| V014 | 旧 device.json 升级兼容 | 仅 device.json 存在（旧格式，无 device_v2.json） | `Register()` | `deviceV2FileExists() = false` → 触发迁移，从 device.json 读取旧 ID 发送到 Server | 向后兼容 |
| V015 | device.json 损坏 | device.json 内容为无效 JSON，无 device_v2.json | `Register()` → `LoadDevice()` | `LoadDevice()` 解析失败返回 nil（空的 device.json）→ 走到首次注册流程 | 容错 |
| V016 | 重复 NotifyOnline（幂等） | 设备 A 已在 GatewayA 在线 | Server 再次收到同一 NotifyOnline（device= X, gateway= GatewayA） | `BindDevice` 返回 `"GatewayA"` → `oldGwID == body.GatewayID` → 不触发关闭 | 幂等性 |
| V017 | 重复 NotifyOffline（幂等） | 设备 A 已离线（device:gateway 无绑定） | Gateway 发 NotifyOffline | `GetDeviceGatewayID` 返回空 → `"" != body.GatewayID` → 不 unbind | 幂等性 |
| V018 | Gateway 重启后 session 丢失 | GatewayA 重启，所有 yamux session 丢失 | 设备 WebSocket 断开，重连 GatewayA | 旧 session 在 manager 中已不存在，`Register` 创建新 session | 进程重启不残留 |
| V019 | 设备注册时 device_id 冲突 | 克隆 B 注册到 Server（POST /devices），device_id 已存在 | Server 的 `RegisterDevice` 处理 | 同 user → 返回已有设备；不同 user → 拒绝除非原 user 已删除 | DB 唯一约束 |
| V020 | cs-cloud 旧版本升级后首次运行 | 旧版本 device.json 中 device_id 由哈希生成，无 device_v2.json | `Register()` | `deviceV2FileExists() = false` → 触发迁移，写入 device_v2.json，删除 device.json | 升级迁移 |
| V024 | close 请求携带旧 connID，设备已重连 | A 被 B 接管，A 在原 gateway 闪断重连为新 connID | Server close 请求到达原 gateway，connID 为旧值 | `CloseIfConnID` 比对 connID 不匹配 → 不关闭新 session | close 精准定位 |
| V025 | NotifyOffline 归属不匹配 | 设备已从 GatewayA 重连到 GatewayB | GatewayA 残留发送 NotifyOffline | `GetDeviceGatewayID` 返回 GatewayB ≠ GatewayA → 不 unbind | 防误下线 |
| V026 | 迁移并发（双克隆同时迁移） | 克隆 A、B 共享同一 device.json，同时首次启动 | 两者同时 POST /devices 带 legacyDeviceId | A 先到 → Server 迁移成功（UPDATE device_id）；B 后到 → legacyDeviceId 查不到旧记录 → 创建全新设备（无 workspace 关联）。consume-once 语义保证无数据损坏 | 并发迁移隔离 |
| V027 | 迁移瞬态失败后重试成功 | 旧设备迁移，enroll 返回 500 | 第一次启动：迁移失败 → 降级使用旧 device.json；第二次启动：Server 恢复 → 迁移成功 | 第一次：设备正常工作（旧身份）；第二次：device_v2.json 写入，旧 device.json 删除 | 瞬态降级 + 重试 |
| V028 | 已迁移设备 auth 失败 | device_v2.json 存在，用户 access_token 过期且 refresh_token 失效 | `Register()` → auth 校验 | 返回 error → `registerWithLogin` 捕获 → 弹出浏览器登录 → 重新登录后设备正常启动 | 凭证前置校验 |
| V029 | NotifyOnline 缺少 connID（旧 gateway 兼容） | 旧版本 gateway 未升级，NotifyOnline body 不含 connID 字段 | Server 收到 NotifyOnline，connID 为空 → BindDevice 存储 connID="" → close 请求 body connID="" | `CloseIfConnID` 收到空 connID → 无条件关闭（向后兼容 fallback） | 滚动升级兼容 |
| V035 | 二进制回滚后重新升级（二次迁移） | 设备已迁移（device_v2.json 存在）→ 回滚旧二进制（读不到 device_v2.json，用哈希重新注册，产生新记录）→ 再次升级新二进制 | 新二进制 `Register()` | device_v2.json 存在 → 跳过迁移。但 Server 上可能有多条记录（v2 对应的 + 回滚期间哈希注册的） | 多记录一致性 |
| V036 | Owner 变更后迁移 | 用户 A 的设备已迁移到 device_v2.json；用户 B 在同一台机器登录 | `Register()` → `ClearDeviceV2()` → 首次注册 | 新设备 ID + 用户 B 的 legacyDeviceId（旧哈希）→ Server 查到旧记录属用户 A → `createDeviceFromLegacyConflict` 创建新设备 | 跨用户身份隔离 |
| V037 | Server 重启发生在 BindDevice 与 close 请求之间 | A 在 GatewayA，B 连 GatewayB，BindDevice 成功（Redis 持久化），Server 进程在发 close 请求前重启 | Server 重启后恢复 | Redis 中绑定已持久化 → B 在线正常。GatewayA 上 A 的 session 未被关闭 → A 可能仍在处理数据直到 A 的连接自然断开或下次接管 | Redis 持久化 vs MemoryStore 差异 |
| V038 | NotifyOnline 网络失败（Gateway 与 Server 分区） | B 连上 GatewayB，WebSocket 建立成功 | GatewayB 发 NotifyOnline 到 Server，网络分区 | BindDevice 未执行 → Server 仍认为 A 在 GatewayA → A 不被踢。B 在 GatewayB 上有 session 但 Server 不知道 → B 的请求路由失败 | 分区期间路由不一致 |
| V039 | 磁盘满/IO 错误写 device_v2.json | 旧设备迁移，磁盘空间不足 | `SaveDevice()` 写 device_v2.json 失败 | 迁移中断：Server 已 UPDATE device_id（新值），但本地无 device_v2.json → 下次启动 LoadDevice 读 device.json → 旧 ID 在 Server 上不存在 → **需要重新注册** | Server-本地状态不一致 |
| V040 | MemoryStore 模式下 Server 重启 | Server 使用 MemoryStore（非 Redis），A 在 GatewayA 在线 | Server 重启 | 所有 device:gateway 绑定丢失 → 设备重连时 BindDevice 返回空 oldGwID → 不触发 close → GatewayA 上旧 session 残留 | 内存存储的持久性限制 |

#### 并发安全专项场景

| 编号 | 场景 | 验证要点 |
|------|------|---------|
| V021 | gw1 的 UnregisterIf 与 gw2 的 Register 在 Server 侧并发 | `BindDevice` Lua 脚本原子执行 → 要么 gw1 先绑定（被 gw2 覆盖 + 通知关闭），要么 gw2 先绑定（gw1 绑定被覆盖） |
| V022 | Gateway 的 Register() 与 UnregisterIf() 并发执行 | Register 持锁写入 map，UnregisterIf 持锁读取+删除，两个操作互斥，不会出现双方都以为是活跃 session |
| V023 | Server 的 DeviceOnlineHandler 并发调用 | Store.BindDevice 使用 Lua 脚本原子 read-then-write（Redis）或互斥锁（MemoryStore），后到的覆盖先到的，旧 gateway 关闭非阻塞（goroutine + 5s timeout） |
| V041 | 迁移期间并发接管 | 设备正在执行 `migrateDeviceID()`（POST /devices 进行中），同时另一克隆用旧 device.json 连接 gateway 触发 NotifyOnline → Server 旧 device_id 正在被 UPDATE 的竞态 | 验证 Server 的 `migrateFromLegacyDeviceID` 与 `BindDevice` 是否存在竞态窗口：迁移期间 NotifyOnline 可能绑定到即将被 UPDATE 的旧 device_id → 迁移完成后绑定丢失 |
| V042 | close 请求与设备多级重连的竞态 | B 接管 A → Server 发 close(connID=1) → A 在 close 到达前已重连为 connID=3 → close 到达 → connID 1≠3 不关闭 ✅。但如果 A 重连时又被 C 接管为 connID=4？ | connID 1≠4 仍不关闭（正确），但 connID=4 的 session 也不会被 connID=1 的 close 请求影响。需验证：C 的接管流程会独立发 close(connID=3) → 关闭 A 的 connID=3 session ✅。最终状态：只有 C 在线 |

### 验证脚本

```bash
#!/bin/bash
# 快速验证脚本

echo "=== 1. 编译验证 ==="
cd D:\DEV\costrict-web && go build ./server/cmd/api/ && echo "  server: OK" || echo "  server: FAIL"
cd D:\DEV\costrict-web/gateway && go build ./... && echo "  gateway: OK" || echo "  gateway: FAIL"
cd D:\DEV\cs-cloud && go build ./... && echo "  cs-cloud: OK" || echo "  cs-cloud: FAIL"

echo "=== 2. 单元测试 ==="
cd D:\DEV\costrict-web && go test ./server/internal/gateway/... && echo "  server gateway: OK" || echo "  server gateway: FAIL"
cd D:\DEV\costrict-web/gateway && go test ./... && echo "  gateway: OK" || echo "  gateway: FAIL"
cd D:\DEV\cs-cloud && go test ./internal/device/... && echo "  cs-cloud device: OK" || echo "  cs-cloud device: FAIL"

echo "=== 3. 静态分析 ==="
cd D:\DEV\costrict-web && go vet ./server/internal/gateway/... && echo "  server vet: OK" || echo "  server vet: FAIL"
cd D:\DEV\costrict-web/gateway && go vet ./... && echo "  gateway vet: OK" || echo "  gateway vet: FAIL"
```
