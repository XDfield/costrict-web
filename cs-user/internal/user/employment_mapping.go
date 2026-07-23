package user

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

	// ExternalClaims is the raw payload from which enterprise field values
	// are extracted via the tenant's field_map config (keys = external field
	// names as configured in field_map.<provider>, values = the claim data
	// from the IdP's JWT or provider API response). Caller is responsible
	// for populating this — typically the OAuth callback handler decodes
	// the IdP's userinfo response into the map.
	//
	// Nil/empty claims + a configured field_map is allowed: every mapped
	// column simply stays NULL on this write. Slice 2's real provider
	// clients will be the canonical source; for Slice 1.5 the caller may
	// pass claims harvested from the OAuth callback.
	ExternalClaims map[string]any
}

// employmentProvidersConfig is the typed shape of the `employment_providers`
// section in tenant_configs.config_yaml. Slice 1 of the field_map feature
// (2026-07-23) added FieldMap; per-provider interval / on_login config still
// ships with real provider clients in slice 2 — yaml.v3 silently ignores
// those fields until we model them. Plan B (2026-07-23) added
// ProviderDetection so new Casdoor-brokered IdPs can be recognized without
// a server-side code change.
type employmentProvidersConfig struct {
	Enabled           []string                  `yaml:"enabled"`
	FieldMap          map[string]FieldMapConfig `yaml:"field_map,omitempty"`
	ProviderDetection []ProviderDetectionRule   `yaml:"provider_detection,omitempty"`
}

// ProviderDetectionRule maps an IdP-recognition signal (e.g. Casdoor JWT's
// `signupApplication` value) to a provider name in the `enabled` list. Used
// when the upstream JWT does not set `provider` explicitly — historically
// server's authidentity/normalize.go hardcoded this switch case-by-case;
// moving it here makes adding a new Casdoor-brokered IdP a config-only
// change.
//
// YAML shape:
//
//	- signup_application: "idtrust"   # matcher (case-insensitive equality)
//	  provider: idtrust               # must be in employment_providers.enabled
//	- signup_application: "mycorp-sso"
//	  provider: mycorp_sso
type ProviderDetectionRule struct {
	SignupApplication string `yaml:"signup_application"`
	Provider          string `yaml:"provider"`
}

// FieldMapConfig maps internal employment_identities columns (the YAML keys,
// which MUST be in allowedEmploymentColumns) to external IdP field names
// (the YAML values, free-form — they depend on the provider's JWT claim or
// API response schema). External field names are not validated at parse time
// because they're provider-specific and only become load-bearing in slice 2
// when real provider clients read them.
//
// Example YAML:
//
//	employment_providers:
//	  enabled: [wxwork]
//	  field_map:
//	    wxwork:
//	      enterprise_uid: "UserId"        # internal column ← external IdP field
//	      employee_number: "JobNumber"
//	      cost_center: "Department"
type FieldMapConfig map[string]string

