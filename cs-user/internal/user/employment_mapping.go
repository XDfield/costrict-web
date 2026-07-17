package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// defaultTenantID mirrors the bootstrap row shipped in A6
// (tenant_id="default"). Phase A runs in implicit-single-tenant mode per
// ROADMAP §5; Phase B introduces real tenant routing.
const defaultTenantID = "default"

// employmentSyncInterval bounds how often ApplyEnterpriseMapping re-syncs an
// existing employment_identities row. Repeat logins inside this window skip
// the provider call. Phase A uses a fixed 24h because per-provider interval
// config (MULTI_TENANCY §9.2 lines 589-597) ships with the real provider
// clients in a follow-up.
const employmentSyncInterval = 24 * time.Hour

// EmploymentMappingParams captures the inputs to ApplyEnterpriseMapping.
// Struct-based so we can extend (claims payload, force refresh) without
// churning every caller.
type EmploymentMappingParams struct {
	// TenantID is the tenant scope for the config lookup. Empty falls back
	// to defaultTenantID — Phase A always uses the bootstrap row.
	TenantID string

	// UserSubjectID is the user's stable application-level identifier
	// (users.subject_id). Required.
	UserSubjectID string

	// Provider is the auth provider used in this login (e.g. "idtrust",
	// "github", "phone"). Compared against employment_providers.enabled.
	Provider string
}

// employmentProvidersConfig is the typed shape of the `employment_providers`
// section in tenant_configs.config_yaml. Phase A only consumes `enabled`;
// per-provider config (interval / on_login / field_map) ships with real
// provider clients in a follow-up — yaml.v3 silently ignores those fields
// until we model them.
type employmentProvidersConfig struct {
	Enabled []string `yaml:"enabled"`
}

// ErrEnterpriseMappingDisabled signals the provider is not in the tenant's
// employment_providers.enabled list (or the tenant_configs row is missing).
// Distinct from a real error so the caller can treat "skipped" as success.
var ErrEnterpriseMappingDisabled = errors.New("enterprise mapping disabled for provider")

// ApplyEnterpriseMapping refreshes the user's employment_identities snapshot
// based on the tenant's employment_providers config.
//
// Flow:
//  1. Load tenant_configs.config_yaml for params.TenantID (default fallback).
//  2. Parse employment_providers.enabled list.
//  3. If params.Provider is NOT enabled → no-op, return ErrEnterpriseMappingDisabled.
//  4. Upsert employment_identities row for the user.
//
// Phase A ships a stub write path: only user_subject_id + provider are
// populated from params; all enterprise fields (employee_number,
// cost_center, direct_manager_*, etc.) remain NULL. Real provider clients
// (idtrust API integration, Azure AD Graph, etc.) and A5's extended JWT
// claims will fill those in — A4's job is to land the gating logic + write
// path + config reader so follow-ups only swap the data source.
//
// Missing tenant_configs row is treated as disabled (ErrEnterpriseMappingDisabled),
// not an error — enterprise mapping is a bonus feature and must not block
// login. Malformed YAML is a real error (operator config bug worth surfacing).
func (s *Service) ApplyEnterpriseMapping(ctx context.Context, params EmploymentMappingParams) error {
	if s == nil || s.db == nil {
		return errors.New("user.Service: nil db")
	}
	if params.UserSubjectID == "" {
		return errors.New("ApplyEnterpriseMapping: empty UserSubjectID")
	}
	if params.Provider == "" {
		return errors.New("ApplyEnterpriseMapping: empty Provider")
	}
	if params.TenantID == "" {
		params.TenantID = defaultTenantID
	}

	enabled, err := s.loadEnabledEmploymentProviders(ctx, params.TenantID)
	if err != nil {
		return fmt.Errorf("load employment_providers config: %w", err)
	}
	if !containsString(enabled, params.Provider) {
		return ErrEnterpriseMappingDisabled
	}

	return s.upsertEmploymentIdentity(ctx, params)
}

// loadEnabledEmploymentProviders reads tenant_configs.config_yaml and
// extracts the employment_providers.enabled list.
//
// Returns (nil, nil) when the tenant_configs row is missing — treated as
// "no providers enabled" rather than an error, because enterprise mapping
// is a bonus feature that shouldn't block login. An operator who wants the
// feature off simply deletes the row (or empties the enabled list).
func (s *Service) loadEnabledEmploymentProviders(ctx context.Context, tenantID string) ([]string, error) {
	var tc models.TenantConfig
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Take(&tc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Empty / "{}" YAML → no providers section → empty list. Skipping the
	// yaml.Unmarshal call avoids paying the parse cost on the common case.
	if tc.ConfigYAML == "" || tc.ConfigYAML == "{}" {
		return nil, nil
	}

	// yaml.v3 ignores unknown fields by default, so this parses cleanly even
	// when the YAML carries per-provider subsections we don't model yet.
	var parsed struct {
		EmploymentProviders employmentProvidersConfig `yaml:"employment_providers"`
	}
	if err := yaml.Unmarshal([]byte(tc.ConfigYAML), &parsed); err != nil {
		return nil, fmt.Errorf("parse config_yaml: %w", err)
	}
	return parsed.EmploymentProviders.Enabled, nil
}

// upsertEmploymentIdentity writes (or refreshes) the user's
// employment_identities row. Phase A semantics: update-in-place when a row
// exists, create when it doesn't. Soft-delete-then-create for audit trail
// is a follow-up — Phase A's row doesn't carry load-bearing data yet.
//
// The 24h NextSyncDueAt window pairs with a future refresh_if_stale
// short-circuit: a caller can check time.Now().Before(NextSyncDueAt) to
// skip the provider call entirely on repeat logins.
func (s *Service) upsertEmploymentIdentity(ctx context.Context, params EmploymentMappingParams) error {
	db := s.db.WithContext(ctx)

	var existing models.EmploymentIdentity
	err := db.Where("user_subject_id = ?", params.UserSubjectID).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := time.Now()
		row := models.EmploymentIdentity{
			UserSubjectID: params.UserSubjectID,
			Provider:      params.Provider,
			SyncStatus:    "fresh",
			LastSyncedAt:  now,
			NextSyncDueAt: now.Add(employmentSyncInterval),
		}
		if err := db.Create(&row).Error; err != nil {
			return fmt.Errorf("create employment_identity: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("query employment_identity: %w", err)
	}

	now := time.Now()
	updates := map[string]any{
		"provider":         params.Provider,
		"sync_status":      "fresh",
		"last_synced_at":   now,
		"next_sync_due_at": now.Add(employmentSyncInterval),
	}
	if err := db.Model(&existing).Updates(updates).Error; err != nil {
		return fmt.Errorf("update employment_identity: %w", err)
	}
	return nil
}

// GetEmploymentIdentity loads the user's employment_identities snapshot.
// Returns (nil, nil) when no row exists — the A7 reissue-token flow treats
// this as "no enterprise context" rather than failing, so users without an
// employment_identities row (provider not enabled, never synced) still get a
// valid token, just without enterprise claims. Soft-deleted rows are
// excluded by gorm's DeletedAt handling.
//
// Empty userSubjectID is a caller-programming error (400-mappable).
func (s *Service) GetEmploymentIdentity(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if userSubjectID == "" {
		return nil, ErrEmptySubjectID
	}
	var row models.EmploymentIdentity
	err := s.db.WithContext(ctx).
		Where("user_subject_id = ?", userSubjectID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query employment_identity: %w", err)
	}
	return &row, nil
}

// containsString reports whether v contains s. Enabled-provider lists are
// tiny (1-5 entries typically) so a linear scan beats a map allocation.
func containsString(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
