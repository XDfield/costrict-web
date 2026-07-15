# 黑灯工厂 PRD

> 状态：Draft
> 类型：业务 PRD（运营层）
> 关联产品 PRD：`RUNTIME_PLATFORM_PRD.md`（runtime 子系统产品 PRD）
> 关联技术方案：`EXTENSIBLE_RUNTIME_DESIGN.md`（机制层）
> 关联提案：`HTTP_TUNNEL_DESIGN.md`、`DEVICE_GATEWAY_DESIGN.md`、`MULTICA_COBUILD_PROPOSAL.md`
> 关联项目：`cs-cloud`（桥接层）、`ai-lab`（runtime provider）、`costrict-jwt-swap`（身份桥）

---

## 人 JOB

### 业务定位

**黑灯工厂（Lights-Out Factory）**——平台 runtime 体系的真正用途不是"给人提供开发环境"，而是"让 AI 在合适的运行位置里自主完成批量任务，人只在策略层介入"。

### 主要用户与场景

**张三（Agent 系统 Owner / 车间主任）**：他负责给"数据采集车间"配置工艺——定义任务模板（爬虫规格、并发数、重试策略）、设预算（每月 ¥3000）、设 SLA（24h 内完成）。配置完后车间 7×24 自己跑。张三每天早上花 5 分钟看 shift report，每周根据产能/良品率微调一次策略。他不关心"今天跑了哪些 runtime"，他关心"昨天工厂是否健康"。

**李四（工厂厂长 / 团队管理者）**：管理多个车间（采集、分析、训练），关注的不是单台机器，而是 OEE（设备综合效率）、单位产出的成本（¥/万条数据）、瓶颈工序在哪。月底要回答老板："这个月花了 ¥50k，产出了什么？"他的工具不是"创建 runtime 按钮"，是**工厂看板**。

**王五（异常处理员 / oncall）**：以前每天接 5-10 起"环境起不来"工单。黑灯工厂运行后，他只接 AI 升级上来的 5% 工单——每个工单 AI 已附完整诊断报告（根因、已尝试的修复、剩余选项），王五拍板时间 < 2 分钟。他的目标是"工单越来越少，自己越来越闲"。

### 核心 JOB（一句话）

> **"AI Agent 系统和定时调度器在 7×24 自主运行各类任务时，需要一个能池化调度、自动选型、自愈恢复、成本可控的运行时工厂——人只定义策略和审批边界，不介入日常派工。"**

### 与"开发环境"视角的根本差异

| 维度 | 开发环境视角（错） | 黑灯工厂视角（对） |
|------|-------------------|-------------------|
| runtime 本质 | 给人用的开发环境 | 给 AI/任务用的生产单元 |
| 启动方式 | 人点按钮 | AI 调度器派工 |
| 生命周期 | 长期持有 | 任务驱动，完成即销 |
| 数量级 | 单实例 / 几个 | 池化 / 几十到上千并发 |
| 主用户 | 开发者 | AI Agent 系统 |
| 人的角色 | 操作者 | Owner / 监督者 |
| 核心指标 | 创建耗时、UX 喜好 | OEE、单位成本、自治率 |

### 当前痛点（这个特性如何帮忙）

| 痛点 | 当前状态 | 黑灯工厂后 |
|------|---------|-----------|
| 任务派工靠人 | PM 手动配机器、起环境 | AI Scheduler 自动选型 + 派工 |
| 资源利用率低 | 每个任务独占 VM，闲时也烧钱 | 池化 + 任务驱动，闲即销毁 |
| 异常处理人工 | 80% 工单重复劳动 | L1/L2 自愈 95%，L3 才升级 |
| 成本不可见 | 月底才知道花了多少 | 任务级实时归因 + 预算熔断 |
| 工厂态势不可见 | 没人能回答"昨天工厂健康吗" | Shift Report 自动产出 |

---

## AI JOB

