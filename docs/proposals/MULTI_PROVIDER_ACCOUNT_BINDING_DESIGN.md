# 多 Provider 账号统一绑定设计稿

## 1. 背景与目标

当前系统已支持 Casdoor 下多个 provider 登录，并已完成以下基础能力：

1. Casdoor JWT / userinfo 归一化
2. `properties.oauth_*` provider 原始资料提取
3. `external_key` 稳定身份键生成
4. 本地 `users` 表中主身份摘要字段持久化

但现有实现仍主要围绕“一个本地 user 对应一个主外部身份”展开。对于同一自然用户混合使用：

- Github
- idtrust
- phone

等多种 provider 登录的场景，当前模型无法完整表达“一个本地用户绑定多个外部身份”的关系，因此需要新增统一绑定能力。

本设计稿明确采用：

> **方案 B：显式绑定**

即系统不自动猜测和合并不同 provider 的账号，而要求用户在已登录状态下显式发起绑定。系统侧仅负责：

1. 保存绑定关系
2. 在后续登录时解析绑定关系并落到同一个本地用户
3. 按 provider 优先级自动升级主资料源

## 2. 非目标

本方案当前不包括：

1. 不支持自动账号归并
2. 不提供“用户手工切换主资料源”功能
3. 不处理两个已存在本地用户的自动合并
4. 不支持管理员后台强制合并账号

## 3. 业务策略

## 3.1 采用显式绑定

只有在以下条件满足时，系统才允许建立新的 provider 绑定：

1. 当前用户已经登录
2. 当前用户主动发起绑定流程
3. 第二个 provider OAuth / 登录校验完整成功
4. 回调 state 验证通过

## 3.2 主资料源自动升级

系统不提供“主资料源”手工配置入口，而采用固定 provider 优先级自动升级：

```text
idtrust > github > phone
```

规则如下：

1. 用户首次登录创建账号时，首条 identity 自动成为 primary identity
2. 后续绑定新 identity 时：
   - 若新 provider 优先级更高，则自动切换为 primary identity
   - 若优先级相同或更低，则保持原 primary identity
3. 解绑当前 primary identity 时，系统从剩余 identity 中重新选择优先级最高的一条作为新的 primary identity

## 3.3 字段级资料聚合

自动升级 primary identity 不等于每个字段都直接被最后绑定的 provider 覆盖。建议采用以下字段策略：

| 字段 | 规则 |
|------|------|
| `display_name` | 优先 primary identity 的显示名 |
| `avatar_url` | 优先 primary identity；若为空则优先 Github 头像 |
| `email` | 仅使用合法邮箱；优先 primary identity，再 fallback 其他 identity |
| `phone` | 优先 phone provider，再 fallback primary identity 中合法手机号 |
| `organization` | 优先 primary identity，尤其 idtrust |
| `username` | 尽量稳定，不随 provider 自动频繁变更 |

## 4. 数据模型设计

## 4.1 现有 `users` 表定位调整

当前 `users` 表中的以下字段：

- `auth_provider`
- `external_key`
- `provider_user_id`
- `phone`

在引入多 provider 绑定后，不再表示“用户唯一真实身份”，而调整为：

> **当前 primary identity 的摘要字段 / 聚合展示字段**

因此 `users` 表继续承担：

1. 本地统一用户实体
2. 聚合后的稳定展示资料
3. 当前主身份摘要

## 4.2 新增 `user_auth_identities` 表

建议新增模型：

```go
type UserAuthIdentity struct {
    ID               uint       `gorm:"primaryKey"`
    UserSubjectID    string     `gorm:"index;not null"`

    Provider         string     `gorm:"size:64;not null"`
    Issuer           string     `gorm:"size:255"`
    ExternalKey      string     `gorm:"uniqueIndex;size:255;not null"`
    ExternalSubject  string     `gorm:"size:191"`
    ExternalUserID   string     `gorm:"size:191"`
    ProviderUserID   string     `gorm:"size:191"`

    DisplayName      *string    `gorm:"size:191"`
    Email            *string    `gorm:"size:191"`
    Phone            *string    `gorm:"size:64"`
    AvatarURL        *string    `gorm:"type:text"`
    Organization     *string    `gorm:"size:191"`

    IsPrimary        bool       `gorm:"not null;default:false"`
    LastLoginAt      *time.Time
    CreatedAt        time.Time
    UpdatedAt        time.Time
}
```

## 4.3 关键索引与约束

建议：

1. `unique index idx_user_auth_identities_external_key (external_key)`
2. `index idx_user_auth_identities_user_subject_id (user_subject_id)`

业务约束：

1. 一个 `external_key` 只能绑定一个本地用户
2. 一个本地用户可以有多条外部身份记录
3. 同一 `user_subject_id` 在任意时刻只能有一条 `is_primary = true`

