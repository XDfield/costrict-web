# 可扩展运行时体系 DESIGN（机制层）

> 状态：Draft
> 类型：技术方案（机制层）
> 关联产品 PRD：`RUNTIME_PLATFORM_PRD.md`（runtime 子系统产品 PRD）
> 关联业务 PRD：`LIGHTS_OUT_FACTORY_PRD.md`（黑灯工厂业务 PRD）
> 关联提案：`HTTP_TUNNEL_DESIGN.md`、`DEVICE_GATEWAY_DESIGN.md`、`SESSION_PROXY_DESIGN.md`
> 关联项目：`cs-cloud`（桥接层）、`ai-lab`（runtime provider）、`costrict-jwt-swap`（身份桥）

---

## 1. 目标与边界

### 1.1 本文档定位

回答**"如何把多种运行位置（docker / cloud / ailab / device）统一接入 costrict-web 平台"**——这是机制层方案，与业务运营（黑灯工厂、任务调度、自治率等）解耦。

三层文档关系：
- **产品层（`RUNTIME_PLATFORM_PRD.md`）**：runtime platform 作为产品的形态、人 JOB / AI JOB、验收标准
- **机制层（本文档）**：Provider 接口、能力域、数据模型、Provision 流程、Action 路由的工程实现
- **业务层（`LIGHTS_OUT_FACTORY_PRD.md`）**：基于 runtime platform 之上的黑灯工厂运营模式

业务侧诉求（AI 自主调度、池化、自愈等）见 `LIGHTS_OUT_FACTORY_PRD.md`；产品形态定义（runtime platform 给谁用、解决什么问题）见 `RUNTIME_PLATFORM_PRD.md`。本文档提供承载产品的"运行时接入底座"。

### 1.2 核心设计原则

1. **接入方式统一化**：所有 runtime 启动后都通过 cs-cloud 桥接层接入，从平台视角看就是 device
2. **provisioning 多样化**：runtime 启动方式按 provider 差异化，启动后统一
3. **零侵入现有链路**：`devices` 表、gateway 隧道、dispatcher、event router 全部不动
4. **能力按执行域分层**：local（容器内）/ platform（provider 平台）/ hybrid（双域）三域管理

### 1.3 反目标（明确不做）

- ❌ 不重新设计隧道协议——复用 cs-cloud + gateway
- ❌ 不替换 device 模型——runtime 接入后就是 device
- ❌ 不自建云 SDK / Docker 客户端 / Web IDE——用现成的

---

## 2. 架构总览

```
┌──────────────────────────────────────────────────────────┐
│  业务层（LIGHTS_OUT_FACTORY_PRD.md 定义）                  │
│  Factory Scheduler / Pool / Task Template / Workflow      │
└─────────────────────────┬────────────────────────────────┘
                          │
            ┌─────────────▼─────────────┐
            │   Runtime Registry        │  ← 业务层唯一入口
            │   (provider 路由)          │
            └─────────────┬─────────────┘
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
  ┌──────────┐      ┌──────────┐      ┌──────────┐
  │ Docker   │      │ Cloud    │      │ AiLab    │  ...
  │ Provider │      │ Provider │      │ Provider │
  └────┬─────┘      └────┬─────┘      └────┬─────┘
       │ Provision       │ Provision       │ Provision
       ▼                 ▼                 ▼
  ┌──────────────────────────────────────────────┐
  │  运行位置（任意环境）                          │
  │  ┌────────────────────────────────────────┐  │
  │  │  cs-cloud daemon（桥接层，零改动）      │  │
  │  │  - tunnel establish                    │  │
  │  │  - device register                     │  │
  │  │  - heartbeat                           │  │
  │  │  - capability probe (local domain)     │  │
  │  └────────────────────────────────────────┘  │
  └──────────────────────┬───────────────────────┘
                         │
                         │ 隧道（复用现有）
                         ▼
  ┌──────────────────────────────────────────────────────────┐
  │  costrict-web 后端（零改动）                              │
  │  gateway / dispatcher / event router / device_service    │
  └──────────────────────────────────────────────────────────┘
```

**关键洞察**：runtime 体系是叠加层。Provider 负责"启动运行位置 + 把 cs-cloud 装进去"，cs-cloud 启动后走现有隧道协议接入，对后端完全透明。

---

## 3. Provider 抽象

### 3.1 Provider 接口

