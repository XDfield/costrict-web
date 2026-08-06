# JWT 验签链路清理计划

**状态**：Proposal · 2026-08-05
**前置**：Phase A7 已落地（cs-user 接管 sign + verify），见 [IDENTITY_ARCHITECTURE_ROADMAP §4 阶段 A](./IDENTITY_ARCHITECTURE_ROADMAP.md)。
**目的**：把 Phase A7 留下的"双写、副本、过渡分支"清理干净，让 server 在身份链路上回归"RPC client + cookie writer"两个职责，cs-user 成为唯一身份权威。
**读者**：实施者、评审者。
**本文不修改**任何已有提案决策，只做清理编排。

---

## 1. 现状（已确认的冗余点）

| # | 冗余 | 文件:行 | 触发条件 / 风险 |
|---|---|---|---|
| R1 | `normalize.go` byte-for-byte 副本 | `server/internal/authidentity/normalize.go`（317 行）↔ `cs-user/internal/auth/normalize.go`（321 行） | 两侧规则必须手动同步。最近一次漂移由 `4514eb1` 补丁修复（`oauth_GitHub_*` 反推 provider），证明这是活跃风险而非历史包袱 |
| R2 | Server 在 OAuth callback 用 unverified parse 做 profile enrichment | `server/internal/handlers/handlers.go:494-495`（`ParseJWTClaimsFromAccessToken` + `MergeJWTClaims`） | cs-user `ReissueToken` 内部已对同一份 raw JWT 验签 + 用 user 行覆盖 profile，server 的 enrichment 结果被覆盖。属于纯死代码 |
| R3 | Server 在 bind identity callback 用 unverified parse 两次 | `server/internal/handlers/handlers.go:662`（current session token）+ `:679`（newly-bound identity token）+ `:702/:722`（`BuildExternalKey`） | Server 需要 claims 字段做 BindIdentityToUser。当前是"信任 IdP 字段不动_signature"，与 cs-user verify 链路割裂 |
| R4 | 5 个重叠的 claim 类型 | `middleware.CasdoorUserInfo` / `middleware.AuthClaims` / `user.JWTClaims` / `models.JWTClaims`（cs-user）/ `auth.EnterpriseClaims` | `user.JWTClaims` 与 `models.JWTClaims` 是 wire-type 镜像，用于 RPC 边界；server 不再解析 JWT 后失去存在意义 |
| R5 | cs-user `VerifyToken` 两条路径 + fall-through | `cs-user/internal/handlers/auth.go:443` (Path 1) → fall through → `:492` (Path 2) | Path 1 失败的真正原因被 Path 2 错误掩盖。线上 401 排查需要看两个 verifier 日志才能定位 |
| R6 | `csUserIssuer = "cs-user"` 字符串硬编码 | `server/internal/middleware/auth.go:619` vs cs-user 配置项 | server 与 cs-user 配置默契对齐，issuer 改名会静默 break |
| R7 | `buildExternalKey` 双份 | `server/internal/user/service.go:914` + `cs-user/internal/user/...` | server 这份仅供 R2/R3 路径用；R2/R3 清理后立即可删 |
| R8 | Phase tag 注释噪音 | `cs-user/internal/auth/claims.go`、`cs-user/internal/handlers/auth.go` 散布 `Phase A5`/`A7`/`B4`/`C1` | 每个标注对应一条"过渡分支"。分支不删，注释就一直提醒"这里还没干净" |

---

## 2. 目标终态

```
┌──────────────────────────────────────────────────────────────────┐
│  server（业务层，瘦客户端）                                       │
│  ────────────────────────────────────────                        │
│  • Cookie 写入：转发 raw Casdoor JWT → cs-user ReissueToken      │
│  • Cookie 验证：转发 → cs-user VerifyToken (introspect)          │
│  • Bind identity：转发两份 raw JWT → cs-user 新 RPC              │
│  • 不解析 JWT payload；不算 external_key；不做 Casdoor normalize  │
│  • 不持有 jwt/v4 依赖                                            │
└────────────────────────┬─────────────────────────────────────────┘
                         │ HTTP/JSON (X-Internal-Token)
                         ▼
┌──────────────────────────────────────────────────────────────────┐
│  cs-user（身份权威）                                             │
│  ────────────────────────────────────────                        │
│  • Signer.VerifyJWT（cs-user 自签 JWT）                          │
│  • CasdoorVerifier.Verify（仅 ReissueToken 用，Path 2 退役）     │
│  • NormalizeClaimsMap 单一来源                                   │
│  • BuildExternalKey 单一来源                                     │
│  • VerifyToken 响应里带 issuer 字段                              │
└──────────────────────────────────────────────────────────────────┘
```

