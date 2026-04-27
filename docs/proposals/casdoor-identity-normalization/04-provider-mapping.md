# 4. Provider 配置化映射方案

## 4.1 设计动机

不同 Casdoor provider 返回的字段名和语义差异较大，仅靠代码中硬编码的 fallback 无法长期维护。为避免每新增一个 provider 就修改 handler、middleware、service 多处代码，建议将 provider 差异抽象为配置。

## 4.2 配置目标

配置化方案需要满足以下要求：

1. 支持按 provider 定义字段候选列表
2. 支持 `default` 兜底配置
3. 支持读取 Casdoor token 中 `properties.oauth_*` 的 provider 原始资料字段
4. 支持将 JWT claims 与 userinfo 视为同一字段空间的候选来源
5. 支持后续扩展更多资料字段或特殊规则

## 4.3 建议配置结构

建议在现有认证配置下新增结构：

```yaml
auth:
  casdoor:
    providers:
      default:
        subjectCandidates:
          - universal_id
          - sub
          - id
        externalUserIdCandidates:
          - id
          - user_id
          - uid
        universalIdCandidates:
          - universal_id
          - universalId
        providerUserIdCandidates:
          - properties.oauth_<Provider>_id
          - id
        usernameCandidates:
          - properties.oauth_<Provider>_username
          - preferred_username
          - name
          - email
        displayNameCandidates:
          - properties.oauth_<Provider>_displayName
          - preferred_username
          - displayName
          - nickname
          - name
        emailCandidates:
          - properties.oauth_<Provider>_email
          - email
        avatarCandidates:
          - properties.oauth_<Provider>_avatarUrl
          - picture
          - avatar
          - avatar_url
          - permanentAvatar
        phoneCandidates:
          - phone_number
          - phone
        organizationCandidates:
          - owner
          - organization
          - tenant

      github:
        subjectCandidates:
          - universal_id
          - sub
        usernameCandidates:
          - properties.oauth_GitHub_username
          - name
        displayNameCandidates:
          - properties.oauth_GitHub_displayName
          - displayName
        emailCandidates:
          - properties.oauth_GitHub_email
          - email
        avatarCandidates:
          - properties.oauth_GitHub_avatarUrl
          - permanentAvatar
        providerUserIdCandidates:
          - properties.oauth_GitHub_id
          - id

      idtrust:
        subjectCandidates:
          - universal_id
          - sub
        providerUserIdCandidates:
          - properties.oauth_Custom_id
          - id
        usernameCandidates:
          - properties.oauth_Custom_username
          - properties.oauth_Custom_id
        displayNameCandidates:
          - properties.oauth_Custom_displayName
          - displayName
        emailCandidates:
          - properties.oauth_Custom_email
        phoneCandidates:
          - phone_number
          - phone
          - properties.oauth_Custom_email
        ignoreTopLevelFields:
          - name

      phone:
        subjectCandidates:
          - universal_id
          - sub
        providerUserIdCandidates:
          - id
        usernameStrategy: generated_from_phone_or_subject
        displayNameCandidates:
          - displayName
        phoneCandidates:
          - phone_number
          - phone
        ignoreTopLevelFields:
          - name
```

## 4.4 配置解析规则

归一化器使用配置的基本规则为：

1. 根据 provider name 查找专属配置
2. 若未命中，则使用 `default`
3. 每个字段按候选列表顺序依次尝试取值
4. `properties.oauth_<Provider>_*` 按 provider 模板展开后参与取值
5. 若 JWT claims 和 userinfo 中都存在候选字段，则按统一来源优先级决策

## 4.4.1 `properties.oauth_*` 模板展开

对于 `properties.oauth_<Provider>_*` 形式的路径，建议归一化器内部支持模板展开。例如：

- provider = `Github` 时，`properties.oauth_<Provider>_id` 展开为 `properties.oauth_GitHub_id`
- provider = `idtrust` 且其接入约定为 `Custom` 时，可通过 provider alias 配置映射到 `properties.oauth_Custom_id`

