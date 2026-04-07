# 用户数据表设计提案

## 1. 背景与目标

### 1.1 当前现状
- 项目使用 Casdoor 作为唯一的用户认证和数据源
- 所有用户信息存储在 Casdoor 中
- 业务数据库只存储用户引用（`sub` 字段）
- 用户信息获取依赖 Casdoor API 或 JWT Token 解析

### 1.2 存在的问题
1. **性能问题**：每次获取用户信息都需要调用 Casdoor API
2. **可用性风险**：Casdoor 服务故障会影响用户信息获取
3. **查询限制**：无法在业务数据库中进行用户相关的高效查询
4. **扩展性差**：难以添加业务特定的用户字段
5. **数据一致性**：多表关联查询时需要频繁调用外部 API

### 1.3 目标
1. 创建本地用户数据表，实现用户信息的本地化存储
2. 支持登录时的 `get_or_create` 操作
3. 替代部分 Casdoor 查询场景，提升性能
4. 为后续用户信息映射和扩展提供基础

## 2. Casdoor 用户模型分析

### 2.1 JWT Token 中的用户字段

根据代码分析 (`server/internal/middleware/auth.go:parseJWTToken`)，JWT Token 包含以下字段：

| 字段名 | 类型 | 说明 | 示例值 |
|--------|------|------|--------|
| `id` | string | 用户 ID | `b84b16c2-d1f1-4f3e-9e5a-7c8d9e0a1b2c` |
| `sub` | string | 用户唯一标识（主要） | `user-group/alice` |
| `universal_id` | string | 通用唯一 ID | `b84b16c2-d1f1-4f3e-9e5a-7c8d9e0a1b2c` |
| `name` | string | 用户名 | `alice` |
| `preferred_username` | string | 显示名称 | `Alice Smith` |
| `email` | string | 邮箱 | `alice@example.com` |
| `picture` | string | 头像 URL | `http://.../avatar.png` |
| `owner` | string | 所属组织 | `user-group` |

### 2.2 用户唯一标识分析

**当前代码中的优先级** (`server/internal/middleware/auth.go:parseJWTToken`):
```go
// 提取用户标识（优先级：id > sub > universal_id）
sub, _ := claims["id"].(string)
if sub == "" {
    sub, _ = claims["sub"].(string)
}
if sub == "" {
    sub, _ = claims["universal_id"].(string)
}
```

**实际使用情况**：
- **主要标识**：`id` 或 `sub` 字段（UUID 格式）
- **业务代码使用**：`c.GetString(middleware.UserIDKey)` 获取的是解析出的用户 ID
- **数据库存储**：所有表的 `user_id` 字段都存储这个 UUID 值

**关于 `{organization}/{username}` 格式**：
- 这是代码中**手动合成**的格式（`casdoor/client.go:63-67`）
- 仅用于 **Casdoor Admin API** 返回的数据（没有 `sub` 字段时）
- **不是 JWT Token 中的实际 `sub` 值**

**结论**：
- **唯一键选择**：使用 JWT Token 中的 `id` 或 `sub` 字段作为主键（UUID 格式）
- **原因**：
  1. 与现有数据库表结构一致（存储的是 UUID）
  2. JWT Token 中必然包含
  3. 真实的用户唯一标识，而非合成值
  4. 全局唯一且稳定

### 2.3 Casdoor API 返回的用户字段

根据代码分析 (`server/internal/casdoor/client.go:CasdoorUser`)：

```go
type CasdoorUser struct {
    Sub              string `json:"sub"`                // 用户唯一标识 (格式: "owner/name")
    Id               string `json:"id"`                 // 用户 ID
    Name             string `json:"name"`               // 用户名
    PreferredUsername string `json:"preferred_username"` // 显示名称
    Email            string `json:"email"`              // 邮箱
    Picture          string `json:"picture"`            // 头像
    Owner            string `json:"owner"`              // 所属组织
}
```

## 3. 用户数据表设计

### 3.1 表结构定义