// allowedEmploymentColumns is the whitelist of internal column names that may
// appear as FieldMapConfig values. Anything else surfaces as a parse error.
// Sourced from the EmploymentIdentity model fields that are operator-mappable
// (excludes primary key, audit timestamps, sync metadata, and the attributes
// JSON blob).
var allowedEmploymentColumns = map[string]struct{}{
	"enterprise_uid":              {},
	"display_name":                {},
	"employee_number":             {},
	"cost_center":                 {},
	"org_path":                    {},
	"direct_manager_subject_id":   {},
	"direct_manager_external_ref": {},
	"job_title":                   {},
	"job_level":                   {},
	"employment_type":             {},
	"hire_date":                   {},
	"regular_date":                {},
	"work_location":               {},
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
// Slice 1.5 makes the field_map actually drive writes: when params.ExternalClaims
// is non-empty AND the tenant configures a field_map for params.Provider, the
// mapped enterprise fields (employee_number, cost_center, enterprise_uid,
// direct_manager_*, hire_date, etc.) are populated from claims on both the
// create and update-in-place paths. Callers that haven't wired ExternalClaims
// yet (e.g. the OAuth callback integration is still Slice 2 territory) get the
// stub-write-path fallback: only user_subject_id + provider land on the row,
// all enterprise fields stay NULL. Missing/nil/unparseable claims are
// soft-failed — the column stays NULL rather than 500-ing the login.
//
// Real provider clients (idtrust API integration, Azure AD Graph, etc.) and
// A5's extended JWT claims will become the canonical ExternalClaims source in
// a follow-up; the mapping logic itself is already in place.
//
// Missing tenant_configs row is treated as disabled (ErrEnterpriseMappingDisabled),
// not an error — enterprise mapping is a bonus feature and must not block
// login. Malformed YAML is a real error (operator config bug worth surfacing).
//
// Provider resolution order:
//  1. params.Provider is used directly if it's in the tenant's enabled list.
//  2. Otherwise, provider_detection rules are evaluated against
//     params.ExternalClaims (e.g. signup_application matcher against
//     ExternalClaims["signupApplication"]). First match wins; the resolved
//     provider must itself be in the enabled list.
//  3. No resolution → ErrEnterpriseMappingDisabled (login still succeeds;
//     the hook is best-effort).
//
// This lets new Casdoor-brokered IdPs be added via tenant config alone —
// server no longer needs a hardcoded case in authidentity/normalize.go for
// each new IdP's signupApplication value.
func (s *Service) ApplyEnterpriseMapping(ctx context.Context, params EmploymentMappingParams) error {
	if s == nil || s.db == nil {
		return errors.New("user.Service: nil db")
	}
	if params.UserSubjectID == "" {
		return errors.New("ApplyEnterpriseMapping: empty UserSubjectID")
	}
	if params.TenantID == "" {
		params.TenantID = defaultTenantID
	}

	cfg, err := s.loadEmploymentProvidersConfig(ctx, params.TenantID)
	if err != nil {
		return fmt.Errorf("load employment_providers config: %w", err)
	}

	provider := params.Provider
	if provider == "" || !containsString(cfg.Enabled, provider) {
		// Server didn't recognize the IdP (or no provider hint at all). Try
		// the tenant's detection rules before giving up.
		if detected := detectProvider(cfg.ProviderDetection, params.ExternalClaims); detected != "" && containsString(cfg.Enabled, detected) {
			provider = detected
		}
	}
	// No provider resolved (empty params.Provider + no/failing detection, or
	// unknown explicit Provider) → treat as "feature not applicable for this
	// login" rather than a caller bug. Enterprise mapping is a bonus hook and
	// must never block login.
	if provider == "" || !containsString(cfg.Enabled, provider) {
		return ErrEnterpriseMappingDisabled
	}
	params.Provider = provider

	return s.upsertEmploymentIdentity(ctx, params, cfg.FieldMap[provider])
}

// detectProvider evaluates the tenant's detection rules in order against the
// raw external claims. First match wins; returns "" when nothing matches.
// The signup_application matcher does case-insensitive equality against the
// ExternalClaims["signupApplication"] value (the key Casdoor uses to tag
// which application a user signed up through).
func detectProvider(rules []ProviderDetectionRule, external map[string]any) string {
	if len(rules) == 0 || len(external) == 0 {
		return ""
	}
	raw, ok := external["signupApplication"]
	if !ok || raw == nil {
		return ""
	}
	signupApp := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
	if signupApp == "" {
		return ""
	}
	for _, rule := range rules {
		if rule.SignupApplication == "" || rule.Provider == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(rule.SignupApplication), signupApp) {
			return rule.Provider
		}
	}
	return ""
}

