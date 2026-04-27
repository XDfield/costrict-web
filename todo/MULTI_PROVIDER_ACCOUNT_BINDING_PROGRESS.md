# 多 Provider 账号绑定实施进度

基于 `docs/proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md`，用于跟踪“显式绑定 + provider 优先级自动升级主资料源”方案的实施进度。

---

## 一、设计基线与前置条件

### 1. 已有基础能力确认

- [x] Casdoor token 归一化能力已建立
- [x] `properties.oauth_*` provider 原始资料提取已建立
- [x] `external_key` 稳定身份键已建立
- [x] `users` 表已支持主身份摘要字段
- [x] `/auth/me` 统一 user DTO 已落地
- [x] richer auth claims 已贯通 middleware / resolver / authz

### 2. 本方案明确范围

- [x] 采用 **方案 B：显式绑定**
- [x] 不做自动账号归并
- [x] 不做“主资料源手工切换”功能
- [x] 采用 provider 优先级自动升级 primary identity
- [x] 确认默认优先级：`idtrust > github > phone`

---

## 二、数据模型与迁移（P0）

### 3. 模型定义（`server/internal/models/models.go`）

- [x] 新增 `UserAuthIdentity` 模型
- [x] 定义字段：`UserSubjectID`
- [x] 定义字段：`Provider`
- [x] 定义字段：`Issuer`
- [x] 定义字段：`ExternalKey`
- [x] 定义字段：`ExternalSubject`
- [x] 定义字段：`ExternalUserID`
- [x] 定义字段：`ProviderUserID`
- [x] 定义字段：`DisplayName`
- [x] 定义字段：`Email`
- [x] 定义字段：`Phone`
- [x] 定义字段：`AvatarURL`
- [x] 定义字段：`Organization`
- [x] 定义字段：`IsPrimary`
- [x] 定义字段：`LastLoginAt`
- [x] 定义 `TableName()`（如需要）

### 4. 数据库索引与约束

- [x] `unique index idx_user_auth_identities_external_key (external_key)`
- [x] `index idx_user_auth_identities_user_subject_id (user_subject_id)`
- [ ] 评估是否增加 provider / provider_user_id 组合索引
- [x] 约束同一 user 仅允许一条 `is_primary=true`（业务保证或数据库约束）

### 5. 数据库迁移

- [x] 新建 identity 表迁移方案
- [x] 决定使用 `AutoMigrate` 还是显式 SQL 迁移
- [x] 为历史库补充索引创建逻辑
- [x] 本地验证迁移可重复执行
- [x] PostgreSQL 环境验证迁移兼容性

### 6. 历史数据回填

- [x] 从 `users` 表回填首批 `user_auth_identities`
- [x] 回填来源：`auth_provider`
- [x] 回填来源：`external_key`
- [x] 回填来源：`provider_user_id`
- [x] 回填来源：`casdoor_universal_id`
- [x] 回填来源：`casdoor_id`
- [x] 回填来源：`casdoor_sub`
- [x] 回填来源：`display_name`
- [x] 回填来源：`email`
- [x] 回填来源：`phone`
- [x] 回填来源：`avatar_url`
- [x] 回填来源：`organization`
- [x] 将回填 identity 标记为 `is_primary=true`
- [ ] 支持 dry-run / summary 输出

---

## 三、Identity Service（P0）

### 7. 新增服务模块

- [ ] 新增 `server/internal/authidentity/service.go` 或等价目录结构
- [ ] 定义 `AuthIdentityService`
- [ ] 初始化依赖：`db *gorm.DB`

### 8. 登录解析能力

- [ ] `ResolveOrCreateUserByIdentity(identity *NormalizedIdentity) (*models.User, error)`
- [ ] 优先按 `user_auth_identities.external_key` 查找
- [ ] 命中后返回已绑定 user
- [ ] 未命中时创建新 user + 首条 identity
- [ ] 更新 identity `last_login_at`
- [ ] 更新 user `last_login_at`
- [ ] 与现有 `GetOrCreateUser` 兼容衔接

### 9. 绑定能力

