# ADR: cs-user Phase 1 服务抽离路径与关键决策

| 字段 | 内容 |
|---|---|
| 状态 | **Accepted** · 2026-07-16 |
| 作者 | DoSun |
| 决策日期 | 2026-07-16 |
| 评审范围 | cs-user / costrict-web(server) / deploy / docs/identity-tenant |
| 关联文档 | [`IDENTITY_ARCHITECTURE_ROADMAP.md`](./IDENTITY_ARCHITECTURE_ROADMAP.md)、[`MULTI_TENANCY_DESIGN.md`](./MULTI_TENANCY_DESIGN.md)、[`CS_USER_SERVICE_DESIGN.md`](./CS_USER_SERVICE_DESIGN.md)、[`USER_CENTER_DESIGN.md`](./USER_CENTER_DESIGN.md)、[`IDENTITY_FEDERATION_DECISION.md`](./IDENTITY_FEDERATION_DECISION.md) |
| 取代 | [`IDENTITY_ARCHITECTURE_ROADMAP.md`](./IDENTITY_ARCHITECTURE_ROADMAP.md) §4 阶段切分（Phase A → Phase D 顺序） |

---

## TL;DR

正式启动 cs-user 服务抽离（不再走 ROADMAP 推荐的「Phase A 先在 monolith 内做、cs-user 抽离为可推迟的 Phase D」路径）。Phase 1 范围限定为 **user 数据 ownership + read-through RPC**，不含 JWT 自签、OAuth callback 接管、employment_identities。本 ADR 锁定 10 项关键实施决策，作为后续 PR 的执行基线。

---

## 1. 背景：为何偏离 ROADMAP

[`IDENTITY_ARCHITECTURE_ROADMAP.md`](./IDENTITY_ARCHITECTURE_ROADMAP.md) §4 推荐：
- 阶段 A（JWT 自签 + employment context）在 `costrict-web/server/` 内做，cs-user 范围职责但物理位置暂留 monolith
- 阶段 D（cs-user 服务抽离）「可无限期推迟，团队 < 10 人 / QPS < 100 不做」

本 ADR **主动选择偏离**：先做 cs-user 服务抽离（Phase 1 = user CRUD only），再做 JWT 自签（Phase 2）/ OAuth callback + employment（Phase 3）。

**偏离理由**：
1. 团队预期将扩展，提前拉服务边界比 monolith 内堆砌后再拆成本更低
2. JWT 自签（Phase 2）天然属于身份服务，先把服务壳搭好，Phase 2 落地无需再迁移
3. 服务边界一旦清晰，employment_identities / tenant_configs 等新表的 ownership 自然归属 cs-user，避免「先在 monolith 写、再迁 cs-user」的二次迁移

**承担的成本**：
- 多一个服务要部署 / 监控 / on-call
- 过渡期 strangler fig 数据一致性的复杂度
- 107 处 user 访问点（27 个文件）的渐进重构

---

## 2. 关键决策（10 项）

