# Multica × Costrict 共建提案

> **发起来源**：costrict-web 团队
> **接收方**：multica 团队
> **状态**：Draft · 待 multica 团队评审
> **关联文档**：
> - `EXTENSIBLE_RUNTIME_DESIGN.md`（costrict-web 机制层）
> - `LIGHTS_OUT_FACTORY_PRD.md`（costrict-web 业务层）

---

## 1. 提案摘要

costrict-web 正在构建"黑灯工厂"——AI 自主运行的运行时管理体系，承载批处理 / 模型训练 / 数据 pipeline 等任务。multica 已经在"人机协作平台"领域（coding agent 当同事）建立了成熟基础。

我们在分析两个项目时发现：

- **5 项 multica 已成熟的基建**值得 costrict-web 直接复用（避免重复造轮子）
- **4 个领域**值得双方共建（统一标准、双向互通）
- **2 项已存在的事实集成**（multica 已调 costrict-web 的 Skill API + 共用 Casdoor）证明团队已有协作 precedent

本提案提议：在保留各自产品定位的前提下，启动为期 ~6 周的 **3 阶段共建**，把"调度 / runtime / 工作流"层的基础设施打通，避免未来重复实现。

---

## 2. 各自定位（明确不重叠）

| 维度 | multica | costrict-web 黑灯工厂 |
|------|---------|----------------------|
| 核心理念 | agent 是同事（人机协作） | 工厂无人值守（AI 自主运行） |
| 典型任务 | 写代码 / 改 issue / code review | 批处理 / 训练 / 数据 pipeline |
| 人的角色 | 与 agent 并肩同步协作 | 定策略、审批、接 5% 工单 |
| runtime 数量级 | 1-10（同事级） | 几十~上千（车间级） |
| 任务粒度 | Issue（小时~天） | Task Template（秒~小时） |

**简单口诀**：写代码 → multica；跑批 / 训练 / 数据 → costrict-web。

> **本提案不合并两个产品**，只打通底层基建层，保留各自的产品哲学。

---

## 3. 已有的事实集成（precendent）

| 已集成项 | 方向 | 证据 |
|---------|------|------|
| Skill API | multica → costrict-web | `multica/server/internal/service/skill_proxy.go:24` 注释明确："internal HTTP client for the costrict-web skill API" |
| Casdoor 身份 | 双向 | multica `server/internal/costrictauth/` |
| PostgreSQL 栈 | 双方都用 | costrict-web 用 GORM，multica 用 sqlc |

> 两个项目已经在数据/身份层互联。本提案只是把这种互联**扩展到调度/runtime 层**。

---

## 4. 复用项（multica → costrict-web，单向借鉴）

### 4.1 Realtime 分片中继（强烈推荐）

- **来源**：`multica/server/internal/realtime/`（hub.go / broadcaster.go / redis_relay.go / sharded_stream_relay.go）
- **价值**：生产验证的 Redis Stream 分片 WS 中继，跨多实例水平扩展
- **costrict-web 用途**：Factory Dashboard 实时推上千 runtime 状态
- **替代成本**：自建至少 1-2 周
- **请求**：希望 multica 团队**保持这套接口稳定**，costrict-web 这边直接搬运适配

### 4.2 Workflow 16 态状态机（直接采用）

- **来源**：`multica/server/internal/service/workflow.go` 的 `NodeRunStatus*` 常量
- **采用为**：costrict-web Orchestration 状态机
- **价值**：含 rework / skip / block / cancelled 的成熟状态机，远胜"二元完成/失败"模型
- **请求**：希望 multica 团队**保持状态机语义稳定**，costrict-web 直接照搬常量命名

### 4.3 EmptyClaimCache 优化（必装）

- **来源**：`multica/server/internal/service/empty_claim_cache.go`
- **价值**：稳定态跳过 DB 扫描，上千 runtime poll 时省 90% DB 负载
- **请求**：希望 multica 团队**接受 costrict-web PR**——若 costrict-web 在 cache key / TTL 策略上有所优化，会反推回 multica

### 4.4 Autopilot 触发时准入模式

- **来源**：`multica/server/internal/service/autopilot.go` `shouldSkipDispatch`
- **价值**：派工前的"runtime 准入闸门"——不健康直接跳过，避免任务堆死 runtime

