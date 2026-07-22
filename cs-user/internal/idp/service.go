// idp/service.go — IdP sources CRUD service.
//
// E2.2: Service for managing per-tenant identity provider sources.
// This service handles CRUD operations on the idp_sources table,
// enabling E2 tenant-level IdP integration.

package idp

import (
	"context"
	"fmt"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// Service is the idp_sources CRUD service. Constructed once in main.go.
type Service struct {
	db       *gorm.DB
	validator *Validator // optional, defaults to NewValidator()
}

// New constructs a Service.
func New(db *gorm.DB) *Service {
	return &Service{
		db:       db,
		validator: NewValidator(),
	}
}

// SetValidator sets a custom validator (for testing or relaxed validation).
func (s *Service) SetValidator(v *Validator) {
	s.validator = v
}

// CreateParams defines the parameters for creating an IdP source.
type CreateParams struct {
	TenantID  string                 // required
	Provider  string                 // required, must match provider_mapping key
	Config    map[string]interface{} // required, provider-specific config
	Scope     string                 // optional, defaults to "tenant-specific"
	Enabled   *bool                  // optional, defaults to true
	Priority  *int                   // optional, defaults to 0
	CreatedBy string                 // optional
}

// Create creates a new IdP source for a tenant.
// Returns the created IdP source on success.
func (s *Service) Create(ctx context.Context, p CreateParams) (*IdPSourceView, error) {
	if p.TenantID == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}
	if p.Provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	if p.Config == nil {
		return nil, fmt.Errorf("config is required")
	}

	// Validate config based on provider type
	if err := s.validator.ValidateConfig(p.Provider, p.Config); err != nil {
		return nil, fmt.Errorf("invalid config for provider %s: %w", p.Provider, err)
	}

	// Apply defaults
	scope := p.Scope
	if scope == "" {
		scope = "tenant-specific"
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	priority := 0
	if p.Priority != nil {
		priority = *p.Priority
	}

	model := &models.IdPSource{
		TenantID:  p.TenantID,
		Provider:  p.Provider,
		Scope:     scope,
		Enabled:   enabled,
		Priority:  priority,
		CreatedBy: p.CreatedBy,
	}
	if err := model.SetConfig(p.Config); err != nil {
		return nil, fmt.Errorf("config serialization: %w", err)
	}

	if err := s.db.WithContext(ctx).Create(model).Error; err != nil {
		return nil, err
	}

	return modelToView(model), nil
}

// Get retrieves an IdP source by tenant_id and provider.
// Returns nil if not found (no error).
func (s *Service) Get(ctx context.Context, tenantID, provider string) (*IdPSourceView, error) {
	if tenantID == "" || provider == "" {
		return nil, fmt.Errorf("tenant_id and provider are required")
	}

	var model models.IdPSource
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ? AND provider = ?", tenantID, provider).
		First(&model).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return modelToView(&model), nil
}

// List retrieves all IdP sources for a tenant.
// Returns empty slice if none found.
func (s *Service) List(ctx context.Context, tenantID string) ([]IdPSourceView, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}

	var modelList []models.IdPSource
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ?", tenantID).
		Order("priority DESC, provider ASC").
		Find(&modelList).Error
	if err != nil {
		return nil, err
	}

	views := make([]IdPSourceView, len(modelList))
	for i, m := range modelList {
		views[i] = *modelToView(&m)
	}
	return views, nil
}

// UpdateParams defines the parameters for updating an IdP source.
type UpdateParams struct {
	TenantID   string                 // required
	Provider   string                 // required
	Config     map[string]interface{} // optional, full replace
	Scope      *string                // optional
	Enabled    *bool                  // optional
	Priority   *int                   // optional
	UpdatedBy  string                 // optional
}

// Update updates an existing IdP source.
// Returns the updated view on success.
// Returns error if not found.
func (s *Service) Update(ctx context.Context, p UpdateParams) (*IdPSourceView, error) {
	if p.TenantID == "" || p.Provider == "" {
		return nil, fmt.Errorf("tenant_id and provider are required")
	}

	var model models.IdPSource
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ? AND provider = ?", p.TenantID, p.Provider).
		First(&model).Error
	if err == gorm.ErrRecordNotFound {
		return nil, fmt.Errorf("IdP source not found")
	}
	if err != nil {
		return nil, err
	}

	// Apply updates
	updates := map[string]interface{}{}
	if p.Config != nil {
		// Validate new config
		if err := s.validator.ValidateConfig(p.Provider, p.Config); err != nil {
			return nil, fmt.Errorf("invalid config for provider %s: %w", p.Provider, err)
		}
		if err := model.SetConfig(p.Config); err != nil {
			return nil, fmt.Errorf("config serialization: %w", err)
		}
		updates["config"] = model.Config
	}
	if p.Scope != nil {
		updates["scope"] = *p.Scope
	}
	if p.Enabled != nil {
		updates["enabled"] = *p.Enabled
	}
	if p.Priority != nil {
		updates["priority"] = *p.Priority
	}
	if p.UpdatedBy != "" {
		updates["updated_by"] = p.UpdatedBy
	}

	if err := s.db.WithContext(ctx).Model(&model).Updates(updates).Error; err != nil {
		return nil, err
	}

	// Reload to get updated state
	if err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ? AND provider = ?", p.TenantID, p.Provider).
		First(&model).Error; err != nil {
		return nil, err
	}

	return modelToView(&model), nil
}