```sql
CREATE TABLE users (
    -- 主键（使用 JWT Token 中的 id 或 sub 字段，UUID 格式）
    id VARCHAR(191) NOT NULL PRIMARY KEY COMMENT '用户唯一标识 (JWT id/sub, UUID)',

    -- 基本信息
    username VARCHAR(191) NOT NULL COMMENT '用户名 (Casdoor name)',
    display_name VARCHAR(191) COMMENT '显示名称 (Casdoor preferred_username)',
    email VARCHAR(191) COMMENT '邮箱',
    avatar_url TEXT COMMENT '头像 URL',

    -- Casdoor 相关字段
    casdoor_id VARCHAR(191) COMMENT 'Casdoor 用户 ID (UUID)',
    casdoor_universal_id VARCHAR(191) COMMENT 'Casdoor 通用唯一 ID (UUID)',
    casdoor_sub VARCHAR(191) COMMENT 'Casdoor OIDC sub (可能为 owner/name 格式)',
    organization VARCHAR(191) COMMENT '所属组织 (Casdoor owner)',

    -- 状态字段
    is_active BOOLEAN NOT NULL DEFAULT TRUE COMMENT '是否激活',
    last_login_at TIMESTAMP COMMENT '最后登录时间',
    last_sync_at TIMESTAMP COMMENT '最后同步时间',

    -- 审计字段
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- 索引
    INDEX idx_username (username),
    INDEX idx_email (email),
    INDEX idx_casdoor_id (casdoor_id),
    INDEX idx_casdoor_universal_id (casdoor_universal_id),
    INDEX idx_casdoor_sub (casdoor_sub),
    INDEX idx_organization (organization)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='用户表';
```

### 3.2 GORM 模型定义

```go
package models

import (
    "time"
    "gorm.io/gorm"
)

type User struct {
    ID                   string    `gorm:"primaryKey;size:191" json:"id"`                   // JWT id/sub (UUID, 主键)
    Username             string    `gorm:"uniqueIndex;not null;size:191" json:"username"`   // Casdoor name
    DisplayName          *string   `gorm:"size:191" json:"display_name"`                    // Casdoor preferred_username
    Email                *string   `gorm:"index;size:191" json:"email"`                     // Email
    AvatarURL            *string   `gorm:"type:text" json:"avatar_url"`                     // 头像 URL

    // Casdoor 相关字段
    CasdoorID            *string   `gorm:"index;size:191" json:"casdoor_id"`                // JWT id (UUID)
    CasdoorUniversalID   *string   `gorm:"index;size:191" json:"casdoor_universal_id"`      // JWT universal_id (UUID)
    CasdoorSub           *string   `gorm:"index;size:191" json:"casdoor_sub"`               // JWT sub (可能是 owner/name 格式)
    Organization         *string   `gorm:"index;size:191" json:"organization"`              // Casdoor owner

    // 状态字段
    IsActive             bool      `gorm:"not null;default:true" json:"is_active"`          // 是否激活
    LastLoginAt          *time.Time `json:"last_login_at"`                                  // 最后登录时间
    LastSyncAt           *time.Time `json:"last_sync_at"`                                   // 最后同步时间

    // 审计字段
    CreatedAt            time.Time `json:"created_at"`
    UpdatedAt            time.Time `json:"updated_at"`
    DeletedAt            gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName 指定表名
func (User) TableName() string {
    return "users"
}
```

### 3.3 字段映射关系

| 业务字段 | JWT 字段 | Casdoor API 字段 | 说明 |
|---------|----------|-----------------|------|
| `id` (PK) | `id` / `sub` | 合成 `owner/name` | 主键，UUID 格式（优先使用 JWT id） |
| `username` | `name` | `name` | 用户名 |
| `display_name` | `preferred_username` | `displayName` | 显示名称 |
| `email` | `email` | `email` | 邮箱 |
| `avatar_url` | `picture` | `avatar` | 头像 URL |
| `casdoor_id` | `id` | `id` | JWT id 字段（UUID） |
| `casdoor_universal_id` | `universal_id` | - | JWT universal_id 字段（UUID） |
| `casdoor_sub` | `sub` | 合成 `owner/name` | JWT sub 字段（可能是 owner/name 格式） |
| `organization` | `owner` | `owner` | 所属组织 |