## 5. Provider 优先级规则

建议新增统一优先级函数：

```go
func ProviderRank(provider string) int {
    switch strings.ToLower(strings.TrimSpace(provider)) {
    case "idtrust":
        return 300
    case "github":
        return 200
    case "phone":
        return 100
    default:
        return 0
    }
}
```

使用原则：

1. rank 越高，优先级越高
2. 新绑定 identity 的 rank 高于当前 primary identity 时，自动升级
3. rank 相同则保持原 primary，不做切换

## 6. 核心服务设计

建议新增独立服务，例如：

- `server/internal/authidentity/service.go`
或
- `server/internal/useridentity/service.go`

## 6.1 服务接口建议

```go
type AuthIdentityService struct {
    db *gorm.DB
}

func (s *AuthIdentityService) ResolveOrCreateUserByIdentity(identity *NormalizedIdentity) (*models.User, error)

func (s *AuthIdentityService) BindIdentityToUser(userSubjectID string, identity *NormalizedIdentity) error

func (s *AuthIdentityService) ListUserIdentities(userSubjectID string) ([]models.UserAuthIdentity, error)

func (s *AuthIdentityService) UnbindIdentity(userSubjectID string, identityID uint) error

func (s *AuthIdentityService) RefreshUserProfileFromIdentities(userSubjectID string) error
```

## 6.2 方法职责

### `ResolveOrCreateUserByIdentity`

用于登录主流程：

1. 先按 `external_key` 查 `user_auth_identities`
2. 命中则找到对应本地 user
3. 未命中则创建新 user + 首条 identity
4. 更新 `last_login_at`
5. 必要时刷新聚合用户资料

### `BindIdentityToUser`

用于绑定流程：

1. 检查该 `external_key` 是否已存在
2. 若不存在，则绑定到当前 user
3. 若已绑定当前 user，则幂等成功
4. 若已绑定其他 user，则返回冲突错误
5. 比较 provider 优先级，必要时自动升级 primary identity
6. 刷新用户聚合资料

### `ListUserIdentities`

返回当前用户所有已绑定 identity 列表，供前端账号设置页展示。

### `UnbindIdentity`

解绑规则：

1. 确认 identity 属于当前 user
2. 若这是唯一绑定方式，则禁止解绑
3. 若解绑的是当前 primary identity，则重新选出新的 primary identity
4. 解绑后刷新用户聚合资料

### `RefreshUserProfileFromIdentities`

该方法是“主资料源自动升级 + 字段聚合”的核心。

它负责：

1. 查询用户全部 identity
2. 找出 `is_primary = true` 的 identity
3. 若没有 primary，则按 provider rank 选出一条
4. 根据字段规则重新计算 `users` 表中的聚合资料
5. 将 `users.auth_provider / external_key / provider_user_id` 更新为当前 primary identity 摘要

## 7. 登录流程设计

## 7.1 首次登录

流程：

1. 登录回调 / 中间件解析得到 `NormalizedIdentity`
2. 先查 `user_auth_identities.external_key`
3. 未命中：
   - 创建 `users`
   - 创建首条 `user_auth_identities`
   - 该 identity 标记 `is_primary = true`
4. 刷新 `users` 聚合资料

## 7.2 已绑定 provider 登录

流程：

1. 登录回调 / 中间件解析 `NormalizedIdentity`
2. 按 `external_key` 查 identity 表
3. 找到对应 `user_subject_id`
4. 返回同一 `users` 记录
5. 更新 identity 的 `last_login_at`

## 8. 绑定流程设计

## 8.1 发起绑定

建议接口：

```http
POST /api/auth/bind/start
```

请求：

```json
{
  "provider": "Github"
}
```

返回：

```json
{
  "authUrl": "https://casdoor..."
}
```

## 8.2 bind state 设计

建议 state 至少携带：

```json
{
  "action": "bind",
  "userSubjectId": "usr_xxx",
  "provider": "Github",
  "nonce": "random",
  "expiredAt": 1710000000
}
```

要求：

1. state 必须签名或加密
2. 与当前登录会话绑定
3. 有较短有效期

## 8.3 绑定回调

建议接口：

```http
GET /api/auth/bind/callback
```

流程：

1. 验证当前用户已登录
2. 验证 bind state
3. 用 code 换 token
4. 归一化得到新 identity
5. 调用 `BindIdentityToUser(currentUser, identity)`
6. 成功后重定向前端账号设置页

## 8.4 冲突处理

若新 identity 已绑定其他本地用户，则返回冲突错误，例如：

```json
{
  "error": "identity_already_bound",
  "message": "该登录方式已绑定其他账号"
}
```

## 9. 解绑流程设计

建议接口：