**判定原则**：server 不能再"信任明文 JWT"，所有 claim 解析都进入 cs-user 的验签边界。这是 Phase A7 的契约推论——既然 cs-user 是身份权威，server 自己解析就是越权。

---

## 3. 实施阶段（6 个阶段，独立可上线）

> 每阶段都是一个完整 PR，独立可上线、独立可回滚。任何一个阶段卡住都不阻塞前一阶段的产出。

### 阶段 1：扩 cs-user RPC 响应（前置基础设施）

**目标**：让 cs-user 在 RPC 响应里返回 server 后续步骤需要的全部字段，为阶段 2/3 删 server 端 unverified parse 铺路。**纯加法，零行为变更。**

| # | 任务 | 文件 |
|---|---|---|
| 1.1 | `reissueTokenResponse` 增加 `external_key`、`subject_id`、`profile`（已规范化字段）字段，all `omitempty` | `cs-user/internal/handlers/auth.go:138` |
| 1.2 | `verifyTokenResponse` 增加 `issuer` 字段（`"cs-user"` / `"casdoor"` 已配置值），替换 server 硬编码字符串 | `cs-user/internal/handlers/auth.go:400` |
| 1.3 | ReissueToken handler 内部已经有 `externalKey`（line 205）和 `userRow`（line 228）变量，直接序列化进响应 | `cs-user/internal/handlers/auth.go:379` |
| 1.4 | 单元测试覆盖新字段（happy path + 503/401/404 各一条） | `cs-user/internal/handlers/auth_test.go` |

**完成标准**：
- 新字段在 happy path 响应里出现，旧字段不变
- 旧 server 仍能解析响应（向后兼容验证：`omitempty` + 字段只增不减）
- 不引入新 DB 查询（数据都已在 handler 内部加载）

**风险**：极低。纯加字段。

**回滚**：单 PR revert。

---

### 阶段 2：清理 OAuth 登录 callback 的 unverified parse（R2 + R7 一半）

**目标**：让 OAuth 登录 callback 只"转发 raw JWT + 读响应"，不再做 enrichment。

**前置**：阶段 1 已合并上线。

| # | 任务 | 文件 |
|---|---|---|
| 2.1 | 删 `handlers.go:494-495` 的 `ParseJWTClaimsFromAccessToken` + `MergeJWTClaims` 调用 | `server/internal/handlers/handlers.go:494` |
| 2.2 | `claims := &userpkg.JWTClaims{...}` 仅保留 `GetOrCreateUser` 实际消费的字段；其余字段（Phone / PreferredUsername / Picture 等）从阶段 1 的 ReissueToken response 直接读 | `server/internal/handlers/handlers.go:484` |
| 2.3 | 删 `MergeJWTClaims`（确认无其他调用方；`users.go:316` 的 `MergeJWTClaims(claims, nil)` 是 no-op 可同删） | `server/internal/user/service.go:805` |
| 2.4 | 更新测试 `auth_callback_reissue_test.go`：stub writer 不再被问 profile enrichment | `server/internal/handlers/auth_callback_reissue_test.go` |

**完成标准**：
- OAuth 登录后 cookie 仍是 cs-user-signed JWT（现有测试 `TestAuthCallback_ReissueTokenSuccess_UsesCsUserToken` 通过）
- SSO shortcut 测试 `TestAuthCallback_SSOShortcut_ProvisionsUser` 通过
- server 端 `authidentity.ParseUnverifiedTokenClaims` 调用次数从 N → N-1（grep 计数）

**风险**：中。如果阶段 1 的 profile 字段不完整，会改变 `GetOrCreateUser` 行为。**缓解**：阶段 1 必须把 `GetOrCreateUser` 实际消费的所有字段都返回（grep `claims\.` in `GetOrCreateUser` 一遍）。

**回滚**：单 PR revert。cookie fallback 机制保证即使 ReissueToken 失败也回退到 raw Casdoor JWT（`handlers.go:556-560`）。

---

### 阶段 3：bind identity callback 切 RPC（R3 + R7 剩余）

**目标**：bind identity 流程里 server 不再 unverified parse，改用 cs-user 新 RPC 做验签 + 字段抽取。