**重要说明**：
- **主键 `id`**：使用 JWT Token 中的 `id` 或 `sub` 字段（UUID 格式）
- **`casdoor_sub`**：单独存储 JWT `sub` 字段，因为它可能是 `{organization}/{username}` 格式（Casdoor Admin API 合成）
- **兼容性**：同时存储 `id`、`sub`、`universal_id`，确保与现有数据和 Casdoor API 的兼容性

## 4. 实现方案

### 4.1 GetOrCreate 操作实现

**重要**：`GetOrCreate` 应该只在**登录回调**时调用，而不是在每个 API 请求的认证中间件中调用。

#### 4.1.1 登录回调时创建用户

```go
package handlers

import (
    "time"
    "your-project/internal/models"
    "your-project/internal/services"
    "github.com/gin-gonic/gin"
)

// AuthCallback 处理 Casdoor OAuth 回调
func (h *Handler) AuthCallback(c *gin.Context) {
    code := c.Query("code")
    state := c.Query("state")

    // 1. 交换 access_token
    token, err := h.CasdoorClient.ExchangeToken(code)
    if err != nil {
        c.JSON(400, gin.H{"error": err.Error()})
        return
    }

    // 2. 解析 JWT 获取用户信息
    claims, err := parseJWTToken(token.AccessToken)
    if err != nil {
        c.JSON(400, gin.H{"error": "Invalid token"})
        return
    }

    // 3. GetOrCreate 用户（只在登录时执行一次）
    user, err := h.UserService.GetOrCreateUser(claims)
    if err != nil {
        logger.Errorf("Failed to get or create user: %v", err)
        // 不阻止登录，降级处理
    }

    // 4. 设置 cookie
    c.SetCookie(
        "auth_token",
        token.AccessToken,
        7*24*3600, // 7 天
        "/",
        "",
        false,
        true,
    )

    // 5. 重定向到前端
    c.Redirect(302, h.getRedirectURL(state))
}
```

```go
package services

import (
    "time"
    "your-project/internal/models"
    "gorm.io/gorm"
)

type UserService struct {
    db *gorm.DB
}

// GetOrCreateUser 根据 JWT Claims 获取或创建用户
// 注意：此方法应该在登录回调时调用，而不是在每次 API 请求时调用
func (s *UserService) GetOrCreateUser(claims *JWTClaims) (*models.User, error) {
    // 1. 确定用户唯一标识（优先使用 id，其次 sub，最后 universal_id）
    userID := claims.ID
    if userID == "" {
        userID = claims.Sub
    }
    if userID == "" {
        userID = claims.UniversalID
    }

    if userID == "" {
        return nil, fmt.Errorf("no valid user identifier in JWT claims")
    }

    // 2. 尝试从数据库获取用户
    var user models.User
    err := s.db.Where("id = ?", userID).First(&user).Error

    now := time.Now()

    if err == nil {
        // 用户已存在，更新最后登录时间和部分可能变化的字段
        user.LastLoginAt = &now
        user.LastSyncAt = &now
        user.IsActive = true

        // 只更新可能变化的字段
        if claims.PreferredUsername != "" {
            user.DisplayName = &claims.PreferredUsername
        }
        if claims.Email != "" {
            user.Email = &claims.Email
        }
        if claims.Picture != "" {
            user.AvatarURL = &claims.Picture
        }

        if err := s.db.Save(&user).Error; err != nil {
            return nil, err
        }

        return &user, nil
    }

    if err != gorm.ErrRecordNotFound {
        return nil, err
    }

    // 3. 用户不存在，创建新用户
    user = models.User{
        ID:                   userID,
        Username:             claims.Name,
        DisplayName:          &claims.PreferredUsername,
        Email:                &claims.Email,
        AvatarURL:            &claims.Picture,
        CasdoorID:            &claims.ID,
        CasdoorUniversalID:   &claims.UniversalID,
        CasdoorSub:           &claims.Sub,
        Organization:         &claims.Owner,
        IsActive:             true,
        LastLoginAt:          &now,
        LastSyncAt:           &now,
    }

    if err := s.db.Create(&user).Error; err != nil {
        return nil, err
    }

    return &user, nil
}
```