> **设计原则：黑灯工厂里 AI 是"运营者"，人是"董事会"。AI 做所有日常决策，人只做策略制定和异常升级。70% 以上的"派工/调度/自愈/优化"决策由 AI 独立完成，剩余 30% 由 AI 起草、人确认。**

### AI 必须独立完成的 JOB

#### JOB-A1：Factory Scheduler（车间主任 AI）—— 派工调度

接管所有任务到 runtime 的派工决策。任务进来 → AI 选 Runtime Class → 从池里提货 → 下发任务包。**人完全不参与单次派工**。

```
任务：批量爬取 1000 个网站
   ↓
AI 决策：
  - 匹配 Runtime Class: light-crawler (0.5C/512MB)
  - 并发度: 50（基于历史成功率 + 当前池容量）
  - 派工: 从 warm pool 取 50 个 + provision 8 个新实例
  - 任务包: 爬虫脚本 + URL 列表分片 + checkpoint 位置
   ↓
派工执行（零人工）→ 失败的自动重建 → 完成的产出归档
```

**输入信号**：任务模板、runtime class 目录、warm pool 状态、历史成功率、当前预算余额。
**输出**：批量的 Provision + Task Dispatch 指令。

#### JOB-A2：Pool Elasticity（池长 AI）—— 弹性预热

预测负载、提前调整 warm pool 大小，避免临时 provision 的冷启动延迟。

```
观察：每周二 10:00 有批量训练任务涌入
   ↓
AI 预测：明早 10:00 需要 5 个 gpu-runner 实例
   ↓
预执行：09:30 开始提前 provision，10:00 任务来时池里已有 5 个待命
```

**关键约束**：池子预热也有成本，AI 必须做"预热成本 vs 冷启动延迟"的权衡。

#### JOB-A3：Autonomous Healing（车间机修 AI）—— 三层自愈

runtime 异常 → AI 三层处理，黑灯度目标 ≥ 95%。

```
症状：device X 心跳超时 / 进程崩溃 / 资源耗尽
   ↓
L1 自愈（80% 比例，零人工）：
   - 进程重启
   - 容器重建（从镜像）
   - 任务从 checkpoint 续跑
   ↓
L2 降级（15% 比例，零人工，记录在案）：
   - 换 Runtime Class（gpu-runner → light-crawler）
   - 缩规格续跑
   - 部分任务跳过/延后
   ↓
L3 升级（5% 比例，推给人）：
   - 先由 Critic AI 复核 Worker AI 诊断（借鉴 multica Worker-Critic 模式）
   - Critic 通过 → 自动执行；Critic 否决 → 仍上报人
   - 真正升级到人时附双签诊断报告（Worker 草案 + Critic 复核意见）
   - 人 2 分钟内拍板：批准重建 / 放弃任务 / 升级规格
```

> **Worker-Critic 双角色设计**（借鉴 multica `server/internal/service/workflow.go`）：单线 L3 容易因 Worker AI 的"假阳性诊断"打扰人。Critic AI 是同模型的二次复核——若 Critic 也认定需要升级人，置信度从 ~70% 提升到 ~90%，能把 L3 假阳性率压一半以上。Critic 也可触发"降级回 L2"（如发现 Worker 过度反应）。

#### JOB-A4：Production Line Orchestrator（产线长 AI）—— 工作流编排

跨车间的工作流编排。前序产出 → 后序消费，AI 决定并行度、背压、断点续跑。

```
采集车间 (50× light-crawler)
   │ AI 监控：采集速率 vs 清洗速率，动态调整并发
   ▼
清洗车间 (5× light-crawler)
   │ AI 决策：数据量 > 阈值时扩容到 10×
   ▼
分析车间 (2× gpu-runner)
   │ AI checkpoint：每 1000 条存一次
   ▼
入库车间 (1× heavy-etl)
```

每道工序是 runtime 池，工序间通过消息队列 / 对象存储传递半成品。AI 自动处理：
- 背压（下游慢时上游暂停）
- 重试（单道工序失败）
- 断点续跑（产线中断后从最近 checkpoint 恢复）

