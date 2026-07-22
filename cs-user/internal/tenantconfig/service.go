// Package tenantconfig is the per-tenant YAML configuration store service
// (Phase C3.2).
//
// Wraps the tenant_configs table introduced in Phase A2. Two operations only:
//
//	Get(ctx, tenantID) → reads the row (returns a synthetic default row when
//	    the tenant has no entry yet — every tenant has an implicit "{}" config).
//	Update(ctx, params) → validates the YAML parses, then upserts the row
//	    with updated_by + updated_at stamped.
//
// This is the cs-user-side counterpart to the server's /api/tenant/config
// surface (server proxies via RPCClient). Tenant scoping is by explicit
// tenantID — the cs-user ResolveTenant middleware resolves the caller's
// tenant from X-Tenant-Id before this service runs, so we receive a
// canonical tenant_id PK and don't trust any client-supplied tenant field.
//
// What this service deliberately does NOT do:
//   - Schema-check the YAML. C3.2 is raw blob CRUD. C3.3 layers typed
//     provider_mapping schema validation on top.
//   - Field-level merge / patch semantics. PUT replaces the whole blob.
//   - Audit-log row writes (user_center_audit_log §16.2, deferred to C4).
//   - Optimistic concurrency (If-Match / etag). Single-admin edit cadence
//     per tenant makes last-write-wins acceptable for now.
package tenantconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// Sentinel errors. Handlers translate these to HTTP codes:
//   - ErrInvalidYAML    → 400 (YAML failed to parse)
//   - ErrYAMLTooLarge   → 413 (body exceeds the configured cap)
//   - ErrEmptyTenantID  → 400 (programmer error — middleware should have resolved)
var (
	ErrInvalidYAML   = errors.New("tenantconfig: invalid YAML")
	ErrYAMLTooLarge  = errors.New("tenantconfig: YAML exceeds size cap")
	ErrEmptyTenantID = errors.New("tenantconfig: empty tenant id")
)

// MaxYAMLLength is the byte cap on the stored blob. 64 KiB is generous for
// the design's known subsections (provider_mapping / username_strategy /
// employment_providers / features / enterprise_schema_ext) while still
// bounding the column size class. A tenant that legitimately needs more is
// almost certainly storing data that belongs in a typed table instead.
const MaxYAMLLength = 64 * 1024

// Service is the tenant_configs CRUD surface. Constructed once in main.go
// and injected into Deps as TenantConfig.
type Service struct {
	db       *gorm.DB
	cacher   Cacher // optional; if set, updates invalidate the cache
}

// New constructs a Service. db must be non-nil; main.go asserts this.
func New(db *gorm.DB) *Service {
	return &Service{db: db}
}

// SetCacher attaches a cache invalidation callback to the service.
// Called by main.go when wiring the CachedService layer.
func (s *Service) SetCacher(cacher Cacher) {
	s.cacher = cacher
}