#### 4.1.2 认证中间件（不查询数据库）

```go
package middleware

import (
    "github.com/gin-gonic/gin"
)

// OptionalAuth 可选认证中间件（不查询数据库）
func OptionalAuth(jwks *JWKSProvider) gin.HandlerFunc {
    return func(c *gin.Context) {
        token := ExtractToken(c)
        if token == "" {
            c.Next()
            return
        }

        userInfo, err := parseJWTToken(token, jwks)
        if err != nil {
            // Fallback to Casdoor API verification
            userInfo, err = fetchUserInfo(casdoorEndpoint, token)
            if err != nil {
                logger.Warn("[OptionalAuth] token validation failed: %v", err)
                c.Next()
                return
            }
        }

        // 只设置从 JWT 解析出的基本信息，不查询数据库
        c.Set(UserIDKey, userInfo.Sub)
        c.Set(UserNameKey, userInfo.PreferredUsername)
        c.Set("accessToken", token)
        c.Next()
    }
}
```

#### 4.1.3 按需查询用户信息

```go
package services

import (
    "context"
    "time"
    "github.com/patrickmn/go-cache"
    "gorm.io/gorm"
)

type CachedUserService struct {
    db    *gorm.DB
    cache *cache.Cache // 内存缓存：10分钟过期
}

// GetUserByID 获取用户信息（按需查询，带缓存）
func (s *CachedUserService) GetUserByID(ctx context.Context, userID string) (*models.User, error) {
    // 1. 尝试从内存缓存获取
    if cached, found := s.cache.Get(userID); found {
        return cached.(*models.User), nil
    }

    // 2. 从数据库获取
    var user models.User
    err := s.db.WithContext(ctx).Where("id = ?", userID).First(&user).Error
    if err != nil {
        return nil, err
    }

    // 3. 存入缓存（10分钟）
    s.cache.Set(userID, &user, 10*time.Minute)

    return &user, nil
}

// GetUsersByIDs 批量获取用户（带缓存）
func (s *CachedUserService) GetUsersByIDs(ctx context.Context, userIDs []string) (map[string]*models.User, error) {
    result := make(map[string]*models.User)
    missing := make([]string, 0, len(userIDs))

    // 1. 从缓存中批量获取
    for _, userID := range userIDs {
        if cached, found := s.cache.Get(userID); found {
            result[userID] = cached.(*models.User)
        } else {
            missing = append(missing, userID)
        }
    }

    // 2. 批量查询数据库
    if len(missing) > 0 {
        var users []*models.User
        err := s.db.WithContext(ctx).Where("id IN ?", missing).Find(&users).Error
        if err != nil {
            return nil, err
        }

        // 3. 存入缓存并返回
        for _, user := range users {
            result[user.ID] = user
            s.cache.Set(user.ID, user, 10*time.Minute)
        }
    }

    return result, nil
}
```

#### 4.1.4 业务代码按需使用