#### JOB-A5：Shift Reporter（值班长 AI）—— 自动报告

每班次（默认 12h）自动产出 shift report，Owner 看报告而非看屏。

```
📋 2026-07-02 白班报告（00:00-12:00）

🏭 产能：完成任务 1,247 件 / 在制 53 件 / 排队 12 件
💰 成本：本班 ¥847 / 预算 ¥1,000（85%）
⚠️ 异常：自愈 23 次 / 升级人工 2 次（黑灯度 91.9%）
🏆 良品：成功 1,244 / 失败 3（良品率 99.76%）
📈 趋势：采集车间利用率 67%（建议缩容 light-crawler 池至 8）
🔮 预测：按当前速率，本周预算可持续至周日凌晨
```

**关键约束**：报告必须 5 分钟内可读完，AI 主动 highlight 异常和建议，不堆数据。

#### JOB-A6：Budget Guard（财务 AI）—— 预算硬约束

任务级预算硬约束，超预算**自动熔断**（不是事后追责）。

```
任务 web-scrape-batch 预算 ¥100
   ↓
AI 监控：累计消耗 ¥98 / 进度 75%
   ↓
AI 决策：剩余 ¥2 预计跑不完
   ↓
执行：
  - 通知任务 Owner（异步）
  - 自动降级（缩并发 / 简化策略）尝试续跑
  - 若 Owner 30 分钟内未响应，熔断销毁所有 runtime
```

月度预测超支时，AI 主动建议砍低优先级任务（按业务定义的优先级表）。

#### JOB-A7：Factory Skills Curator（工艺员 AI）—— 经验沉淀

每次成功的 Runtime Class 配置、Task Template 优化、自愈策略都沉淀为可检索的 Factory Skill。下次同类任务来时 AI 直接复用——这是黑灯度持续提升的正反馈循环。

```
事件：任务 model-train-v2 在 4C16G + GPU 上跑完，良品率 99.5%
   ↓
AI 提炼（自动）：
  - skill_name: "PyTorch 训练 / 中等规模 / 单 GPU"
  - 适用条件: requirements 含 torch + 数据集 < 10GB + 显存需求 < 12GB
  - 推荐 class: gpu-runner（4C16G）
  - 关键参数: batch_size=128, checkpoint_interval=500
  - 失败教训: batch_size=256 会 OOM（已记录避免值）
   ↓
向量化存入 pgvector（skill_embedding 表）
   ↓
下次任务来时 → AI 检索最相似 skill → 直接复用配置（零试错）
```

**借鉴 multica**：multica 的 Skills + pgvector（`server/internal/service/skill_proxy.go`）是成熟实现。costrict-web 可以直接：
- 复用 multica 的 skill 表 schema + 检索 API
- 或自建 `factory_skills` 表但导入相同格式（便于未来互通）

**关键约束**：Skill 提炼必须自动 + 可审计——每次成功任务自动产出，Owner 可在 shift report 里看到"本周新增 N 个 skill / 复用 M 次"。

### AI First 下的交互设计

#### 触发 AI 的三种路径

1. **任务派工**：业务系统 / 定时器 / Agent 调用 → Factory Scheduler AI 自动接手
2. **事件感知**：runtime 异常 → 机修 AI 自动介入；池子低于阈值 → 池长 AI 自动扩容
3. **Owner 干预**：张三通过 shift report 一键反馈"本周策略调整"→ AI 起草新策略草案

#### 决策卡片（升级版 Reviewer 友好）

每个 AI 决策必须带"决策卡片"，特别是 L3 升级时：

