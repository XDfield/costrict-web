package clawagent

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// ProviderManager handles CRUD and model creation for LLM providers.
type ProviderManager struct {
	db            *gorm.DB
	encryptionKey string
	cfg           ClawAgentConfig
}

// NewProviderManager creates a new ProviderManager.
func NewProviderManager(db *gorm.DB, cfg ClawAgentConfig) *ProviderManager {
	return &ProviderManager{
		db:            db,
		encryptionKey: cfg.EncryptionKey,
		cfg:           cfg,
	}
}

// LoadByUser loads all providers for a user, falling back to platform default.
func (m *ProviderManager) LoadByUser(ctx context.Context, userID string) ([]*Provider, error) {
	var providers []*Provider
	err := m.db.WithContext(ctx).Where("user_id = ?", userID).Find(&providers).Error
	if err != nil {
		return nil, err
	}

	if len(providers) == 0 {
		def, derr := m.platformDefault()
		if derr != nil {
			return nil, derr
		}
		providers = []*Provider{def}
	}

	return providers, nil
}

// LoadByID loads a specific provider by ID.
func (m *ProviderManager) LoadByID(ctx context.Context, id uint) (*Provider, error) {
	var prov Provider
	if err := m.db.WithContext(ctx).First(&prov, id).Error; err != nil {
		return nil, err
	}
	return &prov, nil
}

// ListByUser lists all providers for a user.
func (m *ProviderManager) ListByUser(ctx context.Context, userID string) ([]Provider, error) {
	var providers []Provider
	if err := m.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// Create creates a new provider for a user (API key gets encrypted).
func (m *ProviderManager) Create(ctx context.Context, p *Provider) error {
	if p.APIKeyEncrypted != "" {
		encrypted, err := EncryptAPIKey(p.APIKeyEncrypted, m.encryptionKey)
		if err != nil {
			return fmt.Errorf("encrypt API key: %w", err)
		}
		p.APIKeyEncrypted = encrypted
	}

	if p.IsDefault {
		_ = m.db.WithContext(ctx).
			Model(&Provider{}).
			Where("user_id = ? AND is_default = true", p.UserID).
			Update("is_default", false).Error
	}
	return m.db.WithContext(ctx).Create(p).Error
}

// Update updates an existing provider.
func (m *ProviderManager) Update(ctx context.Context, p *Provider) error {
	if p.APIKeyEncrypted != "" {
		// Try decryption first to check if already encrypted
		if _, decErr := DecryptAPIKey(p.APIKeyEncrypted, m.encryptionKey); decErr != nil {
			// Not encrypted yet, encrypt it
			encrypted, err := EncryptAPIKey(p.APIKeyEncrypted, m.encryptionKey)
			if err != nil {
				return fmt.Errorf("encrypt API key: %w", err)
			}
			p.APIKeyEncrypted = encrypted
		}
		// If decrypt succeeded, API key was already encrypted - keep as-is
	}

	if p.IsDefault {
		_ = m.db.WithContext(ctx).
			Model(&Provider{}).
			Where("user_id = ? AND is_default = true AND id != ?", p.UserID, p.ID).
			Update("is_default", false).Error
	}
	return m.db.WithContext(ctx).Save(p).Error
}

// Delete deletes a provider.
func (m *ProviderManager) Delete(ctx context.Context, id uint) error {
	return m.db.WithContext(ctx).Delete(&Provider{}, id).Error
}

// TestProvider tests connectivity to a provider.
func (m *ProviderManager) TestProvider(ctx context.Context, id uint) (*ProviderTestResult, error) {
	prov, err := m.LoadByID(ctx, id)
	if err != nil {
		return nil, err
	}

	apiKey, err := DecryptAPIKey(prov.APIKeyEncrypted, m.encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt API key: %w", err)
	}

	client := NewLLMClient()
	baseURL := prov.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	_, err = client.Generate(ctx, ProviderConfig{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		ModelName: prov.ModelName,
	}, []ChatMessage{
		{Role: "user", Content: "test"},
	})
	if err != nil {
		return &ProviderTestResult{Success: false, Error: err.Error()}, nil
	}

	return &ProviderTestResult{Success: true, Model: prov.ModelName}, nil
}

// platformDefault creates a platform default provider from config (encrypted).
func (m *ProviderManager) platformDefault() (*Provider, error) {
	if m.cfg.DefaultAPIKey == "" {
		return nil, fmt.Errorf("no default LLM API key configured")
	}

	encrypted, err := EncryptAPIKey(m.cfg.DefaultAPIKey, m.encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt default API key: %w", err)
	}

	baseURL := m.cfg.DefaultBaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	providerType := m.cfg.DefaultProvider
	if providerType == "" {
		providerType = "openai"
	}

	return &Provider{
		Name:             "platform-default",
		ProviderType:     providerType,
		APIKeyEncrypted:  encrypted,
		BaseURL:          baseURL,
		ModelName:        m.cfg.DefaultModelName,
		IsDefault:        true,
	}, nil
}

// ProviderTestResult is the result of a provider connectivity test.
type ProviderTestResult struct {
	Success bool   `json:"success"`
	Model   string `json:"model,omitempty"`
	Error   string `json:"error,omitempty"`
}