### 4.5 Events Bus 抽象

- **来源**：`multica/server/internal/events/bus.go`
- **价值**：costrict-web dispatcher 已有事件分发但无抽象，借鉴 Bus 接口统一

---

## 5. 共建项（双方合作，4 项）

### 5.1 Runtime Protocol Spec（最高优先级）

**问题**：
- multica daemon ↔ server：HTTP/WS（runtime 必须公网可达）
- costrict-web cs-cloud ↔ server：反向隧道（runtime 可在内网）
- 两套协议不互通，无法跨平台调度

**共建方案**：

```
runtime-protocol/
├── registration       # 注册（device_id, token, capabilities）
├── heartbeat          # 心跳（status, metrics, local_capabilities）
├── task_dispatch      # 任务下发（task_template, inputs, checkpoint）
├── task_progress      # 进度上报（events, logs, artifacts）
├── task_result        # 结果回收（status, outputs, error）
├── capability_query   # 能力查询（local/platform domain）
└── action_invoke      # 能力动作调用（with domain routing）
```

**两种传输绑定**：
- `transport-http`：multica daemon 用
- `transport-tunnel`：costrict-web cs-cloud 用

**收益**：
- multica 可接入内网 runtime（用 cs-cloud 桥接）
- costrict-web 可用 multica 的 cloud runtime fleet
- 任务可跨平台调度（multica 派工到 costrict-web 池，反之亦然）

**主导**：双方共同，costrict-web 主导 capability domain 部分（我们已设计 local/platform/hybrid 三域）

### 5.2 Task Template 标准

**问题**：multica 有 `workflow_spec` + `issue`；costrict-web 有 `task_template`。两套描述格式。

**共建方案**：统一 YAML 格式，双方都能消费：

```yaml
spec_version: 1
runtime_class: light-crawler        # costrict-web 概念
parallelism: 50
retry: { max: 3, backoff: exp }
budget_yuan: 100                    # costrict-web 独有，multica 可选
on_complete: destroy | archive
workflow:                           # multica 概念
  nodes: [...]
  edges: [...]
worker_critic:                      # multica 独有
  enabled: true
  critic_agent: reviewer-1
capabilities_required:              # costrict-web 概念
  - network.outbound
  - resource.exec
```

**收益**：
- 任务模板可在两个平台间复制粘贴
- 未来"在 multica 提交，派到 costrict-web 跑"成为可能

**主导**：双方共同，multica 主导 workflow 部分（已成熟），costrict-web 主导 budget/capability 部分

### 5.3 Cost & Budget 基建（costrict-web 主导）

**问题**：multica 完全没做预算控制。costrict-web 设计了 Budget Guard + 任务级预算 + 自动熔断。

**共建方案**：把 Budget Guard 抽到 `runtime-protocol/cost` 层：
- 任务级预算字段进 Task Template
- runtime 心跳上报资源消耗
- 熔断规则统一（超阈值 → 自动销毁）

**收益**：multica 反向复用——coding agent 也要花钱（云 VM、API tokens），Budget Guard 同样适用。

**主导**：costrict-web，multica 反向复用

### 5.4 Shift Report 标准（costrict-web 主导）

**问题**：multica 有 analytics 数据但没产出报告；costrict-web 设计了 shift report 标准。

**共建方案**：标准化 shift report 格式（产能 / 成本 / 异常 / 良品 / 趋势 / 预测六大块）

**收益**：multica 也用同套报告生成"团队 agent 工作周报"。

**主导**：costrict-web，multica 反向复用

---

## 6. 各做各的（明确不合并）

| 模块 | 谁做 | 不合并理由 |
|------|------|-----------|
| Board / Issue UI | multica | 人机协作场景专属 |
| Coding CLI 集成（11 种 agent） | multica | 业务专属 |
| Squad + Leader 路由 | multica | "团队"概念属于协作层 |
| cs-cloud 反向隧道 | costrict-web | multica 用 HTTP 就够 |
| Provider 多云抽象（docker/cloud/ailab） | costrict-web | multica 只需 cloudruntime 一类 |
| Runtime Pool / Warm Pool | costrict-web | multica 规模不需要池化 |
| L1/L2/L3 自愈 | costrict-web | multica 人机协作，不需要"全自治自愈" |
| 黑灯度 / OEE 指标 | costrict-web | multica 定位不需要 |
| pgvector Skill 检索 | multica（已实现） | 共用即可 |