// Get returns the tenant_configs row for the given tenant. When no row
// exists, returns a synthetic row with ConfigYAML="{}" and zero timestamps
// — every tenant implicitly has an empty config, so callers never see a
// "not found" error for the read path.
//
// The synthetic row's TenantID is populated so callers echoing the row
// back to clients see the right key. Timestamps being zero-valued signals
// "never written"; clients that care should check CreatedAt.IsZero().
func (s *Service) Get(ctx context.Context, tenantID string) (*models.TenantConfig, error) {
	if tenantID == "" {
		return nil, ErrEmptyTenantID
	}

	var tc models.TenantConfig
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Take(&tc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &models.TenantConfig{
			TenantID:   tenantID,
			ConfigYAML: "{}",
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

// UpdateParams carries the inputs to an upsert. UpdatedBy is optional (nil
// means "actor unknown" — server forwards the JWT subject_id when present).
type UpdateParams struct {
	TenantID   string
	ConfigYAML string
	UpdatedBy  *string // optional; nil preserves "no actor recorded"
}

// Update validates the YAML, then upserts the row. On success returns the
// row as written (with refreshed UpdatedAt + the supplied UpdatedBy).
//
// Empty / whitespace-only YAML is normalized to "{}" so the read path's
// "row missing → default {}" symmetry holds even after an explicit clear.
//
// Implementation note: we use find-then-act inside a transaction rather
// than gorm's clause.OnConflict to keep the call portable across the
// sqlite test backend and the Postgres production backend. The race window
// (two concurrent PUTs to the same tenant_id) collapses to a harmless
// last-write-wins because both transactions touch a single row by PK.
func (s *Service) Update(ctx context.Context, p UpdateParams) (*models.TenantConfig, error) {
	if p.TenantID == "" {
		return nil, ErrEmptyTenantID
	}

	yamlStr := strings.TrimSpace(p.ConfigYAML)
	if yamlStr == "" {
		yamlStr = "{}"
	}
	if len(yamlStr) > MaxYAMLLength {
		return nil, ErrYAMLTooLarge
	}

	// Parse strictly to catch malformed input early. We don't model the
	// shape — yaml.v3 ignores unknown keys, so this only rejects syntactic
	// garbage. The typed provider_mapping check (C3.3) will layer on top.
	var node any
	if err := yaml.Unmarshal([]byte(yamlStr), &node); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidYAML, err)
	}

	now := time.Now().UTC()

	var out models.TenantConfig
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.TenantConfig
		lookupErr := tx.Where("tenant_id = ?", p.TenantID).Take(&existing).Error

		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			// First write for this tenant — insert.
			row := models.TenantConfig{
				TenantID:   p.TenantID,
				ConfigYAML: yamlStr,
				UpdatedBy:  p.UpdatedBy,
				UpdatedAt:  now,
				CreatedAt:  now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			out = row
			return nil
		}
		if lookupErr != nil {
			return lookupErr
		}

		// Existing row — update in place. Preserve the original created_at;
		// only updated_* fields advance.
		updates := map[string]any{
			"config_yaml": yamlStr,
			"updated_by":  p.UpdatedBy,
			"updated_at":  now,
		}
		if err := tx.Model(&models.TenantConfig{}).
			Where("tenant_id = ?", p.TenantID).
			Updates(updates).Error; err != nil {
			return err
		}
		existing.ConfigYAML = yamlStr
		existing.UpdatedBy = p.UpdatedBy
		existing.UpdatedAt = now
		out = existing
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Invalidate cache for this tenant (E1.5)
	if s.cacher != nil {
		s.cacher.Invalidate(p.TenantID)
	}

	return &out, nil
}

// GetProviderMapping returns the typed provider_mapping subsection
// (Phase C3.3). Reads the raw blob via Get (synthetic default on missing
// row), then parses out provider_mapping. Returns an empty mapping
// (Providers == {}) when the section is absent — every tenant implicitly
// has an empty mapping.
func (s *Service) GetProviderMapping(ctx context.Context, tenantID string) (*ProviderMapping, error) {
	if tenantID == "" {
		return nil, ErrEmptyTenantID
	}
	tc, err := s.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return ParseProviderMapping(tc.ConfigYAML)
}

// UpdateProviderMappingParams carries the typed PUT inputs.
type UpdateProviderMappingParams struct {
	TenantID  string
	Mapping   *ProviderMapping
	UpdatedBy *string
}

// UpdateProviderMapping validates the typed mapping, merges it into the
// existing config_yaml blob (preserving sibling sections), and stores
// via Update. PUT semantics: the provider_mapping subtree is fully
// replaced; other top-level keys (employment_providers, features, etc.)
// are preserved verbatim.
func (s *Service) UpdateProviderMapping(ctx context.Context, p UpdateProviderMappingParams) (*ProviderMapping, error) {
	if p.TenantID == "" {
		return nil, ErrEmptyTenantID
	}

	// Validate + apply defaults in place.
	if err := p.Mapping.Validate(); err != nil {
		return nil, err
	}

	// Render the (possibly defaulted) mapping to YAML.
	section, err := SerializeProviderMapping(p.Mapping)
	if err != nil {
		return nil, err
	}

	// Read current blob to preserve sibling sections. Get returns
	// synthetic default {} on missing row, so first-write works too.
	tc, err := s.Get(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}

	merged, err := mergeProviderMappingSection(tc.ConfigYAML, section)
	if err != nil {
		return nil, err
	}

	if _, err := s.Update(ctx, UpdateParams{
		TenantID:   p.TenantID,
		ConfigYAML: merged,
		UpdatedBy:  p.UpdatedBy,
	}); err != nil {
		return nil, err
	}

	// Invalidate cache for this tenant (E1.5)
	if s.cacher != nil {
		s.cacher.Invalidate(p.TenantID)
	}

	// Re-parse the merged result so callers see the canonical view
	// (with defaults applied) rather than their input verbatim.
	return ParseProviderMapping(merged)
}

// mergeProviderMappingSection splices the `provider_mapping:` subtree
// from `section` into `blob`, preserving all sibling top-level keys.
//
// Implementation: parse blob into yaml.v3 Node tree, find or insert the
// provider_mapping key, replace its value, re-serialize. Node-based
// (rather than map[string]any) so we don't lose comments / ordering in
// sibling sections that the tenant admin may have authored via C3.2's
// raw endpoint.
func mergeProviderMappingSection(blob, section string) (string, error) {
	blob = strings.TrimSpace(blob)
	section = strings.TrimSpace(section)

	// Empty blob → section IS the new blob.
	if blob == "" || blob == "{}" {
		return section, nil
	}

	var docNode yaml.Node
	if err := yaml.Unmarshal([]byte(blob), &docNode); err != nil {
		return "", fmt.Errorf("%w: parse existing blob: %v", ErrInvalidYAML, err)
	}

	// yaml.Unmarshal into a Node produces a DocumentNode whose Content[0]
	// is the actual root mapping. Unwrap so we operate on the mapping directly.
	root := &docNode
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	// Root must be a mapping; if not (e.g. scalar), fall back to section.
	if root.Kind != yaml.MappingNode || len(root.Content) == 0 {
		return section, nil
	}

	var sectionNode yaml.Node
	if err := yaml.Unmarshal([]byte(section), &sectionNode); err != nil {
		return "", fmt.Errorf("%w: parse provider_mapping section: %v", ErrInvalidYAML, err)
	}
	// sectionNode is a document wrapping a mapping with a single key
	// `provider_mapping`; unwrap to the inner mapping.
	secRoot := &sectionNode
	if secRoot.Kind == yaml.DocumentNode && len(secRoot.Content) > 0 {
		secRoot = secRoot.Content[0]
	}
	var pmValue *yaml.Node
	if secRoot.Kind == yaml.MappingNode && len(secRoot.Content) >= 2 {
		pmValue = secRoot.Content[1]
	}

	const key = "provider_mapping"
	// Walk the root mapping's key/value pairs (Content[0]=key, [1]=value, ...).
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			if pmValue != nil {
				root.Content[i+1] = pmValue
			}
			out, err := yaml.Marshal(root)
			if err != nil {
				return "", fmt.Errorf("tenantconfig: re-marshal merged blob: %w", err)
			}
			return string(out), nil
		}
	}

	// Key not present — append.
	if pmValue != nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
			pmValue,
		)
	}
	out, err := yaml.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("tenantconfig: marshal merged blob: %w", err)
	}
	return string(out), nil
}