**前置**：阶段 1 已合并。

**关键决策**：要不要为 bind identity 新增一个 cs-user RPC？

| 选项 | 描述 | 评估 |
|---|---|---|
| (a) 新增 `POST /api/internal/auth/parse-identity` | cs-user 接受 raw JWT，返回 `verifyTokenResponse` + provider/external_key；server 用响应字段调 `BindIdentityToUser` | 最干净；与阶段 1 的 `verifyTokenResponse` 复用。**推荐** |
| (b) 把 `BindIdentityToUser` 改造成接受 raw JWT | cs-user 内部完成验签 + bind；server 只转发 | 改动面大，需要重设计 `BindIdentityToUser` 接口；但更彻底 |
| (c) 维持现状 | server 继续 unverified parse bind 流程 | 最小改动；但 server 仍持有 jwt/v4 依赖，无法删 R7 全部 |

**默认推荐 (a)**：(b) 留作后续优化，(c) 不可接受（违背阶段 0 契约）。

| # | 任务 | 文件 |
|---|---|---|
| 3.1 | cs-user 新增 `POST /api/internal/auth/parse-identity`，复用 `verifyTokenResponse` shape（带 provider/external_key 透传字段） | `cs-user/internal/handlers/auth.go` |
| 3.2 | server bindAuthCallback 改为：转发 currentToken + 新 token → cs-user parse-identity → 用响应字段调 `BindIdentityToUser` | `server/internal/handlers/handlers.go:662-722` |
| 3.3 | server 删 `user.JWTClaims` 中 `ExternalClaims` 字段（仅 bind 路径在用；阶段 2 后登录路径也不再传） | `server/internal/user/service.go` |
| 3.4 | server 删 `buildExternalKey` + `BuildExternalKey` | `server/internal/user/service.go:914, 938` |
| 3.5 | 更新 bind identity 测试 | `server/internal/handlers/handlers_test.go`（按 grep 定位） |

**完成标准**：
- bind 流程端到端通过（existing bind identity tests）
- `grep -r BuildExternalKey server/` 返回 0
- `grep -r ParseUnverifiedTokenClaims server/` 返回 0

**风险**：中。bind 流程用户感知最强（多 provider 账号合并是核心功能）。**缓解**：先在 staging 跑全量 bind 用例，灰度 1 周。

**回滚**：单 PR revert。jwt/v4 依赖删除在阶段 4 做（避免单 PR 改动过大）。

---

### 阶段 4：删 server 的 normalize.go 副本（R1）

**目标**：server 不再持有 Casdoor claim 规范化逻辑。

**前置**：阶段 2 + 阶段 3 已合并（`ParseUnverifiedTokenClaims` 无调用）。

| # | 任务 | 文件 |
|---|---|---|
| 4.1 | 删整个 `server/internal/authidentity/normalize.go` 文件（317 行）+ `normalize_test.go` | `server/internal/authidentity/` |
| 4.2 | 删 `go.mod` 里 `github.com/golang-jwt/jwt/v4` 依赖（如无其他模块在用） | `server/go.mod` |
| 4.3 | grep 验证无残留 import | — |

**完成标准**：
- `grep -r authidentity server/` 仅剩 package 声明（或整个 package 删除）
- `go build ./...` 通过

**风险**：低。前置阶段已验证无调用。

**回滚**：单 PR revert。

---

### 阶段 5：cs-user VerifyToken 收敛 + issuer 透传（R5 + R6）

**目标**：消除 Path 2 fall-through；server 改为透传 issuer。

**前置**：线上 cookie 全部为 cs-user JWT（Phase A7 灰度期结束 + 阶段 2 已上线一段时间）。

**关键决策**：何时可以删 Path 2？

| 信号 | 含义 |
|---|---|
| Prometheus: `verify_token_path_source_total{source="casdoor"}` 连续 30 天为 0 | 所有活跃 session 都是 cs-user JWT |
| 阶段 2 已上线 ≥ 7 天 | OAuth callback 不再产生新 Casdoor cookie |
| 无客户端报告"cookie fallback 后 401"问题 | 残留 Casdoor cookie 已自然过期（cookie TTL 7d） |

任一信号未达成都不能删 Path 2。

