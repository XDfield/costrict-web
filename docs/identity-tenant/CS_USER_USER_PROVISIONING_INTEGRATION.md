# cs-user 用户预创建对接文档（无 Casdoor claim 场景）

> 面向外部业务模块（如企业权限绑定模块）：调用方仅持有本系统内的"企业身份 id"，拿不到 Casdoor 侧的 `universal_id` / `sub`，希望 cs-user 端到端算出 `subject_id` 并返回。

## 1. 背景与设计选择

### 1.1 调用方场景

外部模块（下文记为 EB = Enterprise Binding）持有自己体系内的企业身份 id（例：`EXT-12345`），希望：

1. 调用 cs-user 创建一个用户占位，拿到稳定的 `subject_id` 作为业务侧外键；
2. 该用户首次走真实企业 IdP 登录时，cs-user 能自动把 OAuth 回调关联到已预创建的 `subject_id`。

EB **拿不到** Casdoor 内部的 `universal_id` / `sub` / `id`，因此现有 `POST /api/internal/users/get-or-create` 直接调用不通（三者皆空会被 cs-user 拒绝，源码：`cs-user/internal/user/service.go:303`）。

### 1.2 为什么需要新端点

| 选项 | 是否可行 | 原因 |
|---|---|---|
| 直接调 `/get-or-create` | ❌ | 强制要求 `id` / `sub` / `universal_id` 至少一个 |
| EB 自己伪造 `universal_id` | ⚠️ | 短期可行，但和真实 OAuth 回调时的 `universal_id` 几乎不可能一致 → 下次登录建第二个用户 |
| 新增 `/users/provision` 端点 | ✅ | 干净契约，cs-user 内部合成 synthetic claim，并把企业身份 id 持久化到 `employment_identities.enterprise_uid`，为后续关联铺路 |

### 1.3 关联闭环的两条可选路径

cs-user 拿到 EB 的企业身份 id 后，需要保证**未来真实 OAuth 登录时能命中预创建行**。下面两条路径任选其一（推荐 A + B 都落地，互为兜底）：

| 路径 | 关联键 | EB 是否需要介入 OAuth | cs-user 是否需要改造 |
|---|---|---|---|
| **A. enterprise_uid 反查**（推荐） | `employment_identities.(tenant_id, enterprise_uid)` | 否 | 是：`GetOrCreateUser` 多路查找里加一条 enterprise_uid 查找分支 |
| **B. employee_number 反查** | `employment_identities.(tenant_id, employee_number)` | 否（仅需 EB 提供工号） | 否：现成 `SearchUsersByEmployeeNumber` 已支持 |
| **C. OAuth claim 注入** | `users.external_key`（基于 `universal_id`+`provider`） | 是：EB 需确保 OAuth 回调的 `universal_id` = 企业身份 id | 否 |

下文接口设计以路径 A 为主线（覆盖 EB 无工号、也无法介入 OAuth 的最差情况）。

## 2. 接口规范（建议新增）

### 2.1 端点

```
POST {cs_user_base_url}/api/internal/users/provision
```

> 该端点**当前未实现**。本文档为设计契约，待 cs-user 侧落地后生效。落地工作量：1 个 handler + 1 个 service 方法 + `GetOrCreateUser` 增加 enterprise_uid 查找分支 + Swagger 注释 + 单测。

### 2.2 请求头

| Header | 必填 | 说明 |
|---|---|---|
| `Content-Type` | 是 | `application/json` |
| `X-Internal-Token` | 是 | cs-user 与内部服务之间的共享密钥。未配置或错误 → `401`。 |
| `X-Tenant-Id` | 是 | 目标企业的 `tenant_id`。cs-user 据此把新用户落到正确租户，缺失会回落到 `default` 租户。 |

### 2.3 请求体

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `enterprise_provider` | string | 是 | 该企业身份所属的 provider 命名空间。**cs-user 不做格式 / 取值限制，由 EB 自行指定**（例：`idtrust`、`wxwork`、`azure_ad`、自定义业务名均可）。该值会落到 `employment_identities.provider` 与 `user_auth_identities.provider`，作为后续真实 OAuth 登录时 enterprise_uid 反查命中的命名空间隔离边界。EB 自行保证：同一企业身份 id 在不同请求中传相同 provider，避免跨 provider 串号。 |
| `enterprise_uid` | string | 是 | EB 持有的企业身份 id（即本接口的"占位主键"）。落到 `employment_identities.enterprise_uid`，未来真实登录通过它反查命中。 |
| `username` | string | 否 | 初始 username，租户内 unique。不传时默认生成 `ext_<enterprise_uid>`，避免空 username 多次预创建撞 unique。 |
| `display_name` | string | 否 | 显示名。 |
| `email` | string | 否 | 邮箱。 |
| `phone` | string | 否 | 手机号。 |
| `employee_number` | string | 否 | 工号。写入 `employment_identities.employee_number`，便于走路径 B 反查。 |
| `external_claims` | object | 否 | 任意附加字段，原样存到 `employment_identities.attributes`。 |

