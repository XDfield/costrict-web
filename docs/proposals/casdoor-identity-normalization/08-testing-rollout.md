# 8. 测试、上线与回滚方案

## 8.1 测试目标

本次改造的关键不是单点函数正确，而是确保不同认证入口、不同 provider、不同字段风格下都能得到一致且稳定的本地 user 映射结果。

## 8.2 单元测试

## 8.2.1 归一化器测试

需要覆盖以下典型输入：

### 场景 A：标准 OIDC

```json
{
  "sub": "abc",
  "preferred_username": "alice",
  "name": "Alice",
  "email": "alice@example.com",
  "picture": "https://..."
}
```

### 场景 B：Casdoor 风格

```json
{
  "id": "u123",
  "universal_id": "global-1",
  "name": "alice",
  "owner": "built-in"
}
```

### 场景 C：GitHub 风格

```json
{
  "id": "10001",
  "login": "alice-gh",
  "name": "Alice Zhang",
  "avatar_url": "https://..."
}
```

### 场景 D：自定义 OIDC

```json
{
  "user_id": "emp-001",
  "display_name": "Alice",
  "tenant": "corp-a"
}
```

验证项至少包括：

- `Provider`
- `Subject`
- `ExternalKey`
- `Username`
- `DisplayName`
- `AvatarURL`
- `Organization`

## 8.2.2 用户服务测试

覆盖以下场景：

1. 首次登录创建用户
2. 相同 provider 再次登录命中同一用户
3. 旧字段兼容查找命中已有用户并自动回填新字段
4. 缺少稳定身份字段时报错
5. 资料同步策略生效验证

## 8.3 集成测试

需要验证以下三条路径最终结果一致：

1. 登录回调路径
2. Bearer JWT 直解析路径
3. JWT 失败后的 `/api/userinfo` fallback 路径

验证点包括：

- 是否命中同一 `subject_id`
- 返回给前端的用户结构是否一致
- 是否使用同一 `external_key`

## 8.4 回归测试重点

除新增测试外，还需重点回归：

1. 现有登录流程是否受影响
2. JWT 验签失败后的降级是否可用
3. 非 Casdoor provider 默认配置是否行为合理
4. 现有依赖 `subject_id` 的业务是否不受影响

## 8.5 上线策略

建议按阶段上线：

### Phase 1：基础能力上线

1. 新增 `NormalizedIdentity`
2. 新增 provider mapping 配置
3. 新增数据库字段或新表
4. 仅在影子模式下记录归一化结果，不替换旧逻辑

### Phase 2：双写 / 双查阶段

1. 认证链路同时跑旧逻辑和新逻辑
2. 新逻辑生成 `external_key`
3. 用户服务优先新查找，失败后 fallback 旧查找
4. 持续观察日志与指标

### Phase 3：主逻辑切换

1. Handler 与 middleware 全部切到 `NormalizedIdentity`
2. `GetCurrentUser` 统一返回系统 DTO
3. 强化 `external_key` 主查找地位

### Phase 4：收敛与清理

1. 根据指标判断是否可以弱化旧字段 fallback
2. 清理重复用户治理脚本遗留事项
3. 梳理是否需要推进 `user_auth_identities` 二期改造

## 8.6 风险分析

### 风险 1：历史数据无法唯一映射

说明：已有用户可能基于旧字段建立，且不同 provider 曾创建重复用户。

缓解：

1. 上线前做重复用户扫描
2. 保留旧字段 fallback 查找
3. 对冲突记录人工治理

### 风险 2：provider 识别不准确

说明：若 provider 名无法正确识别，会错误应用 default mapping。

缓解：

1. 明确 provider 来源
2. 增加 provider fallback 告警
3. 在配置中支持 issuer 映射

### 风险 3：资料字段覆盖策略不当

说明：新 provider 登录后可能覆盖已有显示名或头像。

缓解：

1. 明确 `fill_only` / `overwrite_if_non_empty` 策略
2. 初期保守更新 `Username`

## 8.7 回滚方案

若新逻辑出现问题，回滚策略应包括：

1. 关闭新归一化逻辑开关
2. 恢复旧的 handler / middleware 字段映射路径
3. 保留已写入的新字段，不做破坏性删除
4. 继续使用旧字段查找逻辑保障登录能力

由于本方案推荐新增字段而非替换原字段，回滚成本相对可控。

## 8.8 最终建议

建议本次改造采用“先统一归一化层、再切主链路、最后收敛旧逻辑”的方式推进。这样既能尽快解决多 provider 字段不齐问题，又能控制对现有认证链路的扰动。
