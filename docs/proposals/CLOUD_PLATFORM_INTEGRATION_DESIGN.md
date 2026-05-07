# 云平台集成与环境管理技术提案

## 目录

- [概述](#概述)
- [背景与动机](#背景与动机)
- [设计目标](#设计目标)
- [总体架构](#总体架构)
- [数据模型](#数据模型)
- [Provider 抽象层](#provider-抽象层)
- [深信服托管云 (SCP) Provider](#深信服托管云-scp-provider)
- [配额系统](#配额系统)
- [环境生命周期管理](#环境生命周期管理)
- [设备引导与自动注册](#设备引导与自动注册)
- [API 设计](#api-设计)
- [权限与鉴权](#权限与鉴权)
- [异步任务与 Worker 扩展](#异步任务与-worker-扩展)
- [实施计划](#实施计划)

---

## 概述

### 背景与动机

当前 costrict-cloud 平台已实现设备管理、网关代理、SSE 实时通信等核心能力，设备端（opencode CLI）通过 `cs cloud` 连接到 Server。但现有模型中：

1. **设备由用户自行准备**：用户需要自己准备运行环境（物理机、VM、云主机），手动安装 opencode CLI 并注册设备。
2. **无云平台对接能力**：Server 无法统一管理计算资源，无法为用户按需分配环境。
3. **无配额管理**：无法控制用户可使用的计算资源上限，存在资源滥用风险。
4. **缺乏多平台支持**：无法对接不同的云平台（公有云、私有云、K8s 等）。

本提案引入**云平台集成与环境管理**模块，使平台管理员能够对接外部云平台（优先对接深信服托管云 SCP），配置资源规格，向用户/组织下发配额，让用户可自助创建环境并自动安装 cs cloud 设备。

### 核心流程

```
管理员配置平台 → 定义资源规格 → 下发配额 → 用户自助创建环境 → 自动安装设备 → 设备上线
```

---

## 设计目标

1. **多平台可扩展**：通过 Provider 抽象层支持多种云平台，优先实现 SCP Provider。
2. **平台账号管理**：管理员可配置多个云平台实例及其凭证（AK/SK、API Token 等）。
3. **配额管理**：支持向用户和组织下发 CPU、内存、磁盘、实例数等配额。
4. **自助环境创建**：用户在配额范围内自助选择规格创建云环境。
5. **自动化引导**：环境创建后自动安装 cs cloud 并完成设备注册。
6. **环境全生命周期**：支持创建、启停、删除、快照等操作。
7. **异步任务追踪**：云平台操作多为异步，需统一任务追踪机制。

---

## 总体架构

```
┌──────────────────────────────────────────────────────────────────┐
│                         用户/管理员                                │
│  ┌─────────────────┐  ┌──────────────────┐  ┌────────────────┐  │
│  │ 平台配置管理     │  │ 配额管理          │  │ 环境自助管理    │  │
│  │ (Admin Console)  │  │ (Admin Console)  │  │ (User Portal)  │  │
│  └────────┬────────┘  └────────┬─────────┘  └───────┬────────┘  │
└───────────┼────────────────────┼────────────────────┼────────────┘
            │                    │                    │
┌───────────▼────────────────────▼────────────────────▼────────────┐
│                    costrict-web Server                            │
│                                                                   │
│  ┌──────────────────────────────────────────────────────────┐    │
│  │                Cloud Platform Module                      │    │
│  │                                                           │    │
│  │  ┌──────────────┐  ┌──────────────┐  ┌────────────────┐  │    │
│  │  │ Platform     │  │ Quota        │  │ Environment    │  │    │
│  │  │ Service      │  │ Service      │  │ Service        │  │    │
│  │  └──────┬───────┘  └──────┬───────┘  └───────┬────────┘  │    │
│  │         │                 │                   │           │    │
│  │  ┌──────▼─────────────────▼───────────────────▼────────┐  │    │
│  │  │              Provider Abstraction Layer              │  │    │
│  │  │                                                      │  │    │
│  │  │  ┌──────────┐  ┌──────────┐  ┌──────────┐          │  │    │
│  │  │  │ SCP      │  │ Aliyun   │  │ K8s      │  ...     │  │    │
│  │  │  │ Provider │  │ Provider │  │ Provider │          │  │    │
│  │  │  └──────────┘  └──────────┘  └──────────┘          │  │    │
│  │  └──────────────────────────────────────────────────────┘  │    │
│  └──────────────────────────────────────────────────────────┘    │
│                                                                   │
│  ┌──────────────────┐  ┌──────────────────┐  ┌────────────────┐  │
│  │ Provisioning     │  │ Device Bootstrap │  │ Device         │  │
│  │ Worker           │  │ Service          │  │ Service (已有) │  │
│  └──────────────────┘  └──────────────────┘  └────────────────┘  │
└───────────────────────────────────────────────────────────────────┘
            │
┌───────────▼───────────────────────────────────────────────────────┐
│                      外部云平台                                    │
│  ┌──────────────────┐  ┌──────────────┐  ┌──────────────────┐    │
│  │ 深信服托管云 SCP  │  │ 阿里云/腾讯云 │  │ K8s Cluster      │    │
│  └──────────────────┘  └──────────────┘  └──────────────────┘    │
└───────────────────────────────────────────────────────────────────┘
```

---

## 数据模型

### ER 关系图

```
CloudPlatform ──1:N──> CloudResourceSpec       (平台下的资源规格)
CloudPlatform ──1:N──> CloudEnvironment        (平台上的环境实例)
CloudPlatform ──1:N──> CloudPlatformAZ         (平台可用区缓存)
CloudPlatform ──1:N──> CloudPlatformImage      (平台镜像缓存)

CloudQuota ──N:1──> CloudPlatform              (配额属于某平台)
CloudQuota ──N:1──> User / Organization        (配额分配给用户或组织)

CloudEnvironment ──N:1──> User                 (环境属于某用户)
CloudEnvironment ──1:1──> Device               (环境关联已注册设备)

CloudProvisioningTask ──N:1──> CloudEnvironment (环境的操作任务)
```

### 模型定义

#### CloudPlatform — 云平台配置

```go
type CloudPlatform struct {
    ID           string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    Name         string         `gorm:"not null;uniqueIndex" json:"name"`
    ProviderType string         `gorm:"not null;index" json:"providerType"` // scp | aliyun | k8s | ...
    Description  string         `json:"description"`
    Status       string         `gorm:"not null;default:'active'" json:"status"` // active | inactive | error

    // 连接凭证（加密存储）
    Endpoint     string         `gorm:"not null" json:"endpoint"`     // API 地址
    Credentials  datatypes.JSON `gorm:"type:jsonb;not null" json:"-" swaggerignore:"true"` // 加密凭证

    // Provider 特定配置
    Config       datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"config" swaggertype:"object"`

    // 同步状态
    LastSyncAt   *time.Time     `json:"lastSyncAt"`
    SyncStatus   string         `gorm:"default:'idle'" json:"syncStatus"` // idle | syncing | error
    SyncError    string         `json:"syncError"`

    CreatedBy    string         `gorm:"not null" json:"createdBy"`
    CreatedAt    time.Time      `json:"createdAt"`
    UpdatedAt    time.Time      `json:"updatedAt"`
    DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`
}
```

**凭证结构**（按 Provider 类型不同）：

```go
// SCP (深信服托管云)
type SCPCredentials struct {
    AccessKey string `json:"accessKey"`
    SecretKey string `json:"secretKey"`
    Region    string `json:"region"`    // cn-south-1
    Service   string `json:"service"`   // sdk-api
}

// K8s
type K8sCredentials struct {
    KubeConfig string `json:"kubeConfig"` // base64 编码的 kubeconfig
    Namespace  string `json:"namespace"`
}
```

#### CloudResourceSpec — 资源规格模板

```go
type CloudResourceSpec struct {
    ID           string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    PlatformID   string         `gorm:"not null;index" json:"platformId"`
    Name         string         `gorm:"not null" json:"name"`          // 如 "标准 2C4G"
    Description  string         `json:"description"`
    SpecType     string         `gorm:"not null;default:'vm'" json:"specType"` // vm | container

    // 资源规格
    Cores        int            `gorm:"not null" json:"cores"`          // CPU 核数
    MemoryMB     int            `gorm:"not null" json:"memoryMB"`       // 内存 MB
    DiskMB       int            `gorm:"not null;default:0" json:"diskMB"` // 磁盘 MB
    GPUCount     int            `gorm:"default:0" json:"gpuCount"`

    // Provider 映射（关联云平台的规格标识）
    ProviderSpec datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"providerSpec" swaggertype:"object"`
    // SCP 示例: {"image_id": "...", "az_id": "...", "disks": [...], "networks": [...]}

    // 定价（可选）
    PricePerHour float64        `json:"pricePerHour"`

    IsPublic     bool           `gorm:"default:true" json:"isPublic"`   // 是否公开可用
    Enabled      bool           `gorm:"default:true" json:"enabled"`
    SortOrder    int            `gorm:"default:0" json:"sortOrder"`

    CreatedBy    string         `gorm:"not null" json:"createdBy"`
    CreatedAt    time.Time      `json:"createdAt"`
    UpdatedAt    time.Time      `json:"updatedAt"`
    DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`
}
```

#### CloudQuota — 配额

```go
type CloudQuota struct {
    ID           string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    PlatformID   string    `gorm:"not null;index" json:"platformId"`

    // 配额归属（二选一）
    TargetType   string    `gorm:"not null;index" json:"targetType"` // user | organization
    TargetID     string    `gorm:"not null;index" json:"targetId"`   // UserID 或组织标识

    // 资源上限
    MaxCores     int       `gorm:"not null;default:0" json:"maxCores"`     // CPU 核数上限
    MaxMemoryMB  int       `gorm:"not null;default:0" json:"maxMemoryMB"`  // 内存上限 MB
    MaxDiskMB    int       `gorm:"not null;default:0" json:"maxDiskMB"`    // 磁盘上限 MB
    MaxInstances int       `gorm:"not null;default:0" json:"maxInstances"` // 实例数上限

    // 已使用量（由系统维护）
    UsedCores     int `gorm:"not null;default:0" json:"usedCores"`
    UsedMemoryMB  int `gorm:"not null;default:0" json:"usedMemoryMB"`
    UsedDiskMB    int `gorm:"not null;default:0" json:"usedDiskMB"`
    UsedInstances int `gorm:"not null;default:0" json:"usedInstances"`

    GrantedBy  string     `gorm:"not null" json:"grantedBy"`
    ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
    CreatedAt  time.Time  `json:"createdAt"`
    UpdatedAt  time.Time  `json:"updatedAt"`

    Platform *CloudPlatform `gorm:"foreignKey:PlatformID" json:"platform,omitempty"`
}
```

#### CloudEnvironment — 云环境实例

```go
type CloudEnvironment struct {
    ID           string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    PlatformID   string         `gorm:"not null;index" json:"platformId"`
    UserID       string         `gorm:"not null;index" json:"userId"`

    // 环境信息
    Name         string         `gorm:"not null" json:"name"`
    Description  string         `json:"description"`
    SpecID       string         `gorm:"not null;index" json:"specId"`

    // 云平台实例信息
    ProviderInstanceID string   `gorm:"index" json:"providerInstanceId"` // 云平台返回的 server_id
    ProviderStatus     string   `gorm:"default:'unknown'" json:"providerStatus"` // 云平台原始状态
    IPAddress   string         `json:"ipAddress"`
    VNCURL      string         `gorm:"type:text" json:"vncUrl,omitempty"` // 远程控制台 URL

    // 本地状态
    Status       string         `gorm:"not null;default:'provisioning'" json:"status"`
    // provisioning | running | stopped | terminated | error

    // 资源占用（从 Spec 复制，用于已删除 Spec 后的历史记录）
    Cores        int            `gorm:"not null" json:"cores"`
    MemoryMB     int            `gorm:"not null" json:"memoryMB"`
    DiskMB       int            `gorm:"not null;default:0" json:"diskMB"`

    // 关联设备（环境创建成功后自动注册）
    DeviceID     *string        `gorm:"index" json:"deviceId,omitempty"`

    // 元数据
    Metadata     datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata" swaggertype:"object"`
    ErrorMessage string         `gorm:"type:text" json:"errorMessage,omitempty"`

    CreatedAt    time.Time      `json:"createdAt"`
    UpdatedAt    time.Time      `json:"updatedAt"`
    DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`

    Platform *CloudPlatform    `gorm:"foreignKey:PlatformID" json:"platform,omitempty"`
    Spec     *CloudResourceSpec `gorm:"foreignKey:SpecID" json:"spec,omitempty"`
    Device   *Device           `gorm:"foreignKey:DeviceID" json:"device,omitempty"`
}
```

#### CloudProvisioningTask — 云资源操作任务

```go
type CloudProvisioningTask struct {
    ID             string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    EnvironmentID  string         `gorm:"not null;index" json:"environmentId"`
    TaskType       string         `gorm:"not null" json:"taskType"`
    // create | delete | start | stop | reboot | snapshot_create | snapshot_restore

    Status         string         `gorm:"not null;default:'pending';index" json:"status"`
    // pending | running | success | failed | cancelled

    // 云平台异步任务追踪
    ProviderTaskID string         `gorm:"index" json:"providerTaskId"`
    ProviderStatus string         `json:"providerStatus"` // 云平台任务状态

    // 重试
    RetryCount     int            `gorm:"default:0" json:"retryCount"`
    MaxAttempts    int            `gorm:"default:10" json:"maxAttempts"`
    LastError      string         `gorm:"type:text" json:"lastError"`

    // 输入/输出
    Input          datatypes.JSON `gorm:"type:jsonb" json:"input" swaggertype:"object"`
    Result         datatypes.JSON `gorm:"type:jsonb" json:"result" swaggertype:"object"`

    ScheduledAt    time.Time      `gorm:"not null;index" json:"scheduledAt"`
    StartedAt      *time.Time     `json:"startedAt"`
    FinishedAt     *time.Time     `json:"finishedAt"`
    CreatedAt      time.Time      `json:"createdAt"`

    Environment *CloudEnvironment `gorm:"foreignKey:EnvironmentID" json:"environment,omitempty"`
}
```

---

## Provider 抽象层

### 接口定义

```go
// internal/cloudprovider/provider.go

type ServerSpec struct {
    Name        string
    Cores       int
    MemoryMB    int
    Disks       []DiskSpec
    Networks    []NetworkSpec
    ImageID     string
    AzID        string
    Count       int
    PowerOn     bool
    Description string
    Metadata    map[string]interface{}
}

type DiskSpec struct {
    ID          string // "ide0", "ide1"
    Type        string // "new_disk"
    SizeMB      int
    Preallocate string // "off" = 精简
}

type NetworkSpec struct {
    VpcID    string
    SubnetID string
}

type ServerInfo struct {
    ID         string
    Name       string
    Status     string
    PowerState string
    IPAddress  string
    Cores      int
    MemoryMB   int
    Disks      []DiskInfo
    VNCURL     string
    Metadata   map[string]interface{}
}

type TaskInfo struct {
    ID     string
    Status string // pending | running | success | failure
    Result map[string]interface{}
}

type ImageInfo struct {
    ID         string
    Name       string
    OSType     string
    DiskFormat string
}

type AZInfo struct {
    ID   string
    Name string
    Type string // public | private
}

type VPCInfo struct {
    ID   string
    Name string
    AzID string
}

type SubnetInfo struct {
    ID        string
    Name      string
    VpcID     string
    AzID      string
    CIDR      string
    Gateway   string
}

type CloudProvider interface {
    // 生命周期
    CreateServer(ctx context.Context, spec *ServerSpec) (taskID string, err error)
    GetServer(ctx context.Context, serverID string) (*ServerInfo, error)
    DeleteServer(ctx context.Context, serverID string, force bool) (taskID string, err error)
    StartServer(ctx context.Context, serverID string) (taskID string, err error)
    StopServer(ctx context.Context, serverID string, force bool) (taskID string, err error)
    RebootServer(ctx context.Context, serverID string, force bool) (taskID string, err error)

    // 远程控制台
    GetRemoteConsole(ctx context.Context, serverID string) (url string, err error)

    // 快照
    CreateSnapshot(ctx context.Context, serverID string, name string) (taskID string, err error)
    RestoreSnapshot(ctx context.Context, serverID, snapshotID string) (taskID string, err error)
    DeleteSnapshot(ctx context.Context, serverID, snapshotID string) (taskID string, err error)
    ListSnapshots(ctx context.Context, serverID string) ([]interface{}, error)

    // 查询
    ListServers(ctx context.Context, params map[string]string) ([]ServerInfo, error)
    ListImages(ctx context.Context, params map[string]string) ([]ImageInfo, error)
    ListAZs(ctx context.Context) ([]AZInfo, error)
    ListVPCs(ctx context.Context, params map[string]string) ([]VPCInfo, error)
    ListSubnets(ctx context.Context, params map[string]string) ([]SubnetInfo, error)

    // 任务追踪
    GetTask(ctx context.Context, taskID string) (*TaskInfo, error)

    // 健康检查
    Ping(ctx context.Context) error
}

// ProviderFactory 根据类型创建 Provider 实例
type ProviderFactory func(endpoint string, credentials json.RawMessage, config json.RawMessage) (CloudProvider, error)

// 全局注册表
var providerRegistry = map[string]ProviderFactory{}

func RegisterProvider(providerType string, factory ProviderFactory) {
    providerRegistry[providerType] = factory
}

func NewProvider(providerType, endpoint string, creds, config json.RawMessage) (CloudProvider, error) {
    factory, ok := providerRegistry[providerType]
    if !ok {
        return nil, fmt.Errorf("unknown provider type: %s", providerType)
    }
    return factory(endpoint, creds, config)
}
```

---

## 深信服托管云 (SCP) Provider

### SDK 结构

```
internal/cloudprovider/
├── provider.go          # 接口定义 + 注册表
├── scp/
│   ├── provider.go      # SCP CloudProvider 实现
│   ├── client.go        # SCP HTTP 客户端
│   ├── signer.go        # AWS4-HMAC-SHA256 签名
│   ├── types.go         # SCP 请求/响应类型
│   └── provider_test.go
```

### 签名实现

直接复用 SCP API 文档中提供的 `Signer` 和 `NewSCPRequest` 实现（参见 `sangfor_cloud_api.md` 第 2.2-2.4 节），封装为 `scp.Client`：

```go
// internal/cloudprovider/scp/client.go

type Client struct {
    Host    string
    Signer  *Signer
    HTTP    *http.Client
}

func NewClient(host, ak, sk, region, service string, insecureSkipVerify bool) *Client {
    // ...
}

func (c *Client) DoRequest(ctx context.Context, method, path string, query url.Values, body interface{}) (*SCPResponse, error) {
    // 构建签名请求、发送、解析响应
    // 幂等 Token 自动生成 UUID
    // 异步操作返回 task_id
}
```

### Provider 实现要点

1. **异步轮询**：SCP 所有写操作返回 `task_id`，Provider 的 `CreateServer` 等方法只返回 `taskID`，由上层 ProvisioningWorker 负责轮询。

2. **TLS 配置**：SCP 可能为自签证书，需支持 `InsecureSkipVerify`（通过 `CloudPlatform.Config` 配置）。

3. **Cookie 头**：每次请求生成随机 `aCMPAuthToken`，参与签名。

4. **资源查询缓存**：镜像、AZ、VPC、子网等不常变的信息定期同步到本地表。

---

## 配额系统

### 配额检查逻辑

```go
// internal/services/cloud_quota_service.go

type QuotaCheckResult struct {
    Allowed    bool
    Exceeded   []string // 超限的资源类型
    Current    CloudQuotaUsage
    Requested  CloudQuotaUsage
}

func (s *CloudQuotaService) CheckQuota(ctx context.Context, platformID, targetType, targetID string, requested CloudQuotaUsage) (*QuotaCheckResult, error) {
    quota, err := s.getQuota(ctx, platformID, targetType, targetID)
    if err != nil {
        return nil, err
    }

    result := &QuotaCheckResult{
        Current: CloudQuotaUsage{
            Cores:     quota.UsedCores,
            MemoryMB:  quota.UsedMemoryMB,
            DiskMB:    quota.UsedDiskMB,
            Instances: quota.UsedInstances,
        },
        Requested: requested,
    }

    if quota.UsedCores + requested.Cores > quota.MaxCores {
        result.Exceeded = append(result.Exceeded, "cores")
    }
    if quota.UsedMemoryMB + requested.MemoryMB > quota.MaxMemoryMB {
        result.Exceeded = append(result.Exceeded, "memory")
    }
    if quota.UsedDiskMB + requested.DiskMB > quota.MaxDiskMB {
        result.Exceeded = append(result.Exceeded, "disk")
    }
    if quota.UsedInstances + requested.Instances > quota.MaxInstances {
        result.Exceeded = append(result.Exceeded, "instances")
    }

    result.Allowed = len(result.Exceeded) == 0
    return result, nil
}
```

### 配额分配策略

- **用户配额**：直接分配给用户，优先消耗。
- **组织配额**：分配给组织（Casdoor Organization），组织内成员共享。
- **查找顺序**：先查用户配额 → 再查用户所属组织配额 → 无配额则拒绝。
- **配额过期**：支持设置 `expiresAt`，过期后配额失效（已创建的环境不受影响，但不允许新建）。

### 配额消耗与回收

```
创建环境 → 扣减配额（used + spec）
删除环境 → 释放配额（used - spec）
环境异常 → 释放配额（后台巡检回收）
```

配额扣减使用数据库事务 + 行锁保证原子性：

```sql
UPDATE cloud_quotas
SET used_cores = used_cores + ?, used_memory_mb = used_memory_mb + ?,
    used_disk_mb = used_disk_mb + ?, used_instances = used_instances + 1
WHERE id = ? AND platform_id = ?
  AND used_cores + ? <= max_cores
  AND used_memory_mb + ? <= max_memory_mb
  AND used_disk_mb + ? <= max_disk_mb
  AND used_instances + 1 <= max_instances
RETURNING id;
```

---

## 环境生命周期管理

### 状态机

```
                    ┌──────────────┐
                    │ provisioning │ ← 创建中
                    └──────┬───────┘
                           │ 创建成功
                    ┌──────▼───────┐
              ┌─────│    running    │─────┐
              │     └──────────────┘     │
              │ 停止                     │ 删除
       ┌──────▼───────┐          ┌──────▼───────┐
       │    stopped    │          │  terminating │
       └──────┬───────┘          └──────┬───────┘
              │ 启动                     │ 删除完成
              └──────────┐       ┌──────▼───────┐
                         │       │  terminated   │
                         └──────►└──────────────┘
                    ┌──────────────┐
                    │    error     │ ← 任何阶段异常
                    └──────────────┘
```

### 创建环境流程

```
1. 用户选择规格，发起创建请求
2. Server 检查配额
3. 创建 CloudEnvironment 记录 (status=provisioning)
4. 创建 CloudProvisioningTask (type=create, status=pending)
5. ProvisioningWorker 拾取任务
6. 调用 Provider.CreateServer() 获取 providerTaskID
7. 轮询 Provider.GetTask() 直到 finish/failure
8. 成功: 获取 server 信息 → 更新 environment (status=running, ipAddress)
        → 扣减配额 → 触发设备引导
9. 失败: 更新 environment (status=error, errorMessage)
        → 清理（若已部分创建则尝试删除）
```

### 设备引导流程（创建成功后）

```
1. 获取环境 IP 地址
2. 通过 SSH（或 cloud-init）连接到新实例
3. 下载并安装 cs cloud（opencode CLI）
4. 配置 server 连接地址 + 注册 token
5. 启动 cs cloud → 设备自动注册到 Server
6. Server 创建 Device 记录，关联到 CloudEnvironment
```

对于 SCP 平台，可通过以下方式实现引导：
- **cloud-init / user-data**：创建 VM 时传入启动脚本（SCP `advance_param` 支持扩展）。
- **SSH 引导**：创建完成后通过 IP + 密钥 SSH 连接执行安装。
- **镜像预装**：制作预装 cs cloud 的镜像，直接使用该镜像创建 VM。

---

## 设备引导与自动注册

### Bootstrap 脚本

```bash
#!/bin/bash
# 由 Server 生成，包含一次性注册 token
CS_CLOUD_SERVER="${SERVER_URL}"
CS_CLOUD_TOKEN="${REGISTRATION_TOKEN}"

# 下载 cs cloud
curl -sL "${CS_CLOUD_SERVER}/releases/latest/download/cs-cloud-linux-amd64" -o /usr/local/bin/cs
chmod +x /usr/local/bin/cs

# 配置并启动
cs config set server.url "${CS_CLOUD_SERVER}"
cs config set device.token "${CS_CLOUD_TOKEN}"
cs cloud start
```

### 注册 Token 预生成

环境创建时，Server 预生成一个带元数据的设备注册 Token（关联 `environment_id`），设备注册时自动完成绑定：

```go
type BootstrapToken struct {
    EnvironmentID string
    PlatformID    string
    ExpiresAt     time.Time
}
```

---

## API 设计

### 平台管理 (`/api/admin/cloud/platforms`) — platform_admin

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/cloud/platforms` | 列出所有平台 |
| POST | `/admin/cloud/platforms` | 创建平台配置 |
| GET | `/admin/cloud/platforms/:id` | 获取平台详情 |
| PUT | `/admin/cloud/platforms/:id` | 更新平台配置 |
| DELETE | `/admin/cloud/platforms/:id` | 删除平台 |
| POST | `/admin/cloud/platforms/:id/test` | 测试平台连通性 |
| POST | `/admin/cloud/platforms/:id/sync` | 同步平台资源（镜像、AZ等） |

### 资源规格 (`/api/admin/cloud/specs`) — platform_admin

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/cloud/platforms/:id/specs` | 列出平台规格 |
| POST | `/admin/cloud/platforms/:id/specs` | 创建规格 |
| PUT | `/admin/cloud/specs/:id` | 更新规格 |
| DELETE | `/admin/cloud/specs/:id` | 删除规格 |

### 配额管理 (`/api/admin/cloud/quotas`) — platform_admin

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/cloud/quotas` | 列出配额（支持过滤） |
| POST | `/admin/cloud/quotas` | 创建/更新配额 |
| GET | `/admin/cloud/quotas/:id` | 配额详情 |
| PUT | `/admin/cloud/quotas/:id` | 更新配额 |
| DELETE | `/admin/cloud/quotas/:id` | 删除配额 |
| GET | `/admin/cloud/quotas/summary` | 配额使用概览 |

### 环境管理 (`/api/cloud/environments`) — 所有认证用户

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/cloud/platforms` | 列出可用平台（公开的） |
| GET | `/cloud/platforms/:id/specs` | 列出可用规格 |
| GET | `/cloud/environments` | 列出我的环境 |
| POST | `/cloud/environments` | 创建环境 |
| GET | `/cloud/environments/:id` | 环境详情 |
| DELETE | `/cloud/environments/:id` | 删除环境 |
| POST | `/cloud/environments/:id/start` | 启动环境 |
| POST | `/cloud/environments/:id/stop` | 停止环境 |
| POST | `/cloud/environments/:id/reboot` | 重启环境 |
| GET | `/cloud/environments/:id/console` | 获取 VNC 控制台 |
| GET | `/cloud/environments/:id/snapshots` | 快照列表 |
| POST | `/cloud/environments/:id/snapshots` | 创建快照 |
| DELETE | `/cloud/environments/:id/snapshots/:sid` | 删除快照 |
| POST | `/cloud/environments/:id/snapshots/:sid/restore` | 恢复快照 |
| GET | `/cloud/quotas/me` | 查看我的配额 |

---

## 权限与鉴权

### 角色权限矩阵

| 操作 | platform_admin | business_admin | 普通用户 |
|------|---------------|----------------|---------|
| 管理 CloudPlatform | ✓ | - | - |
| 管理资源规格 | ✓ | - | - |
| 管理配额 | ✓ | - | - |
| 查看配额概览 | ✓ | ✓ | - |
| 查看自己的配额 | ✓ | ✓ | ✓ |
| 创建环境 | - | - | ✓（配额内） |
| 管理自己的环境 | - | - | ✓ |

### 资源权限注册

在 `resource_permissions` 表中新增：

```sql
INSERT INTO resource_permissions (id, resource_code, resource_type, allowed_roles) VALUES
  ('...', 'admin:cloud:platforms', 'menu', '{platform_admin}'),
  ('...', 'admin:cloud:specs', 'menu', '{platform_admin}'),
  ('...', 'admin:cloud:quotas', 'menu', '{platform_admin}'),
  ('...', 'admin:cloud:quotas:summary', 'menu', '{platform_admin,business_admin}'),
  ('...', 'cloud:environments', 'menu', '{}'),  -- 空数组 = 所有登录用户
  ('...', 'cloud:quotas:me', 'api', '{}');
```

---

## 异步任务与 Worker 扩展

### ProvisioningWorker

在现有 Worker 机制基础上新增 `ProvisioningWorkerPool`：

```go
// internal/worker/cloud_provisioning_worker.go

type ProvisioningWorkerPool struct {
    DB          *gorm.DB
    Providers   *ProviderRegistry
    Concurrency int
    PollInterval time.Duration
}

func (p *ProvisioningWorkerPool) Start(ctx context.Context) {
    for i := 0; i < p.Concurrency; i++ {
        go p.pollLoop(ctx)
    }
}

func (p *ProvisioningWorkerPool) pollLoop(ctx context.Context) {
    ticker := time.NewTicker(p.PollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            p.processNextTask(ctx)
        }
    }
}
```

### 任务处理逻辑

```go
func (p *ProvisioningWorkerPool) processTask(ctx context.Context, task *CloudProvisioningTask) error {
    switch task.TaskType {
    case "create":
        return p.handleCreate(ctx, task)
    case "delete":
        return p.handleDelete(ctx, task)
    case "start":
        return p.handlePower(ctx, task, "start")
    case "stop":
        return p.handlePower(ctx, task, "stop")
    case "reboot":
        return p.handlePower(ctx, task, "reboot")
    }
}
```

每个任务类型的处理分为两步：
1. **发起操作**：调用 Provider API，获取 `providerTaskID`。
2. **轮询结果**：后续 poll 周期中检查 `ProviderTaskID` 状态，更新本地任务。

### 轮询策略

```
任务状态=pending: 调用 Provider API 发起操作
任务状态=running + 有 providerTaskID: 轮询 Provider.GetTask()
  → finish → 更新为 success → 执行后续动作（如设备引导）
  → failure → 重试或标记 failed
  → doing → 等待下次 poll
```

---

## 实施计划

### Phase 1: 基础框架 + SCP Provider（2 周）

| 任务 | 说明 |
|------|------|
| 数据模型 + 迁移 | 创建 `cloud_platforms`、`cloud_resource_specs`、`cloud_quotas`、`cloud_environments`、`cloud_provisioning_tasks` 表 |
| Provider 接口 | 定义 `CloudProvider` 接口和注册表 |
| SCP SDK | 实现 SCP Client（签名、请求构建） |
| SCP Provider | 实现 SCP CloudProvider 接口 |
| 平台管理 API | CRUD + 连通性测试 + 资源同步 |
| 资源规格 API | 规格模板 CRUD |

### Phase 2: 配额 + 环境创建（1.5 周）

| 任务 | 说明 |
|------|------|
| 配额 Service | 配额 CRUD、检查、扣减、释放 |
| 配额管理 API | 管理端配额管理 |
| 环境创建 API | 用户自助创建环境 |
| ProvisioningWorker | 异步任务处理 |
| 环境生命周期 | 启停、删除等操作 |

### Phase 3: 设备引导 + 完善（1.5 周）

| 任务 | 说明 |
|------|------|
| 设备引导 Service | Bootstrap Token 生成、脚本生成 |
| 自动注册集成 | 环境创建成功后自动触发设备注册 |
| 快照管理 | 快照 CRUD + 恢复 |
| VNC 控制台 | 远程控制台代理 |
| 错误处理 | 重试机制、异常恢复、配额泄漏检测 |

### Phase 4: 管理界面 + 监控（1 周）

| 任务 | 说明 |
|------|------|
| 配额概览 | 管理端配额使用统计 |
| 环境监控 | 环境状态巡检、异常告警 |
| 权限集成 | 资源权限注册、菜单控制 |
| Swagger 文档 | API 文档更新 |

### 后续扩展

| Provider | 说明 |
|----------|------|
| K8s Provider | 对接 Kubernetes 集群，创建 Pod/Deployment 作为环境 |
| 阿里云 Provider | 对接阿里云 ECS |
| 通用 Terraform Provider | 通过 Terraform 支持任意云平台 |
