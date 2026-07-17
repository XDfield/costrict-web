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
	db *gorm.DB
}

// New constructs a Service. db must be non-nil; main.go asserts this.
func New(db *gorm.DB) *Service {
	return &Service{db: db}
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
	return &out, nil
}
