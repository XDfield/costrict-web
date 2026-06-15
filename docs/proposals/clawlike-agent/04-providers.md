# 4. Provider 管理

## 4.1 设计目标

costrict-web 当前 LLM 配置是**全局单 Provider**（`config.LLMConfig`，硬编码 ZhipuAI GLM）。本方案需要支持：

- 每个用户**自配**多个 LLM Provider（OpenAI / DeepSeek / Ollama / Anthropic 等）
- Agent 根据用户配置**动态选择** Model
- API Key **加密存储**，运行时解密
- 平台预置**默认 Provider**（降级方案）

## 4.2 数据模型

详见 [09-database.md](./09-database.md#agent_providers-表)。

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

使用 trpc-agent-go 的 **AgentFactory** 模式实现 per-user 动态 Provider：

```go
// 在 Runner 构建时注册 AgentFactory
runtimeRunner := runner.NewRunner("clawagent", defaultAgent,
    runner.WithSessionService(sessionSvc),
    runner.WithMemoryService(memorySvc),
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

        // 加载 Persona
        persona, _ := personaMgr.Load(ctx, userID)

        // 构建 per-user Agent
        return llmagent.New("user-agent-"+userID,
            llmagent.WithModels(models),
            llmagent.WithInstruction(personaMgr.BuildInstruction(persona)),
            llmagent.WithTools(tools),
            llmagent.WithSkillsRepository(skillsRepo),
            llmagent.WithPreloadMemory(10),
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

平台预置一个默认 Provider（从 `config.LLMConfig` 读取），当用户未配置任何 Provider 时使用：

```go
func (m *ProviderManager) LoadByUser(ctx context.Context, userID string) ([]*Provider, error) {
    var providers []*Provider
    err := m.db.Where("user_id = ?", userID).Find(&providers).Error
    if err == nil && len(providers) == 0 {
        // 返回平台默认
        providers = []*Provider{m.platformDefault()}
    }
    return providers, err
}

func (m *ProviderManager) platformDefault() *Provider {
    return &Provider{
        Name:         "platform-default",
        ProviderType: "openai",
        APIKeyEncrypted: encrypt(cfg.APIKey),
        BaseURL:      cfg.BaseURL,
        ModelName:    cfg.Model,
        IsDefault:    true,
    }
}
```