```go
// internal/runtime/provider.go
package runtime

type Provider interface {
    // 元信息
    Type() string  // "local" | "docker" | "cloud" | "ailab"

    // 生命周期：启动/销毁运行位置
    Provision(ctx context.Context, opts ProvisionOpts) (*ProvisionResult, error)
    Destroy(ctx context.Context, runtimeID string) error
    Status(ctx context.Context, runtimeID string) (Status, error)

    // 能力（platform 域）：声明 + 同步 + 动作处理
    DefaultCapabilities() []capabilities.Ref
    SyncCapabilities(ctx context.Context, runtimeID string) ([]capabilities.Instance, error)
    HandleAction(ctx context.Context, req ActionRequest) (*ActionResult, error)
}

type ProvisionOpts struct {
    UserID      string
    DisplayName string
    RuntimeClass string  // 引用业务层的 runtime class（如 light-crawler / gpu-runner）
    Config      map[string]any
}

type ProvisionResult struct {
    RuntimeID string    // = device.DeviceID
    Bootstrap BootstrapConfig
}

type BootstrapConfig struct {
    CloudBaseURL string
    DeviceToken  string  // 预签发的 device 注册 token
    DeviceID     string
}
```

### 3.2 Provision 流程（统一）

```
1. 调 device_service.PreRegister() 预签发 device_id + token
2. Provider 实现：
   a. 用 provider 特定方式拉起运行位置（docker run / cloud API / ai-lab API）
   b. 把 BootstrapConfig 作为环境变量/启动参数注入
   c. 运行位置的 ENTRYPOINT 启动 cs-cloud（镜像内置）
3. cs-cloud 启动 → 主动连 gateway → register device → 进入 online
4. 写 runtime_sources 记录来源类型 + 元数据
5. （异步）Provider SyncCapabilities 写 platform 域能力
```

### 3.3 四种 Provider 的差异化实现

| Provider | 启动方式 | cs-cloud 如何进入 | 典型能力 |
|----------|----------|-------------------|---------|
| **Local** | 用户在自己机器手动跑 `cs-cloud` | 用户自己装 | exec, web_terminal, file_browser |
| **Docker** | `docker run` 拉起容器 | 镜像 ENTRYPOINT 启动 cs-cloud | lifecycle.*, port_mapping, exec, web_terminal |
| **Cloud** | 调云 API 创建 VM + user-data | cloud-init 装 + 启动 cs-cloud | lifecycle.*, public_endpoint, snapshot, resize, ssh |
| **AiLab** | 调 ai-lab API 创建 CodeBox | CodeBox 镜像内置 cs-cloud | lifecycle.*, port_mapping, web_ide, ssh, file_browser |

**Docker Provider 示例**：

```go
func (p *DockerProvider) Provision(ctx context.Context, opts ProvisionOpts) (*ProvisionResult, error) {
    // 1. 预签发
    device, token := p.deviceSvc.PreRegister(opts.UserID, opts.DisplayName)

    // 2. docker run
    resp, err := p.client.ContainerCreate(ctx,
        &container.Config{
            Image: p.config.Image,  // 内置 cs-cloud 二进制
            Env: []string{
                "CS_CLOUD_BASE_URL=" + p.config.BaseURL,
                "CS_CLOUD_DEVICE_ID=" + device.DeviceID,
                "CS_CLOUD_DEVICE_TOKEN=" + token,
            },
        },
        &container.HostConfig{Resources: resourcesFromConfig(opts.Config)},
        nil, nil, "",
    )
    if err != nil { return nil, err }
    p.client.ContainerStart(ctx, resp.ID, container.StartOptions{})

    // 3. 写 runtime_sources
    p.db.Create(&models.RuntimeSource{
        DeviceID: device.DeviceID,
        SourceType: "docker",
        ExternalID: resp.ID,
        ProviderMeta: toJSON(opts.Config),
    })

    return &ProvisionResult{RuntimeID: device.DeviceID}, nil
}
```

**AiLab Provider 示例**（复用 jwt-swap）：

