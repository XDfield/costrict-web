# Runtime Platform PRD（运行时平台产品 PRD）

> 状态：Draft
> 类型：子系统 PRD（产品层）
> 关联机制层：`EXTENSIBLE_RUNTIME_DESIGN.md`
> 关联业务层：`LIGHTS_OUT_FACTORY_PRD.md`
> 关联提案：`MULTICA_COBUILD_PROPOSAL.md`
> 关联项目：`cs-cloud`（桥接层）、`ai-lab`（runtime provider）、`costrict-jwt-swap`（身份桥）

---

## 0. 文档定位

回答 **"runtime platform 作为产品是什么、给谁用、解决什么问题"**。它定义 runtime 子系统的产品形态，是 `EXTENSIBLE_RUNTIME_DESIGN`（机制）与 `LIGHTS_OUT_FACTORY_PRD`（业务运营）之间的中间层。

### 三层文档关系

```
┌─────────────────────────────────────┐
│  本文档（RUNTIME_PLATFORM_PRD）       │  ← 产品视角
│  runtime platform 是什么产品           │
│  人 JOB / AI JOB（runtime 子系统）     │
└────────────────┬────────────────────┘
                 │ 产品定义驱动机制设计
┌────────────────▼────────────────────┐
│  EXTENSIBLE_RUNTIME_DESIGN           │  ← 机制视角
│  Provider 接口 / Capability Domain    │
│  runtime_sources 表 / Provision 流程  │
└────────────────┬────────────────────┘
                 │ 机制承载业务运营
┌────────────────▼────────────────────┐
│  LIGHTS_OUT_FACTORY_PRD              │  ← 业务视角
│  Factory Scheduler / Pool / Line      │
│  Shift Report / Lights-Out Score      │
└─────────────────────────────────────┘
```

### 三层文档的边界

| 文档 | 关心 | 不关心 |
|------|------|--------|
| **本文档（PRD）** | runtime platform 作为产品的形态 | 黑灯工厂运营、Provider 实现细节 |
| **EXTENSIBLE_RUNTIME_DESIGN** | Provider 怎么实现、能力怎么发现 | runtime 产品形态、黑灯工厂业务 |
| **LIGHTS_OUT_FACTORY_PRD** | 黑灯工厂怎么运营、产线怎么编排 | runtime 子系统产品形态、Provider 实现 |

---

## 1. 人 JOB

### 1.1 业务定位

**Runtime Platform**——给"任务 / agent"提供按需启动、能力可声明、生命周期可管理的运行位置。它的客户不是"想开发的人"，是"想跑任务的 AI 调度器"。

**定位关键句**：runtime platform 是"运行位置的批发市场"——AI 调度器是买家，Provider 是供货商，平台做市。

### 1.2 主要用户与场景

**张三（车间主任 / Agent 系统 Owner）**：定义工厂需要哪几种 runtime class（`light-crawler` / `gpu-runner` / `heavy-etl`），每种 class 的规格、预算、池化策略。他不关心今天有几台 runtime，他关心"我定义的 class 够用吗、Provider 切换顺不顺、能力目录准不准"。

**李四（Provider 集成方 / 平台开发）**：把新的 Provider 接入平台（如新增华为云、新增内部 ai-lab 集群）。他需要清晰的 Provider 接口、声明式的能力目录、可观测的接入诊断。

**王五（产线 Owner / 业务方）**：通过 task template 提需求"我需要能跑 PyTorch 训练的 runtime，要求 GPU + 16GB 显存"。他不想学 `docker run`，他想要"runtime class 目录"。

**赵六（成本 Owner / 财务 partner）**：知道每个 runtime 的成本归因（哪个任务、哪个产线、用了哪个 Provider），用于月度对账和成本优化决策。

### 1.3 核心 JOB（一句话）

> **"让 AI 调度器把任务派到合适的运行位置——运行位置可以是 docker / cloud / ailab / device 中任意一种，平台统一管理它们的生命周期、能力声明、成本归因。人对 runtime 的介入仅限于定义 class、接入 Provider、看健康看板。"**

### 1.4 与"开发环境"视角的根本差异