// loadEmploymentProvidersConfig reads tenant_configs.config_yaml and parses
// the employment_providers section (enabled list + optional per-provider
// field_map). Returns the zero value (no enabled providers, no field_map)
// when the tenant_configs row is missing — treated as "feature off" rather
// than an error, because enterprise mapping is a bonus feature that
// shouldn't block login. An operator who wants the feature off simply
// deletes the row (or empties the enabled list).
//
// Field_map values are validated against allowedEmploymentColumns at parse
// time — unknown internal column names surface as a config error so a typo
// fails loudly instead of silently mapping to nothing at runtime.
func (s *Service) loadEmploymentProvidersConfig(ctx context.Context, tenantID string) (employmentProvidersConfig, error) {
	var cfg employmentProvidersConfig

	var tc models.TenantConfig
	err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Take(&tc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}

	// Empty / "{}" YAML → no providers section → zero value. Skipping the
	// yaml.Unmarshal call avoids paying the parse cost on the common case.
	if tc.ConfigYAML == "" || tc.ConfigYAML == "{}" {
		return cfg, nil
	}

	// yaml.v3 ignores unknown fields by default, so this parses cleanly even
	// when the YAML carries per-provider subsections we don't model yet
	// (interval / on_login — slice 2 territory).
	var parsed struct {
		EmploymentProviders employmentProvidersConfig `yaml:"employment_providers"`
	}
	if err := yaml.Unmarshal([]byte(tc.ConfigYAML), &parsed); err != nil {
		return cfg, fmt.Errorf("parse config_yaml: %w", err)
	}

	for provider, m := range parsed.EmploymentProviders.FieldMap {
		for internal := range m {
			if _, ok := allowedEmploymentColumns[internal]; !ok {
				return cfg, fmt.Errorf(
					"employment_providers.field_map.%s.%s: unknown internal column (see allowedEmploymentColumns)",
					provider, internal,
				)
			}
		}
	}

	// Detection rules must resolve to an enabled provider; otherwise the rule
	// is dead config that silently never fires. Catch typos at config load.
	seenMatchers := make(map[string]string) // lower(signup_application) → provider
	for _, rule := range parsed.EmploymentProviders.ProviderDetection {
		if strings.TrimSpace(rule.SignupApplication) == "" || strings.TrimSpace(rule.Provider) == "" {
			return cfg, fmt.Errorf(
				"employment_providers.provider_detection: signup_application and provider are both required",
			)
		}
		if !containsString(parsed.EmploymentProviders.Enabled, rule.Provider) {
			return cfg, fmt.Errorf(
				"employment_providers.provider_detection[signup_application=%s]: provider %q is not in employment_providers.enabled",
				rule.SignupApplication, rule.Provider,
			)
		}
		key := strings.ToLower(strings.TrimSpace(rule.SignupApplication))
		if _, dup := seenMatchers[key]; dup {
			return cfg, fmt.Errorf(
				"employment_providers.provider_detection: duplicate signup_application matcher %q",
				rule.SignupApplication,
			)
		}
		seenMatchers[key] = rule.Provider
	}

	return parsed.EmploymentProviders, nil
}

// upsertEmploymentIdentity writes (or refreshes) the user's
// employment_identities row. Phase A semantics: update-in-place when a row
// exists, create when it doesn't. Soft-delete-then-create for audit trail
// is a follow-up — Phase A's row doesn't carry load-bearing data yet.
//
// The 24h NextSyncDueAt window pairs with a future refresh_if_stale
// short-circuit: a caller can check time.Now().Before(NextSyncDueAt) to
// skip the provider call entirely on repeat logins.
func (s *Service) upsertEmploymentIdentity(ctx context.Context, params EmploymentMappingParams, fieldMap FieldMapConfig) error {
	db := s.db.WithContext(ctx)

	// Compute the mapped column values once; same map applies to both the
	// create and update paths. applyFieldMap is no-op-safe: empty field_map
	// or empty claims yields an empty map, preserving the stub write path
	// for callers that haven't wired ExternalClaims yet.
	mapped := applyFieldMap(fieldMap, params.ExternalClaims)

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
		applyMappedToRow(&row, mapped)
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
	for col, val := range mapped {
		updates[col] = val
	}
	if err := db.Model(&existing).Updates(updates).Error; err != nil {
		return fmt.Errorf("update employment_identity: %w", err)
	}
	return nil
}

// applyMappedToRow writes applyFieldMap output onto an EmploymentIdentity
// struct for the Create path. Column-name-keyed dispatch mirrors the model
// field names; we cover only the operator-mappable columns (the same set as
// allowedEmploymentColumns).
func applyMappedToRow(row *models.EmploymentIdentity, mapped map[string]any) {
	for col, val := range mapped {
		switch col {
		case "enterprise_uid":
			if s, ok := val.(string); ok {
				row.EnterpriseUID = stringPtr(s)
			}
		case "display_name":
			if s, ok := val.(string); ok {
				row.DisplayName = stringPtr(s)
			}
		case "employee_number":
			if s, ok := val.(string); ok {
				row.EmployeeNumber = stringPtr(s)
			}
		case "cost_center":
			if s, ok := val.(string); ok {
				row.CostCenter = stringPtr(s)
			}
		case "org_path":
			if s, ok := val.(string); ok {
				row.OrgPath = stringPtr(s)
			}
		case "direct_manager_subject_id":
			if s, ok := val.(string); ok {
				row.DirectManagerSubjectID = stringPtr(s)
			}
		case "direct_manager_external_ref":
			if s, ok := val.(string); ok {
				row.DirectManagerExternalRef = stringPtr(s)
			}
		case "job_title":
			if s, ok := val.(string); ok {
				row.JobTitle = stringPtr(s)
			}
		case "job_level":
			if s, ok := val.(string); ok {
				row.JobLevel = stringPtr(s)
			}
		case "employment_type":
			if s, ok := val.(string); ok {
				row.EmploymentType = stringPtr(s)
			}
		case "hire_date":
			if t, ok := val.(time.Time); ok {
				row.HireDate = timePtr(t)
			}
		case "regular_date":
			if t, ok := val.(time.Time); ok {
				row.RegularDate = timePtr(t)
			}
		case "work_location":
			if s, ok := val.(string); ok {
				row.WorkLocation = stringPtr(s)
			}
		}
	}
}

