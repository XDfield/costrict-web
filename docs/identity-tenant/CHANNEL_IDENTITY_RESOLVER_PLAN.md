# 双向渠道身份解析重构计划

**状态**：Proposal · 2026-08-05
**前置**：Phase A7 已落地（cs-user 接管身份 sign + verify，server 侧 `user_auth_identities` 表进入"僵尸副本"状态），见 [IDENTITY_ARCHITECTURE_ROADMAP §4 阶段 A](./IDENTITY_ARCHITECTURE_ROADMAP.md)。
**目的**：把 `channel.ChannelService`（双向渠道：webhook 入站 + 出站发送）对 `user_auth_identities` 的 4 处本地查询切到 cs-user，让 cs-user 成为唯一的身份权威；同步移除 server 端"配置即身份"的 `enrichWecomChannelConfig` 残留逻辑。
**读者**：实施者、评审者。
**本文不修改**任何已有提案决策，只做清理编排。
**范围声明**：仅覆盖 `channel.ChannelService`（wecom / wecom-bot / wechat 双向链路）。`notification.NotificationService`（单向推送）不在本次范围。

---

## 1. 现状（已确认的 4 处本地查询）

`channel.ChannelService` 直接打 server 本地 DB 查 `user_auth_identities` 表的 4 处：

| # | 方向 | 文件:行 | 触发条件 / 现状 |
|---|---|---|---|
| Q1 | 反向（`provider_user_id → subject_id`） | `server/internal/channel/service.go:117-119` | `HandleWebhook` 入站时，根据 webhook payload 里的 `externalUserID`（即 idtrust 的 `provider_user_id`）反查平台 `subject_id` 用于路由消息到 conversation |
| Q2 | 正向（`subject_id → provider_user_id`） | `server/internal/channel/service.go:343-347` | `ListConfigs` 自动建配置判定：若用户有 idtrust 绑定但无 wecom 配置，自动建一条默认 config |
| Q3 | 正向 | `server/internal/channel/service.go:414-416` | `enrichWecomChannelConfig`：把 `provider_user_id` 注入 `config.userId`；查不到就把 `Enabled` 强制设为 `false` |
| Q4 | 正向 | `server/internal/channel/service.go:533-535` | `CreateSenderForUser` 出站发送时，根据 `subject_id` 解析 `externalUserID`（企微 userId）下发给渠道 adapter |

**问题**：

1. **数据漂移**：Phase A7 之后 `user_auth_identities` 在 server 侧已经是僵尸副本。用户在 cs-user 解绑 / 重绑 idtrust 后，server 本地表不会同步，Q1-Q4 的判定全部失真。
2. **职责越权**：`enrichWecomChannelConfig`（Q3）把"用户身份"塞进"渠道配置"，导致 config 表既是配置又是身份快照。前端 POST 一个 userId 进来会被无条件覆盖；身份变化后旧 config 静默失效。
3. **生产 bug 直接表现**：`/api/channels` 对 idtrust-bound 用户无法自动创建 wecom / wecom-bot config（Q2 判定走的是僵尸表）。

---

## 2. 目标终态

```
┌──────────────────────────────────────────────────────────────────┐
│  channel.ChannelService（双向渠道业务）                          │
│  ────────────────────────────────────────                        │
│  • HandleWebhook：externalUserID → IdentityResolver → subject_id │
│  • ListConfigs：subject_id → IdentityResolver → 有无绑定         │
│  • CreateSenderForUser：subject_id → IdentityResolver →          │
│    externalUserID（发送时按需解析，不缓存进 config）             │
│  • Config 不再持有 userId；Enabled 由用户控制，不再被身份强制改写│
│  • 不直接打 user_auth_identities 表                              │
└────────────────────────┬─────────────────────────────────────────┘
                         │ 接口（IdentityResolver，本地或 RPC）
                         ▼
┌──────────────────────────────────────────────────────────────────┐
│  server/internal/user                                            │
│  ────────────────────────────────────────                        │
│  • IdentityResolver 接口（两个方法，正反向）                     │
│  • UserService 实现（local 模式：本机 DB，仅单进程部署保留）     │
│  • RPCClient 实现（rpc 模式：HTTP → cs-user，30s 缓存 fail-open）│
└────────────────────────┬─────────────────────────────────────────┘
                         │ HTTP/JSON (X-Internal-Token)
                         ▼
┌──────────────────────────────────────────────────────────────────┐
│  cs-user（身份权威）                                             │
│  ────────────────────────────────────────                        │
│  • GET /api/internal/users/by-provider-user-id（新增）           │
│      ?provider=...&provider_user_id=...                          │
│      → 返回 bare user（含 subject_id）                           │
│  • GET /api/internal/users/:subject_id/auth-identities（已存在） │
│      → 返回 identity 列表，前端按 provider 过滤取 provider_user_id│
└──────────────────────────────────────────────────────────────────┘
```