```
┌─────────────────────────────────────────────────────┐
│ 🤖 L3 升级：device-X 在 gpu-runner 上 OOM 3 次       │
├─────────────────────────────────────────────────────┤
│ 已尝试（L1/L2 失败记录）：                            │
│  • 09:15 重启进程 → 09:18 再次 OOM                   │
│  • 09:20 换 heavy-etl class → 该 class 配额已满      │
│                                                     │
│ 根因分析（置信度 87%）：                              │
│  • 任务 model-train-v2 实际内存需求 32GB             │
│  • gpu-runner 规格为 16GB，不足以承载                 │
│                                                     │
│ 三个选项：                                            │
│  [A] 升级到 cloud 32GB VM（成本 +¥15/h，需 Reviewer）│
│  [B] 优化任务（拆分 batch size 至 64）               │
│  [C] 放弃任务（标记为资源不足失败）                   │
│                                                     │
│ AI 推荐：B（成本最低，预计成功率 80%）                │
├─────────────────────────────────────────────────────┤
│ [采纳 B]  [采纳 A 并提审]  [自定义处置]               │
└─────────────────────────────────────────────────────┘
```

#### Reviewer 角色设计

| Reviewer | 把关对象 | 触发条件 |
|---------|---------|---------|
| **预算 Reviewer**（团队管理者） | 单次任务成本 > 阈值 / 月度趋势超支 | 自动进审批队列 |
| **安全 Reviewer**（运维/SRE） | 新任务类型首次上线 / 跨域数据流动 | 任务模板审批 |
| **架构 Reviewer**（资深工程师） | 新 Runtime Class 定义 / Provider 接入 | 一次性 review |

Reviewer 看到的是 AI 决策卡片 + 风险评估，**1-2 分钟内能拍板**。

#### AI 与人的协作边界

| 工作类型 | 默认执行者 | 理由 |
|---------|----------|------|
| 单任务派工 | AI 全自动 | 信息综合型工作，AI 比人快 |
| 池子预热 / 扩容 | AI 全自动 | 失败可回滚 |
| L1 故障自愈（重启） | AI 全自动 | 可逆 |
| L2 故障降级（换 class） | AI 全自动 + 记录 | 业务可恢复 |
| L3 故障升级 | AI 起草 → 人拍板 | 不可逆 / 高成本 |
| 新任务模板上线 | AI 起草 → Reviewer 批 | 影响面广 |
| 月度预算调整 | 人决定 | 战略层 |
| 工厂停摆 / 大规模销毁 | 必须人主动 | 底线 |

---

## 站在巨人肩膀上

### 竞品借鉴

| 产品 | 借鉴点 | 不借鉴什么 |
|------|--------|-----------|
| **K8s Job / CronJob** | JobSpec（backoffLimit、parallelism、completions）；Job 状态机 | K8s 全套依赖（我们已有 cs-cloud 桥接层） |
| **Argo Workflows** | DAG/steps 编排；retryStrategy；checkpoint；templating | 强绑定 K8s |
| **Tekton** | Task + Pipeline 分离；Step 概念 | K8s TaskRun 复杂度 |
| **Temporal** | workflow-as-code；activity 重试；状态持久化；durable execution | 编程模型太重 |
| **Airflow** | DAG 调度；operator 生态；SLA 概念 | Python-centric + 单点调度器 |
| **Fly Machines** | 秒级启停的轻量计算单元；按秒计费；auto-stop | 它的全球边缘网络（我们是内部平台） |
| **Modal** | 面向 AI 工作的无服务器执行；`@app.function` 装饰器；image/serial 模型 | SaaS-only |
| **Replicate / Baseten** | 模型即 endpoint；GPU 按需调度 | 模型中心定位 |
| **Ray** | actor 模型；任务图；分布式调度；故障恢复 | 通用分布式计算定位 |
| **AWS Batch / GCP Batch** | Job queue + compute environment 分离；managed orchestrator | 云锁定 |
| **工业 MES/SCADA 系统** | OEE 公式（可用率 × 性能率 × 良品率）；停机分析；车间看板 | — |

**核心借鉴逻辑**：
- **K8s Job + Argo Workflows** 的"任务 spec + 调度 + 重试"模型 → 我们的 Task Template + Factory Scheduler
- **Fly Machines + Modal** 的"秒级启停 + 按需计费"哲学 → 我们的 Runtime Pool + 完成即销
- **工业 MES** 的"OEE + 良品率 + 黑灯度"指标体系 → 我们的验收维度