```go
func (p *AiLabProvider) Provision(ctx context.Context, opts ProvisionOpts) (*ProvisionResult, error) {
    device, token := p.deviceSvc.PreRegister(opts.UserID, opts.DisplayName)

    // 通过 jwt-swap 拿 ai-lab JWT（仅 provisioning 阶段使用）
    jwt := p.jwtSwap.Swap(opts.UserID, p.config.NodeUUID)

    // 调 ai-lab API 创建 CodeBox（镜像内置 cs-cloud）
    codebox := p.httpClient.Post(
        p.config.URL+"/api/v1/codebox",
        map[string]any{
            "name":  opts.DisplayName,
            "image": p.config.CodeBoxImage,
            "env": map[string]string{
                "CS_CLOUD_BASE_URL":      p.config.BaseURL,
                "CS_CLOUD_DEVICE_TOKEN":  token,
            },
        },
        WithBearer(jwt),
    )

    p.db.Create(&models.RuntimeSource{
        DeviceID: device.DeviceID,
        SourceType: "ailab",
        ExternalID: codebox.UUID,
    })
    return &ProvisionResult{RuntimeID: device.DeviceID}, nil
}
```

---

## 4. 数据模型

### 4.1 新增 `runtime_sources` 表（不破坏 `devices`）

```go
// internal/models/runtime_source.go
type RuntimeSource struct {
    ID           string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()"`
    DeviceID     string         `gorm:"uniqueIndex;not null"` // 关联 devices.device_id
    SourceType   string         `gorm:"not null;index"`       // local|docker|cloud|ailab
    ExternalID   string                                       // container_id / instance_id / codebox_uuid
    ProviderMeta datatypes.JSON `gorm:"type:jsonb"`           // 镜像/规格/区域等

    // 能力（双源存储）
    LocalCapabilities    datatypes.JSON `gorm:"type:jsonb"` // cs-cloud 上报
    PlatformCapabilities datatypes.JSON `gorm:"type:jsonb"` // Provider 同步
    LocalSyncedAt    *time.Time
    PlatformSyncedAt *time.Time

    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"`
}
```

### 4.2 现有表零改动

| 表 | 改动 | 说明 |
|----|------|------|
| `devices` | 不动 | runtime 接入后写一条 device 记录 |
| `device_command_results` | 不动 | runtime 收到的命令一样落表 |
| `gateway_*` | 不动 | runtime 走同一隧道 |
| 新增 `runtime_sources` | 1 张 | 元数据 + 能力 |

---

## 5. 能力域（Capability Domain）系统

### 5.1 为什么需要分域

能力的"在哪里执行"决定了：
- **谁发现它**：cs-cloud 探测 vs Provider 平台查询
- **谁执行它**：通过隧道转发 vs API 调用
- **配置存哪里**：容器内 vs 平台元数据

### 5.2 三种执行域

```go
// internal/runtime/capabilities/catalog.go
type Domain string

const (
    DomainLocal    Domain = "local"     // 仅容器内执行
    DomainPlatform Domain = "platform"  // 仅平台层执行
    DomainHybrid   Domain = "hybrid"    // 双域都可执行
)
```

| 域 | 执行者 | 发现方式 | 调用路径 | 典型能力 |
|----|--------|---------|---------|---------|
| Local | cs-cloud daemon | 进程/端口/二进制探测 | `costrict-web → 隧道 → cs-cloud` | web_terminal, exec, file_browser, monitoring |
| Platform | Provider API | 平台查询接口 | `costrict-web → Provider API` | lifecycle.*, snapshot, resize, public_endpoint |
| Hybrid | 任一域 | 双源探测 | 按用户/策略选择 | port_mapping, web_ide, ssh |

### 5.3 能力目录（Catalog）

```go
type Capability struct {
    Name         string         `json:"name"`         // 唯一标识，如 "network.port_mapping"
    Category     string         `json:"category"`     // lifecycle/network/access/resource
    DisplayName  string         `json:"displayName"`
    Domain       Domain         `json:"domain"`
    LocalImpl    *ImplSpec      `json:"localImpl,omitempty"`
    PlatformImpl *ImplSpec      `json:"platformImpl,omitempty"`
    ConfigSchema map[string]Field `json:"configSchema,omitempty"`
}

