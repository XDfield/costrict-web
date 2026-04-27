# 2. 当前实现分析

## 2.1 关键代码位置

当前 Casdoor 认证转 user 信息的主链路涉及以下模块：

| 文件 | 关键函数 | 作用 |
|------|----------|------|
| `server/internal/handlers/handlers.go` | `AuthCallback` | OAuth 回调、换 token、获取 userinfo、触发本地用户创建 |
| `server/internal/casdoor/client.go` | `GetUserInfo`、`CasdoorUser.UnmarshalJSON` | 调 Casdoor `/api/userinfo` 并解析返回 |
| `server/internal/middleware/auth.go` | `parseJWTToken`、`fetchUserInfo`、`setAuthContext` | 请求期 Bearer Token 解析与认证上下文设置 |
| `server/internal/user/service.go` | `ParseJWTClaimsFromAccessToken`、`GetOrCreateUser`、`ResolveSubjectID` | JWT claims 补充、用户查找创建、subject 映射 |
| `server/cmd/api/main.go` | `middleware.SetSubjectResolver(...)` | 将认证结果接入用户服务 |

## 2.2 当前三条认证映射路径

### 2.2.1 登录回调路径

当前 `AuthCallback` 的主要流程为：

1. `ExchangeCodeForToken()` 获取 `access_token`
2. `GetUserInfo()` 调 Casdoor `/api/userinfo`
3. `ParseJWTClaimsFromAccessToken()` 从 access token 中补充字段
4. 构造 `user.JWTClaims`
5. 调 `GetOrCreateUser()` 查找或创建本地 `models.User`

特点：

- 使用 userinfo 作为主输入
- access token claims 只作为补充来源
- 最终以 `user.JWTClaims` 形式交给用户服务

### 2.2.2 请求中间件 JWT 直解析路径

当前 `parseJWTToken` 负责：

1. 验证 JWT 签名
2. 读取固定 claim 字段
3. 产出中间件内部的 `CasdoorUserInfo`
4. 在 `setAuthContext` 中进一步映射到系统上下文

该路径对身份字段的主要假设是：

- 优先 `id`
- 否则 `sub`
- 否则 `universal_id`

这意味着中间件内部的 `Sub` 并不一定是 JWT 标准 `sub`，而可能是 `id` 或 `universal_id` 的替代值。

### 2.2.3 `/api/userinfo` fallback 路径

当 JWT 解析失败时，中间件会 fallback 到 Casdoor `/api/userinfo`。该路径再次进行一套独立映射，但它与登录回调使用的 `CasdoorUser` 映射规则并不完全一致，例如：

- 不统一读取 `preferred_username`
- 不统一处理头像字段
- 要求 `sub` 必须存在

## 2.3 当前字段映射方式

### 2.3.1 本地用户字段映射

当前本地 `models.User` 的核心映射大致如下：

| 外部字段 | 本地字段 |
|----------|----------|
| `Name` | `Username` |
| `PreferredUsername` | `DisplayName` |
| `Email` | `Email` |
| `Picture` | `AvatarURL` |
| `ID` | `CasdoorID` |
| `UniversalID` | `CasdoorUniversalID` |
| `Sub` | `CasdoorSub` |
| `Owner` | `Organization` |

### 2.3.2 用户查找顺序

当前 `GetOrCreateUser` 大致按以下顺序查找已有用户：

1. `casdoor_universal_id`
2. `casdoor_id`
3. `casdoor_sub`
4. `username = claims.Name`

最后 fallback 到用户名查找属于高风险行为，因为用户名并不一定稳定或唯一。

## 2.4 当前实现的主要问题

### 2.4.1 固定字段假设过强

代码主要识别以下字段：

- 身份相关：`id`、`sub`、`universal_id`
- 资料相关：`name`、`preferred_username`、`email`、`picture`、`owner`

如果不同 provider 使用以下替代字段，则当前实现大概率会丢值：

- `user_id`
- `uid`
- `preferredUsername`
- `display_name`
- `avatar_url`
- `tenant`
- `organization`

### 2.4.2 三条路径行为不一致

同一个用户在以下场景下可能得到不同结果：

1. 首次登录回调
2. 后续 Bearer JWT 请求
3. JWT 失败后的 userinfo fallback

这会导致：

- `DisplayName` 来源不一致
- `AvatarURL` 某些路径可取到、某些路径取不到
- `Organization` 有时存在、有时为空

### 2.4.3 身份字段语义混乱

当前某些逻辑把 `id`、`sub`、`universal_id` 混合作为 `Sub`。这会带来两个后果：

1. 业务方难以知道系统内到底拿到的是哪种身份字段
2. 数据库存储的 `casdoor_sub` 与请求上下文中的 `userId` 语义不一定一致

### 2.4.4 重复用户风险

当多个 provider 返回的身份字段差异较大时，系统可能无法将其匹配到同一条本地用户记录，从而创建多个本地用户。这是当前最核心的业务风险。

## 2.5 结论

当前系统的问题本质不是单一字段缺失，而是：

> 系统缺少一个统一、可配置、可审计的身份归一化层。

因此后续改造重点不应只是“多加几个 fallback 字段”，而应该建立标准化身份模型，并将所有入口收敛到同一套解析与查找逻辑。