### 开源 / 框架集成（直接用，不造轮子）

| 能力 | 用什么 |
|------|--------|
| 任务队列 | Redis Streams / NATS JetStream / Kafka（已有则复用） |
| 工作流编排 | 视复杂度选 Temporal（durable）或自研轻量 DAG |
| 状态机 | 现有 worker 模式扩展 / 自研（业务简单时） |
| 指标采集 | Prometheus（已有则复用） |
| 看板 | Grafana（OEE/黑灯度等业务指标） + 自研 shift report |
| AI 调度决策 | 底层 LLM SDK + 自研 prompt（不用 LangChain/AutoGen） |
| 预算追踪 | 现有数据库 + 聚合查询（不上专门成本平台） |

### 自建理由

#### 必须自建 1：Factory Scheduler + Runtime Class + Task Template

**为什么不能直接用 K8s Job/Argo**：
- K8s Job 假设任务在 K8s 集群内跑，我们的 runtime 在 cs-cloud 接入的任意位置
- Argo 假设 Pod 化，但我们的 docker/cloud/ailab 是异构的
- 我们要的是"任务跨 runtime 类型调度"，K8s 生态没有这套抽象

#### 必须自建 2：L1/L2/L3 三层自愈模型

**为什么不能直接用 K8s self-healing**：
- K8s 的自愈是"重建 Pod"，我们的 runtime 重建涉及"换 class / 换 provider"
- L2 降级（换规格续跑）是业务级决策，K8s 做不到
- L3 升级到人需要 AI 诊断报告，K8s 只能告警

#### 必须自建 3：Shift Report + Lights-Out Score

**为什么必须自建**：
- OEE/黑灯度等指标是业务独有的，没有现成产品
- shift report 的格式依赖我们的任务模板和能力域结构
- 工业 MES 太重，我们只需要其中"指标 + 报告"概念

#### 不该自建（明确反对）

- ❌ 不要自己实现任务队列——用 Redis/NATS/Kafka
- ❌ 不要自己写工作流引擎——能用 Temporal 就用 Temporal
- ❌ 不要自己写指标系统——用 Prometheus + Grafana
- ❌ 不要自己实现 K8s-like 调度器——我们的调度域是"runtime 类型"不是 Pod

---

## 验收标准：先悦己再悦人

### 第一关（悦己）—— 团队成员作为真实用户自评

**评估方式**：每个团队成员独立完成下述场景并打分（1-5 分），平均 ≥ 4 分才过这关。打分 ≤ 3 必须写理由，作为返工依据。

| # | 场景 | 期望体感 | 打分维度 |
|---|------|---------|---------|
| 1 | **一周托管**：配置一个任务模板后让它跑一周 | 一周内我不需要介入任何细节 | 1-5 |
| 2 | **shift report**：周一早看上周报告 | 5 分钟内能回答"工厂健康吗" | 1-5 |
| 3 | **L3 升级**：AI 推一个工单给我 | 诊断报告完整到我能直接拍板 | 1-5 |
| 4 | **成本归因**：月底看每个任务的开销 | 每分钱能追溯到任务/项目 | 1-5 |
| 5 | **周末无人**：周一回来检查 | 所有异常都被 AI 处理了 | 1-5 |
| 6 | **池化效率**：对比"独占 VM"和"池化 + 即销" | 月度成本明显下降 | 1-5 |
| 7 | **预算熔断**：故意提交超预算任务 | 5 分钟内自动熔断 + 通知 | 1-5 |
| 8 | **跨车间编排**：跑一条多步产线 | 中间失败能自动从 checkpoint 续跑 | 1-5 |

**关键问题**："凑合能管"不算"喜欢管"。这一关要问的是：**未来我愿意把多少类工作托付给这个工厂？** 任何一个团队成员回答"我宁愿自己写脚本跑"都意味着没过这一关。