```go
package handlers

// GetDevice 获取设备详情（需要用户详细信息时才查询）
func (h *Handler) GetDevice(c *gin.Context) {
    deviceID := c.Param("id")

    // 1. 获取设备（只需要 user_id）
    var device models.Device
    if err := h.db.Where("id = ?", deviceID).First(&device).Error; err != nil {
        c.JSON(404, gin.H{"error": "Device not found"})
        return
    }

    // 2. 按需获取用户详细信息（带缓存）
    user, err := h.UserService.GetUserByID(c.Request.Context(), device.UserID)
    if err != nil {
        logger.Warn("Failed to get user: %v", err)
        // 降级：使用 JWT 中的基本信息
        c.JSON(200, gin.H{
            "device": device,
            "user": gin.H{
                "id":   device.UserID,
                "name": c.GetString("userName"), // 从 JWT 获取
            },
        })
        return
    }

    c.JSON(200, gin.H{
        "device": device,
        "user":   user,
    })
}

// ListDevices 列出设备（批量查询优化）
func (h *Handler) ListDevices(c *gin.Context) {
    var devices []*models.Device
    if err := h.db.Find(&devices).Error; err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }

    // 1. 提取所有 user_id
    userIDs := make([]string, 0, len(devices))
    userIDSet := make(map[string]bool)
    for _, device := range devices {
        if device.UserID != "" && !userIDSet[device.UserID] {
            userIDs = append(userIDs, device.UserID)
            userIDSet[device.UserID] = true
        }
    }

    // 2. 批量获取用户信息（带缓存）
    userMap, err := h.UserService.GetUsersByIDs(c.Request.Context(), userIDs)
    if err != nil {
        logger.Warn("Failed to batch get users: %v", err)
    }

    // 3. 组装返回
    result := make([]gin.H, len(devices))
    for i, device := range devices {
        result[i] = gin.H{
            "device": device,
            "user":   userMap[device.UserID], // 可能为 nil
        }
    }

    c.JSON(200, result)
}
```

### 4.2 认证中间件集成

**重要**：认证中间件只解析 JWT Token，不查询数据库。用户详细信息按需查询。

```go
package middleware

import (
    "your-project/internal/services"
    "github.com/gin-gonic/gin"
)

// OptionalAuth 可选认证中间件（不查询数据库，只解析 JWT）
func OptionalAuth(jwks *JWKSProvider) gin.HandlerFunc {
    return func(c *gin.Context) {
        token := ExtractToken(c)
        if token == "" {
            c.Next()
            return
        }

        userInfo, err := parseJWTToken(token, jwks)
        if err != nil {
            // Fallback to Casdoor API verification
            userInfo, err = fetchUserInfo(casdoorEndpoint, token)
            if err != nil {
                logger.Warn("[OptionalAuth] token validation failed: %v", err)
                c.Next()
                return
            }
        }

        // 只设置从 JWT 解析出的基本信息，不查询数据库
        c.Set(UserIDKey, userInfo.Sub)
        c.Set(UserNameKey, userInfo.PreferredUsername)
        c.Set("accessToken", token)
        c.Next()
    }
}

// RequireAuth 必须认证中间件（不查询数据库，只解析 JWT）
func RequireAuth(jwks *JWKSProvider) gin.HandlerFunc {
    return func(c *gin.Context) {
        token := ExtractToken(c)
        if token == "" {
            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
            return
        }

        userInfo, err := parseJWTToken(token, jwks)
        if err != nil {
            // Fallback to Casdoor API verification
            userInfo, err = fetchUserInfo(casdoorEndpoint, token)
            if err != nil {
                logger.Warn("[RequireAuth] token validation failed: %v", err)
                c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
                return
            }
        }

        // 只设置从 JWT 解析出的基本信息，不查询数据库
        c.Set(UserIDKey, userInfo.Sub)
        c.Set(UserNameKey, userInfo.PreferredUsername)
        c.Set("accessToken", token)
        c.Next()
    }
}
```

### 4.3 用户查询服务