---

## 7. 落地路径（3 阶段，~6 周）

### Phase 1：协议抽离（Week 1-2）

**目标**：建立 runtime-protocol 共享仓库

**任务**：
1. 新建仓库 `runtime-protocol`（或在 costrict-web 下）— **costrict-web 负责**
2. 定义 7 类消息 schema（registration / heartbeat / dispatch / progress / result / capability / action）
3. 定义 HTTP 与 tunnel 两种 transport binding
4. multica 这边把 daemon 协议整理为 spec — **multica 负责**
5. costrict-web 这边把 cs-cloud 心跳扩展为 spec — **costrict-web 负责**

**交付物**：
- `runtime-protocol/` 共享仓库（含 schema + 双方 transport binding）
- 双方各自实现 protocol 层适配器（不替换现有实现，作为新接口共存）

### Phase 2：复用 + 共建并行（Week 3-4）

**目标**：5 项复用落地 + Task Template 标准达成

**任务**：
1. costrict-web 搬运 multica Realtime 包 — **costrict-web 负责**
2. costrict-web 采用 multica 16 态状态机 — **costrict-web 负责**
3. costrict-web 抄 EmptyClaimCache（+ 反推优化 PR 给 multica）— **costrict-web 负责**
4. 共建 Task Template 标准 YAML — **双方共同**
5. 共建 Cost & Budget 字段进 Task Template — **costrict-web 主导**

**交付物**：
- costrict-web 内部新增 Realtime/Workflow/Cache 实现
- `task_template_v1.yaml` 标准 spec
- multica 这边可选适配新 protocol（不强制）

### Phase 3：双向互通（Week 5-6）

**目标**：跨平台任务派工最小可用

**任务**：
1. costrict-web Factory Scheduler 接受新 protocol — **costrict-web 负责**
2. multica WorkflowService 接受新 protocol（可选）— **multica 负责**
3. 建立 webhook 互通：multica → costrict-web runtime fleet 调用；costrict-web → multica code review 派工 — **双方共同**
4. Shift Report 标准化 — **costrict-web 主导**

**交付物**：
- 跨平台任务调度最小可用（demo：在 multica 提交任务，派到 costrict-web 工厂跑）
- 双方 shift report 统一格式

---

## 8. 责任与依赖

### 8.1 双方责任矩阵

| 任务 | 主责方 | 协作方 | 阻塞条件 |
|------|--------|--------|---------|
| runtime-protocol 仓库 | costrict-web | multica 提供 daemon 协议参考 | multica 不公开 daemon 协议 |
| Realtime 包搬运 | costrict-web | multica 保持接口稳定 | multica 重构 Realtime |
| 16 态状态机采用 | costrict-web | multica 保持常量命名 | multica 改状态机语义 |
| EmptyClaimCache PR | costrict-web | multica 接受 PR | multica 不接受外部 PR |
| Task Template 标准 | 双方共同 | — | 任一方退出 |
| Cost & Budget | costrict-web 主导 | multica 反向复用 | — |
| Shift Report | costrict-web 主导 | multica 反向复用 | — |
| Webhook 互通 | 双方共同 | — | 任一方未完成 protocol 适配 |

### 8.2 我们对 multica 的承诺

为降低 multica 团队协作成本，costrict-web 这边承诺：

1. **复用不改源**：搬运 multica Realtime / Workflow 状态机时，**保留 multica 文件头注释**（attribution），不冒充自研
2. **优化反推**：在 multica 代码上做的优化（EmptyClaimCache / autopilot 改进）一律 PR 回 multica，不私藏
3. **接口稳定**：runtime-protocol 这边 costrict-web 单方面先做适配，multica 可按自己节奏接入
4. **不污染 multica 仓库**：所有共建通过 PR 流程，不直接 push

### 8.3 我们对 multica 的请求