### 第二关（悦人）—— 非 power user 视角

通过悦己关后再进入，评估以下用户角色：

| 角色 | JOB | 验收问题 |
|------|-----|---------|
| **业务方 PM** | 提交批量任务、关注结果 | 不看文档，能否 5 分钟内提交一个任务并理解进度？ |
| **财务** | 月度核对成本 | 成本报表能否对账？是否能识别异常消耗？ |
| **跨团队 Reviewer** | 审批新任务模板 | 看到审批卡片能否独立决策，不需要追问？ |
| **老板 / 决策者** | 看 ROI | shift report 能否回答"这个月产出什么、值不值"？ |

### 用户视角单一问题的修正

任何"为某类用户而设计但团队成员自己不用"的功能，禁止进入开发。例如：
- ❌ "为业务方做的任务提交界面" —— 如果我们自己提任务都不用它，业务方也不会用
- ❌ "为老板做的看板" —— 如果我们自己管工厂都不看，老板也不会看
- ❌ "为财务做的对账报表" —— 如果我们自己核算成本都嫌它不准，财务更不会用

### 量化红线（任一不过即返工）

| 指标 | 红线 | 测量方式 |
|------|------|---------|
| **黑灯度**（L1+L2 自治率） | ≥ 95% | (L1+L2 成功处理) / 总异常 |
| 任务良品率 | ≥ 99% | 成功任务 / 总任务 |
| Runtime 平均利用率（OEE 核心） | ≥ 60% | 实际工时 / 总工时 |
| 单任务成本归因覆盖率 | 100% | 有归因的任务 / 总任务 |
| Shift report 可读性 | ≤ 5 分钟读懂 | 阅读时长埋点 |
| Warm pool 命中率（提货不等待） | ≥ 90% | 池中提货 / 总派工 |
| 超预算自动熔断延迟 | ≤ 5 分钟 | 熔断时间 - 超阈值时间 |
| L3 升级响应时长（人工拍板） | ≤ 10 分钟（工作时间） | 升级到拍板的耗时 |
| 团队自评均分（悦己关） | ≥ 4.0 / 5.0 | 匿名打分 |

---

## 附：运营层概念定义

供后续实现引用。

### Runtime Class（运行时型号）
预定义规格，调度器按型号选型，不每次重算。如 `light-crawler` / `gpu-runner` / `heavy-etl`。

### Task Template（任务工单模板）
定义一类工作的需求：runtime class、并发、重试策略、预算、生命周期。是 Factory Scheduler 的输入。

### Runtime Pool（资源池）
预热 + 弹性的池化资源，调度器从池里"提货"而非临时 provision。每个 class 一个池。

### Production Line（产线 / Workflow）
跨车间的工作流编排，前序产出 → 后序消费，支持背压、重试、断点续跑。

### Shift Report（交班报告）
AI 周期性自动产出，含产能 / 成本 / 异常 / 良品 / 趋势 / 预测六大块。

### Lights-Out Score（黑灯度）
工厂级核心 KPI。`黑灯度 = 1 - (人工介入工时 / 总工时)`，目标 ≥ 95%。

---

## 附：与 multica 的边界与复用

> 团队内 **multica**（`D:/DEV/multica`）项目是"人机协作平台"——把 coding agent（Claude Code、Codex 等）变成看板上的"同事"。本节明确二者边界，避免重复造轮子。

### 边界划分（定位差异）

| 维度 | multica | costrict-web 黑灯工厂 |
|------|---------|----------------------|
| 核心理念 | "agent 是同事" — 人机协作 | "工厂无人值守" — AI 自主运行 |
| 典型任务 | 写代码 / 改 issue / code review | 批处理 / 模型训练 / 数据 pipeline |
| 人的角色 | 与 agent 并肩（同步协作） | 定策略、审批、接 5% 工单（监督） |
| runtime 数量 | 同事量级（1-10） | 车间量级（几十~上千并发） |

**简单口诀**：写代码 → multica；跑批 / 训练 / 数据 → costrict-web。两者通过 webhook 互通，不互嵌。