### 2.4 请求示例

```bash
curl -X POST 'https://cs-user.example.com/api/internal/users/provision' \
  -H 'Content-Type: application/json' \
  -H 'X-Internal-Token: <shared-secret>' \
  -H 'X-Tenant-Id: acme' \
  -d '{
    "enterprise_provider": "idtrust",
    "enterprise_uid": "EXT-12345",
    "display_name": "张三",
    "email": "zhangsan@example.com",
    "employee_number": "EMP-001",
    "external_claims": {
      "department": "研发部"
    }
  }'
```

### 2.5 成功响应（200）

```json
{
  "user": {
    "subject_id": "usr_5f8e2c1a-3b4d-49a7-9e21-1c6f7a3b2d12",
    "tenant_id": "acme",
    "username": "ext_EXT-12345",
    "display_name": "张三",
    "email": "zhangsan@example.com",
    "is_active": true,
    "status": "active",
    "profile_completed_at": null,
    "created_at": "2026-07-25T10:30:00Z"
  },
  "is_new_user": true
}
```

- `subject_id`：**EB 必须持久化**此值，作为后续所有业务关联的外键。
- `is_new_user`：`true` 表示本次新建；`false` 表示已存在（幂等重试）。EB 可以安全重试。

### 2.6 错误码

| HTTP | 含义 | 触发条件 |
|---|---|---|
| 400 | 参数错误 | `enterprise_uid` / `enterprise_provider` 缺失 |
| 401 | 鉴权失败 | `X-Internal-Token` 缺失或错误 |
| 409 | 唯一约束冲突 | 同租户下 `enterprise_uid` 已绑定到不同 subject_id（数据异常） |
| 500 | 内部错误 | DB 写入失败、tenant 未找到等 |

## 3. cs-user 内部行为契约

新端点服务端语义（实现侧参考）：

1. **幂等查找**：在 `employment_identities` 表按 `(tenant_id, enterprise_uid)` 命中既有行 → 直接返回对应 `users` 行，`is_new_user=false`。`employment_identities.enterprise_uid` 已有 `(tenant_id, enterprise_uid)` 部分唯一索引（`cs-user/internal/models/employment_identity.go:28`），可直接走。
2. **预创建**：未命中时，cs-user 内部合成一个 synthetic `JWTClaims`：
   - `UniversalID = enterprise_uid`（仅用于触发 `GetOrCreateUser` 创建分支；记入 `users.casdoor_universal_id` 作为可观测痕迹，不强求与真实 OAuth 一致）
   - `Provider = enterprise_provider`
   - `Name = username or "ext_" + enterprise_uid`
   - `ExternalClaims` 把 `employee_number` 等字段塞进去，复用现有 `applyEnterpriseMappingOnLogin` 写 `employment_identities`
3. **委托既有写入路径**：调用 `user.Service.GetOrCreateUser(ctx, syntheticClaims)`，复用 users / user_auth_identities / outbox `user.created` 事件 / employment_identities 的全套既有逻辑，**不重写**。
4. **返回**：从结果中取 `subject_id` 返回给 EB。

### 3.1 未来 OAuth 登录的自动关联（路径 A 实现）

需要扩展 `cs-user/internal/user/service.go:GetOrCreateUser` 的多路查找链，在现有 5 路（external_key → universal_id → casdoor_id → sub → username）之后追加一路：

```
if !found && claims.ExternalClaims["enterprise_uid"] != "" {
    // 反查 employment_identities.(tenant_id, enterprise_uid)
    // 命中 → 取 user_subject_id → 加载 users 行
}
```

更稳健的做法：`applyEnterpriseMappingOnLogin` 已经在真实登录时把 IdP claim 里的 `enterprise_uid` 落到 `employment_identities` 表（前提是 tenant 的 `employment_providers.field_map` 配了该字段映射）。那么 `GetOrCreateUser` 只需在创建分支前先按 enterprise_uid 查一次 employment_identities 即可——命中则视为 found，复用既有 subject_id。

> ⚠️ 这一步是关联闭环的关键。**不实现此分支，预创建的用户在真实登录时仍会被当成新用户**。本文档作为对接契约，要求 cs-user 落地此端点时一并实现。

## 4. 集成时序