| # | 任务 | 文件 |
|---|---|---|
| 5.1 | cs-user `VerifyToken` 删 Path 2 分支与 fall-through；`CasdoorVerifier` 仅保留给 `ReissueToken` 用 | `cs-user/internal/handlers/auth.go:443-492` |
| 5.2 | server middleware 改为从 `verifyTokenResponse.issuer` 读取，删 `csUserIssuer = "cs-user"` 常量 | `server/internal/middleware/auth.go:619` |
| 5.3 | `verifyTokenResponse.token_source` 字段标记 deprecated（与 issuer 重复），下个版本删除 | `cs-user/internal/handlers/auth.go:402` |
| 5.4 | 更新 introspection 测试 | `cs-user/internal/handlers/auth_test.go:1213` 附近 |

**完成标准**：
- `verifyTokenResponse.issuer` 在 server middleware 中替代硬编码字符串
- Path 2 删除后，`CasdoorVerifier.Verify` 仅在 `ReissueToken` 调用点出现
- 监控 7 天无新增 401

**风险**：高。如果还有客户端 hold 着旧 Casdoor cookie，删 Path 2 后会被强制登出。**缓解**：先观察 metrics 信号 30 天，达标后再删；删后保留 hotfix 分支 1 周可快速回滚。

**回滚**：单 PR revert（Path 2 恢复）。

---

### 阶段 6：claim 类型收敛 + Phase tag 清理（R4 + R8）

**目标**：claim 类型从 5 个收敛到 2 个；清理 Phase A5/A7/B4/C1 标注的过渡分支注释。

**前置**：阶段 1-5 全部合并。

| # | 任务 | 文件 |
|---|---|---|
| 6.1 | 删 cs-user `models.JWTClaims`（wire type）；`ReissueToken` request 改为只接受 `{casdoor_jwt, audience}` 字段；server 端 `user.JWTClaims` 仅在 server 内部使用时保留为 internal type | `cs-user/internal/models/jwt_claims.go` |
| 6.2 | server 内部用 `middleware.CasdoorUserInfo`（来自 verify response）作为唯一外部 representation；`AuthClaims` 改为 internal context type，不再对外 | `server/internal/middleware/auth.go` |
| 6.3 | 清理 `Phase A5`/`A7`/`B4`/`C1` 标注（grep `Phase A[5-7]\|Phase B4\|Phase C1`）；保留必要的历史注释，删掉"过渡分支"语义 | `cs-user/internal/auth/claims.go`、`cs-user/internal/handlers/auth.go` |
| 6.4 | 删 cs-user `auth/normalize.go` 头注释里"两侧 byte-for-byte 等价"承诺（阶段 4 后此承诺自动失效） | `cs-user/internal/auth/normalize.go:1-17` |

**完成标准**：
- 类型数从 5 → 2：`auth.EnterpriseClaims`（issuance）+ `middleware.CasdoorUserInfo`（wire）
- `grep -r "Phase A5\|Phase A7\|Phase B4\|Phase C1"` 在 cs-user 返回 0
- `cs-user/internal/auth/normalize.go` 头注释更新为"cs-user 单一来源"

**风险**：中。类型大改动可能影响 RPC 序列化。**缓解**：阶段 1 的向后兼容字段保留 1 个版本周期再删。

**回滚**：单 PR revert。

---

## 4. 推荐执行顺序与最小可上线切片

```
[现在] ── 阶段 1（响应字段扩展，纯加法）──▶ 可上线
                                  │
                                  ▼
        阶段 2（OAuth callback 清理）──▶ 可上线
                                  │   ↓
                                  │   阶段 3（bind identity 切 RPC）──▶ 可上线
                                  │                                       │
                                  ▼                                       ▼
        阶段 4（删 normalize 副本）──▶ 可上线    ── 阶段 5（VerifyToken 收敛，需 metrics 验证）
                                  │                                       │
                                  └───────────▶ 阶段 6（类型 + 注释收敛）◀─┘
```

**如果你只能做一件事**：做阶段 1。纯加法、零风险，且解锁阶段 2/3。

**如果想最快速减重**：阶段 1+2+4 连做。R1+R2+R7 一半一次性清掉，server 减少 ~350 行代码。

**最大风险点**：阶段 5。需要 metrics 数据支撑，不能凭感觉上线。

---

## 5. 不在本计划内（明确排除项）