因此建议配置支持：

- `providerAliases`
- `propertyPrefix`

例如：

```yaml
idtrust:
  propertyPrefix: oauth_Custom
```

## 4.5 来源优先级建议

建议对原始数据源也定义统一优先级：

1. 已验签 JWT claims
2. `/api/userinfo`
3. 未验签 access token claims

在当前 Casdoor 实际样本中，provider 原始资料信息主要出现在 token 的 `properties` 字段中，因此对资料字段提取，JWT claims 往往比 `/api/userinfo` 更完整。

说明：

- 中间件中已验签 JWT claims 更可信
- `/api/userinfo` 是 Casdoor 当前用户视图，也应具备高优先级
- access token 未验签解析只适合补字段，不适合作为唯一真相来源

## 4.6 Provider 识别方式

为正确应用 provider mapping，需要先识别当前身份来自哪个 provider。建议来源优先级如下：

1. Casdoor 已提供的 provider 标识字段
2. 配置中按 issuer 映射 provider
3. OAuth 登录 state / callback context 中传递的 provider 信息
4. 若仍无法识别，使用 `default`

补充建议：对于手机号 / 本地账号等 token 中可能缺失 `provider` 的场景，可增加基于字段特征的兜底识别，例如：

- 存在 `phone_number` 且 отсутствует provider 时，识别为 `phone`

如果当前系统尚未保存 provider 名称，需要在登录回调或认证上下文中补充该信息。

## 4.7 特殊规则扩展

配置除了字段候选列表外，未来还可扩展特殊行为，例如：

- `usernameTransform`: lower / slug / keep
- `usernameStrategy`: direct / fill_only / generated_from_phone_or_subject
- `subjectPrefix`: 为某些 provider 增加固定前缀
- `requireEmail`: 某些 provider 强制邮箱存在
- `ignoreClaims`: 忽略部分低可信字段
- `ignoreTopLevelFields`: 显式忽略顶层低可信字段，例如 idtrust 的 `name`

第一阶段可以先不实现这些高级规则，但配置结构应为未来扩展预留空间。

## 4.8 配置错误处理

若 provider 配置缺失或字段候选全部取不到，处理建议：

1. 记录 error 日志，包含 provider 与 claims 摘要
2. 对身份字段解析失败直接终止认证映射
3. 对资料字段解析失败允许降级为空

## 4.9 三类已知 provider 映射矩阵

### 4.9.1 Github

| 系统字段 | 来源 |
|----------|------|
| `Subject` | `universal_id` |
| `ProviderUserID` | `properties.oauth_GitHub_id` |
| `Username` | `properties.oauth_GitHub_username` |
| `DisplayName` | `properties.oauth_GitHub_displayName` |
| `Email` | `properties.oauth_GitHub_email`（需校验） |
| `AvatarURL` | `properties.oauth_GitHub_avatarUrl` |

### 4.9.2 idtrust / Custom

| 系统字段 | 来源 |
|----------|------|
| `Subject` | `universal_id` |
| `ProviderUserID` | `properties.oauth_Custom_id` |
| `Username` | `properties.oauth_Custom_username`，为空则 `properties.oauth_Custom_id` |
| `DisplayName` | `properties.oauth_Custom_displayName` |
| `Email` | `properties.oauth_Custom_email`（仅合法邮箱时写入） |
| `Phone` | `phone_number` / `phone` / `properties.oauth_Custom_email`（若为手机号） |

补充：顶层 `name` 为随机生成值，不参与 username / display name 决策。

### 4.9.3 phone

| 系统字段 | 来源 |
|----------|------|
| `Subject` | `universal_id` |
| `ProviderUserID` | `id` |
| `Phone` | `phone_number`，否则 `phone` |
| `DisplayName` | `displayName` |
| `Username` | 系统生成，例如 `phone_<number>` |

## 4.10 结论

provider 配置化的价值不只是“多几个字段 fallback”，而是把“provider 差异”从代码逻辑中剥离出来，使认证归一化层具备可维护性和可扩展性。
