# Casdoor 多 Provider 身份归一化设计提案

> **实现状态：提案中**
>
> - 状态：设计阶段
> - 目标位置：`server/internal/authidentity/`、`server/internal/user/`、`server/internal/middleware/`
> - 关注问题：Casdoor 多 provider 下 JWT / userinfo 字段不一致导致的身份映射不稳定

---

## 文档索引

| 文档 | 说明 |
|------|------|
| [01-overview.md](./01-overview.md) | 背景、问题定义、目标与非目标 |
| [02-current-state-analysis.md](./02-current-state-analysis.md) | 当前实现分析、关键链路、现存风险 |
| [03-normalized-identity.md](./03-normalized-identity.md) | 统一身份模型与字段归一化规则 |
| [04-provider-mapping.md](./04-provider-mapping.md) | Provider 配置化映射方案 |
| [05-service-and-flow.md](./05-service-and-flow.md) | Handler / Middleware / UserService 改造方案与时序 |
| [06-database-migration.md](./06-database-migration.md) | 数据模型、迁移策略、兼容方案 |
| [07-api-and-observability.md](./07-api-and-observability.md) | API 返回约束、日志、监控与审计 |
| [08-testing-rollout.md](./08-testing-rollout.md) | 测试方案、分阶段上线、风险与回滚 |