**判定原则**：`channel.ChannelService` 不再拥有 `user_auth_identities` 的查询权。所有身份解析走 `IdentityResolver` 接口；接口实现由 main.go 根据 `USER_SERVICE_BACKEND` 注入 local 或 rpc 版本，与 `UserReader` 的注入路径完全对齐。

**接口设计**：

```go
// server/internal/user/identity_resolver.go（新文件）
type IdentityResolver interface {
    // ResolveProviderUserID 正向解析：subject_id + provider → provider_user_id
    // 用途：Q2（绑定存在性）、Q4（出站发送）
    // 错误：gorm.ErrRecordNotFound 表示该用户没有此 provider 的绑定
    ResolveProviderUserID(ctx context.Context, subjectID, provider string) (string, error)

    // ResolveSubjectByProviderUser 反向解析：provider + provider_user_id → subject_id
    // 用途：Q1（webhook 入站路由）
    // 错误：gorm.ErrRecordNotFound 表示该 externalUserID 未绑定任何平台用户
    ResolveSubjectByProviderUser(ctx context.Context, provider, providerUserID string) (string, error)
}
```

接口为 provider-agnostic（参数化 provider 字符串，不写死 "idtrust"），为未来接入 SSO / 其他企业身份源留余地。

---

## 3. 决策摘要（已与产品方对齐）

| 编号 | 决策点 | 选择 | 理由 |
|---|---|---|---|
| D1 | 接口位置 | `server/internal/user/IdentityResolver` | 与 `UserReader` 同包，main.go 注入路径复用 `userModule.Reader.(*RPCClient)` 已有的 type-assertion 模式 |
| D2 | cs-user 新端点 | `GET /api/internal/users/by-provider-user-id?provider=...&provider_user_id=...` | 已有的 `by-identity` 用的是 Casdoor 的 `universal_id`，与 channel 需要的 `provider_user_id` 不是同一字段；不复用、不重载 |
| D3 | 缓存策略 | 30s TTL，fail-open，仅缓存成功结果 | 与 `RPCClient.byIdentityCacheTTL` 完全对齐；cs-user 抖动时 fail-open 让短期不可用不级联成 5xx |
| D4 | 移除 `enrichWecomChannelConfig` | 完全删除，config 不再带 userId | "配置"与"身份"职责分离；出站发送时按需通过 IdentityResolver 解析 externalUserID，不在 config 里固化身份快照 |
| D5 | `WebhookVerified` 字段语义 | 不变；仍由系统在首次入站帧时翻转，标记绑定完成 | 与本次重构正交，仅记录以免误改 |

---

## 4. 实施阶段（5 个阶段，独立可上线）

> 每阶段都是一个完整 PR，独立可上线、独立可回滚。任何一个阶段卡住都不阻塞前一阶段的产出。

### 阶段 1：cs-user 新增 by-provider-user-id 端点（前置基础设施）

**目标**：让 cs-user 暴露 `provider_user_id → user` 的反向查询能力，为阶段 2/3 的 RPC client 提供下游。**纯加法，零行为变更。**