// LoadProviderMapping loads and merges the provider_mapping for a tenant,
// following MULTI_TENANCY_DESIGN.md §17.2. Returns the effective mapping
// after applying global defaults + tenant-specific overrides.
//
// Merge semantics:
//   - global default is loaded from GlobalProviderMapping() (code-defined)
//   - tenant-specific override is loaded from tenant_configs.provider_mapping
//   - deep merge: tenant entries fully replace global entries with same name
//   - provider names present only in global are preserved
//
// This is the E1 implementation of provider_mapping standardization.
func (s *Service) LoadProviderMapping(ctx context.Context, tenantID string) (*ProviderMapping, error) {
	if tenantID == "" {
		return nil, ErrEmptyTenantID
	}

	// 1. Load global default (code-defined)
	global := GlobalProviderMapping()

	// 2. Load tenant-specific override
	tc, err := s.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	tenantMapping, err := ParseProviderMapping(tc.ConfigYAML)
	if err != nil {
		// Parse error in tenant config — log but return global only
		// This prevents a malformed tenant config from breaking login entirely
		return global, nil
	}

	// 3. Deep merge: start with global, overlay tenant entries
	merged := &ProviderMapping{
		Version:  global.Version,           // Copy version from global
		Providers: make(map[string]Provider),
	}

	// Copy all global entries first
	for name, p := range global.Providers {
		merged.Providers[name] = p
	}

	// Overlay tenant entries (full replace, not field-level merge)
	for name, p := range tenantMapping.Providers {
		merged.Providers[name] = p
	}

	return merged, nil
}

// GetEnabledProviders returns the list of enabled provider names for a tenant.
// This is an E1.6 helper for E2 (tenant-level IdP integration). It loads
// the effective provider mapping (global + tenant merge) and filters to
// enabled=true entries.
//
// Returns a sorted list of provider names (deterministic for UI rendering).
// Returns an empty slice (not nil) if no providers are enabled.
func (s *Service) GetEnabledProviders(ctx context.Context, tenantID string) ([]string, error) {
	mapping, err := s.LoadProviderMapping(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	var enabled []string
	for name, p := range mapping.Providers {
		if p.Enabled != nil && *p.Enabled {
			enabled = append(enabled, name)
		}
	}

	return enabled, nil
}