func timePtr(t time.Time) *time.Time { return &t }

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

// dateEmploymentColumns lists internal employment_identities columns whose
// DB type is timestamptz, so applyFieldMap coerces their external claim
// values through time.Parse(RFC3339). Anything else maps to a string column.
var dateEmploymentColumns = map[string]struct{}{
	"hire_date":   {},
	"regular_date": {},
}

// applyFieldMap extracts external claim values via fieldMap and coerces them
// to the type expected by each internal column. Returns a map of
// internal_column → typed value (ready for gorm.Updates), and a map of
// internal_column → external_field for diagnostics.
//
// Coercion rules:
//   - Date columns (hire_date, regular_date): parse the external value as
//     RFC 3339. Unparseable / missing / nil → column skipped (stays NULL).
//     This is a deliberate soft-fail: a malformed IdP date shouldn't 500 the
//     login, the column just remains unset.
//   - String columns: stringify via fmt.Sprint to handle non-string claims
//     (numbers, booleans) without forcing the caller to pre-cast.
//   - Missing external field key → column skipped (stays NULL on the row).
//
// FieldMapConfig is map[string]string (the YAML decoder rejects non-string
// values at parse time), so externalField is always a string here. The parser
// already validates KEYS against allowedEmploymentColumns at config load
// time, so we don't re-validate here.
//
// externalField supports dotted-path traversal of nested claim maps, e.g.
// "properties.oauth_Custom.id" walks claims["properties"]["oauth_Custom"]["id"].
// This lets field_map reach into Casdoor-brokered IdP payloads where the
// per-provider data sits under a `properties.<oauth_prefix>.<field>` namespace.
// A path component that is missing or non-map short-circuits to "column
// skipped" (stays NULL) — same soft-fail semantic as a missing top-level key.
func applyFieldMap(fieldMap FieldMapConfig, claims map[string]any) map[string]any {
	out := make(map[string]any, len(fieldMap))
	if len(fieldMap) == 0 || len(claims) == 0 {
		return out
	}
	for internalCol, externalField := range fieldMap {
		if externalField == "" {
			continue
		}
		raw, present := lookupClaimPath(claims, externalField)
		if !present || raw == nil {
			continue
		}
		if _, isDate := dateEmploymentColumns[internalCol]; isDate {
			t, ok := parseClaimDate(raw)
			if !ok {
				continue
			}
			out[internalCol] = t
			continue
		}
		out[internalCol] = fmt.Sprint(raw)
	}
	return out
}

// lookupClaimPath resolves a (possibly dotted) external field reference
// against the claims map. Single-component names do a direct map lookup
// (backward compat). Multi-component names walk nested map[string]any layers;
// any missing key or non-map intermediate returns (nil, false).
func lookupClaimPath(claims map[string]any, field string) (any, bool) {
	if !strings.Contains(field, ".") {
		v, ok := claims[field]
		return v, ok
	}
	var cursor any = claims
	for _, seg := range strings.Split(field, ".") {
		m, ok := cursor.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := m[seg]
		if !present {
			return nil, false
		}
		cursor = v
	}
	return cursor, true
}

// parseClaimDate accepts the common date shapes an IdP might emit (RFC 3339
// string, int64 Unix seconds, int64 Unix millis, time.Time) and returns the
// normalized time. Returns ok=false on any unparseable input.
func parseClaimDate(raw any) (time.Time, bool) {
	switch v := raw.(type) {
	case time.Time:
		return v, true
	case string:
		if v == "" {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	case int64:
		return time.Unix(v, 0), true
	case int:
		return time.Unix(int64(v), 0), true
	case float64:
		// JWT NumericDate is JSON number → float64 through encoding/json.
		// Treat whole numbers as Unix seconds; sub-second fraction is
		// discarded (employment dates don't need sub-second precision).
		return time.Unix(int64(v), 0), true
	default:
		return time.Time{}, false
	}
}
