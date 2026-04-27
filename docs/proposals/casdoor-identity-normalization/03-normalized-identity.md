# 3. 统一身份模型与归一化规则

## 3.1 设计目标

为解决不同 provider 字段差异导致的身份不稳定问题，建议在认证层和用户服务层之间新增标准模型 `NormalizedIdentity`。此模型作为所有认证入口唯一的标准输出，后续用户查找、用户创建、资料同步、上下文设置都只依赖该模型。

## 3.2 标准身份模型

建议新增结构如下：

```go
type NormalizedIdentity struct {
    Provider       string         // Casdoor provider 名称或系统定义别名
    Issuer         string         // iss
    Subject        string         // 归一化后的主身份字段
    RawSubject     string         // JWT 标准 sub 的原始值
    ExternalUserID string         // id / user_id / uid 等 provider 内部用户标识
    ProviderUserID string         // provider 原始用户 ID，例如 oauth_Custom_id
    UniversalID    string         // universal_id
    ExternalKey    string         // 稳定外部身份键

    Username       string
    DisplayName    string
    Email          string
    Phone          string
    AvatarURL      string
    Organization   string

    ProviderProfile map[string]any

    Source         string         // jwt / userinfo / merged
    RawClaims      map[string]any
    RawUserInfo    map[string]any
}
```

## 3.3 字段职责划分

### 3.3.1 身份字段

以下字段用于判断“是不是同一个外部用户”：

- `Provider`
- `Issuer`
- `Subject`
- `ExternalUserID`
- `ProviderUserID`
- `UniversalID`
- `ExternalKey`

这些字段必须具备稳定性，且应优先于展示类字段参与本地用户映射。

### 3.3.2 资料字段

以下字段用于本地 user 资料补充和展示：

- `Username`
- `DisplayName`
- `Email`
- `Phone`
- `AvatarURL`
- `Organization`

这些字段允许为空，允许按策略同步更新，但不能作为唯一身份主键。

## 3.4 统一归一化规则

## 3.4.1 `RawSubject`

`RawSubject` 只承接 JWT 标准 `sub` 字段，不与 `id`、`universal_id` 混用。规则：

1. 读取 `sub`
2. 若不存在则留空

## 3.4.2 `ExternalUserID`

用于保存 Casdoor 顶层用户 ID。基于当前真实样本，它更适合作为 Casdoor 记录 ID 或 Casdoor 侧用户 ID，而不是系统全局主身份。候选来源建议为：

1. `id`
2. `user_id`
3. `uid`

## 3.4.3 `ProviderUserID`

用于保存 provider 原始用户 ID，优先从 `properties.oauth_*` 中提取。该字段用于：

1. 保留 provider 侧原始身份
2. 调试和审计 provider 映射问题
3. 在资料同步或二期账号绑定时作为补充参考

例如：

- Github：`properties.oauth_GitHub_id`
- idtrust / Custom：`properties.oauth_Custom_id`

## 3.4.4 `UniversalID`

候选来源建议为：

1. `universal_id`
2. `universalId`

## 3.4.5 `Subject`

`Subject` 是系统定义的标准外部主体标识。结合当前已知 Github、idtrust、手机号三类真实样本，`universal_id` 在不同 provider 下表现出最好的稳定性，且当前样本中与 `sub` 保持一致。因此建议默认优先级为：

1. `universal_id`
2. `sub`
3. `id`
4. provider 配置指定字段
5. 否则报错

说明：

- `universal_id` 更适合跨 provider 的稳定识别
- `sub` 是 OIDC 标准字段，当前样本下基本等价于 `universal_id`
- `id` 在不同 provider 下语义并不稳定，只作为补充字段
- provider 自定义字段作为最后补充

## 3.4.6 `ExternalKey`

基于当前 Casdoor 实际样本，建议优先将 Casdoor 作为统一身份权威，而不是直接以下游 provider 作为主身份权威。因此建议优先生成：

```text
external_key = casdoor:<universal_id>
```

若 `universal_id` 缺失，则依次降级：

```text
external_key = casdoor-sub:<sub>
external_key = casdoor-id:<id>
```

如果未来需要跨 Casdoor 实例区分，可增强为：

```text
external_key = <issuer>|casdoor|<universal_id>
```

要求：

1. 只在成功解析出稳定 `Subject` 后生成
2. 全系统使用同一种拼接规则
3. 持久化到数据库，作为用户查找的首选键

## 3.5 资料字段优先级

结合当前真实 token 样本，资料字段应采用双层模型：

1. Casdoor 顶层统一字段，如 `displayName`、`email`、`phone`、`phone_number`
2. `properties.oauth_<Provider>_*` 中的 provider 原始资料字段

对于资料字段，建议默认优先 provider 原始资料字段，再回退到 Casdoor 顶层字段，最后使用系统生成 fallback。

### 3.5.1 `DisplayName`

默认优先级建议：