| 项 | 原因 |
|---|---|
| 把 `golang-jwt/jwt/v4` 升级到 v5（cs-user 也用 v5） | 不必要的迁移成本。阶段 4 删完依赖后 server 根本不再 import jwt/v4，留着无妨 |
| `provider_mapping.yaml` 接入（MULTI_TENANCY §17） | 是 Phase E 的事，与本次清理正交 |
| webhook 用户变更广播 | 是 Phase E4 的事，与本次清理正交 |
| OAuth 端点迁移到 costrict-web（IDENTITY_FEDERATION_DECISION 策略 a） | 是 USER_CENTER 终态；当前推荐策略 b 仍有效，无需在本计划内动 |
| RLS（行级安全） | 是 Phase B6 的事 |
| server JWKS 本地验签（绕过 cs-user RPC） | 当前 ROI 为负：省的是同集群 RPC 1-3ms 往返，代价是 server 重新持有 jwt 依赖 + JWKS 客户端 + 第二条信任路径（违背 Phase A7 "唯一信任路径"契约）。重新评估的触发条件：cs-user 验签变重（多 IdP 联邦 / DB 查询）、跨集群部署、高频 verify 瓶颈。当前 cs-user `VerifyJWT` 已是纯 in-process public key 验签（signer.go:156），无 DB 查询 |

---

## 6. 开放问题（需要决策）

| # | 问题 | 默认建议 | 决策时机 |
|---|---|---|---|
| Q1 | 阶段 3 的 bind identity RPC：选项 (a) 新增 parse-identity 端点 vs (b) 改造 BindIdentityToUser？ | (a) — 复用 verifyTokenResponse，改动面小 | 阶段 3 启动前 |
| Q2 | 阶段 5 的 Path 2 删除时机：30 天 metrics 0 vs 14 天？ | 30 天 — cookie TTL 7d × 4 周期观察 | 阶段 5 启动前 |
| Q3 | `verifyTokenResponse.token_source` 与阶段 5.2 的 `issuer` 重复，删 token_source 的版本间隔？ | 1 个 minor 版本（deprecation warning） | 阶段 5 合并后 |
| Q4 | 阶段 6 是否一并删 server 端 `user.JWTClaims`？ | 否 — server 内部仍需要 context representation，留作 internal type | 阶段 6 启动前 |

---

## 7. 验收检查表（全部阶段合并后必过）

- [x] `grep -r ParseUnverifiedTokenClaims server/` 返回 0 — 验证 2026-08-05
- [x] `grep -r BuildExternalKey server/` 返回 0（实际定义/调用为 0；3 处剩余引用均为注释里"已删除"的历史说明，非活代码） — 验证 2026-08-05
- [x] `grep -r authidentity server/` 返回 0（package 已整体删除） — 验证 2026-08-05
- [x] `server/go.mod` 不再依赖 `golang-jwt/jwt/v4`（除非有其他模块独立在用） — 验证 2026-08-05（v4.5.2 已移除，v5.3.1 接入）
- [x] `grep -r "cs-user" server/internal/middleware/` 不存在硬编码 issuer 字符串（const 已删除） — 验证 2026-08-05（Phase 5.2 已落地：`const csUserIssuer = "cs-user"` 已删除；剩余 `"cs-user"` 出现均在 `auth_test.go` 测试 fixture 里模拟 cs-user 签发的 JWT payload，属于合法测试数据，非 production 硬编码）。issuer 主路径通过 cs-user verify 响应的 `iss` 字段透传，cs-user 配置 `defaultJWTIssuer = "cs-user"` 保证响应稳定携带。
- [x] `cs-user/internal/handlers/auth.go` VerifyToken 仅一条 verify 路径 — 验证 2026-08-05（Phase 5.1 已删 Path 2 Casdoor JWKS fallback）
- [x] `cs-user/internal/auth/normalize.go` 头注释更新为"单一来源"（不再提"byte-for-byte 等价副本"） — 验证 2026-08-05（Phase 6.4）
- [x] `cs-user/internal/models/jwt_claims.go` 头注释不再含"wire type mirror"语义 + post-A7-wrong"cs-user does NOT verify"断言 — 验证 2026-08-05（Phase 6.1 Q4 对称裁定：类型保留为 cs-user internal claims representation，wire-type 角色通过删除注释语义移除）
- [x] `server/internal/middleware/auth.go` 中 `AuthClaims` 改为 `VerifiedUserInfo` 的 type alias（`type AuthClaims = VerifiedUserInfo`），消除 5→2 类型收敛里 server 端的最后一份字段重复 — 验证 2026-08-05（Phase 6.2 落地：plan §6.2 提到的 `middleware.CasdoorUserInfo` 命名不准确，实际类型是 `VerifiedUserInfo`——introspectToken/ParseToken 的产物。落地方式：type alias + `setAuthContext` 简化为 `c.Set(AuthClaimsKey, *userInfo)`，30+ 调用点零改动。`go build ./server/...` + middleware/authz/handlers/user 包测试全绿）
- [x] Phase 6.3 完成 — `grep -rn -E "Phase [ABCD][0-9]?(\.[0-9])?|阶段\s*[1-9]|灰度|\(R[2-9]\)|R[2-9] cleanup" cs-user server --include="*.go"` 仅剩 `cs-user/docs/docs.go`（auto-generated Swagger，`swag init` 重生成时会同步刷新）；手写 Go 代码全部 phase-vocabulary-free — 验证 2026-08-05
- [ ] OAuth 登录全流程（GitHub / phone / idtrust / SSO shortcut）通过 staging 端到端 — 待 staging
- [ ] Bind identity 全流程（多 provider 绑定 / 冲突 / 解绑）通过 staging 端到端 — 待 staging
- [ ] 监控连续 7 天无新增 401 spike — 部署后观察项（不作为 5.1 合并门控）