### 已经在复用的（事实）

| 项 | 方向 | 证据 |
|----|------|------|
| Skill API | multica → costrict-web | `multica/server/internal/service/skill_proxy.go` 已调 costrict-web |
| Casdoor 身份 | 双向 | multica `server/internal/costrictauth/` |

### 直接复用 multica 的成熟基建（5 项）

| 复用项 | 来源 | 用途 |
|--------|------|------|
| **Realtime 分片中继** | `multica/server/internal/realtime/` | Factory Dashboard 实时推上千 runtime 状态 |
| **Workflow 16 态状态机** | `multica/server/internal/service/workflow.go` | Production Line 的 Orchestration 状态机 |
| **EmptyClaimCache** | `multica/server/internal/service/empty_claim_cache.go` | 稳定态跳过 DB 扫描的优化 |
| **Autopilot 触发时准入** | `multica/server/internal/service/autopilot.go` | runtime 不健康直接跳过派工 |
| **Events Bus** | `multica/server/internal/events/bus.go` | 进程内事件总线抽象 |

### 共建项（4 项，详见 `MULTICA_COBUILD_PROPOSAL.md`）

| 共建项 | 主导方 |
|--------|--------|
| Runtime Protocol Spec | 双方 |
| Task Template 标准 | 双方 |
| Cost & Budget 基建 | costrict-web 主导，multica 反向复用 |
| Shift Report 标准 | costrict-web 主导，multica 反向复用 |

### 各做各的（不合并）

- **multica 独有**：Board UI / 11 种 coding CLI 集成 / Squad 协作
- **costrict-web 独有**：cs-cloud 反向隧道 / 多云 Provider / Runtime Pool / L1-L3 自愈 / 黑灯度 OEE

---

## 后续衔接

本文档定义**业务层（运营层）**的 JOB/AI/竞品/验收。技术底座（Provider 抽象 / 能力域 / 接入流程）见 `EXTENSIBLE_RUNTIME_DESIGN.md`，runtime 子系统的产品形态见 `RUNTIME_PLATFORM_PRD.md`。

三层文档的关系：

```
┌─────────────────────────────────────┐
│  本文档：LIGHTS_OUT_FACTORY_PRD     │  ← 业务（黑灯工厂怎么运营）
│  - 人 JOB / AI JOB                  │
│  - Factory Scheduler / Pool / Line  │
│  - Shift Report / Lights-Out Score  │
└────────────────┬────────────────────┘
                 │ 基于 runtime platform 提供的能力
┌────────────────▼────────────────────┐
│  RUNTIME_PLATFORM_PRD               │  ← 产品（runtime 是什么产品）
│  - runtime platform 人 JOB / AI JOB  │
│  - Provider / 能力 / 生命周期         │
└────────────────┬────────────────────┘
                 │ 产品定义驱动机制设计
┌────────────────▼────────────────────┐
│  EXTENSIBLE_RUNTIME_DESIGN          │  ← 机制（runtime 怎么接入）
│  - Provider 抽象                    │
│  - Capability Domain                │
│  - Runtime Registry                 │
└─────────────────────────────────────┘
```

### 业务层 AI JOB 与 runtime platform AI JOB 的协作关系

| 业务层 JOB | 调用的 runtime platform JOB | 说明 |
|-----------|----------------------------|------|
| JOB-A1 Factory Scheduler | JOB-R1 Provisioning Curator | A1 决定"派什么任务"，R1 决定"派到哪个 runtime" |
| JOB-A3 Autonomous Healing | JOB-R3 Health Watcher | R3 是 A3 的"传感器层"，A3 是 R3 的"决策层" |
| JOB-A6 Budget Guard | JOB-R4 Cost Attributor | R4 提供 100% 归因数据，A6 基于此熔断 |
| JOB-A2 Pool Elasticity | JOB-R5 Lifecycle Manager | R5 管"完成即销"，A2 管"提前预热" |
