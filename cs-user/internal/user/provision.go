// Package user — provision.go exposes ProvisionByEnterprise, the entry point
// for external business modules (e.g. the enterprise permission binding module)
// that hold their own "enterprise identity id" but lack the Casdoor-side
// universal_id / sub needed by GetOrCreateUser.
//
// Design notes:
//   - The endpoint reuses GetOrCreateUser via a synthetic JWTClaims so all
//     existing side effects (identity row, outbox user.created event,
//     applyEnterpriseMappingOnLogin) fire unchanged.
//   - The synthetic claim's UniversalID is set to the caller's enterprise_uid.
//     This is intentional: it persists the caller's handle into
//     users.casdoor_universal_id as an observable trace, and lets a future
//     real OAuth callback that happens to carry the same universal_id hit
//     via the existing universal_id lookup branch (path C in the integration
//     doc).
//   - The employment_identities row is force-written after GetOrCreateUser
//     returns, bypassing the tenant's employment_providers.enabled gate. The
//     gate is a tenant-admin config concern; pre-provisioning is an explicit
//     admin-level intent from the calling module, so we don't refuse when the
//     provider isn't registered for the tenant. The reverse-lookup branch
//     added to GetOrCreateUser reads this row back on real OAuth login.
package user

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"gorm.io/gorm"
)

// ProvisionByEnterpriseParams captures the inputs to ProvisionByEnterprise.
// All identity signals come from the caller — no Casdoor claim is required.
type ProvisionByEnterpriseParams struct {
	// EnterpriseProvider is the provider namespace for the enterprise identity.
	// Stored on employment_identities.provider and user_auth_identities.provider.
	// cs-user does NOT validate this against tenant_configs.employment_providers.enabled —
	// pre-provisioning is treated as an explicit admin intent that bypasses the
	// tenant's provider gate. The caller owns provider-name consistency across
	// repeat calls for the same enterprise identity.
	EnterpriseProvider string

	// EnterpriseUID is the caller-side stable identifier for the enterprise
	// identity. Stored on employment_identities.enterprise_uid and used as the
	// reverse-lookup key on future OAuth login (see GetOrCreateUser path 6).
	EnterpriseUID string

	// Username is the optional initial username. Falls back to "ext_<EnterpriseUID>"
	// when empty to avoid the users.username unique-index collision on multiple
	// pre-provisions with empty Name.
	Username string

	// DisplayName / Email / Phone are optional profile fields written to the
	// new user row.
	DisplayName string
	Email       string
	Phone       string

	// EmployeeNumber is optional. Written to employment_identities.employee_number
	// for the existing SearchUsersByEmployeeNumber reverse-lookup path.
	EmployeeNumber string

	// ExternalClaims is an optional passthrough bag. Merged into the synthetic
	// claim's ExternalClaims so applyEnterpriseMappingOnLogin's field_map can
	// harvest additional fields when tenant_configs has a field_map configured.
	// Keys "enterprise_uid" / "employee_number" are reserved — caller values
	// for those keys are ignored (the explicit params win).
	ExternalClaims map[string]any
}

// directProvisionFieldMap is the fixed FieldMapConfig used when force-writing
// the employment_identities row post-creation. The self-referential mapping
// (internal column → same-named ExternalClaims key) is intentional: it lets
// ProvisionByEnterprise populate known columns without depending on tenant
// field_map configuration. Columns are a subset of allowedEmploymentColumns.
var directProvisionFieldMap = FieldMapConfig{
	"enterprise_uid":  "enterprise_uid",
	"employee_number": "employee_number",
	"display_name":    "display_name",
	"job_title":       "job_title",
	"cost_center":     "cost_center",
	"org_path":        "org_path",
}

// ErrEnterpriseUIDCollision signals the (tenant_id, enterprise_uid) row is
// already bound to a different user_subject_id than the one GetOrCreateUser
// returned. This shouldn't happen because the idempotent pre-check at the top
// of ProvisionByEnterprise would have routed to the existing user; surfacing
// it as a sentinel lets the handler map to 409.
var ErrEnterpriseUIDCollision = errors.New("enterprise_uid already bound to a different user")