// Delete removes an IdP source.
// Returns error if not found.
func (s *Service) Delete(ctx context.Context, tenantID, provider string) error {
	if tenantID == "" || provider == "" {
		return fmt.Errorf("tenant_id and provider are required")
	}

	result := s.db.WithContext(ctx).
		Where("tenant_id = ? AND provider = ?", tenantID, provider).
		Delete(&models.IdPSource{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("IdP source not found")
	}
	return nil
}

// GetTenantIdPsInternal returns all enabled IdP sources for a tenant WITH
// full config (including secrets). This is the server-to-server variant used
// by costrict-web server/ to initiate OAuth flows — it requires the caller
// to be behind the internal-token gate (registered under /api/internal/* in
// app.go). Same filtering semantics as GetTenantIdPs (provider_mapping aware).
//
// SECURITY: Never expose via a public route. Only internal routes.
func (s *Service) GetTenantIdPsInternal(ctx context.Context, tenantID string, tenantConfig TenantConfigProvider) ([]InternalIdPSourceView, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}

	var modelList []models.IdPSource
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ? AND enabled = ?", tenantID, true).
		Order("priority DESC, provider ASC").
		Find(&modelList).Error
	if err != nil {
		return nil, err
	}

	if tenantConfig == nil {
		views := make([]InternalIdPSourceView, len(modelList))
		for i, m := range modelList {
			views[i] = *modelToInternalView(&m)
		}
		return views, nil
	}

	enabledProviders, err := tenantConfig.GetEnabledProviders(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to get enabled providers: %w", err)
	}
	providerEnabled := make(map[string]bool, len(enabledProviders))
	for _, p := range enabledProviders {
		providerEnabled[p] = true
	}

	var filtered []InternalIdPSourceView
	for _, m := range modelList {
		if providerEnabled[m.Provider] {
			filtered = append(filtered, *modelToInternalView(&m))
		}
	}
	if filtered == nil {
		filtered = []InternalIdPSourceView{}
	}
	return filtered, nil
}

// GetInternal returns a single IdP source with full config (secrets included).
// Internal-only — for server-to-server OAuth initiation. Returns nil if not found.
func (s *Service) GetInternal(ctx context.Context, tenantID, provider string) (*InternalIdPSourceView, error) {
	if tenantID == "" || provider == "" {
		return nil, fmt.Errorf("tenant_id and provider are required")
	}

	var model models.IdPSource
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ? AND provider = ?", tenantID, provider).
		First(&model).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return modelToInternalView(&model), nil
}

// GetTenantIdPs returns all enabled IdP sources for a tenant, filtered by
// provider_mapping and sorted by priority (highest first).
//
// This is the E2.3 implementation that queries idp_sources table and
// cross-references with tenantconfig.Service.GetEnabledProviders to ensure
// only providers defined in provider_mapping are returned.
//
// Parameters:
//   - ctx: context
//   - tenantID: tenant identifier
//   - tenantConfig: optional tenantconfig service for filtering; if nil,
//     returns all enabled IdP sources without provider_mapping filtering
//
// Returns empty slice if no enabled IdP sources found.
func (s *Service) GetTenantIdPs(ctx context.Context, tenantID string, tenantConfig TenantConfigProvider) ([]IdPSourceView, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}

	// Query enabled IdP sources from database
	var modelList []models.IdPSource
	err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "provider", "config", "scope", "enabled", "priority", "created_by", "updated_by").
		Where("tenant_id = ? AND enabled = ?", tenantID, true).
		Order("priority DESC, provider ASC").
		Find(&modelList).Error
	if err != nil {
		return nil, err
	}

	// If no tenant config provider, return all enabled sources
	if tenantConfig == nil {
		views := make([]IdPSourceView, len(modelList))
		for i, m := range modelList {
			views[i] = *modelToView(&m)
		}
		return views, nil
	}

	// Get enabled providers from provider_mapping
	enabledProviders, err := tenantConfig.GetEnabledProviders(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to get enabled providers: %w", err)
	}

	// Build a lookup map for O(1) filtering
	providerEnabled := make(map[string]bool)
	for _, p := range enabledProviders {
		providerEnabled[p] = true
	}

	// Filter to only include providers that are in provider_mapping
	var filtered []IdPSourceView
	for _, m := range modelList {
		if providerEnabled[m.Provider] {
			filtered = append(filtered, *modelToView(&m))
		}
	}

	return filtered, nil
}