```go
package services

import (
    "your-project/internal/models"
    "gorm.io/gorm"
)

// GetUserByID 根据 ID 获取用户
func (s *UserService) GetUserByID(userID string) (*models.User, error) {
    var user models.User
    err := s.db.Where("id = ?", userID).First(&user).Error
    return &user, err
}

// GetUsersByIDs 批量获取用户
func (s *UserService) GetUsersByIDs(userIDs []string) (map[string]*models.User, error) {
    var users []*models.User
    err := s.db.Where("id IN ?", userIDs).Find(&users).Error
    if err != nil {
        return nil, err
    }

    userMap := make(map[string]*models.User, len(users))
    for _, user := range users {
        userMap[user.ID] = user
    }
    return userMap, nil
}

// SearchUsers 搜索用户
func (s *UserService) SearchUsers(keyword string, limit int) ([]*models.User, error) {
    var users []*models.User
    query := s.db.Where("is_active = ?", true)

    if keyword != "" {
        pattern := "%" + keyword + "%"
        query = query.Where(
            "username LIKE ? OR display_name LIKE ? OR email LIKE ?",
            pattern, pattern, pattern,
        )
    }

    if limit > 0 {
        query = query.Limit(limit)
    }

    err := query.Find(&users).Error
    return users, err
}
```

## 5. 迁移策略

### 5.1 数据迁移步骤

1. **创建用户表**
   ```bash
   # 创建迁移文件
   server/internal/database/migrations/20250101_create_users_table.go
   ```

2. **迁移现有数据**
   ```sql
   -- 从现有表迁移用户引用
   INSERT INTO users (id, username, created_at, updated_at)
   SELECT DISTINCT user_id, 
                  SUBSTRING_INDEX(user_id, '/', -1) as username,
                  NOW(), NOW()
   FROM devices
   WHERE user_id IS NOT NULL
   UNION
   SELECT DISTINCT owner_id,
                  SUBSTRING_INDEX(owner_id, '/', -1) as username,
                  NOW(), NOW()
   FROM repositories
   WHERE owner_id IS NOT NULL;
   ```

3. **代码迁移**
   - 逐步替换 Casdoor API 调用为本地数据库查询
   - 保留 Casdoor 作为用户认证和管理的源头
   - 本地数据库作为查询缓存和扩展字段存储

### 5.2 兼容性保证

1. **向后兼容**：
   - 保持 `sub` 字段作为主键，与现有表结构一致
   - 不改变现有 API 接口
   - 保留 Casdoor API 调用作为降级方案

2. **渐进式迁移**：
   - 第一步：实现 GetOrCreate 逻辑
   - 第二步：替换读操作（优先查本地，未命中查 Casdoor）
   - 第三步：添加业务特定字段
   - 第四步：实现用户信息同步机制

## 7. 性能优化策略

### 7.1 查询策略分层

**三层查询架构**：

1. **认证层（中间件）**：只解析 JWT Token，不查询数据库
2. **缓存层（UserService）**：内存缓存（10分钟 TTL）
3. **数据库层（PostgreSQL）**：持久化存储

**性能对比**：

| 场景 | 原方案（每次查 Casdoor） | 优化方案（本地缓存） |
|------|------------------------|-------------------|
| 用户认证 | JWT 解析（0.1ms） | JWT 解析（0.1ms） |
| 获取用户信息 | Casdoor API（50-200ms） | 内存缓存（<0.1ms） |
| 批量获取用户 | N 次 API 调用 | 1 次数据库查询 + 缓存 |
| 数据一致性 | 实时 | 最终一致（10分钟） |

### 7.2 缓存策略

```go
package services

import (
    "context"
    "time"
    "github.com/patrickmn/go-cache"
    "gorm.io/gorm"
)

type CachedUserService struct {
    db    *gorm.DB
    cache *cache.Cache // 内存缓存：10分钟过期，30分钟清理
}

func NewCachedUserService(db *gorm.DB) *CachedUserService {
    return &CachedUserService{
        db:    db,
        cache: cache.New(10*time.Minute, 30*time.Minute),
    }
}

// GetUserByID 获取用户信息（带缓存）
func (s *CachedUserService) GetUserByID(ctx context.Context, userID string) (*models.User, error) {
    // 1. 尝试从内存缓存获取
    if cached, found := s.cache.Get(userID); found {
        return cached.(*models.User), nil
    }

    // 2. 从数据库获取
    var user models.User
    err := s.db.WithContext(ctx).Where("id = ?", userID).First(&user).Error
    if err != nil {
        return nil, err
    }

    // 3. 存入缓存（10分钟）
    s.cache.Set(userID, &user, cache.DefaultExpiration)

    return &user, nil
}

// InvalidateCache 使缓存失效
func (s *CachedUserService) InvalidateCache(userID string) {
    s.cache.Delete(userID)
}

// WarmupCache 预热缓存（用于启动时或定期同步）
func (s *CachedUserService) WarmupCache(ctx context.Context) error {
    var users []*models.User
    err := s.db.WithContext(ctx).
        Where("is_active = ?", true).
        Find(&users).Error
    if err != nil {
        return err
    }

    for _, user := range users {
        s.cache.Set(user.ID, user, cache.DefaultExpiration)
    }

    return nil
}
```

