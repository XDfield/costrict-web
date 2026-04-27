# 6. 数据模型与迁移方案

## 6.1 现有模型问题

当前本地用户表保存了部分 Casdoor 相关字段：

- `casdoor_id`
- `casdoor_universal_id`
- `casdoor_sub`
- `organization`

但缺少一个真正统一的稳定外部身份键，因此在多 provider 场景下，系统很难基于单一键完成稳定查找。

## 6.2 方案对比

## 6.2.1 方案 A：在 `users` 表上增量扩展

建议新增字段：

- `auth_provider`
- `auth_issuer`
- `external_subject`
- `external_key`

示意：

```go
type User struct {
    SubjectID          string
    Username           string
    DisplayName        string
    Email              string
    AvatarURL          string

    AuthProvider       string
    AuthIssuer         string
    ExternalSubject    string
    ExternalKey        string

    CasdoorID          string
    CasdoorUniversalID string
    CasdoorSub         string
    Organization       string
}
```

建议索引：

- `unique index idx_users_external_key (external_key)`
- `index idx_users_auth_provider (auth_provider)`

### 优点

1. 改动较小
2. 易于落地
3. 兼容现有用户表结构

### 缺点

1. 一个用户只能自然表达一组主认证身份
2. 后续若需绑定多个 provider 身份，扩展性一般

## 6.2.2 方案 B：新增 `user_auth_identities` 表

建议将外部身份独立建表：

```go
type UserAuthIdentity struct {
    ID              uint
    UserSubjectID   string
    Provider        string
    Issuer          string
    ExternalSubject string
    ExternalUserID  string
    UniversalID     string
    ExternalKey     string
    RawSub          string
    Organization    string
    CreatedAt       time.Time
    UpdatedAt       time.Time
}
```

建议索引：

- `unique index idx_user_auth_identities_external_key (external_key)`
- `index idx_user_auth_identities_user_subject_id (user_subject_id)`

### 优点

1. 最符合多 provider / 多身份绑定场景
2. 可支持未来账号绑定、解绑、合并
3. 外部身份与本地用户职责更清晰

### 缺点

1. 改造量更大
2. 查询与迁移路径稍复杂

## 6.3 推荐选择

### 短期推荐

若目标是尽快解决当前多 provider 字段不齐和重复用户问题，推荐先做 **方案 A**。

### 中长期推荐

若系统后续存在：

- 多 SSO 来源
- 账号合并需求
- 一个本地用户绑定多个外部身份需求

则推荐演进到 **方案 B**。

## 6.4 迁移策略

## 6.4.1 第一步：结构变更

按所选方案完成：

- 新增字段，或
- 新建 `user_auth_identities` 表

## 6.4.2 第二步：回填旧数据

基于现有字段生成新的稳定身份信息：

`external_subject` 建议优先级：

1. `casdoor_universal_id`
2. `casdoor_sub`
3. `casdoor_id`

`external_key` 建议默认生成：

```text
casdoor-default:<external_subject>
```

在无法识别历史 provider 时，先以 `casdoor-default` 作为兼容值。

## 6.4.3 第三步：登录即迁移

新代码上线后，用户登录时：

1. 先按新 `external_key` 查找
2. 若未命中，则 fallback 旧字段查找
3. 命中旧记录后自动补写 `auth_provider` / `external_subject` / `external_key`

这样可以逐步把老用户迁移到新结构，而不要求一次性全量迁移完成。

## 6.4.4 第四步：观察与收敛

在一段观察期内：

1. 保留旧字段兼容查找
2. 监控重复用户与未命中情况
3. 待覆盖率稳定后，再考虑减少旧逻辑依赖

## 6.5 唯一性约束建议

迁移完成后，应尽量将本地用户查找主键切换为：

- `external_key`

注意：

1. 上线前要确认历史数据不会导致唯一索引冲突
2. 若已有重复用户，需要先人工或脚本治理

## 6.6 与现有字段关系

即使新增 `external_key`，短期内仍建议保留：

- `casdoor_id`
- `casdoor_universal_id`
- `casdoor_sub`

原因：

1. 兼容旧代码与已有数据
2. 方便排查 provider 差异
3. 可作为迁移期 fallback 查找条件

## 6.7 结论

数据库改造的核心不是“多存几个 Casdoor 字段”，而是建立一个可唯一索引、可稳定回查的外部身份键，并设计平滑迁移路径。