| # | 任务 | 文件 |
|---|---|---|
| 1.1 | `UserService` 接口增加 `GetUserByProviderUserID(ctx, provider, providerUserID) (*models.User, error)` 方法 | `cs-user/internal/handlers/users.go:37-85`（接口定义） |
| 1.2 | `userService`（生产实现）实现该方法：`WHERE provider = ? AND provider_user_id = ? AND deleted_at IS NULL`，取 `user_auth_identities` 第一条命中 join `users`；未命中返回 `gorm.ErrRecordNotFound` | `cs-user/internal/user/...`（具体文件由实施者定位） |
| 1.2b | `unavailableUserService`（stub 实现）返回 `errServiceUnavailable` | `cs-user/internal/app/app.go:359-429` |
| 1.3 | 新增 handler `usersAPI.GetUserByProviderUserID`：query 参数校验 → 调 service → 404/200 响应（bare user body，与 `GetUserByIdentity` 同形） | `cs-user/internal/handlers/users.go`（参考 `:144-165` 既有 `GetUserByIdentity`） |
| 1.4 | 路由注册：`users.GET("/by-provider-user-id", usersAPI.GetUserByProviderUserID)`，放在 `/by-identity` 同一组静态字面路径里 | `cs-user/internal/app/app.go:175` 附近 |
| 1.5 | 单元测试覆盖：happy path / 404 / 缺参 400 / service unavailable 503 | `cs-user/internal/handlers/users_test.go` |

**完成标准**：
- happy path 返回 bare user JSON，结构与 `GET /by-identity` 一致
- 404 时 cs-user 返回 HTTP 404（让 server 侧 RPCClient 自动翻译成 `gorm.ErrRecordNotFound`）
- 旧端点行为零变化

**风险**：低。纯加端点。唯一注意点是路由顺序：`/by-provider-user-id` 必须在 `/:subject_id` 之类的捕获组之前注册，否则会被当成 subject_id 字面值。参考已有 `/by-identity` 的位置即可。

**回滚**：单 PR revert。

---

### 阶段 2：server/internal/user 落地 IdentityResolver 接口 + 双实现

**目标**：在 `server/internal/user` 包内提供 `IdentityResolver` 接口，并给出 `*UserService`（local）与 `*RPCClient`（rpc）两份实现。channel 模块此阶段不接入。

| # | 任务 | 文件 |
|---|---|---|
| 2.1 | 新建 `server/internal/user/identity_resolver.go`：定义 `IdentityResolver` 接口（两方法） | 新文件 |
| 2.2 | `*UserService` 实现 `IdentityResolver`：两方法都打 server 本地 `user_auth_identities` 表（保留作为单进程 local 模式 fallback） | `server/internal/user/service.go` |
| 2.3 | `*RPCClient` 实现 `IdentityResolver`：正向调用已存在的 `GET /api/internal/users/:subject_id/auth-identities` 后在客户端按 provider 过滤；反向前置阶段 1 的新端点 `GET /api/internal/users/by-provider-user-id` | `server/internal/user/rpc_client.go` |
| 2.4 | RPC 反向查询加 30s 缓存（key = `provider\|provider_user_id`），与现有 `byIdentityCache` 同结构：RWMutex + map + expiresAt；错误不缓存 | `server/internal/user/rpc_client.go`（参考 `:160-183` 既有 `byIdentityCacheGet/Set`） |
| 2.5 | 加 `InvalidateUserProviderCache(provider, providerUserID)` 公开方法，与既有 `InvalidateUserIdentityCache` 对称，供 admin 解绑 / 重绑后立即失效 | `server/internal/user/rpc_client.go:153` |
| 2.6 | 单测：local 实现命中 / 未命中；rpc 实现命中 / 404 / 5xx；缓存命中 / 过期 / 并发 | `server/internal/user/service_test.go` / `rpc_client_test.go` |

**完成标准**：
- `var _ IdentityResolver = (*UserService)(nil)` 与 `var _ IdentityResolver = (*RPCClient)(nil)` 两个编译期断言通过
- local 与 rpc 实现在等价输入下产出等价结果（共享表数据时）
- rpc 缓存 TTL = 30s，与 `byIdentityCacheTTL` 同常量来源（可抽 `rpcCacheTTL` 统一常量）