```http
POST /api/auth/identities/:id/unbind
```

规则：

1. 只允许解绑当前登录用户自己的 identity
2. 不允许解绑最后一个 identity
3. 如果解绑的是 primary identity，则重新按 provider 优先级选出新的 primary identity
4. 完成后刷新 `users` 聚合资料

## 10. 资料聚合规则

建议统一通过 `RefreshUserProfileFromIdentities()` 聚合写回 `users` 表。

## 10.1 选择 primary identity

```text
优先使用 is_primary = true
若不存在，则按 provider rank 最高者选 primary
```

## 10.2 DisplayName 聚合

规则：

1. primary identity.display_name
2. 其他 identity 中优先级最高的 display_name
3. 若都为空，则回退到 `users.username`

## 10.3 Avatar 聚合

规则：

1. primary identity.avatar_url
2. Github identity.avatar_url
3. 其他 identity.avatar_url

## 10.4 Email 聚合

规则：

1. primary identity 的合法邮箱
2. 其他 identity 的合法邮箱
3. 非法邮箱格式不写入 `users.email`

## 10.5 Phone 聚合

规则：

1. `phone` provider 的手机号
2. primary identity 中的合法手机号
3. 其他 identity 中的合法手机号

## 10.6 Organization 聚合

规则：

1. primary identity.organization
2. 其他 identity 中优先级最高的 organization

## 10.7 Username 策略

建议保持保守：

1. 初次创建时初始化 username
2. 后续不因 provider 切换频繁自动变更
3. 仅在旧 username 明显低质量时，允许升级，例如：
   - `phone_<number>`
   - UUID 风格随机值

## 11. 迁移策略

## 11.1 新建 identity 表

新增 `user_auth_identities` 表与索引。

## 11.2 从 `users` 表回填首批 identity

从当前 `users` 表已存在字段回填：

- `auth_provider`
- `external_key`
- `provider_user_id`
- `casdoor_universal_id`
- `casdoor_id`
- `casdoor_sub`
- `email`
- `phone`
- `display_name`
- `avatar_url`
- `organization`

回填原则：

1. 每个 user 至少生成一条 identity
2. 若 `external_key` 存在，则直接用作 identity 主键字段
3. 回填 identity 后，将其标记为 `is_primary = true`

## 11.3 登录主流程切换

切换为：

1. 优先查 `user_auth_identities.external_key`
2. 再 fallback `users.external_key` 与旧 Casdoor 字段
3. 兼容期内命中旧逻辑时自动补 identity 表

## 12. 安全规则

## 12.1 绑定必须基于已登录会话

绑定不是普通登录，必须确保当前用户已登录。

## 12.2 严格校验 state

必须防止：

- CSRF
- 会话串绑
- provider 回调错绑

## 12.3 不允许覆盖已绑定他人的 identity

对于已绑定其他 user 的 `external_key`：

1. 不允许 silent rebind
2. 不允许自动迁移
3. 直接报冲突错误

## 12.4 不允许解绑最后一个登录方式

防止用户把自己锁死。

## 13. 代码落点建议

建议涉及以下模块：

### 模型层

- `server/internal/models/models.go`

新增：

- `UserAuthIdentity`

### 服务层

- `server/internal/authidentity/service.go`
或
- `server/internal/user/identity_service.go`

### Handler 层

新增接口：

- `StartBindAuth`
- `BindAuthCallback`
- `ListBoundIdentities`
- `UnbindIdentity`

### 登录流程

改造：

- `AuthCallback`
- `middleware auth`
- `GetOrCreateUser / ResolveOrCreateUserByIdentity`

### 迁移层

- `server/cmd/migrate/main.go`

## 14. 实施建议

建议按以下顺序推进：

### Phase 1

1. 新增 `user_auth_identities` 表
2. 回填首批 identity 数据
3. 新增 `AuthIdentityService`

### Phase 2

1. 登录流程优先切到 identity 表
2. 兼容旧字段 fallback
3. 自动刷新 `users` 聚合资料

### Phase 3

1. 新增绑定 / 解绑接口
2. 前端账号设置页接入

### Phase 4

1. 观察 identity 命中率与冲突情况
2. 收敛旧 `users` 表上的查找逻辑

## 15. 结论

在显式绑定方案下，系统不需要自动猜测用户是否应被合并，而是通过：

1. 新增 `user_auth_identities` 表表达一对多身份关系
2. 在已登录态下显式绑定第二 provider
3. 通过 provider 优先级自动升级 primary identity
4. 通过字段级聚合规则刷新 `users` 展示资料

来实现“同一自然用户可以混合使用多个 provider 登录同一账号”的能力。

这一方案既能控制安全风险，又能在产品复杂度可控的前提下满足多 provider 统一账号诉求。