| 维度 | 开发环境视角（错） | 运行时平台视角（对） |
|------|-------------------|---------------------|
| runtime 本质 | 给人用的开发环境 | 给任务 / 调度器用的执行单元 |
| 启动方式 | 人点按钮创建 | AI Scheduler 派工触发 |
| 生命周期 | 长期持有 | 任务驱动，完成即销 |
| 数量级 | 单实例 / 几个 | 池化 / 几十到上千并发 |
| 异构性 | 单一形态（VM 或容器） | docker + cloud + ailab + device 混合 |
| 能力管理 | 用户自己装 | 平台声明 + 双源发现 |
| 主用户 | 开发者 | AI Agent 系统 / 调度器 |
| 核心指标 | 创建耗时、UX 喜好 | 启动延迟、能力命中率、单位成本 |

### 1.5 当前痛点（这个特性如何帮忙）

| 痛点 | 当前状态 | runtime platform 后 |
|------|---------|---------------------|
| Provider 各自为政 | 接 docker 写一套，接 ai-lab 又一套 | 统一 Provider 抽象，新 Provider 接入 ≤ 1 周 |
| 能力声明散乱 | runtime 能干啥靠人记 | 平台目录 + 双源发现，AI 可查询 |
| 任务跨 Provider 跑不通 | docker 跑的任务挪不到 cloud | 统一接入，runtime class 跨 Provider 复用 |
| 生命周期不可控 | runtime 起了就忘，资源泄漏 | lifecycle 域能力（start/stop/destroy/snapshot） |
| 成本归因缺失 | 月底对账分不清谁用了多少 | runtime_sources 关联任务 / 产线，归因 100% |
| 接入门槛高 | 每种 Provider 学习成本高 | 声明式接入，Provider 只实现 5 个方法 |

---

## 2. AI JOB

> **设计原则：runtime platform 里 AI 是"运行时管家"，人是"class 设计师 + Provider 集成方"。AI 做日常的选型 / 派工 / 健康 / 成本决策，人只定义边界和接入新 Provider。**

### 2.1 AI 必须独立完成的 JOB

#### JOB-R1：Provisioning Curator（runtime 选型官 AI）—— 派工选型

任务进来 → AI 根据任务需求匹配 runtime class → 选择最合适的 Provider → provision。

```
任务：跑一个 PyTorch 训练，需要 GPU + 16GB 显存
   ↓
AI 决策：
  - 匹配 runtime class: gpu-runner（4C16G + 1×T4）
  - 选 Provider（按成本 / 可用性 / SLA）：
    · option A: ailab codebox（成本 ¥3/h，2 min 启动）
    · option B: cloud VM（成本 ¥8/h，5 min 启动）
    · option C: docker（成本 ¥0，但本机无 GPU，跳过）
  - 选 A，从 warm pool 提货
   ↓
派工执行（零人工）→ 失败自动 fallback 到 B → 完成的产出归档 + runtime 销毁
```

**输入信号**：task template 的 capability requirements、runtime class 目录、各 Provider 实时可用性、warm pool 状态、成本表。
**输出**：`Provider.Provision()` 调用 + `ProvisioningOpts`。
**与业务层协作**：被 `LIGHTS_OUT_FACTORY_PRD` 的 JOB-A1（Factory Scheduler）调用，R1 是 A1 的"runtime 选型子模块"。

#### JOB-R2：Capability Catalog Curator（能力目录官 AI）—— 目录维护

维护能力目录的准确性——能力声明是否齐全、双源发现是否一致、目录是否覆盖新接入的 Provider。

```
事件：新接入华为云 Provider
   ↓
AI 检测：
  - 华为云有"安全组"概念，对应 hybrid 域的 network.port_mapping
  - 华为云有"EVS 快照"，对应 platform 域的 lifecycle.snapshot
  - 但目录里没声明 lifecycle.resize（华为云支持但其他云不支持）
   ↓
AI 提议：
  - 在 catalog 加 lifecycle.resize（domain=platform）
  - 标注华为云 Provider 支持，docker/cloud 视情况
  - 提请 Reviewer 审核（架构 Reviewer 一次性 review）
```

**关键约束**：能力目录修改影响调度决策，必须 Reviewer 审核后生效。AI 只起草，不直接改。

#### JOB-R3：Health Watcher（健康观察员 AI）—— 健康观测

实时观测所有 runtime 的健康状态——心跳、资源使用、能力可用性。异常时上报给自愈 AI 决策。