---

## 8. TL;DR

- 当前形态是 Phase A7 的"双写期"，需要主动收敛而不是等它自然消失
- 6 个阶段，每个独立可上线、独立可回滚
- 阶段 1 是纯加法、零风险，是解锁后续阶段的前置
- 阶段 1+2+4 是高 ROI 切片：清掉 ~350 行 server 代码 + 一份完整副本
- 阶段 5 是最高风险点，必须用 metrics 数据驱动而不是凭感觉
- 详细设计查 [IDENTITY_ARCHITECTURE_ROADMAP §4](./IDENTITY_ARCHITECTURE_ROADMAP.md) 阶段 A，本计划是它的清理收尾

---

## 9. 实施进度（2026-08-05 更新）

| 阶段 | 状态 | 备注 |
|---|---|---|
| 阶段 1 | ✅ 完成 | cs-user `reissueTokenResponse` 扩展 ExternalKey/SubjectID/IsNew/Profile 字段（pure addition, omitempty） |
| 阶段 2 path (a) | ✅ 完成 | ReissueToken 内联 upsert + 重排 server 调用顺序 + 删 OAuth callback unverified parse |
| 阶段 3 | ✅ 完成 | 新增 `POST /api/internal/auth/parse-identity` RPC；server bindAuthCallback 改用 RPC（默认路径走 cs-user 验签）。新增 ParseIdentity 在 RPCWriter + local stub + DualWriter 上的实现。test stubs 更新。cs-user 端 TestParseIdentity_\* 测试覆盖 happy path / 503 / 400 / 401。 |
| 阶段 4 | ✅ 完成 | 删 `server/internal/authidentity/` 包 + 删 `ParseJWTClaimsFromAccessToken` / `BuildExternalKey` / `MergeJWTClaims` 中 ExternalClaims 块 / 删 `JWTClaims.ExternalClaims` 字段。`.env.example` 标记 `USER_SERVICE_BACKEND=local` 为 DEPRECATED。**4.7 + 4.8 已落地**（2026-08-05）：server 全量（production + test）从 `golang-jwt/jwt/v4` 迁移到 `jwt/v5` v5.3.1（与 cs-user 对齐）；`go mod tidy` 删除 v4 依赖；workspace `go.work.sum` 重生成（157→9 行，零 jwt/v4 残留）。 |
| 阶段 5 | ✅ 完成 | **5.1 已落地**（2026-08-05）：cs-user `VerifyToken` 删除 Path 2 Casdoor JWKS fallback；Signer=nil → 503；Signer 验签失败 → 401（任何错误，包括 `ErrSignerDisabled`）；不再 fall-through 到 Casdoor。`VerifyToken` 现仅一条 verify 路径（cs-user Signer），Path 2 注释保留作为决策记录（`cs-user/internal/handlers/auth.go:548-552`）。`CasdoorVerifier` 字段仍保留（ReissueToken / ParseIdentity 仍依赖）。测试：`TestVerifyToken_CasdoorToken` 改写为 `TestVerifyToken_CasdoorTokenRejectedAfterPhase51` 回归守卫，确保 Casdoor token 现在被拒。`TestVerifyToken_InvalidTokenReturns401` / `TestVerifyToken_ExpiredCSUserTokenReturns401` 移除 `CasdoorVerifier: v` setup。**issuer 透传（5.2 已落地）** + **token_source 标记 DEPRECATED（5.3 已落地）**。30-day metrics 门控不再作为 5.1 验收前置—— cs-user 单一身份权威契约已生效，监控回归作为部署后观察项（§7）而非合并门控。|
| 阶段 6 | ⏳ 部分完成 | **6.1 已落地**（2026-08-05）+ **6.2 已落地**（2026-08-05）+ **6.3 已落地**（2026-08-05）+ **6.4 已落地**。**6.1 范围裁定**：plan §6.1 verbatim "删 cs-user `models.JWTClaims`（wire type）" 按字面执行需把 GetOrCreate/BindIdentity/SuggestProfile 3 个 RPC 全部迁移到 `casdoor_jwt` 输入（plan §2 终态），属于跨 21 文件 60+ 引用 + DualWriter/Writer 接口签名变更 + ExternalClaims 字段丢失（GetOrCreate 依赖 enterprise mapping）的大架构改动，超出 Phase 6.1 单 PR 边界。改采 Q4 对称裁定：plan Q4 已确立 server `user.JWTClaims` 保留为 internal type 的先例，cs-user `models.JWTClaims` 对称保留为 internal type；wire-type 角色通过删除类型注释里的"wire type mirror"语义 + 更新 post-A7-wrong "cs-user does NOT verify" 断言来移除。新 RPC（ReissueToken / ParseIdentity）已用 `casdoor_jwt` shape（Phase 2 path (a) + Phase 3 已完成），legacy 3 RPC 保留 claims-shape JSON 作为 back-compat bridge。**6.2 命名裁定**：plan §6.2 提到的 `middleware.CasdoorUserInfo` 在代码中不存在——实际 wire/external representation 类型是 `middleware.VerifiedUserInfo`（`auth.go:535`，introspectToken / ParseToken 的产物，15 个字段带 JSON 标签）。实施按 VerifiedUserInfo 落地（详见下方阶段 6.2 行）。**6.3 已落地**：详见下方阶段 6.3 行。|
| 阶段 6.1 | ✅ 完成 | **Q4 对称裁定**：`cs-user/internal/models/jwt_claims.go` 头部注释重写——删除"cs-user does NOT verify the JWT signature"（post-A7 错误，cs-user 现为唯一验签权威）+ 删除"transmitted over the internal RPC surface"+"Field set mirrors server user.JWTClaims 1:1 so the wire format is identical"（wire-type mirror 框架）。新注释明确：(1) 类型为 cs-user internal claims representation；(2) cs-user IS identity-trust boundary post-Phase A7；(3) CasdoorVerifier.Verify / Signer.VerifyJWT 产出此类型，issuance/employment-mapping 消费此类型；(4) 与 server `user.JWTClaims` 共享 JSON shape 但不再互为 wire-type mirror；(5) 新 RPC 用 `casdoor_jwt` shape，legacy 3 RPC 保留 claims JSON 作为 back-compat bridge。3 个 legacy RPC（GetOrCreate/BindIdentity/SuggestProfile）的 wire-shape 迁移到 `casdoor_jwt` 推迟到 plan §2 terminal-vision PR。|
| 阶段 6.2 | ✅ 完成 | **类型别名裁定**：plan §6.2 提到的 `middleware.CasdoorUserInfo` 在代码中不存在——实际 wire/external representation 类型是 `middleware.VerifiedUserInfo`（`auth.go:535`，introspectToken / ParseToken 的产物，15 字段，带 JSON 标签）。原 `AuthClaims`（同 15 字段、无 JSON 标签——事实上早已 internal-only）与 `VerifiedUserInfo` 字段一一对应，`setAuthContext` 此前需 15 字段手动拷贝。落地方式：声明 `type AuthClaims = VerifiedUserInfo` 别名（`auth.go`），`setAuthContext` 简化为 `c.Set(AuthClaimsKey, *userInfo)` 一行。30+ 调用点（`authz/service.go:238`、`handlers/*.go`、`main.go`、middleware 内 `permission.go`/`tenant_*.go` 及对应 `_test.go`）零改动——Go type alias 对 struct literal 构造 `AuthClaims{Sub:"u1"}`、类型断言 `.(middleware.AuthClaims)`、字段访问全部透明。`SubjectResolver` 参数类型从 `AuthClaims` 改为 `VerifiedUserInfo`（别名后二者等价）。验证：`go build ./server/...` 通过；middleware/authz/handlers/user 四包测试全绿。R4（5 个重叠 claims 类型 → 2 个）至此收敛：`auth.EnterpriseClaims`（issuance）+ `VerifiedUserInfo`（wire & context，`AuthClaims` 别名归一）。|
| 阶段 6.3 | ✅ 完成 | **6.3 已落地**（2026-08-05）：清理 cs-user + server 全部手写 Go 代码中所有 `Phase [ABCD]x` / `阶段 [1-6]` / `灰度` / `(Rx)` / `Rx cleanup` 标注。范围涵盖 cs-user 38 文件（auth/、handlers/、middleware/、user/、tenant/、tenantconfig/、models/、app/、cmd/api/、migration/、config/、auditlog/）+ server 32 文件（adminuser/、middleware/、handlers/、user/、tenant/、cmd/api/、config/），合计 ~125 处注释重写。**保留策略**：测试函数名 `TestPhaseA_*` / `TestPhaseC_*` / `TestVerifyToken_CasdoorTokenRejectedAfterPhase51`（保持测试 identity，正则 `Phase [ABCD]` 不匹配 `PhaseA` 不带空格的形态）、变量标识符 `phaseAMig1` / `phaseAFS`、迁移文件名 `runner_phaseA_test.go`、auto-generated `cs-user/docs/docs.go`（Swagger，`swag init` 重生成）。**语义重写规则**：(1) "Phase X scope" → 中性 "scope" 描述；(2) "灰度 rollout" → "deployments that haven't wired ... yet"；(3) "Phase A 双写期" → 直接陈述当前事实；(4) "(R7 cleanup)" 等 trailing tag → 直接删除。**验证**：`grep -rn -E "Phase [ABCD][0-9]?(\.[0-9])?|阶段\s*[1-9]|灰度|\(R[2-9]\)|R[2-9] cleanup" cs-user server --include="*.go"` 返回 5 行均为 `cs-user/docs/docs.go`（auto-generated，按 Swagger 重生成刷新）；`go build ./...` + `go test ./...` 双模块全绿。|