**风险**：低。接口纯加法；rpc 反向查询复用既有 `do()` 框架（`decodeBareUser` 策略 + 404 → `gorm.ErrRecordNotFound`）。

**回滚**：单 PR revert。

---

### 阶段 3：channel 模块接入 IdentityResolver，移除 enrichWecomChannelConfig

**目标**：把 `ChannelService` 的 4 处本地查询全部替换为 `IdentityResolver` 调用，删除 `enrichWecomChannelConfig` 函数与其调用点。

**前置**：阶段 2 已合并上线。

| # | 任务 | 文件:行 |
|---|---|---|
| 3.1 | `ChannelService` 结构体增加 `resolver user.IdentityResolver` 字段；`NewChannelService` 构造函数增加同名参数 | `server/internal/channel/service.go`（构造函数附近） |
| 3.2 | Q1 反向：`HandleWebhook` L117-119 改为 `subjectID, err := s.resolver.ResolveSubjectByProviderUser(ctx, "idtrust", msg.ExternalUserID)`；`errors.Is(err, gorm.ErrRecordNotFound)` 时按"未绑定用户"路径走（保持现有兜底逻辑） | `server/internal/channel/service.go:117` |
| 3.3 | Q2 正向：`ListConfigs` L343-347 改为 `_, err := s.resolver.ResolveProviderUserID(ctx, userID, "idtrust")`；`err == nil` 即 `hasIDTrust = true` | `server/internal/channel/service.go:343` |
| 3.4 | Q4 正向：`CreateSenderForUser` L533-535 改为 `externalUserID, err := s.resolver.ResolveProviderUserID(ctx, userID, "idtrust")`；未命中返回既有 `fmt.Errorf("no idtrust identity for user %s", userID)` | `server/internal/channel/service.go:533` |
| 3.5 | **删除** `enrichWecomChannelConfig` 函数整体（L411-437）及其在 `ListConfigs` / `SendTestMessage` 内的全部调用点 | `server/internal/channel/service.go:411-437` + 调用点 |
| 3.6 | config 不再写 `userId` 字段；wecom-bot 的 `botQRCode` 字段若是 `enrichWecomChannelConfig` 唯一注入方，改为构造 config 时直接注入（不依赖身份解析） | `server/internal/channel/service.go:430-432` |
| 3.7 | `Module` 与 `New` 函数（channel 包入口）增加 `resolver user.IdentityResolver` 形参，向下透传给 `ChannelService` | `server/internal/channel/channel.go` |
| 3.8 | 单测：`service_test.go` 用 stub `IdentityResolver`（map-driven fake）替换 `user_auth_identities` 表 seeding；覆盖 4 个调用点的命中 / 未命中分支 | `server/internal/channel/service_test.go` |

**完成标准**：
- `grep -n "user_auth_identities" server/internal/channel/` 零命中
- `grep -n "enrichWecomChannelConfig" server/internal/channel/` 零命中
- 所有 channel 包测试通过
- `/api/channels` GET 对 idtrust-bound 用户能正确自动创建 wecom / wecom-bot config（生产 bug 修复验证）

**风险**：中。
- **风险 A**：`enrichWecomChannelConfig` 删除后，前端如果有依赖 `config.userId` 字段渲染的逻辑会断。**缓解**：前端此前已确认只展示 enable/disable，userId 仅作发送时使用；发送链路改为出站时按需解析（Q4 改造点）。
- **风险 B**：测试改造工作量较大（4 个调用点 + 多分支）。**缓解**：stub resolver 设计成 map-driven，所有用例只需填 map，无需再 seed DB。

**回滚**：单 PR revert。回滚后 `enrichWecomChannelConfig` 恢复，但 cs-user 数据已经稳定，不影响线上。

---

### 阶段 4：main.go 注入 resolver，完成装配

**目标**：在 server 启动时根据 `USER_SERVICE_BACKEND` 把对应的 `IdentityResolver` 注入 `channel.New(...)`。