```
观测：
  - device-X 心跳延迟 30s（> 阈值 10s）
  - device-Y CPU 持续 95% 持续 5min
  - device-Z 的 access.web_terminal 能力失效（探测失败 3 次）
   ↓
分级上报：
  - device-X: 上报自愈 AI（触发 L1 重启 cs-cloud daemon）
  - device-Y: 上报自愈 AI（触发 L2 降级到 heavy-etl class）
  - device-Z: 标记能力失效，调度器不再派依赖该能力的任务
```

**与自愈协作**：Health Watcher 只负责"发现和分类"，自愈决策由 `LIGHTS_OUT_FACTORY_PRD` 的 JOB-A3（Autonomous Healing）负责。R3 是 A3 的"传感器层"。

#### JOB-R4：Cost Attributor（成本归因官 AI）—— 成本归因

实时归因每个 runtime 的成本到任务 / 产线 / Owner。预算监控由 `LIGHTS_OUT_FACTORY_PRD` 的 JOB-A6（Budget Guard）负责，归因由 R4 负责。

```
runtime device-X 启动于 09:00，销毁于 11:00
   ↓
AI 计算：
  - Provider: ailab codebox
  - 规格: 4C16G
  - 时长: 2h
  - 单价: ¥3/h
  - 总成本: ¥6
   ↓
归因到:
  - task_id: web-scrape-batch-123
  - production_line: data-collect-line
  - owner: 张三
   ↓
写入 runtime_cost ledger（任务级实时归因）
```

**关键约束**：归因 100% 覆盖率（任何 runtime 都必须能追溯到任务），是 Budget Guard 熔断的基础。

#### JOB-R5：Lifecycle Manager（生命周期管家 AI）—— 生命周期

runtime 不是"起了就忘"——AI 管其完整生命周期：`provision → warm → claim → run → idle → destroy`。

```
runtime device-X 生命周期：
  09:00 provisioned (warm pool 待命)
  09:15 claimed (派给 task-123)
  09:15-11:00 working
  11:00 task done → 进入 idle
  11:05 AI 决策：
    - warm pool 已超阈值 → destroy
    - 还是池子缺货 → 留 warm
   ↓
destroy 或留 warm（零人工）
```

**核心原则**：完成即销毁是默认，留 warm 是例外（基于池长 AI 的预测）。

### 2.2 AI First 下的交互设计

#### 触发 AI 的三种路径

1. **任务派工触发**：任务进入 → Provisioning Curator AI 选 Provider + class
2. **事件感知触发**：runtime 心跳异常 → Health Watcher AI 介入
3. **集成方干预**：李四接入新 Provider → Capability Curator AI 起草能力目录草案

#### 决策卡片（Reviewer 友好）

每个 AI 决策必须带"决策卡片"，特别是新 Provider 接入、目录修改时：

```
┌─────────────────────────────────────────────────────┐
│ 🤖 新 Provider 接入审核：huawei-cloud                │
├─────────────────────────────────────────────────────┤
│ 集成方：李四                                          │
│ 提交的 Provider 实现：5 个方法 ✓                      │
│                                                     │
│ AI 检测：                                            │
│  • 与已有 cloud Provider 重叠度 80%（建议合并？）     │
│  • 独有能力：lifecycle.resize, network.eip           │
│  • 成本：比 AWS 同规格低 30%                          │
│                                                     │
│ 三个选项：                                            │
│  [A] 接入为独立 Provider（推荐）                      │
│  [B] 合并到 cloud Provider 作为 region 选项           │
│  [C] 暂不接入（功能重叠太多）                          │
│                                                     │
│ AI 推荐：A（成本优势明显，能力互补）                   │
├─────────────────────────────────────────────────────┤
│ [采纳 A]  [采纳 B 并合并]  [退回修改]                 │
└─────────────────────────────────────────────────────┘
```

#### Reviewer 角色设计

| Reviewer | 把关对象 | 触发条件 |
|---------|---------|---------|
| **架构 Reviewer**（资深工程师） | 新 Provider 接入、新 Runtime Class 定义、能力目录修改 | 一次性 review |
| **成本 Reviewer**（团队管理者） | 新 class 的预算基线、Provider 成本阈值 | 一次性 review |
| **安全 Reviewer**（运维 / SRE） | Provider 凭证管理、跨域数据流动 | 接入时 review |

#### AI 与人的协作边界