1. **维持稳定**：Realtime / Workflow 状态机 / Events Bus 这三块的接口/常量命名在 Phase 2 期间不要破坏性改动
2. **公开 daemon 协议**：把现有 daemon ↔ server 协议整理为文档，作为 runtime-protocol 起草参考
3. **指派 reviewer**：每项共建 multica 这边指派至少一位 reviewer，PR 1 周内给反馈
4. **互通 webhook**：Phase 3 协助建立跨平台任务派工的 webhook 测试

---

## 9. 风险与缓解

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| multica 团队优先级不一致 | 中 | 高 | 先做单向复用（不需要 multica 配合），共建项后续推 |
| runtime-protocol 设计分歧 | 中 | 中 | 指定双方各出 1 人组成 protocol SIG，定期 sync |
| Task Template 双方都用但又改 | 低 | 中 | spec_version 字段 + 向后兼容承诺 |
| Realtime 包搬运后 multica 重构导致同步困难 | 中 | 中 | costrict-web 这边 fork 版本号固定，multica 重构不影响 |
| Webhook 互通安全性 | 中 | 高 | webhook 加签名（HMAC）+ IP 白名单 |

---

## 10. 不做的事（明确反对）

为防止 scope creep，本提案**明确不做**：

- ❌ 不合并两个产品（multica 和 costrict-web 各自独立部署）
- ❌ 不统一技术栈（multica 用 sqlc/pgx，costrict-web 用 GORM，各留各的）
- ❌ 不强制 multica 接入 runtime-protocol（multica 按自己节奏）
- ❌ 不引入新依赖（如 gRPC、Kafka）——继续用 HTTP/WS/Redis
- ❌ 不重新设计 Skill API（multica 已在调，保持现状）
- ❌ 不替换 multica 的 daemon/cloudruntime（multica 内部架构不动）

---

## 11. 验收标准

### Phase 1 完成（协议层）

- [ ] `runtime-protocol/` 仓库建立，含 7 类消息 schema
- [ ] 双方各出一份"现有协议盘点文档"
- [ ] 双方 reviewer 签字确认 schema 可行

### Phase 2 完成（复用 + 标准）

- [ ] costrict-web 系统里出现 Realtime 包（带 multica attribution）
- [ ] costrict-web Orchestration 状态机使用 multica 16 态常量
- [ ] EmptyClaimCache PR 提交到 multica
- [ ] `task_template_v1.yaml` 双方签字
- [ ] Cost & Budget 字段进 Task Template

### Phase 3 完成（互通）

- [ ] demo：在 multica UI 提交一个任务，派到 costrict-web 工厂执行并回传结果
- [ ] shift report 双方共用同一 JSON 格式
- [ ] webhook 互通用 HMAC 签名 + IP 白名单

---

## 12. 时间表

| Week | 里程碑 |
|------|--------|
| Week 1 | 双方 reviewer 评审本提案；若通过，组建 protocol SIG |
| Week 2 | runtime-protocol 仓库初版；multica 协议盘点文档 |
| Week 3 | costrict-web 完成 Realtime/Workflow/Cache 搬运 |
| Week 4 | Task Template 标准签字；Cost 字段加入 |
| Week 5 | Webhook 互通 demo 开发 |
| Week 6 | Phase 3 验收；后续路线图讨论 |

---

## 13. 联系与决策机制

- **costrict-web 这边主接口人**：（待填，建议指定一位 runtime 体系 owner）
- **multica 这边主接口人**：（待 multica 团队指派）
- **决策机制**：所有共建项通过 PR + 双方 reviewer 签字；本提案本身需 multica 团队书面确认（issue comment / 邮件回复均可）
- **分歧升级**：双方接口人定期 sync；无法解决的分歧升级到双方 team lead

---

## 附：一句话总结

> **multica 在 Realtime / Workflow / Task Queue / Autopilot 四块基建上比 costrict-web 成熟，直接复用能省 ~1 个月工作；Runtime Protocol / Task Template / Cost / Shift Report 四个领域值得共建，能避免未来重复造轮子。本提案请求 multica 团队评审，最低诉求仅"维持 Realtime/Workflow 接口稳定 + 公开 daemon 协议盘点"，其他都由 costrict-web 主动推进。**