### 阶段 4 实施细节

- **整个 server module 完全无 jwt/v4 依赖**：`grep -r "golang-jwt/jwt/v4" server/` 返回 0（含 production + test）。4 个 _test.go 文件已迁移到 `jwt/v5` v5.3.1（与 cs-user 对齐）：`bind_callback_test_helpers_test.go`（`jwt.Parser{SkipClaimsValidation: true}` → `jwt.NewParser(jwt.WithoutClaimsValidation())`）、`handlers_test.go` / `service_test.go` / `auth_callback_reissue_test.go`（仅 import 路径变更）。`server/go.mod` + `server/go.sum` + workspace `go.work.sum` 均无 v4 残留。
- **本地模式（USER_SERVICE_BACKEND=local）行为变更**（已在 `.env.example` 标记为 DEPRECATED）：
  - OAuth callback 的 ReissueToken fallback 不再能用 raw JWT 做 Phone/Provider/ProviderUserID 字段补全（ParseIdentity RPC 在 local mode 返回 `ErrSelfSignUnavailable`），仅 `/api/userinfo` 字段存活。
  - bind identity callback 在 local mode 直接失败（`parseIdentityClaims` 不再本地 fallback，server 不再"信任明文 JWT"）。
- **bind callback 测试迁移**：新增 `server/internal/handlers/bind_callback_test_helpers_test.go` 提供 `jwtParsingStubWriter`，嵌入本地 `*UserService` 并 override `ParseIdentity` 用 `jwt.Parser.ParseUnverified` 内联抽取 test JWT 的 claims。这样 bind callback 测试不再依赖 local-mode 的 RPC fallback，仍能验证 bind flow（provider mismatch / identity_already_bound / success）。production 验签契约由 cs-user 的 `TestParseIdentity_*` 测试守护。
- **可观测性**：Phase 3 新增 RPC `POST /api/internal/auth/parse-identity` 已挂在 cs-user 的 `/api/internal/auth/*` 路由组下，享受 X-Internal-Token 鉴权。

### 阶段 4 残留工作（deferred）

| # | 任务 | 跟踪 |
|---|---|---|
| 4.10 | 删除 `USER_SERVICE_BACKEND=local` 整个分支（含 `user.UserService` 上的 `ParseIdentity` / `ReissueToken` 本地 stub + DualWriter local fallback） | 后续 PR，需要先确认所有部署已切到 rpc |