| 工作类型 | 默认执行者 | 理由 |
|---------|----------|------|
| 单 runtime provision | AI 全自动 | 信息综合型工作，AI 比人快 |
| Provider 选择 | AI 全自动 | 失败可 fallback |
| 能力目录查询 | AI 全自动 | 读操作 |
| 能力目录修改 | AI 起草 → 架构 Reviewer 批 | 影响调度决策 |
| 新 Runtime Class 定义 | 人起草 → AI 评估 → 人批 | 业务规格，需对齐业务 |
| 新 Provider 接入 | 集成方开发 → AI 起草评估 → 架构 Reviewer 批 | 影响面广 |
| Runtime destroy | AI 全自动（默认完成即销） | 可逆（可重新 provision） |
| 大规模 destroy（> 10 个） | 必须人主动 | 底线 |

---

## 3. 站在巨人肩膀上

### 3.1 竞品借鉴

| 产品 | 借鉴点 | 不借鉴什么 |
|------|--------|-----------|
| **DevPod / Daytona** | Provider 抽象概念；devcontainer 配置格式 | 它们的 daemon 是"被 SSH 拉起"——我们反向（cs-cloud 主动接入） |
| **K8s Device Plugin** | 能力声明 + 调度器查询模式 | K8s 调度域是 Pod，我们是 runtime 类型 |
| **Terraform Provider** | 多云抽象；声明式 provisioning | 全套 IaC 复杂度（我们只需 provisioning 子集） |
| **AWS ECS Task Placement** | 任务到容器的匹配策略 | ECS 锁定 AWS |
| **Nomad** | 多 driver（docker/exec/java/...）抽象 | Nomad 是单一调度器，我们是多 Provider 入口 |
| **devcontainers/spec** | features 元数据格式 | features 只描述容器内能力（我们要分 local/platform 域） |
| **OpenTelemetry** | 资源属性（resource attributes）语义 | 全套观测体系（我们只关心 runtime 健康子集） |

**核心借鉴逻辑**：
- **DevPod + Terraform** 的"多 Provider 抽象"思路 → 我们的 Provider 接口
- **devcontainers spec** 的"声明式能力"格式 → 我们的能力 catalog schema
- **K8s Device Plugin** 的"能力上报 + 调度查询"模式 → 我们的双源发现

### 3.2 开源 / 框架集成（直接用，不造轮子）

| 能力 | 用什么 |
|------|--------|
| Docker 控制 | `docker/docker/client` 官方 SDK |
| 公有云接入 | Terraform Provider 或云厂商 SDK |
| Web IDE | code-server（coder/code-server） |
| Web Terminal | ttyd 或 gotty（cs-cloud 集成） |
| 文件管理 | filebrowser |
| devcontainer 标准 | devcontainers/spec 元数据格式 |
| 配置即代码 | devcontainer.json + Terraform .tf |
| 健康检查 | Prometheus exporter（cs-cloud 暴露 metrics） |
| 成本归因 | 现有数据库 + 聚合查询（不上专门成本平台） |

### 3.3 自建理由

#### 必须自建 1：Provider 抽象层（不能直接用 DevPod/Daytona）

**为什么不能直接用**：
- 它们的 daemon 是"被 SSH 拉起"的，我们的 cs-cloud 是"内置在运行位置主动接入"，连接方向相反
- 它们没有"device + docker + cloud + ailab"混合管理场景
- 它们不与 Casdoor / 企业身份系统深度集成
- 它们的目标是"给人提供开发环境"，我们是"给 AI 调度器提供运行位置"

#### 必须自建 2：能力域管理系统（不能直接用 devcontainer features）

**为什么不能直接用**：
- devcontainer features 只描述"容器内"能力，不区分 platform/local 两域
- 我们的 hybrid 能力（如 port_mapping）是业务独有的——既可在容器内 socat 转发，也可在云平台安全组配置
- 能力需要与 cs-cloud 心跳、Provider SyncCapabilities 双通道对齐

#### 必须自建 3：Runtime Registry 与 device 表的扩展关系

**为什么不能直接用现有 device 表**：
- `devices` 表只回答"谁是设备"，不回答"这个设备从哪来、能干啥、成本多少"
- `runtime_sources` 表是 device 的扩展（不是替换），记录来源 + 能力 + 成本元数据
- 现有 device 链路（gateway/dispatcher/event router）零改动，保证存量稳定

#### 不该自建（明确反对）