var Catalog = []Capability{
    // Local 域
    {Name: "access.web_terminal", Domain: DomainLocal, LocalImpl: &ImplSpec{
        DiscoveryMethod: "process:check ttyd"}},
    {Name: "access.file_browser", Domain: DomainLocal},
    {Name: "resource.exec", Domain: DomainLocal},
    {Name: "resource.monitoring", Domain: DomainLocal},

    // Platform 域
    {Name: "lifecycle.start", Domain: DomainPlatform},
    {Name: "lifecycle.stop", Domain: DomainPlatform},
    {Name: "lifecycle.destroy", Domain: DomainPlatform},
    {Name: "lifecycle.snapshot", Domain: DomainPlatform},
    {Name: "lifecycle.resize", Domain: DomainPlatform},
    {Name: "network.public_endpoint", Domain: DomainPlatform},

    // Hybrid 域
    {Name: "network.port_mapping", Domain: DomainHybrid,
        LocalImpl:    &ImplSpec{Description: "容器内 socat/nginx 转发"},
        PlatformImpl: &ImplSpec{Description: "云平台安全组/ELB"}},
    {Name: "access.web_ide", Domain: DomainHybrid,
        LocalImpl:    &ImplSpec{Description: "容器内 code-server"},
        PlatformImpl: &ImplSpec{Description: "Provider 代管 IDE"}},
    {Name: "access.ssh", Domain: DomainHybrid},
}
```

### 5.4 双源发现机制

```
┌────────────────────────────────────────────────────┐
│         Capability Resolver（合并视图）             │
└─────────────────┬──────────────────────────────────┘
                  │
    ┌─────────────┴──────────────┐
    ▼                            ▼
 Local 源                     Platform 源
 (cs-cloud 心跳上报)          (Provider SyncCapabilities)
```

**cs-cloud 心跳只报 Local 域能力**：

```json
{
  "device_id": "...",
  "local_capabilities": [
    {"name": "access.web_terminal", "enabled": true},
    {"name": "access.web_ide", "enabled": true,
     "config": {"port": 8080, "ide_type": "code-server"}}
  ]
}
```

**Provider SyncCapabilities 只报 Platform 域能力**：

```go
func (p *AiLabProvider) SyncCapabilities(ctx context.Context, deviceID string) ([]Instance, error) {
    codebox := p.fetchCodeBox(deviceID)
    return []Instance{
        {Name: "lifecycle.start", Domain: "platform", Enabled: true},
        {Name: "lifecycle.stop", Domain: "platform", Enabled: true},
        {Name: "network.port_mapping", Domain: "platform", Enabled: true,
         Config: map[string]any{"implementation": "platform"}},
        {Name: "access.web_ide", Domain: "platform", Enabled: codebox.WebIDEEnabled},
    }, nil
}
```

### 5.5 Resolver 合并规则

```go
type Resolved struct {
    Name        string         `json:"name"`
    Domain      Domain         `json:"domain"`
    Enabled     bool           `json:"enabled"`
    LocalAvail  *Availability  `json:"localAvail,omitempty"`
    PlatformAvail *Availability `json:"platformAvail,omitempty"`
}

func Resolve(cat Capability, source *models.RuntimeSource) Resolved {
    rc := Resolved{Name: cat.Name, Domain: cat.Domain}
    switch cat.Domain {
    case DomainLocal:
        rc.LocalAvail = findInJSON(source.LocalCapabilities, cat.Name)
        rc.Enabled = rc.LocalAvail != nil && rc.LocalAvail.Enabled
    case DomainPlatform:
        rc.PlatformAvail = findInJSON(source.PlatformCapabilities, cat.Name)
        rc.Enabled = rc.PlatformAvail != nil && rc.PlatformAvail.Enabled
    case DomainHybrid:
        rc.LocalAvail = findInJSON(source.LocalCapabilities, cat.Name)
        rc.PlatformAvail = findInJSON(source.PlatformCapabilities, cat.Name)
        rc.Enabled = (rc.LocalAvail != nil && rc.LocalAvail.Enabled) ||
                     (rc.PlatformAvail != nil && rc.PlatformAvail.Enabled)
    }
    return rc
}
```

---

## 6. 动作路由

### 6.1 Action 接口

```go
type ActionRequest struct {
    DeviceID   string         `json:"deviceId"`
    Capability string         `json:"capability"`  // "access.web_ide"
    Domain     Domain         `json:"domain"`      // local | platform（hybrid 必填）
    Action     string         `json:"action"`      // "open" / "stop" / ...
    Params     map[string]any `json:"params"`
}