// ProvisionByEnterprise pre-creates a user keyed by an external enterprise
// identity. Idempotent: a second call with the same (tenant, enterprise_uid)
// returns the existing user with isNew=false. The ctx's tenant signal
// (set by the X-Tenant-Id middleware) decides which tenant the new row lands
// in; params does not override it — that's the single source of truth.
func (s *Service) ProvisionByEnterprise(ctx context.Context, params ProvisionByEnterpriseParams) (*models.User, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("user.Service: nil db")
	}
	if strings.TrimSpace(params.EnterpriseProvider) == "" {
		return nil, false, errors.New("ProvisionByEnterprise: empty EnterpriseProvider")
	}
	if strings.TrimSpace(params.EnterpriseUID) == "" {
		return nil, false, errors.New("ProvisionByEnterprise: empty EnterpriseUID")
	}

	db := s.db.WithContext(ctx)
	tenantID := tenant.IDFromContext(ctx)

	// 1. Idempotent lookup by (tenant_id, enterprise_uid). Partial unique
	//    index backs this query — at most one row.
	var existingEI models.EmploymentIdentity
	err := db.Where("tenant_id = ? AND enterprise_uid = ? AND deleted_at IS NULL", tenantID, params.EnterpriseUID).
		Take(&existingEI).Error
	if err == nil {
		// Hit: load the user and return without re-running create side effects.
		var existingUser models.User
		if err := db.Where("subject_id = ?", existingEI.UserSubjectID).Take(&existingUser).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, false, fmt.Errorf("employment_identity references missing user subject_id=%s", existingEI.UserSubjectID)
			}
			return nil, false, fmt.Errorf("load user for existing enterprise_uid: %w", err)
		}
		return &existingUser, false, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("query employment_identity by enterprise_uid: %w", err)
	}

	// 2. Build synthetic JWTClaims and delegate to GetOrCreateUser. The
	//    UniversalID is set to EnterpriseUID so users.casdoor_universal_id
	//    carries the trace and a future OAuth callback with the same
	//    universal_id hits via the existing universal_id lookup branch.
	username := strings.TrimSpace(params.Username)
	if username == "" {
		username = "ext_" + params.EnterpriseUID
	}

	synthetic := &models.JWTClaims{
		UniversalID:       params.EnterpriseUID,
		Provider:          params.EnterpriseProvider,
		Name:              username,
		PreferredUsername: params.DisplayName,
		Email:             params.Email,
		Phone:             params.Phone,
		ExternalClaims:    buildProvisionExternalClaims(params),
	}

	user, isNew, err := s.GetOrCreateUser(ctx, synthetic)
	if err != nil {
		return nil, false, fmt.Errorf("GetOrCreateUser via synthetic claims: %w", err)
	}

	// 3. Force-write the employment_identities row with explicit enterprise_uid.
	//    GetOrCreateUser already tried applyEnterpriseMappingOnLogin, but it
	//    silently no-ops when the provider isn't in tenant.enabled. We bypass
	//    that gate by calling upsertEmploymentIdentity directly with
	//    directProvisionFieldMap, which extracts the columns we know about
	//    from the synthetic ExternalClaims.
	if err := s.upsertEmploymentIdentity(ctx, EmploymentMappingParams{
		TenantID:       tenantID,
		UserSubjectID:  user.SubjectID,
		Provider:       params.EnterpriseProvider,
		ExternalClaims: synthetic.ExternalClaims,
	}, directProvisionFieldMap); err != nil {
		// The (tenant_id, enterprise_uid) partial unique index may fire if a
		// concurrent provision won the race. upsertEmploymentIdentity already
		// handles this via reconcileByEnterpriseUID, but as a last resort we
		// surface a sentinel so the handler can map to 409.
		if isDuplicateKeyError(err) {
			return nil, false, ErrEnterpriseUIDCollision
		}
		return nil, false, fmt.Errorf("force-write employment_identity: %w", err)
	}

	// Re-read so the returned user reflects the post-create state. GetOrCreateUser
	// already does this, but be defensive in case the employment write touched
	// audit fields.
	if refreshed, err := s.GetUserByID(ctx, user.SubjectID); err == nil {
		return refreshed, isNew, nil
	}
	return user, isNew, nil
}

// buildProvisionExternalClaims assembles the ExternalClaims map for the
// synthetic JWTClaims. It seeds the canonical keys (enterprise_uid /
// employee_number / display_name) from explicit params, then merges caller-
// supplied ExternalClaims while refusing to overwrite the reserved keys.
func buildProvisionExternalClaims(params ProvisionByEnterpriseParams) map[string]any {
	out := make(map[string]any, 4+len(params.ExternalClaims))
	out["enterprise_uid"] = params.EnterpriseUID
	if params.EmployeeNumber != "" {
		out["employee_number"] = params.EmployeeNumber
	}
	if params.DisplayName != "" {
		out["display_name"] = params.DisplayName
	}
	for k, v := range params.ExternalClaims {
		// Reserved keys — explicit params win. This keeps the contract obvious:
		// the canonical fields cannot be silently overridden via the passthrough bag.
		if k == "enterprise_uid" || k == "employee_number" || k == "display_name" {
			continue
		}
		out[k] = v
	}
	return out
}