| # | 决策 | 选择 | 替代方案（被否决） | 理由 |
|---|---|---|---|---|
| D1 | 服务化路径 | 直接抽离独立 cs-user 服务 | monolith 内模块化 / 混合骨架 | 边界清晰、避免二次迁移 |
| D2 | 命名冲突解决 | 提案表 `enterprise_identities` 改名 `employment_identities`；保留现有 `enterprise_customers` 表与 `server/internal/enterprise/` 包不变 | 重命名现有 enterprise 包 / 接受歧义并存 | 与现有 `employment_providers` 概念对齐；消除提案内部不一致；零 churn 现有代码 |
| D3 | 数据库 | 独立 PostgreSQL 实例（cs-user 独占 user 表 ownership） | 共享 PG + RLS / 渐进拆 DB | 真服务化的 ownership 边界；独立扩展 / 备份 |
| D4 | 迁移策略 | Strangler fig：保留 `UserService` 接口，内部改 RPC；107 处 occurrence 逐步迁 | Big bang / Read-only replica + CDC | 现有 abstraction 是天然 seam；可独立验证；风险分散 |
| D5 | 服务间协议 | REST only（HTTP/JSON） | REST + gRPC / gRPC + grpc-gateway | 零工具链增加；与现有 gin 栈一致；调试友好；后续按需加 gRPC |
| D6 | 过渡期数据流 | Write cs-user + read-through RPC（CachedUserService 复用为本地缓存） | 双写过渡 / CDC 单向同步 | 单一真相源；无双写 drift；cutover 后 costrict-web 表直接进入下线流程 |
| D7 | JWT/OAuth 主权（Phase 2/3 落地） | cs-user 拥有 JWT 自签 + OAuth callback 接管 | cs-user 仅提供 JWT 签名服务，OAuth 仍走 costrict-web / Phase 1 仅 user 数据 | 终态架构最干净；身份主权彻底归 cs-user；符合 USER_CENTER_DESIGN |
| D8 | 服务间认证 | 共享密钥 / API key in HTTP header（`X-Internal-Token`） | Service-to-service JWT / mTLS / 网络隔离无应用层认证 | 实现最简单；与现有 query_key 风格一致；内网部署足够 |
| D9 | 仓库结构 | Monorepo：`costrict-web/cs-user/`（独立 go.mod，与 server/ /gateway/ 平级） | 独立仓库 / Monorepo + go.work | 与现有 monorepo 模式一致；跨服务改动原子化提交；CI 按路径触发 |
| D10 | Phase 1 范围 | 最小 cs-user：骨架 + users/user_auth_identities CRUD + ETL + read-through RPC | Phase A 全集（user + JWT + employment）/ 完整身份服务（含 OAuth callback） | 风险分散；每个 Phase 独立可验证；cutover 范围可控 |

---

## 3. Phase 1 交付清单

### 3.1 cs-user 服务骨架

```
cs-user/
├── cmd/api/main.go                  # gin server entry
├── internal/
│   ├── config/                      # DB + shared-secret 配置加载
│   ├── models/                      # User, UserAuthIdentity（从 server/internal/models 迁出）
│   ├── user/                        # CRUD service（业务逻辑迁移自 server/internal/user/）
│   ├── handlers/                    # REST handlers + Swagger 注释
│   └── middleware/                  # X-Internal-Token 验证
├── migrations/                      # goose migrations（从 server/migrations/ 复制 user 相关）
├── Dockerfile
├── go.mod
└── README.md
```

### 3.2 部署 chart

```
deploy/charts/cs-user/
├── Chart.yaml
├── values.yaml
└── templates/
    ├── deployment.yaml
    ├── service.yaml
    └── secret.yaml                  # 共享密钥 + DB 凭据
```

参考 [`deploy/charts/api/`](../../deploy/charts/api/) 模式。cs-user 服务**仅 cluster-internal**，不暴露公网（network policy 限制）。

### 3.3 ETL 脚本

`cs-user/cmd/etl/main.go`：一次性脚本，从 costrict-web DB 导出 users + user_auth_identities → 导入 cs-user DB。

- 支持 `--dry-run` 模式（输出 diff 报告，不写入）
- idempotent（基于 subject_id UPSERT）
- 验证：行数对齐 + 抽样字段对比 + casdoor_universal_id 唯一性检查

### 3.4 read-through RPC client

`server/internal/user/rpc_client.go`：

- 实现 `UserService` 接口（不变），内部走 HTTP 调 cs-user
- 复用现有 `CachedUserService`（LRU + TTL）
- 失败降级：缓存命中返回 stale data + log warning；缓存未命中返 503

### 3.5 验证标准（Phase 1 验收）