**前置**：阶段 3 已合并上线。

| # | 任务 | 文件:行 |
|---|---|---|
| 4.1 | `channelModule = channel.New(db, ..., resolver)` 增加实参 | `server/cmd/api/main.go:915` |
| 4.2 | resolver 来源：复用 `userModule.Reader` 的类型断言模式（`server/cmd/api/main.go:884`、`:154`），reader 本身已经实现 `IdentityResolver`（阶段 2 已落），直接传 `userModule.Reader.(userpkg.IdentityResolver)` 即可；若 local 模式下 reader 是 `*UserService`，它也实现了接口 | `server/cmd/api/main.go:884` 附近 |
| 4.3 | 启动期校验：rpc 模式下若 `RPCClient.Configured() == false`，记 warn 但不阻塞启动（channel 链路 fail-open，调用时报 `ErrNotConfigured`） | `server/cmd/api/main.go:154` 附近 |

**完成标准**：
- local 模式启动 → channel 走本地 `UserService` 实现
- rpc 模式启动 → channel 走 `RPCClient` 实现，HTTP 链路通到 cs-user 阶段 1 新端点
- 端到端：webhook 入站 / `/api/channels` 自动建配置 / 出站发送 全链路在 rpc 模式下打通

**风险**：低。装配改动；type-assertion 失败会在启动期立即暴露。

**回滚**：单 PR revert。

---

### 阶段 5：清理与回归

**目标**：确认 `user_auth_identities` 表在 server 侧彻底失去 channel 模块这最后一个消费者；回归测试覆盖。

**前置**：阶段 4 已合并上线。

| # | 任务 | 文件 |
|---|---|---|
| 5.1 | grep 验证：`server/internal/channel/` 不再出现 `user_auth_identities` / `UserAuthIdentity` 任何引用 | 全包 |
| 5.2 | 端到端回归测试：wecom / wecom-bot webhook 入站 → conversation 路由正确；出站发送 → externalUserID 解析正确；`/api/channels` 自动建 config 触发 | 手工 + 自动化 |
| 5.3 | 文档更新：在 [IDENTITY_ARCHITECTURE_ROADMAP](./IDENTITY_ARCHITECTURE_ROADMAP.md) 的 Phase A7 清理段补一条"channel 模块身份解析已迁移" | `docs/identity-tenant/IDENTITY_ARCHITECTURE_ROADMAP.md` |

**完成标准**：
- grep 零命中
- 端到端测试全绿
- 路线图文档反映最新状态

**风险**：无。

**回滚**：N/A（清理阶段，无代码改动需要回滚）。

---

## 5. 与现有提案的关系

| 现有提案 | 关系 |
|---|---|
| [IDENTITY_ARCHITECTURE_ROADMAP](./IDENTITY_ARCHITECTURE_ROADMAP.md) | Phase A7 的后续清理，与 [JWT_VERIFY_CLEANUP_PLAN](./JWT_VERIFY_CLEANUP_PLAN.md) 是同一清理批次的两条独立线（JWT 链路 vs 渠道链路），互不阻塞 |
| [CS_USER_SERVICE_DESIGN](./CS_USER_SERVICE_DESIGN.md) | 阶段 1 新端点符合该设计的 `UserService` 接口扩展模式 |
| [TEAM_ORG_UNIFICATION](./TEAM_ORG_UNIFICATION.md) | 无直接关系（团队 / 组织链路不经过 channel 模块） |

---

## 6. 未决项（暂不处理，记录留痕）

- **U1**：`notification.NotificationService`（单向推送）也存在类似的本地身份查询。本次范围明确排除；后续视单向推送链路需求另立提案。
- **U2**：server 侧 `user_auth_identities` 表本身的下线时机。channel 模块迁完后，剩余消费者排查完确认无引用后可单独走下线提案；本计划不绑定。
- **U3**：IdentityResolver 接口的 provider 参数当前调用方都传字面量 `"idtrust"`。未来若接入多企业身份源，可考虑抽常量或 enum；现阶段保持字符串以避免过早抽象。