1. provider properties 中的 display name，例如 `properties.oauth_<Provider>_displayName`
2. provider 配置字段
3. `preferred_username`
4. `displayName`
5. `nickname`
6. `name`
7. `email`

### 3.5.2 `Username`

`Username` 不应再视为一个统一字段优先级问题，而应按 provider 特征决策。默认策略建议：

1. provider properties 中的 username，例如 `properties.oauth_<Provider>_username`
2. provider 配置字段
3. `preferred_username`
4. `name`
5. `email` 的 `@` 前缀
6. `phone_number` / `phone` 派生值
7. `subject` 的安全缩写

补充约束：

- 对某些 provider 可显式忽略顶层 `name`
- 对手机号类 provider，可直接使用系统生成 username，如 `phone_<number>`

### 3.5.3 `AvatarURL`

默认优先级建议：

1. provider properties 中的 avatar，例如 `properties.oauth_<Provider>_avatarUrl`
2. provider 配置字段
3. `picture`
4. `avatar`
5. `avatar_url`
6. `permanentAvatar`

### 3.5.4 `Phone`

默认优先级建议：

1. `phone_number`
2. `phone`
3. provider properties 中可识别为手机号的字段

### 3.5.5 `Organization`

默认优先级建议：

1. provider 配置字段
2. `owner`
3. `organization`
4. `tenant`

### 3.5.6 `Email`

默认优先级建议：

1. 顶层 `email`
2. provider properties 中的 email，例如 `properties.oauth_<Provider>_email`

但无论来源如何，均应进行邮箱格式校验；若值不符合邮箱格式，则不应写入系统 `email` 字段。必要时可转入 `phone` 候选判断。

后续可扩展读取：

- `email_verified`

## 3.5.7 `properties.oauth_*` 提取

建议为 Casdoor token 中的 `properties` 字段增加统一提取逻辑，而不是在各个 provider 分支内直接硬编码读 map key。

例如新增帮助函数：

```go
func ExtractProviderProfile(provider string, properties map[string]any) map[string]any
```

该逻辑负责提取：

- provider 原始 ID
- provider 原始 username
- provider 原始 display name
- provider 原始 email
- provider 原始 avatar URL

这样可将 Github 的 `oauth_GitHub_*`、idtrust 的 `oauth_Custom_*`、未来其他 provider 的 `oauth_<Provider>_*` 统一抽象处理。

## 3.5.8 已知 provider 特殊规则

### Github

- `Subject = universal_id`
- `ProviderUserID = properties.oauth_GitHub_id`
- `Username = properties.oauth_GitHub_username`
- `DisplayName = properties.oauth_GitHub_displayName`
- `Email = validated(properties.oauth_GitHub_email)`
- `AvatarURL = properties.oauth_GitHub_avatarUrl`

### idtrust / Custom

根据已知样本，顶层 `name` 为随机生成值，不具备业务语义，因此：

- 顶层 `name` 不参与 username / display name 决策
- `ProviderUserID = properties.oauth_Custom_id`
- `Username = properties.oauth_Custom_username`，若为空则回退到 `properties.oauth_Custom_id`
- `DisplayName = properties.oauth_Custom_displayName`
- `Email = validated(properties.oauth_Custom_email)`

若 `properties.oauth_Custom_email` 不是合法邮箱、而是手机号样式，则不应写入 `email`，可进入 `phone` 候选判断。

### 手机号 / 本地 phone 类 provider

- `Subject = universal_id`
- `ProviderUserID = id`
- `Phone = phone_number > phone`
- `DisplayName = displayName`
- `Username` 不使用顶层 `name`，优先采用系统生成值，如 `phone_<number>` 或基于 `subject` 的稳定缩写

## 3.6 归一化组件接口

建议新增独立模块，例如：

- `server/internal/authidentity/normalizer.go`

接口建议如下：

```go
type NormalizeInput struct {
    ProviderName string
    Issuer       string
    AccessToken  string
    JWTClaims    map[string]any
    UserInfo     map[string]any
}

type IdentityNormalizer interface {
    Normalize(input NormalizeInput) (*NormalizedIdentity, error)
}
```

## 3.7 冲突与异常处理

### 3.7.1 无法解析稳定身份

如果以下字段全部缺失：

- `universal_id`
- `sub`
- `id`
- provider 配置的 subject 字段

则应：

1. 记录错误日志
2. 拒绝创建本地用户
3. 返回明确错误 `unable to resolve stable external identity`

### 3.7.2 JWT 与 userinfo 冲突

若 JWT 与 `/api/userinfo` 在关键身份字段上冲突，应：

1. 记录 warning 日志
2. 依据统一优先级决策
3. 保留原始值到 `RawClaims` / `RawUserInfo` 便于排查

## 3.8 结论

统一 `NormalizedIdentity` 后，系统不再直接依赖“某个 provider 恰好返回哪些字段”，而是依赖“归一化后系统如何理解这个外部身份”。这是解决多 provider 差异问题的核心前提。