- ❌ 不要自己写 Docker API 客户端——用 `docker/docker/client`
- ❌ 不要自己写云 SDK——用云厂商官方 SDK 或 Terraform
- ❌ 不要自己写 Web IDE / Web Terminal——用 code-server / ttyd
- ❌ 不要自己造 devcontainer-like 配置格式——用 devcontainers/spec
- ❌ 不要自己造多云抽象层——Terraform Provider 已做

---

## 4. 验收标准：先悦己再悦人

### 4.1 第一关（悦己）—— 团队成员作为真实用户自评

**评估方式**：每个团队成员独立完成下述场景并打分（1-5 分），平均 ≥ 4 分才过这关。打分 ≤ 3 必须写理由，作为返工依据。

| # | 场景 | 期望体感 | 打分维度 |
|---|------|---------|---------|
| 1 | **新 Provider 接入**：接入一个新的 docker registry 镜像源 | 接入 ≤ 1 天，包括能力声明 | 1-5 |
| 2 | **runtime class 目录**：定义一个新的 light-crawler class | 5 分钟内定义完，调度器立即能用 | 1-5 |
| 3 | **能力查询**：问"哪些 runtime 支持 web_ide" | 一次 API 调用拿到准确结果 | 1-5 |
| 4 | **跨 Provider 派工**：docker 跑失败的任务挪到 cloud | 调度器自动 fallback，无需改任务 | 1-5 |
| 5 | **成本归因**：查某任务的 runtime 成本 | 实时查询，100% 归因到任务 | 1-5 |
| 6 | **健康观测**：runtime 心跳异常 | AI 自愈，无需人介入 | 1-5 |
| 7 | **完成即销**：观察 task 完成后 runtime 状态 | 自动销毁，资源不泄漏 | 1-5 |
| 8 | **能力 hybrid 选择**：port_mapping 走 local 还是 platform | AI 智能选择，无需人指定 | 1-5 |

**关键问题**：这一关要问的是——**未来我接入新 Provider 时，愿意用这个平台而不是直接写脚本吗？** 任何一个团队成员回答"我宁愿直接调 docker API"都意味着没过这一关。

### 4.2 第二关（悦人）—— 非 power user 视角

通过悦己关后再进入，评估以下用户角色：

| 角色 | JOB | 验收问题 |
|------|-----|---------|
| **业务方 PM** | 通过 task template 提需求 | 不看文档，能否 5 分钟内理解"我的任务需要什么 runtime class"？ |
| **集成方开发** | 接入新 Provider | Provider 接口是否清晰？是否需要看大量源码？ |
| **跨团队 Reviewer** | 审批新 Provider / Class | 看到审批卡片能否独立决策？ |
| **财务 partner** | 月度成本核对 | runtime 成本归因报表能否对账？ |

### 4.3 用户视角单一问题的修正

任何"为某类用户而设计但团队成员自己不用"的功能，禁止进入开发。例如：
- ❌ "为业务方做的 runtime 选择界面" —— 如果我们自己选 runtime 都不用它，业务方也不会用
- ❌ "为集成方做的 Provider 接入向导" —— 如果我们自己接 Provider 都嫌它麻烦，集成方更不会用
- ❌ "为财务做的成本看板" —— 如果我们自己核算成本都嫌它不准，财务更不会用

### 4.4 量化红线（任一不过即返工）

| 指标 | 红线 | 测量方式 |
|------|------|---------|
| **新 Provider 接入时长** | ≤ 1 周（含能力声明） | 接入到首任务派工的耗时 |
| **Runtime class 目录定义** | ≤ 5 分钟 | 从创建到调度器可用的耗时 |
| **能力查询准确率** | 100% | 双源发现一致的能力 / 总能力 |
| **跨 Provider fallback 成功率** | ≥ 95% | fallback 后任务正常完成 / 总 fallback |
| **runtime 成本归因覆盖率** | 100% | 有归因的 runtime / 总 runtime |
| **runtime 启动延迟（warm pool 命中）** | ≤ 10s | provision 到 online 的耗时 |
| **runtime 启动延迟（冷启动）** | ≤ 5 min | 同上但 pool 未命中 |
| **完成即销毁执行率** | ≥ 95% | 自动销毁 / 应销毁 |
| **能力 hybrid 智能选择正确率** | ≥ 90% | AI 选择符合预期 / 总选择 |
| **团队自评均分（悦己关）** | ≥ 4.0 / 5.0 | 匿名打分 |