- [ ] `BindIdentityToUser(userSubjectID string, identity *NormalizedIdentity) error`
- [ ] 检查 `external_key` 是否已存在
- [ ] 未绑定时创建新 identity
- [ ] 已绑定当前 user 时幂等成功
- [ ] 已绑定其他 user 时返回冲突错误
- [ ] 根据 provider rank 判断是否切换 primary identity
- [ ] 完成后刷新 `users` 聚合资料

### 10. 查询与解绑能力

- [x] `ListUserIdentities(userSubjectID string) ([]models.UserAuthIdentity, error)`
- [x] `UnbindIdentity(userSubjectID string, identityID uint) error`
- [x] 禁止解绑最后一个 identity
- [x] 若解绑的是 primary identity，则自动重新选择新的 primary identity
- [x] 解绑后刷新 `users` 聚合资料

---

## 四、Primary Identity 与资料聚合（P0）

### 11. Provider 优先级规则

- [x] 实现 `ProviderRank(provider string) int`
- [x] 支持 `idtrust`
- [x] 支持 `github`
- [x] 支持 `phone`
- [x] 未知 provider 统一降级为最低优先级

### 12. Primary Identity 自动升级

- [x] 首次 identity 自动设为 primary
- [x] 新绑定 identity 优先级更高时自动升级为 primary
- [x] 优先级相同不切换 primary
- [x] 解绑 primary 时自动重选

### 13. 聚合方法

- [x] `RefreshUserProfileFromIdentities(userSubjectID string) error`
- [x] 查询当前用户全部 identities
- [x] 找出 `is_primary=true` 的 identity
- [x] 若 primary 缺失则按 rank 选出新的 primary
- [x] 回写 `users.auth_provider`
- [x] 回写 `users.external_key`
- [x] 回写 `users.provider_user_id`

### 14. 字段聚合规则

- [x] `display_name`：primary 优先
- [x] `avatar_url`：primary 优先，Github 头像 fallback
- [x] `email`：仅合法邮箱参与聚合
- [x] `phone`：phone provider 优先
- [x] `organization`：primary 优先
- [x] `username`：保持稳定，不频繁自动变更
- [x] 明确低质量 username 升级条件（如 `phone_*` / UUID 风格）

---

## 五、登录主流程切换（P0）

### 15. Handler / Middleware / Resolver 接入

- [x] `AuthCallback` 切换为优先通过 identity 表查人
- [ ] `RequireAuth` / `OptionalAuth` 链路优先通过 identity 表解析 user
- [ ] authz 的 token 校验链路兼容 identity 表
- [x] `/auth/me` 在 identity 表模式下返回一致结果

### 16. 兼容旧逻辑 fallback

- [x] 未命中 identity 表时 fallback `users.external_key`
- [ ] 未命中 identity 表时 fallback `casdoor_universal_id`
- [ ] 未命中 identity 表时 fallback `casdoor_sub`
- [ ] 未命中 identity 表时 fallback `casdoor_id`
- [x] 命中旧逻辑后自动补 identity 记录

---

## 六、绑定接口（P1）

### 17. 发起绑定

- [x] `POST /api/auth/bind/start`
- [x] 参数校验：provider 必填
- [x] 必须要求当前用户已登录
- [x] 生成 bind state
- [x] 返回绑定 OAuth URL

### 18. 绑定回调

- [x] `GET /api/auth/bind/callback`
- [x] 验证当前用户会话
- [x] 验证 bind state
- [x] 用 code 换 token
- [x] 归一化新的 identity
- [x] 调用 `BindIdentityToUser`
- [x] 绑定成功后重定向账号设置页

### 19. 已绑定身份查询

- [x] `GET /api/auth/identities`
- [x] 返回 provider 列表
- [x] 返回 `providerUserId`
- [x] 返回 `displayName`
- [x] 返回 `email`
- [x] 返回 `phone`
- [x] 返回 `externalKey`
- [x] 返回 `isPrimary`
- [x] 返回 `lastLoginAt`

### 20. 解绑接口

- [x] `POST /api/auth/identities/:id/unbind`
- [x] 校验 identity 属于当前 user
- [x] 禁止解绑最后一个 identity
- [x] primary identity 解绑后自动重选
- [ ] 返回更新后的 identity 列表或成功状态

