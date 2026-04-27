# 7. API、一致性约束与可观测性

## 7.1 API 返回统一目标

当前 `GetCurrentUser` 存在两种返回风格：

1. 命中本地用户时返回系统内 user 结构
2. 未命中本地用户时 fallback 返回 Casdoor 原始 userinfo

这会使前端在不同场景下拿到不同数据模型，增加适配复杂度。

本提案建议：

> 所有用户相关接口都应返回统一的系统内 user DTO，不直接透传 Casdoor 原始 userinfo。

## 7.2 建议返回结构

建议统一为：

```json
{
  "id": "usr_xxx",
  "subjectId": "usr_xxx",
  "username": "alice",
  "displayName": "Alice Zhang",
  "email": "alice@example.com",
  "avatarUrl": "https://...",
  "organization": "my-org",
  "auth": {
    "provider": "github",
    "issuer": "https://casdoor.example.com",
    "externalSubject": "12345678"
  }
}
```

## 7.3 一致性约束

### 7.3.1 用户接口

以下接口应保证统一口径：

- 登录回调后的当前用户获取
- 请求上下文中的当前用户查询
- 任何基于认证上下文返回的 user 信息

### 7.3.2 上下文字段

建议认证中间件在上下文中统一存储：

- `subject_id`
- `user`
- `normalized_identity`

避免业务代码直接依赖旧的 `sub` / `userId` 混合语义。

## 7.4 日志设计

为便于排查多 provider 身份问题，建议在认证归一化和用户查找过程中增加结构化日志。

### 7.4.1 建议日志字段

- `provider`
- `issuer`
- `source` (`jwt` / `userinfo` / `merged`)
- `resolved_subject`
- `raw_sub`
- `external_user_id`
- `universal_id`
- `external_key`
- `matched_by` (`external_key` / `casdoor_universal_id` / `casdoor_sub` / `casdoor_id`)
- `user_subject_id`

### 7.4.2 冲突日志

当出现以下情况时，建议记录 warning：

1. JWT 与 userinfo 中的身份字段冲突
2. provider 识别失败，回退到 default 配置
3. 无法解析头像、组织等关键资料字段
4. 新查找失败后只能靠旧字段命中

## 7.5 监控指标建议

建议新增认证映射相关指标：

- `auth_identity_normalize_total`
- `auth_identity_normalize_failed_total`
- `auth_identity_provider_fallback_total`
- `auth_user_lookup_by_external_key_total`
- `auth_user_lookup_by_legacy_field_total`
- `auth_user_created_total`
- `auth_user_duplicate_suspected_total`

通过这些指标可观察：

1. 新逻辑是否稳定
2. 旧字段 fallback 是否还大量存在
3. 是否仍有疑似重复用户问题

## 7.6 审计与问题排查

建议在调试级别日志或安全审计日志中保留：

- 归一化前的关键信息摘要
- 最终选中的 subject 字段来源
- 最终 external key

注意：

1. 不建议完整打印原始 token
2. 邮箱、头像 URL 等字段应按现有日志规范做必要脱敏

## 7.7 结论

API 统一和可观测性并非附属工作，而是确保该方案可落地、可验证、可排障的必要部分。