func (r *Registry) ExecuteAction(ctx context.Context, req ActionRequest) (*ActionResult, error) {
    cat, _ := capabilities.Lookup(req.Capability)
    if !domainSupports(cat.Domain, req.Domain) {
        return nil, ErrDomainNotSupported
    }
    switch req.Domain {
    case DomainLocal:
        return r.forwardToCsCloud(ctx, req)        // 走现有隧道
    case DomainPlatform:
        provider, _ := r.GetProviderByDeviceID(req.DeviceID)
        return provider.HandleAction(ctx, req)
    }
}
```

### 6.2 Local 域转发（复用隧道，零新增基础设施）

```go
func (r *Registry) forwardToCsCloud(ctx context.Context, req ActionRequest) (*ActionResult, error) {
    // 通过 gateway 隧道调 cs-cloud 的本地 control plane
    target := fmt.Sprintf("/device/%s/proxy/cs-cloud/action", req.DeviceID)
    resp, err := r.gwClient.ProxyRequest(ctx, target, req)
    return parseResult(resp)
}
```

### 6.3 Hybrid 域智能选择

对 hybrid 能力，若调用方未指定 domain，后端按策略选择：

```go
func (r *Registry) resolveHybridDomain(req ActionRequest, rc Resolved) Domain {
    if pref := getDomainPreference(req.DeviceID, req.Capability); pref != "" {
        return pref           // 1. 用户预设偏好优先
    }
    if rc.LocalAvail != nil && rc.LocalAvail.Enabled {
        return DomainLocal    // 2. local 可用就优先（更快）
    }
    return DomainPlatform     // 3. fallback
}
```

### 6.4 跨域编排（Orchestration）

某些业务动作需多域协作（如"公网可访问的 Web 服务"= Local 启应用 + Platform 配公网）：

```go
type Orchestration interface {
    Execute(ctx context.Context, req OrchestrationRequest) error
}

type PublicWebServiceOrchestration struct{}