---

## 七、安全与状态控制（P0）

### 21. bind state 设计

- [x] `action=bind` 标识
- [x] 包含 `userSubjectID`
- [x] 包含 `provider`
- [x] 包含 `nonce`
- [x] 包含过期时间
- [ ] 与当前登录会话绑定
- [x] 签名或加密

### 22. 安全规则

- [x] 绑定必须要求已登录
- [ ] 严格防止跨会话串绑
- [x] 禁止覆盖已绑定他人的 identity
- [x] 冲突时返回明确错误码 `identity_already_bound`
- [x] 解绑最后一个 identity 时返回明确错误

---

## 八、测试（P0）

### 23. Service 单元测试

- [ ] `ResolveOrCreateUserByIdentity` 首次创建测试
- [ ] `ResolveOrCreateUserByIdentity` 命中既有 identity 测试
- [x] `BindIdentityToUser` 新绑定成功测试
- [ ] `BindIdentityToUser` 幂等绑定测试
- [ ] `BindIdentityToUser` 绑定冲突测试
- [x] `UnbindIdentity` 成功测试
- [x] `UnbindIdentity` 最后一个 identity 保护测试

### 24. 聚合策略测试

- [x] `phone -> github` 自动升级 primary 测试
- [ ] `github -> idtrust` 自动升级 primary 测试
- [ ] 同优先级不切换 primary 测试
- [x] primary 解绑后自动重选测试
- [ ] 头像 Github fallback 测试
- [ ] 非法邮箱不写入 `users.email` 测试
- [ ] phone provider 优先写入 `users.phone` 测试

### 25. Handler / API 测试

- [x] `bind/start` 测试
- [ ] `bind/callback` 成功测试
- [ ] `bind/callback` state 校验失败测试
- [x] `GET /api/auth/identities` 测试
- [x] `unbind` 测试

### 26. 集成测试

- [ ] 首次 phone 登录创建 user + identity
- [ ] 绑定 Github 后同账号登录命中同一 user
- [ ] 绑定 idtrust 后 primary 自动升级
- [ ] 三种 provider 混合登录命中同一 `subject_id`

---

## 九、文档与上线（P1）

### 27. 文档更新

- [ ] 更新设计稿与实现差异说明
- [ ] 补充迁移执行说明
- [ ] 补充绑定接口使用说明

### 28. 上线准备

- [x] 先在测试环境验证迁移与回填
- [ ] 观察 identity 冲突情况
- [ ] 评估历史重复 user 是否需要人工治理
- [ ] 确认回滚策略

---

## 进度概览

| 阶段 | 内容 | 状态 |
|------|------|------|
| 一 | 设计基线与前置条件 | 已完成 |
| 二 | 数据模型与迁移 | 大部分完成 |
| 三 | Identity Service | 大部分完成 |
| 四 | Primary Identity 与资料聚合 | 大部分完成 |
| 五 | 登录主流程切换 | 部分完成 |
| 六 | 绑定接口 | 大部分完成 |
| 七 | 安全与状态控制 | 部分完成 |
| 八 | 测试 | 部分完成 |
| 九 | 文档与上线 | 未开始 |

---

## 实施说明

### 优先级说明

- **P0**：必须完成，决定绑定能力是否可用
- **P1**：重要功能，决定绑定能力是否可上线
- **P2**：后续优化

### 当前建议实施顺序

1. 先建 `user_auth_identities` 表
2. 再做 identity service
3. 再切登录主流程
4. 最后补绑定 / 解绑接口

### 关键注意事项

1. `users` 表在本方案中是聚合资料结果，不再是唯一身份真相来源
2. `user_auth_identities.external_key` 是登录命中的主查找键
3. `username` 应保持稳定，避免资料抖动造成业务副作用
4. `idtrust > github > phone` 仅用于 primary identity 选择，不代表所有字段都由 primary 覆盖

---

## 参考文档

- [多 Provider 账号统一绑定设计稿](../docs/proposals/MULTI_PROVIDER_ACCOUNT_BINDING_DESIGN.md)
- [Casdoor 多 Provider 身份归一化设计提案](../docs/proposals/casdoor-identity-normalization/README.md)