- [ ] cs-user Dockerfile 构建通过，本地 docker-compose 起得来
- [ ] `/healthz` endpoint 返回 200
- [ ] ETL dry-run 在生产数据快照上：行数一致 + 0 字段 drift
- [ ] costrict-web 任意 user API（如 `GET /api/users/:id`）走 RPC 路径返回正确数据
- [ ] CachedUserService 命中率 > 90%（连续 1 小时压测）
- [ ] cs-user DB 独立备份恢复测试通过
- [ ] costrict-web users 表进入 READONLY 模式（写入路由到 cs-user）

---

## 4. Phase 2 / Phase 3 概要（后续 ADR 细化）

### Phase 2：JWT 自签（cs-user 内）

- RS256 + JWKS endpoint（`/.well-known/jwks.json`）
- 保留所有现有 claim 名称（`universal_id` / `sub` / `provider` / `email` / `phone` / `exp` 等，详见 ROADMAP §9.2）
- 新增字段为追加（`employment` / `primary_provider` / `tenant_id`）
- 双格式 reader 已在 cs-cloud + costrict-web 落地（ROADMAP §9.6）
- 密钥存储：文件 + 启动加载（Phase 3 后迁 KMS）

### Phase 3：OAuth callback 接管 + employment_identities

- 接管 `/oidc-auth/api/v1/plugin/login` + `/token` 端点（cs-cloud / csc 依赖）
- **关键风险**：csc 依赖 OAuth token endpoint JSON body 的 `profile.account.{uuid, email, display_name, created_at}` + `profile.organization.*` 字段（ROADMAP §9.2 #6）——cs-user 必须完整复现此 shape，写集成测试用真实 csc login 流程验证
- `employment_identities` 表 + `ApplyEnterpriseMapping` 步骤
- `tenant_configs` 表（最小 schema：tenant_id + yaml 列）
- 默认 tenant bootstrap（`default`）
- Casdoor JWT 30 天兼容窗口 + 灰度序列（ROADMAP §9.4）

---

## 5. 风险登记

| 风险 | 严重度 | 缓解 |
|---|---|---|
| 107 处 user 访问点迁移中遗漏，导致读路径部分仍走老 DB | 高 | Strangler fig 完成时 grep 验证 `models.User` 直接访问清零；保留过渡期 costrict-web users 表 READONLY 作为兜底 |
| 共享密钥泄露 → cs-user 内部 API 可被任意调用 | 中 | 密钥仅 K8s secret 注入；定期轮换；network policy 限制 cs-user 仅接受同 namespace 流量 |
| ETL cutover 时数据丢失 / 不一致 | 高 | dry-run + diff 报告；cutover 窗口停写；cutover 后 row count 审计；保留 costrict-web DB 备份 30 天 |
| cs-user 服务故障 → costrict-web user API 全挂 | 高 | CachedUserService stale 兜底；cs-user 多副本；监控 + 告警 |
| Phase 3 接管 OAuth callback 时 csc 兼容性破坏 | 高 | 集成测试覆盖 csc 真实 login 流程；灰度（1% → 10% → 100%）；30 天 Casdoor 兼容窗口（ROADMAP §9.4） |
| `employment_identities` 改名在提案文档中遗漏 | 低 | 本 ADR 同步重命名 MULTI_TENANCY_DESIGN / CS_USER_SERVICE_DESIGN / IDENTITY_ARCHITECTURE_ROADMAP（本 PR 一并完成） |

---

## 6. 决策追踪

| 日期 | 决策 | 状态 |
|---|---|---|
| 2026-07-16 | D1-D10 全部锁定（本 ADR） | ✅ Accepted |
| 2026-07-16 | `enterprise_identities` → `employment_identities` 改名同步到 3 份提案文档 | 🔄 进行中 |
| 待定 | Phase 2 启动前：JWT 密钥存储方式（文件 / KMS）决策 | ⏳ 待决策 |
| 待定 | Phase 3 启动前：employment resolution_strategy 默认值（first_wins / last_wins） | ⏳ 待决策 |
| 待定 | cutover 执行窗口（停机 / 灰度比例序列） | ⏳ 待决策 |
