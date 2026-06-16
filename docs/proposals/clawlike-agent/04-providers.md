# 4. Provider 管理

## 4.1 设计目标

costrict-web 当前 LLM 配置是**全局单 Provider**（`config.LLMConfig`，硬编码 ZhipuAI GLM）。本方案需要支持：

- 每个用户**自配**多个 LLM Provider（OpenAI / DeepSeek / Ollama / Anthropic 等）
- Agent 根据用户配置**动态选择** Model
- API Key **加密存储**，运行时解密
- 平台预置**默认 Provider**（降级方案）

## 4.2 数据模型

详见 [10-database.md](./10-database.md#agent_providers-表)。

```go
// server/internal/clawagent/provider_models.go

type Provider struct {
    ID              uint      `gorm:"primaryKey"`
    UserID          string    `gorm:"size:255;not null;index"`
    Name            string    `gorm:"size:255;not null"`          // 用户自定义名称
    ProviderType    string    `gorm:"size:50;not null"`           // openai/deepseek/ollama/anthropic
    APIKeyEncrypted string    `gorm:"type:text"`                  // AES-256-GCM 加密
    BaseURL         string    `gorm:"type:text"`                  // API base URL
    ModelName       string    `gorm:"size:255;not null"`          // 默认模型名
    Models          string    `gorm:"type:text"`                  // JSON: 可用模型列表
    IsDefault       bool      `gorm:"default:false"`
    CreatedAt       time.Time `gorm:"autoCreateTime"`
    UpdatedAt       time.Time `gorm:"autoUpdateTime"`
}
```

## 4.3 Provider 类型映射

| provider_type | trpc-agent-go 模块 | 说明 |
|---------------|-------------------|------|
| `openai` | `model/openai` (Variant: openai) | OpenAI 官方及兼容 API |
| `deepseek` | `model/openai` (Variant: deepseek) | DeepSeek |
| `groq` | `model/openai` (Variant: groq) | Groq |
| `anthropic` | `model/anthropic` | Claude（独立模块） |
| `gemini` | `model/gemini` | Google Gemini（独立模块） |
| `ollama` | `model/ollama` | 本地 Ollama |
| `hunyuan` | `model/hunyuan` | 腾讯混元 |

## 4.4 Model 创建

```go
// server/internal/clawagent/providers.go

func (m *ProviderManager) CreateModel(prov *Provider) (model.Model, error) {
    apiKey := decrypt(prov.APIKeyEncrypted)

    switch prov.ProviderType {
    case "openai":
        return openaimodel.New(prov.ModelName,
            openaimodel.WithAPIKey(apiKey),
            openaimodel.WithBaseURL(prov.BaseURL),
            openaimodel.WithVariant(openaimodel.VariantOpenAI),
        ), nil

    case "deepseek":
        return openaimodel.New(prov.ModelName,
            openaimodel.WithAPIKey(apiKey),
            openaimodel.WithBaseURL(prov.BaseURL),
            openaimodel.WithVariant(openaimodel.VariantDeepSeek),
        ), nil

    case "ollama":
        return ollamamodel.New(prov.ModelName,
            ollamamodel.WithBaseURL(prov.BaseURL),
        ), nil

    case "anthropic":
        return anthropicmodel.New(prov.ModelName,
            anthropicmodel.WithAPIKey(apiKey),
        ), nil

    default:
        return nil, fmt.Errorf("unsupported provider type: %s", prov.ProviderType)
    }
}
```

## 4.5 Per-User 动态加载

使用 trpc-agent-go 的 **AgentFactory** 模式实现 per-user 动态 Provider。Memory 不走 trpc-agent-go 自带后端，而是自建 `agent_memories` 表（详见 [03-soul-and-memory.md §3.2](./03-soul-and-memory.md)），由 Handler 在 final response 后异步刷新：

```go
// 在 Runner 构建时注册 AgentFactory
runtimeRunner := runner.NewRunner("clawagent", defaultAgent,
    runner.WithSessionService(sessionSvc),
    runner.WithAgentFactory("user-agent", func(ctx context.Context, ro agent.RunOptions) (agent.Agent, error) {
        // 从 RunOptions 获取 userID
        userID := ro.UserID

        // 加载该用户的 Providers
        providers, err := providerMgr.LoadByUser(ctx, userID)
        if err != nil || len(providers) == 0 {
            // 降级到平台默认 Provider
            return defaultAgent, nil
        }

        // 构建 models map
        models := make(map[string]model.Model)
        for _, prov := range providers {
            m, err := providerMgr.CreateModel(prov)
            if err != nil {
                continue
            }
            models[prov.Name] = m
        }

        // 加载 Persona 和 Memory（Memory 是单 TEXT 字段，全量拼到 system prompt）
        persona, _ := personaMgr.Load(ctx, userID)
        memoryContent, _ := memoryMgr.Load(ctx, userID)

        // 构建 per-user Agent
        return llmagent.New("user-agent-"+userID,
            llmagent.WithModels(models),
            llmagent.WithInstruction(personaMgr.BuildInstruction(persona, memoryContent)),
            llmagent.WithTools(tools),  // 仅 memory_view/memory_update（备用）+ workspace_*
            // 不注入 WithSkillsRepository（Skill 暂时禁用）
            // 不调用 WithPreloadMemory（Memory 已拼到 instruction）
        ), nil
    }),
)
```

## 4.6 API Key 加密

使用 AES-256-GCM 加密 API Key：

```go
// server/internal/clawagent/crypto.go

func encryptAPIKey(plaintext, encryptionKey string) (string, error) {
    key := sha256.Sum256([]byte(encryptionKey))
    block, _ := aes.NewCipher(key[:])
    gcm, _ := cipher.NewGCM(block)
    nonce := make([]byte, gcm.NonceSize())
    rand.Read(nonce)
    ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
    return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptAPIKey(encText, encryptionKey string) (string, error) {
    // ... 解密逻辑
}
```

加密密钥从环境变量 `CLAWAGENT_ENCRYPTION_KEY` 读取。

## 4.7 默认 Provider 降级

平台预置一个默认 Provider（从 `config.LLMConfig` 读取），当用户未配置任何 Provider 时使用。

**关键修复**：默认 Provider 也走完整加密链路，避免明文 API Key 进入运行时数据结构。`platformDefault()` 在构造 Provider 对象时**就地加密** cfg 明文，与持久化 Provider 走同一条 `decryptAPIKey()` 解密路径。

```go
func (m *ProviderManager) LoadByUser(ctx context.Context, userID string) ([]*Provider, error) {
    var providers []*Provider
    err := m.db.Where("user_id = ?", userID).Find(&providers).Error
    if err == nil && len(providers) == 0 {
        // 返回平台默认（已加密）
        def, derr := m.platformDefault()
        if derr != nil {
            return nil, derr
        }
        providers = []*Provider{def}
    }
    return providers, err
}

// platformDefault 返回平台默认 Provider（API Key 已加密）
// 不写库，仅用于运行时构造，每次调用都从 cfg 现加密
func (m *ProviderManager) platformDefault() (*Provider, error) {
    encrypted, err := encryptAPIKey(cfg.LLM.APIKey, m.encryptionKey)
    if err != nil {
        return nil, fmt.Errorf("failed to encrypt platform default API key: %w", err)
    }
    return &Provider{
        Name:             "platform-default",
        ProviderType:     "openai",
        APIKeyEncrypted:  encrypted,
        BaseURL:          cfg.LLM.BaseURL,
        ModelName:        cfg.LLM.Model,
        IsDefault:        true,
    }, nil
}
```

### 加密密钥一致性

`platformDefault()` 与持久化 Provider 共用 `m.encryptionKey`（从 `CLAWAGENT_ENCRYPTION_KEY` 环境变量读取），保证：

- 任意 Provider（持久化或默认）在 `CreateModel()` 里都走 `decryptAPIKey(prov.APIKeyEncrypted, m.encryptionKey)` 统一解密
- cfg 明文 API Key **不**直接进入 Model 构造路径
- 旋转加密密钥时只需重写库内 Provider + 改环境变量

### 边界处理

| 场景 | 行为 |
|------|------|
| `CLAWAGENT_ENCRYPTION_KEY` 未设置 | 启动时 fail-fast（setup.go 校验） |
| `cfg.LLM.APIKey` 为空 | `platformDefault()` 返回错误，用户必须自配 Provider |
| 加密失败 | `LoadByUser` 返回错误，触发上层降级到错误提示 |

### Provider 类型映射（默认）

默认 Provider 类型按 `cfg.LLM.Provider` 推断（新增字段，默认 `openai`），以支持未来平台级切换 Zhipu/DeepSeek 等：

```go
providerType := cfg.LLM.Provider
if providerType == "" {
    providerType = "openai"
}
```