---

## 5. 实施优先级（与机制层 DESIGN 对齐）

| P | 任务 | 价值 | 依赖本 PRD 哪个 JOB |
|---|------|------|---------------------|
| P0 | `Runtime` Provider 接口 + `Registry` | 基础设施 | R1 / R5 |
| P0 | `LocalProvider` 包装现有 device 流程 | 验证抽象正确性 | R1 |
| P0 | `runtime_sources` 表 + 能力域 Resolver | 数据底座 | R2 / R4 |
| P1 | `DockerProvider` 实现 | 立即可用 | R1 |
| P1 | 能力目录 + Action 路由 + API | 业务可用 | R2 |
| P2 | `AiLabProvider` 实现 | 串联 ai-lab / jwt-swap | R1 |
| P2 | cs-cloud 心跳扩展 `local_capabilities` 字段 | 双源发现 | R2 / R3 |
| P2 | Health Watcher + Cost Attributor（R3 / R4） | 运营基础 | R3 / R4 |
| P3 | `CloudProvider`（先支持 1 个云） | 公有云扩展 | R1 |
| P3 | 跨域 Orchestration 框架 | 业务编排底座 | R2 |

---

## 6. 后续衔接

本文档定义 runtime 子系统的产品形态。落地需要：

- **机制层实现**：见 `EXTENSIBLE_RUNTIME_DESIGN.md`——Provider 接口、能力域 Resolver、runtime_sources 表、Provision 流程、Action 路由的工程方案
- **业务层运营**：见 `LIGHTS_OUT_FACTORY_PRD.md`——基于 runtime platform 之上构建的黑灯工厂运营模式
- **跨团队共建**：见 `MULTICA_COBUILD_PROPOSAL.md`——runtime-protocol / Task Template / Cost & Budget / Shift Report 的双方共建

### 6.1 子系统间的依赖关系

```
              ┌──────────────────────────┐
              │  LIGHTS_OUT_FACTORY_PRD   │
              │  ─────────────────       │
              │  JOB-A1 Factory Scheduler │ ─── 调用 ───┐
              │  JOB-A3 Autonomous Healing│ ─── 调用 ───┐│
              │  JOB-A6 Budget Guard      │ ─── 调用 ───┐││
              └──────────────────────────┘             │││
                                                       ▼▼▼
              ┌──────────────────────────┐
              │  本文档（RUNTIME_PLATFORM_PRD）│
              │  ─────────────────       │
              │  JOB-R1 Provisioning      │ ◄── A1 调用
              │  JOB-R3 Health Watcher    │ ◄── A3 调用
              │  JOB-R4 Cost Attributor   │ ◄── A6 调用
              └────────────┬─────────────┘
                           │ 实现
                           ▼
              ┌──────────────────────────┐
              │  EXTENSIBLE_RUNTIME_DESIGN│
              │  ─────────────────       │
              │  Provider 接口            │
              │  Capability Domain        │
              │  runtime_sources 表       │
              │  Provision / Action 流程  │
              └──────────────────────────┘
```

---

## 附：核心概念定义

供后续实现引用。

### Runtime（运行位置）
一个能跑任务 / agent 的执行单元。可能是 docker 容器、cloud VM、ai-lab CodeBox、本地设备中的任意一种。从平台视角看都是 device。

### Provider（运行位置供货商）
提供 runtime 启动方式的具体实现。Local / Docker / Cloud / AiLab 四种，每种实现 Provider 接口的 5 个方法（`Type` / `Provision` / `Destroy` / `Status` / `SyncCapabilities`）。

### Runtime Class（运行时型号）
预定义规格，调度器按型号选型。如 `light-crawler` / `gpu-runner` / `heavy-etl`。是 task template 与 Provider 之间的桥梁。

### Capability Domain（能力执行域）
能力"在哪里执行"的分类。Local（容器内）/ Platform（Provider API）/ Hybrid（双域）三域。决定能力的发现方式和调用路径。

### Runtime Registry（运行时登记处）
runtime platform 的核心入口。业务层（Factory Scheduler）只与 Registry 交互，Registry 路由到具体 Provider。

### runtime_sources 表
`devices` 表的扩展表，记录 runtime 的来源（`SourceType`）、外部 ID（`ExternalID`）、Provider 元数据、能力（双源存储：`LocalCapabilities` / `PlatformCapabilities`）。