```
[EB 模块]                       [cs-user]                   [Casdoor/IdP]
   |                               |                            |
   | ① POST /users/provision       |                            |
   |   (enterprise_uid=EXT-12345)  |                            |
   |------------------------------>|                            |
   |                               | ② 查 employment_identities |
   |                               |    按 (tenant, EXT-12345)  |
   |                               |    未命中 → 合成 claims     |
   |                               |    → GetOrCreateUser       |
   |                               |    → 写 users + identities |
   |                               |    → 写 employment_identities
   |                               |    (enterprise_uid=EXT-12345)
   |   {subject_id: usr_xxx}       |                            |
   |<------------------------------|                            |
   |                               |                            |
   | ③ EB 持久化 subject_id        |                            |
   |                               |                            |
   ~ ~ ~ ~ ~ ~ ~ ~ 一段时间后 ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~ ~
   |                               |                            |
   |                               | ④ 用户首次走企业 IdP 登录    |
   |                               |    OAuth 回调到达           |
   |                               |<---------------------------|
   |                               | ⑤ server 调 cs-user        |
   |                               |    /get-or-create          |
   |                               |    GetOrCreateUser 多路查  |
   |                               |    → 命中 employment_identities
   |                               |      (tenant, EXT-12345)   |
   |                               |    → found=true            |
   |                               |    → 复用 subject_id       |
   |                               |    → 刷新 last_login_at    |
   |                               |    (不触发 user.created)   |
   |                               |---------------------------->|
   |                               |    登录成功                 |
```

## 5. EB 侧实现 Checklist

- [ ] 从 cs-user 维护者获取：`X-Internal-Token` 共享密钥、目标 `tenant_id`。
- [ ] 与 cs-user 维护者确认：`enterprise_provider` 字符串命名（cs-user 不限制取值，但 EB 内部需对同一企业身份始终用相同 provider）。
- [ ] 在 EB 数据库为 `(tenant_id, enterprise_uid)` 建唯一索引，避免重复调本接口（虽然本接口幂等，但减少 RPC 往返）。
- [ ] EB 持久化 `subject_id`，作为业务侧外键。
- [ ] 实现重试与 401 / 5xx 告警（幂等可安全重试）。
- [ ] **确认 cs-user 侧已落地 §3.1 的 enterprise_uid 反查分支**——否则预创建的 user 在真实登录时会重复创建，关联闭环失效。

## 6. cs-user 侧实现 Checklist

- [ ] 新增 `POST /api/internal/users/provision` handler（`cs-user/internal/handlers/users.go`）。
- [ ] 新增 `UserService.ProvisionByEnterprise` 方法（`cs-user/internal/user/service.go`），内部合成 claims + 委托 `GetOrCreateUser`。
- [ ] `GetOrCreateUser` 多路查找追加 `employment_identities.enterprise_uid` 反查分支（§3.1）。
- [ ] 单测覆盖：首次预创建、幂等重入、真实登录命中预创建行（subject_id 不变）。
- [ ] Swagger 同步生成（项目规范：API 改动必须同步 swagger，见 `AGENTS.md`）。
- [ ] 如果 EB 无 `enterprise_uid` 但有工号，可走路径 B：现有 `SearchUsersByEmployeeNumber` 已经支持，本端点可加一个 `lookup_only=true` 模式跳过创建。

## 7. FAQ

**Q：能不能不传 `enterprise_provider`？**
A：不行。`employment_identities.provider` 是 NOT NULL。但 cs-user **不对 provider 取值做校验或枚举限制**，EB 可以传任意字符串。provider 的唯一作用是作为命名空间隔离不同来源的企业身份——只要 EB 在同一企业身份的预创建 / 后续业务调用中传一致的值即可。

**Q：EB 调本端点后，未来用户改了企业身份 id 怎么办？**
A：`enterprise_uid` 在 `employment_identities` 上有部分唯一索引，但 cs-user 没有暴露修改接口。如需变更，EB 应调用 `POST /api/internal/users/transfer-identity` 或联系 cs-user 维护者走 admin 流程。

**Q：如果 cs-user 暂时无法落地 §3.1 的反查分支怎么办？**
A：临时降级方案：EB 在用户首次真实登录后，由 EB 侧主动调 `POST /api/internal/users/:subject_id/bind-identity`，把 OAuth 回调里的真实 claim 绑定到预创建的 subject_id。这需要 EB 能感知 OAuth 回调（或由 server 在登录成功后回调 EB）。

**Q：本端点会不会触发 Gitea provisioning 等下游开通？**
A：会。因为底层走 `GetOrCreateUser`，新建时会发 outbox `user.created` 事件，server 侧消费者会按既有规则触发下游。如不希望预创建阶段就触发，需在 server 侧事件消费逻辑加判断（cs-user 侧不关闭）。

## 8. 相关文档

- 设计基线：`docs/identity-tenant/CS_USER_SERVICE_DESIGN.md`
- 多租户契约：`docs/identity-tenant/MULTI_TENANCY_DESIGN.md`
- 实现源码：
  - `cs-user/internal/handlers/users.go`（handler 入口）
  - `cs-user/internal/user/service.go:GetOrCreateUser`（核心 upsert 逻辑）
  - `cs-user/internal/user/employment_mapping.go`（employment_identities 写入）
  - `cs-user/internal/models/employment_identity.go`（schema 与索引）
- OpenAPI / Swagger：`cs-user/docs/swagger.yaml`（端点落地后补 `/api/internal/users/provision` 条目）
