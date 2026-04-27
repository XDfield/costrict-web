# 5. 服务层改造与流程设计

## 5.1 总体思路

改造后的核心思路是：

> 所有认证入口先产出 `NormalizedIdentity`，然后统一由用户服务完成用户查找、创建和资料同步。

即不再允许 handler、middleware、service 分别维护不同的字段映射规则。

## 5.2 新增统一解析入口

建议新增统一入口函数，例如：

```go
func ResolveNormalizedIdentity(ctx context.Context, token string) (*NormalizedIdentity, error)
```

内部流程建议为：

1. 尝试解析并验签 JWT
2. 提取 raw claims
3. 提取 `properties` 中的 provider 原始资料
4. 按需调用 `/api/userinfo`
5. 识别 provider / issuer
6. 调用 normalizer 输出 `NormalizedIdentity`

## 5.3 Handler 层改造

### 5.3.1 `AuthCallback`

当前 `AuthCallback` 手工组装 `user.JWTClaims`，建议改为：

1. 交换 OAuth token
2. 获取 raw claims + userinfo
3. 读取并展开 `properties.oauth_*` provider 原始资料
4. 调用归一化器生成 `NormalizedIdentity`
5. 调 `GetOrCreateUserByIdentity(identity)`
6. 设置 cookie 并跳转

伪代码如下：

```go
token := ExchangeCodeForToken(code)
userinfo := GetUserInfo(token.AccessToken)
claims := ParseTokenClaimsIfPossible(token.AccessToken)

identity := normalizer.Normalize(NormalizeInput{
    ProviderName: provider,
    Issuer: issuer,
    AccessToken: token.AccessToken,
    JWTClaims: claims,
    UserInfo: userinfo,
})

user := userService.GetOrCreateUserByIdentity(identity)
```

### 5.3.2 `GetCurrentUser`

建议永远返回系统内统一 user 结构，不再直接透传 Casdoor 原始 userinfo。

即使本地用户不存在，也应先尝试建立标准化 identity，再转成统一返回结构。

## 5.4 Middleware 层改造

### 5.4.1 旧逻辑问题

当前中间件中：

- `parseJWTToken`
- `fetchUserInfo`
- `setAuthContext`

分别承担了 token 解析、userinfo fallback 和身份写上下文的职责，但字段处理不一致。

### 5.4.2 新逻辑建议

改造后流程建议为：

1. `ExtractToken`
2. `ResolveNormalizedIdentity`
3. `GetOrCreateUserByIdentity`
4. `setAuthContextFromUser`

上下文建议统一写入：

- `subject_id`
- `user`
- `normalized_identity`
- `provider_profile`

这样既方便业务层读取，也便于问题排查。

## 5.5 UserService 改造

建议新增方法：

```go
func (s *Service) GetOrCreateUserByIdentity(identity *NormalizedIdentity) (*models.User, error)
```

替代当前 `GetOrCreateUser(claims)` 的直接 claims 依赖。

## 5.6 用户查找逻辑

### 5.6.1 新查找顺序

建议首选：

1. `external_key`

兼容期 fallback：

2. `casdoor_universal_id`
3. `casdoor_sub`
4. `casdoor_id`

不建议继续使用：

5. `username = claims.Name`

因为用户名不能可靠表达唯一身份。

补充：在当前 Casdoor 实际样本下，推荐 `external_key` 优先采用 `casdoor:<universal_id>`。这意味着用户查找主逻辑应优先围绕 `universal_id` 建立，而不是围绕 provider-specific `id` 或顶层 `name` 建立。

### 5.6.2 首次创建逻辑

若按新规则未找到用户：

1. 创建新的本地 `subject_id`
2. 落库稳定身份字段
3. 同步资料字段
4. 写入最后登录时间、最后同步时间

## 5.7 资料同步策略

建议显式定义字段更新策略。

### 5.7.1 建议只填充或谨慎更新

- `Username`

原因：系统内部用户名一旦用于展示、审计、关联资源，频繁变化风险较高。

补充：对于 idtrust / phone 这类 provider，`Username` 取值需严格执行 provider-aware 规则，不允许简单从顶层 `name` 回填。特别是 idtrust 顶层 `name` 为随机值，phone 顶层 `name` 可能是 UUID，均不应写入本地用户名。

### 5.7.2 建议可按登录同步

- `DisplayName`
- `AvatarURL`
- `Email`
- `Phone`
- `Organization`

建议引入策略配置，例如：

```yaml
syncProfile:
  username: fill_only
  displayName: overwrite_if_non_empty
  avatarUrl: overwrite_if_non_empty
  email: overwrite_if_non_empty
  phone: overwrite_if_non_empty
  organization: overwrite_if_non_empty
```

## 5.7.3 provider 资料校验

在把 provider 原始资料写入本地用户前，建议增加基础语义校验：

1. `email` 需通过邮箱格式校验
2. `phone` 需通过手机号格式校验
3. 不符合语义的字段不应因字段名而强制落库

例如：`properties.oauth_Custom_email = 15500000001` 时，不应直接写入 `email`，而应优先识别为 `phone` 候选值。

## 5.8 时序建议

### 5.8.1 登录回调时序

1. 前端触发 Casdoor 登录
2. Casdoor 回调到服务端 `AuthCallback`
3. 服务端获取 token / userinfo
4. 服务端归一化身份
5. 服务端查找或创建本地用户
6. 设置认证 cookie
7. 跳转前端

### 5.8.2 请求鉴权时序

1. 客户端携带 Bearer token / cookie
2. 中间件提取 token
3. 中间件归一化身份
4. 用户服务解析本地用户
5. 将 `subject_id` 和 `user` 写入上下文
6. 业务 handler 继续执行

## 5.9 结论

服务层改造的关键不是“把旧逻辑搬到别的函数”，而是要把认证结果统一抽象成 `NormalizedIdentity`，再围绕这个模型重建用户查找与资料同步流程。
