# 用户表实施进度

基于 `docs/proposals/USER_TABLE_DESIGN.md`，用户数据表设计与实施任务跟踪。

## 任务列表

### 阶段一：数据库与模型（P0）

#### 1. 数据模型定义
- [ ] `server/internal/models/models.go` — 追加 `User` 结构体
- [ ] `server/internal/models/models.go` — 更新 `AutoMigrate` 注册 `User` 表

#### 2. 数据库迁移
- [ ] 创建迁移文件 `server/internal/database/migrations/20250401_create_users_table.go`
- [ ] 编写 SQL 迁移脚本（包含索引）
- [ ] 测试迁移脚本执行

### 阶段二：UserService 实现（P0）

#### 3. UserService 基础功能（`server/internal/user/service.go`）
- [ ] `NewUserService(db *gorm.DB) *UserService`
- [ ] `GetUserByID(userID string) (*models.User, error)` — 根据 ID 获取用户
- [ ] `GetUsersByIDs(userIDs []string) (map[string]*models.User, error)` — 批量获取用户
- [ ] `SearchUsers(keyword string, limit int) ([]*models.User, error)` — 搜索用户
- [ ] `GetOrCreateUser(claims *JWTClaims) (*models.User, error)` — 登录时获取或创建用户

#### 4. CachedUserService 缓存层（`server/internal/user/cached_service.go`）
- [ ] `NewCachedUserService(db *gorm.DB) *CachedUserService`
- [ ] `GetUserByID(ctx context.Context, userID string) (*models.User, error)` — 带缓存
- [ ] `GetUsersByIDs(ctx context.Context, userIDs []string) (map[string]*models.User, error)` — 批量带缓存
- [ ] `InvalidateCache(userID string)` — 使缓存失效
- [ ] `WarmupCache(ctx context.Context) error` — 预热缓存

### 阶段三：认证集成（P0）

#### 5. 登录回调集成（`server/internal/handlers/auth.go`）
- [ ] 修改 `AuthCallback` 处理器
  - [ ] 解析 JWT Token 获取用户信息
  - [ ] 调用 `UserService.GetOrCreateUser` 创建或更新用户
  - [ ] 记录日志（成功/失败）

#### 6. 认证中间件保持不变
- [x] `OptionalAuth` — 只解析 JWT，不查询数据库（保持现有实现）
- [x] `RequireAuth` — 只解析 JWT，不查询数据库（保持现有实现）

### 阶段四：用户查询接口改造（P1）

#### 7. SearchUsers 改造（`server/internal/handlers/users.go`）
- [ ] 修改 `SearchUsers` 处理器
  - [ ] 改用 `UserService.SearchUsers` 查询本地数据库
  - [ ] 保留降级方案（本地未找到时查 Casdoor）
  - [ ] 更新 Swagger 文档注释

#### 8. GetUserNames 改造（`server/internal/handlers/users.go`）
- [ ] 修改 `GetUserNames` 处理器
  - [ ] 改用 `CachedUserService.GetUsersByIDs` 查询本地数据库
  - [ ] 保留内存缓存作为二级缓存
  - [ ] 更新 Swagger 文档注释

#### 9. 模块初始化（`server/internal/user/user.go`）
- [ ] `Module` 结构体 + `New(db *gorm.DB) *Module`
- [ ] `RegisterRoutes(apiGroup *gin.RouterGroup)` — 注册用户相关路由（如需要）

### 阶段五：数据迁移与同步（P1）

#### 10. 历史数据迁移
- [ ] 编写数据迁移脚本
  - [ ] 从 `devices` 表迁移 `user_id`
  - [ ] 从 `repositories` 表迁移 `owner_id`
  - [ ] 从其他表迁移用户相关字段
- [ ] 执行数据迁移
- [ ] 验证数据完整性

#### 11. 用户同步机制（可选，P2）
- [ ] 实现定期同步任务
  - [ ] 从 Casdoor 同步活跃用户信息
  - [ ] 更新 `last_sync_at` 字段
- [ ] 实现 Webhook 监听（如果 Casdoor 支持）
  - [ ] 用户创建/更新事件
  - [ ] 用户删除事件

### 阶段六：测试与文档（P0）

#### 12. 单元测试
- [ ] `UserService` 单元测试
  - [ ] `GetUserByID` 测试
  - [ ] `GetUsersByIDs` 测试
  - [ ] `SearchUsers` 测试
  - [ ] `GetOrCreateUser` 测试
- [ ] `CachedUserService` 单元测试
  - [ ] 缓存命中测试
  - [ ] 缓存未命中测试
  - [ ] 缓存失效测试

#### 13. 集成测试
- [ ] 登录流程集成测试
  - [ ] 新用户登录（创建）
  - [ ] 老用户登录（更新）
- [ ] 用户查询接口集成测试
  - [ ] `SearchUsers` 测试
  - [ ] `GetUserNames` 测试

#### 14. 文档更新
- [ ] 更新 API 文档（Swagger）
- [ ] 更新部署文档（数据库迁移说明）
- [ ] 更新开发文档（UserService 使用说明）

### 阶段七：监控与优化（P2）

#### 15. 性能监控
- [ ] 添加性能指标
  - [ ] 用户表查询耗时
  - [ ] 缓存命中率
  - [ ] GetOrCreate 成功率
- [ ] 添加告警规则
  - [ ] 查询超时告警
  - [ ] 缓存命中率低告警

#### 16. 性能优化
- [ ] 数据库查询优化
  - [ ] 分析慢查询
  - [ ] 优化索引
- [ ] 缓存优化
  - [ ] 调整缓存 TTL
  - [ ] 考虑使用 Redis 替代内存缓存

---

## 进度统计

- **总任务数**: 50
- **已完成**: 2 (4%)
- **进行中**: 0
- **待开始**: 48

### 阶段完成度

| 阶段 | 任务数 | 已完成 | 完成度 |
|------|--------|--------|--------|
| 阶段一：数据库与模型 | 3 | 0 | 0% |
| 阶段二：UserService 实现 | 9 | 0 | 0% |
| 阶段三：认证集成 | 1 | 0 | 0% |
| 阶段四：用户查询接口改造 | 3 | 0 | 0% |
| 阶段五：数据迁移与同步 | 2 | 0 | 0% |
| 阶段六：测试与文档 | 3 | 0 | 0% |
| 阶段七：监控与优化 | 2 | 0 | 0% |

---

## 实施说明

### 优先级说明
- **P0**: 核心功能，必须完成
- **P1**: 重要功能，建议完成
- **P2**: 优化功能，可选完成

### 关键设计原则
1. **性能优先**: 认证中间件不查询数据库，只解析 JWT
2. **按需查询**: 用户详细信息按需查询，带缓存
3. **降级方案**: 保留 Casdoor API 作为降级方案
4. **数据一致性**: 登录时同步用户信息，确保数据最新

### 风险与注意事项
1. **数据迁移风险**: 历史数据迁移需要充分测试
2. **缓存一致性**: 需要确保缓存与数据库的一致性
3. **降级策略**: 需要确保降级方案的可靠性
4. **性能监控**: 需要监控缓存命中率和查询性能

### 回滚方案
1. 保留 Casdoor API 调用代码
2. 通过配置开关控制是否使用本地用户表
3. 确保可以快速切换回纯 Casdoor 模式

---

## 参考文档

- [用户表设计提案](../docs/proposals/USER_TABLE_DESIGN.md)
- [Casdoor 客户端实现](../server/internal/casdoor/client.go)
- [认证中间件实现](../server/internal/middleware/auth.go)
- [用户处理器实现](../server/internal/handlers/users.go)