### 6.2 批量查询优化

```go
// BatchGetUsersWithMissing 批量获取用户，返回未找到的用户 ID
func (s *UserService) BatchGetUsersWithMissing(userIDs []string) (map[string]*models.User, []string, error) {
    var users []*models.User
    err := s.db.Where("id IN ?", userIDs).Find(&users).Error
    if err != nil {
        return nil, nil, err
    }

    userMap := make(map[string]*models.User)
    missing := make([]string, 0)

    for _, userID := range userIDs {
        found := false
        for _, user := range users {
            if user.ID == userID {
                userMap[userID] = user
                found = true
                break
            }
        }
        if !found {
            missing = append(missing, userID)
        }
    }

    return userMap, missing, nil
}
```

## 7. 数据一致性保障

### 7.1 同步机制

1. **登录时同步**：每次登录时更新用户信息
2. **定期同步**：后台任务定期同步活跃用户
3. **事件驱动**：监听 Casdoor Webhook（如果支持）

### 7.2 降级策略

```go
// GetUserWithFallback 优先从本地获取，失败时降级到 Casdoor
func (s *UserService) GetUserWithFallback(ctx context.Context, userID string) (*models.User, error) {
    // 1. 尝试从本地数据库获取
    user, err := s.GetUserByID(userID)
    if err == nil {
        return user, nil
    }

    if err != gorm.ErrRecordNotFound {
        return nil, err
    }

    // 2. 本地未找到，从 Casdoor 获取
    casdoorUser, err := s.casdoorClient.GetUserByID(userID)
    if err != nil {
        return nil, err
    }

    // 3. 创建本地用户记录
    user = &models.User{
        ID:                   casdoorUser.Sub,
        Username:             casdoorUser.Name,
        DisplayName:          &casdoorUser.PreferredUsername,
        Email:                &casdoorUser.Email,
        AvatarURL:            &casdoorUser.Picture,
        CasdoorID:            &casdoorUser.Id,
        Organization:         &casdoorUser.Owner,
        IsActive:             true,
    }

    if err := s.db.Create(user).Error; err != nil {
        return nil, err
    }

    return user, nil
}
```

## 8. 后续扩展

### 8.1 业务特定字段

```go
type User struct {
    // ... 现有字段 ...

    // 业务扩展字段
    Phone              *string `gorm:"size:191" json:"phone"`
    Bio                *string `gorm:"type:text" json:"bio"`
    Location           *string `gorm:"size:191" json:"location"`
    Website            *string `gorm:"size:191" json:"website"`
    Preferences        datatypes.JSON `json:"preferences"` // 用户偏好设置
    NotificationSettings datatypes.JSON `json:"notification_settings"` // 通知设置
}
```

### 8.2 用户关系表

```sql
-- 用户关注关系
CREATE TABLE user_follows (
    id VARCHAR(191) NOT NULL PRIMARY KEY,
    follower_id VARCHAR(191) NOT NULL COMMENT '关注者 ID',
    following_id VARCHAR(191) NOT NULL COMMENT '被关注者 ID',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_follow (follower_id, following_id),
    INDEX idx_follower (follower_id),
    INDEX idx_following (following_id),
    FOREIGN KEY (follower_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (following_id) REFERENCES users(id) ON DELETE CASCADE
) COMMENT='用户关注关系';
```

### 8.3 用户活动日志