// TenantConfigProvider is the interface for provider mapping lookup.
// Implemented by tenantconfig.Service (or CachedService).
type TenantConfigProvider interface {
	GetEnabledProviders(ctx context.Context, tenantID string) ([]string, error)
}

// IdPSourceView is the API-facing view of an IdP source.
type IdPSourceView struct {
	TenantID  string                 `json:"tenant_id"`
	Provider  string                 `json:"provider"`
	Config    map[string]interface{} `json:"config"`
	Scope     string                 `json:"scope"`
	Enabled   bool                   `json:"enabled"`
	Priority  int                    `json:"priority"`
	CreatedAt string                 `json:"created_at,omitempty"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
	CreatedBy string                 `json:"created_by,omitempty"`
	UpdatedBy string                 `json:"updated_by,omitempty"`
}

// InternalIdPSourceView is the server-to-server view of an IdP source.
// Unlike IdPSourceView, Config contains the raw values including secrets
// (client_secret, bind_password, etc.). Exposed only behind /api/internal/*
// routes guarded by RequireInternalToken — never return this via a public
// route or embed it in a response that crosses the trust boundary.
type InternalIdPSourceView struct {
	TenantID  string                 `json:"tenant_id"`
	Provider  string                 `json:"provider"`
	Config    map[string]interface{} `json:"config"` // RAW — includes secrets
	Scope     string                 `json:"scope"`
	Enabled   bool                   `json:"enabled"`
	Priority  int                    `json:"priority"`
	CreatedAt string                 `json:"created_at,omitempty"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
	CreatedBy string                 `json:"created_by,omitempty"`
	UpdatedBy string                 `json:"updated_by,omitempty"`
}

// sensitiveFields are field names that should be redacted in API responses.
var sensitiveFields = map[string]bool{
	"client_secret":         true, // OAuth client secret
	"client_secret_expiry":  true, // OAuth client secret expiration
	"bind_password":         true, // LDAP bind password
	"api_key":               true, // Generic API key
	"secret":                true, // Generic secret
	"password":              true, // Generic password
	"token":                 true, // Generic token
	"access_token":          true, // OAuth access token
	"refresh_token":         true, // OAuth refresh token
	"id_token":              true, // OIDC ID token
	"private_key":           true, // Private key for JWT signing
	"webhook_secret":        true, // Webhook verification secret
}

func modelToView(m *models.IdPSource) *IdPSourceView {
	cfg, _ := m.GetConfig() // ignore error, return empty map on failure

	// Redact sensitive fields
	redacted := make(map[string]interface{})
	for key, val := range cfg {
		if sensitiveFields[key] {
			redacted[key] = "******"
		} else {
			redacted[key] = val
		}
	}

	view := &IdPSourceView{
		TenantID: m.TenantID,
		Provider: m.Provider,
		Config:   redacted,
		Scope:    m.Scope,
		Enabled:  m.Enabled,
		Priority: m.Priority,
		CreatedBy: m.CreatedBy,
		UpdatedBy: m.UpdatedBy,
	}
	if !m.CreatedAt.IsZero() {
		view.CreatedAt = m.CreatedAt.Format("2006-01-02T15:04:05.000Z07:00")
	}
	if !m.UpdatedAt.IsZero() {
		view.UpdatedAt = m.UpdatedAt.Format("2006-01-02T15:04:05.000Z07:00")
	}
	return view
}

// modelToInternalView is the raw-config variant — secrets NOT redacted.
// Used only by GetTenantIdPsInternal / GetInternal; callers must be behind
// the internal-token gate.
func modelToInternalView(m *models.IdPSource) *InternalIdPSourceView {
	cfg, _ := m.GetConfig()

	view := &InternalIdPSourceView{
		TenantID:  m.TenantID,
		Provider:  m.Provider,
		Config:    cfg,
		Scope:     m.Scope,
		Enabled:   m.Enabled,
		Priority:  m.Priority,
		CreatedBy: m.CreatedBy,
		UpdatedBy: m.UpdatedBy,
	}
	if !m.CreatedAt.IsZero() {
		view.CreatedAt = m.CreatedAt.Format("2006-01-02T15:04:05.000Z07:00")
	}
	if !m.UpdatedAt.IsZero() {
		view.UpdatedAt = m.UpdatedAt.Format("2006-01-02T15:04:05.000Z07:00")
	}
	return view
}