func (o *PublicWebServiceOrchestration) Execute(ctx context.Context, req OrchestrationRequest) error {
    r.ExecuteAction(ctx, ActionRequest{Capability: "resource.exec", Domain: DomainLocal, ...})
    r.ExecuteAction(ctx, ActionRequest{Capability: "network.public_endpoint", Domain: DomainPlatform, ...})
    return nil
}
```

Orchestration 是叠加在 Action 之上的高层语义，由业务层（PRD 中的 Factory Scheduler）调用。

---

## 7. API 设计

### 7.1 Provisioning（新增）

```
POST   /api/v1/runtimes:provision    启动一个运行位置
DELETE /api/v1/runtimes/:device_id    销毁（按 source_type 路由到 Provider）
GET    /api/v1/runtimes/types         列出支持的 Provider 类型 + schema
GET    /api/v1/runtimes               列表（可按 source_type 过滤）
```

### 7.2 Capability 查询（新增）

```
GET /api/v1/runtime-capabilities/catalog              # 能力字典
GET /api/v1/devices/:device_id/capabilities           # 某 runtime 合并后的能力
GET /api/v1/devices?capability=access.web_ide         # 按能力筛选
PATCH /api/v1/devices/:device_id/capabilities         # 更新实例能力
```

### 7.3 Action 执行（新增）

```
POST /api/v1/devices/:device_id/actions
Body: { capability, domain, action, params }
```

### 7.4 现有 API（完全保留）

```
GET    /api/devices/*                # 设备列表/详情
POST   /api/devices/:id/commands     # 命令下发
GET    /api/devices/:id/events       # 事件订阅
ANY    /api/devices/:id/proxy/*      # 代理（runtime 接入后可用）
```

---

## 8. 站在巨人肩膀上

### 8.1 直接集成的开源（不造轮子）

| 能力 | 用什么 |
|------|--------|
| Docker 控制 | `docker/docker/client` 官方 SDK |
| 公有云接入 | Terraform Provider 或 Pulumi |
| Web IDE | code-server（coder/code-server） |
| Web Terminal | ttyd 或 gotty（cs-cloud 集成） |
| 文件管理 | filebrowser |
| devcontainer 标准 | devcontainers/spec 元数据格式 |
| 配置即代码 | devcontainer.json + Terraform .tf |

### 8.2 必须自建的硬理由

#### Provider 抽象层（不能直接用 DevPod/Daytona）

- 它们的 daemon 是"被 SSH 拉起"的，我们的 cs-cloud 是"内置在运行位置主动接入"，连接方向相反
- 它们没有"device + docker + cloud + ailab"混合管理场景
- 它们不与 Casdoor/企业身份系统深度集成

#### 能力域管理系统（不能直接用 devcontainer features）

- devcontainer features 只描述"容器内"能力，不区分 platform/local 两域
- 我们的 hybrid 能力（如 port_mapping）是业务独有的
- 能力需要与 cs-cloud 心跳、Provider SyncCapabilities 双通道对齐

### 8.3 不该自建（明确反对）

- ❌ 不要自己写 Docker API 客户端
- ❌ 不要自己写云 SDK
- ❌ 不要自己写 Web IDE / Web Terminal
- ❌ 不要自己造 devcontainer-like 配置格式
- ❌ 不要自己造多云抽象层（Terraform Provider 已做）

### 8.4 复用 multica 基建（团队内已有成熟实现）

团队内 multica 项目（`D:/DEV/multica`）已有 5 块成熟基建，本设计直接借鉴/复用，避免重复造轮子：

#### 复用 1：Realtime 分片中继（直接搬运）

**来源**：`multica/server/internal/realtime/`（hub.go / broadcaster.go / redis_relay.go / sharded_stream_relay.go）

**用途**：Factory Dashboard 实时推上千 runtime 状态变化。Redis Stream 分片中继已生产验证，能跨多实例水平扩展。

**为什么必抄**：当前 costrict-web 的 SSE 单实例，规模化到几千个 runtime 并发推会爆。multica 这套是现成方案。

#### 复用 2：Workflow 16 态状态机（直接采用为 Orchestration 状态机）

**来源**：`multica/server/internal/service/workflow.go` `NodeRunStatus*` 常量集

**采用为 Orchestration 状态机的节点状态**：
```
pending → format_checking → format_ok → worker_assigned → working
       → awaiting_critic → critic_reviewing → critic_approved / critic_rework
       → completed / failed / blocked / skipped / cancelled
```

**价值**：相比"工序完成/失败"二元模型，这套含 rework / skip / block 的状态机更适合真实工厂场景。

#### 复用 3：EmptyClaimCache（性能优化必装）

**来源**：`multica/server/internal/service/empty_claim_cache.go`

**用途**：daemon poll 时稳定态无任务跳过 DB 扫描。Redis 缓存"该 runtime 当前无任务"判定。上千 runtime 并发 poll 时省 90% DB 负载。

#### 复用 4：Autopilot 触发时准入

**来源**：`multica/server/internal/service/autopilot.go` 的 `shouldSkipDispatch` 模式

**用途**：Factory Scheduler 派工前的"准入闸门"——runtime 不健康直接跳过派工，避免任务堆在死 runtime 上。

#### 复用 5：Events Bus（抽象层借鉴）

**来源**：`multica/server/internal/events/bus.go`

**用途**：costrict-web dispatcher 已有事件分发但没抽象。借鉴 multica 的 Bus 抽象层，作为 Factory 内部解耦的标准通道。

#### 不复用的部分（明确边界）

| 不复用项 | 理由 |
|---------|------|
| multica daemon 协议 | 多是 HTTP/WS，与我们 cs-cloud 反向隧道方向相反 |
| multica cloudruntime.Client | 只支持单一 cloud runtime，不如我们的 Provider 抽象通用 |
| multica issue / board 模型 | 属于人机协作层，黑灯工厂不需要 |

#### 共建（详见 `MULTICA_COBUILD_PROPOSAL.md`）

| 共建项 | 主导方 | 价值 |
|--------|--------|------|
| Runtime Protocol Spec | 双方 | 双向互通基础 |
| Task Template 标准 | 双方 | 跨平台任务调度 |
| Cost & Budget 基建 | costrict-web | multica 反向复用 |
| Shift Report 标准 | costrict-web | multica 反向复用 |

---

## 9. 与现有系统的边界

| 范围 | 现状 | 本提案后 |
|------|------|---------|
| `devices` 表 | 唯一身份来源 | **不动** |
| `gateway/` 隧道 | 设备连接通道 | **不动** |
| `cs-cloud` 桥接层 | device 端 daemon | **扩展心跳字段**（加 local_capabilities） |
| `costrict-jwt-swap` | 身份桥 | **不动**，仅 ailab provider provisioning 阶段使用 |
| `services/device_service.go` | 设备注册/管理 | **不动** + 加 `PreRegister` 方法 |
| `dispatcher/` | 事件分发 | **不动** |
| 新增：`internal/runtime/` | — | Provider + Capability + Registry |
| 新增：`runtime_sources` 表 | — | 来源元数据 + 能力 |

**核心承诺**：现有 device / gateway / cs-cloud 主链路零改动（cs-cloud 仅心跳协议加字段，向后兼容）。

---

## 10. 实施优先级

| P | 任务 | 价值 |
|---|------|------|
| P0 | `Runtime` Provider 接口 + `Registry` | 基础设施 |
| P0 | `LocalProvider` 包装现有 device 流程 | 验证抽象正确性 |
| P0 | `runtime_sources` 表 + 能力域 Resolver | 数据底座 |
| P1 | `DockerProvider` 实现 | 立即可用 |
| P1 | 能力目录 + Action 路由 + API | 业务可用 |
| P2 | `AiLabProvider` 实现 | 串联 ai-lab / jwt-swap |
| P2 | cs-cloud 心跳扩展 local_capabilities 字段 | 双源发现 |
| P3 | `CloudProvider`（先支持 1 个云） | 公有云扩展 |
| P3 | 跨域 Orchestration 框架 | 业务编排底座 |

---

## 11. 后续衔接

机制层就位后，业务层（黑灯工厂）在本文档之上构建：

- **Runtime Class**（运行时型号）：基于 Provider + 能力 + 规格，预定义型号供调度器选择
- **Runtime Pool**（资源池）：基于 Provider 的 warm pool，提前 provision 待命
- **Task Template**（任务模板）：基于能力需求匹配 Runtime Class
- **Factory Scheduler**（调度 AI）：基于 Registry 选 Provider + Class
- **Shift Report**（运行报告）：基于能力使用数据汇总

产品形态定义见 `RUNTIME_PLATFORM_PRD.md`，业务运营模式见 `LIGHTS_OUT_FACTORY_PRD.md`。

### 11.1 待办：能力扩展验证与业务概念补充

> 本节记录当前抽象已留口子、但落地阶段仍需补充验证或设计的点。不阻塞本提案通过，仅作为后续 P2 / P3 阶段的 checklist。

**AiLab Provider 落地时需重点验证（P2 阶段）**

- `network.port_mapping` / `access.web_ide` 归属 Hybrid 能力域后，双源发现的实际行为：ai-lab 上报的运行时端点与 device 上报的本地端口在 Resolver 中如何排序、去重、降级，需要在真实环境跑通用例
- `SyncCapabilities` 上报 `web_ide` / `ssh` / `file_browser` 等平台侧可用性时，配置同步的完整性（能否覆盖 ailab 实例重启 / 端口漂移 / 凭证失效等场景）
- `costrict-jwt-swap` 当前仅在 ailab provisioning 阶段使用，运行时若需要 ailab ↔ costrict 双向操作（例如把 ai-lab 上跑的 agent 结果回写平台），是否需要复用 jwt-swap 鉴权链需单独评估

**云平台新手 Workspace 落地前需补充的设计（P3 阶段）**

当前抽象支持「CloudProvider 起一个 workspace」，但「新用户注册即获得一个开箱即用的上手 workspace」这一业务语义尚未在机制层定义。落地前需补四块设计：

| 缺口 | 当前状态 | 待补内容 |
|------|---------|---------|
| RuntimeClass 注册表机制 | §3.3 仅给出概念 | 预置型号的注册、版本管理、可见范围（公开 / 私有 / 团队） |
| 预置内容承载 schema | 未定义 | workspace 内置 plugin / skill / 示例项目的声明格式与注入时机 |
| 「注册即自动创建」触发机制 | 未定义 | casdoor 用户创建事件 → CloudProvider.Provision 的接驳点与失败重试策略 |
| 新手 workspace 专属生命周期 | 未定义 | 与按需 workspace 在 TTL / 配额 / 计费 / 销毁策略上的差异 |

**关联演进方向**

以上 P2 / P3 待办与 [`RUNTIME_PLATFORM_PRD.md`](./RUNTIME_PLATFORM_PRD.md) 的产品形态、[`LIGHTS_OUT_FACTORY_PRD.md`](./LIGHTS_OUT_FACTORY_PRD.md) 的黑灯工厂调度相互绑定，落地前需对照 PRD 复核业务语义，避免机制层抽象跑偏。