```sql
CREATE TABLE user_activities (
    id VARCHAR(191) NOT NULL PRIMARY KEY,
    user_id VARCHAR(191) NOT NULL COMMENT '用户 ID',
    activity_type VARCHAR(191) NOT NULL COMMENT '活动类型',
    metadata JSON COMMENT '活动元数据',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_user (user_id),
    INDEX idx_type (activity_type),
    INDEX idx_created (created_at),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) COMMENT='用户活动日志';
```

## 9. 总结

### 9.1 核心设计要点

1. **主键选择**：使用 JWT Token 中的 `id` 或 `sub` 字段（UUID 格式）
   - 优先级：`id` > `sub` > `universal_id`
   - 与现有数据库表结构一致（存储的是 UUID）
   - 真实的用户唯一标识，而非合成值

2. **多字段存储**：同时存储 `id`、`sub`、`universal_id`
   - `id`：主键，JWT 中的主要标识（UUID）
   - `sub`：可能包含 `{organization}/{username}` 格式（Casdoor Admin API 合成）
   - `universal_id`：备用唯一标识（UUID）
   - 确保与各种数据源的兼容性

3. **JWT 字段映射**：完整映射 JWT Token 中的所有用户相关字段
4. **Casdoor 字段存储**：保存所有 Casdoor 相关字段用于后续关联
5. **GetOrCreate 机制**：登录时自动创建或更新用户记录（不在每次 API 请求时执行）

6. **性能优化**：
   - 认证中间件只解析 JWT，不查询数据库
   - 按需查询用户信息，带内存缓存
   - 批量查询优化

### 9.2 实施建议

1. **分阶段实施**：
   - **第一阶段**：创建用户表，在登录回调时实现 GetOrCreate
   - **第二阶段**：实现带缓存的 UserService，按需查询用户信息
   - **第三阶段**：逐步替换高频场景的 Casdoor API 调用
   - **第四阶段**：添加业务扩展字段和同步机制

2. **性能指标**：
   - 登录时 GetOrCreate：< 10ms（数据库查询）
   - 缓存命中：> 95%（活跃用户）
   - 用户信息查询：< 1ms（缓存命中）
   - 批量查询优化：减少 90%+ 的数据库查询

3. **回滚方案**：
   - 保留 Casdoor API 调用代码作为降级方案
   - 通过配置开关控制是否使用本地用户表
   - 确保可以快速切换回纯 Casdoor 模式

### 9.3 性能对比

| 指标 | 当前方案（Casdoor API） | 优化方案（本地缓存） | 提升 |
|------|----------------------|-------------------|------|
| 用户认证 | JWT 解析（0.1ms） | JWT 解析（0.1ms） | - |
| 获取用户信息 | API 调用（50-200ms） | 内存缓存（<0.1ms） | **500-2000x** |
| 批量获取（100用户） | 100 次 API（5-20s） | 1 次 DB 查询（10-50ms） | **100-400x** |
| 数据库负载 | 无 | 轻量（有缓存） | - |
| Casdoor 依赖 | 强依赖 | 弱依赖 | **可用性提升** |

### 9.4 预期收益

1. **性能提升**：
   - 用户信息查询从 50-200ms 降低到 <1ms（缓存命中）
   - 批量查询性能提升 100-400 倍
   - 减少 90%+ 的 Casdoor API 调用

2. **查询能力**：
   - 支持复杂的用户相关查询（如按组织、活跃度筛选）
   - 支持用户统计和分析
   - 支持全文搜索和模糊匹配

3. **扩展性**：
   - 便于添加业务特定字段（如偏好设置、通知配置）
   - 支持用户关系和社交功能
   - 支持用户行为分析和个性化推荐

4. **可用性**：
   - Casdoor 故障时不影响已登录用户的查询
   - 减少外部依赖，提升系统稳定性
   - 降低网络延迟对用户体验的影响

5. **数据一致性**：
   - 统一的数据源和查询接口
   - 支持事务保证数据完整性
   - 便于数据备份和恢复