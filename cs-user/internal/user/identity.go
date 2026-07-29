// Package user — identity.go ports the primary-selection + profile-refresh
// helpers from server/internal/user/service.go. These run inside write-path
// transactions to keep the user row consistent with the bound identity set
// after every bind / unbind / transfer.
//
// Faithful port — see claims.go for the format-mismatch risk during the
// P0-8b dual-write canary.
package user

import (
	"context"
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"gorm.io/gorm"
)

// providerRank ranks identity providers by trustworthiness for primary
// selection. Higher rank wins; ties break on lower DB id (earliest bound).
// Values MUST match server:1121 — idtrust=300, github=200, phone=100,
// default=0. Reordering breaks primary cascade: an unbind that should
// promote a phone identity would silently keep a lower-rank primary.
func providerRank(provider string) int {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "idtrust":
		return 300
	case "github":
		return 200
	case "phone":
		return 100
	default:
		return 0
	}
}

// selectBestPrimary returns the identity that should be primary. Server:1134
// — picks max providerRank; ties break on lowest ID for determinism. Returns
// nil if the input slice has no non-nil entries.
func selectBestPrimary(identities []*models.UserAuthIdentity) *models.UserAuthIdentity {
	var best *models.UserAuthIdentity
	for _, identity := range identities {
		if identity == nil {
			continue
		}
		if best == nil || providerRank(identity.Provider) > providerRank(best.Provider) || (providerRank(identity.Provider) == providerRank(best.Provider) && identity.ID < best.ID) {
			best = identity
		}
	}
	return best
}

// refreshUserProfileFromIdentitiesTx recomputes the user row's
// provider-tracking fields (auth_provider / external_key / provider_user_id)
// from the best-primary identity and promotes the best-rank identity to
// is_primary when needed. Writes back iff something actually changed —
// server:1147's change detection avoids touching updated_at on repeat
// operations, which would mask real drift in ops dashboards.
//
// User-facing profile (display_name / email / phone / avatar_url /
// organization / username) is deliberately NOT touched here. Those fields
// are user-owned — same boundary as GetOrCreateUser's existing-user branch
// (service.go §1 / §7). Auto-clobbering them on bind/unbind/transfer would
// race the self-edit (UpdateProfile) and registration-complete
// (CompleteRegistration) flows.
//
// The tx argument is the caller's open transaction; the function commits
// nothing itself. Returns nil if the user has no identities or no field
// changed.
//
// B5 write scoping: every read / update in this helper is scoped to
// tenant.IDFromContext(ctx). The caller already applied the same scope to
// the surrounding tx work, so a no-match here means the subject_id is in
// another tenant — fail-closed (treat as "not found") rather than leak
// across tenants.
func refreshUserProfileFromIdentitiesTx(ctx context.Context, tx *gorm.DB, userSubjectID string) error {
	scope := tenant.Scope(ctx)
	var user models.User
	if err := tx.Scopes(scope).Where("subject_id = ?", userSubjectID).Take(&user).Error; err != nil {
		return err
	}
	var identities []*models.UserAuthIdentity
	if err := tx.Scopes(scope).Where("user_subject_id = ?", userSubjectID).Order("is_primary DESC, id ASC").Find(&identities).Error; err != nil {
		return err
	}
	if len(identities) == 0 {
		return nil
	}
	primary := selectBestPrimary(identities)
	if primary == nil {
		return nil
	}
	if !primary.IsPrimary {
		if err := tx.Scopes(scope).Model(&models.UserAuthIdentity{}).Where("user_subject_id = ?", userSubjectID).Update("is_primary", false).Error; err != nil {
			return err
		}
		if err := tx.Scopes(scope).Model(&models.UserAuthIdentity{}).Where("id = ?", primary.ID).Update("is_primary", true).Error; err != nil {
			return err
		}
	}

	// Compute new provider-tracking values from primary identity only.
	newAuthProvider := stringPtr(primary.Provider)
	newExternalKey := stringPtr(primary.ExternalKey)
	newProviderUserID := primary.ProviderUserID

	// Check if any field actually changed before writing.
	changed := !equalStringPtr(user.AuthProvider, newAuthProvider) ||
		!equalStringPtr(user.ExternalKey, newExternalKey) ||
		!equalStringPtr(user.ProviderUserID, newProviderUserID)

	if !changed {
		return nil
	}

	user.AuthProvider = newAuthProvider
	user.ExternalKey = newExternalKey
	user.ProviderUserID = newProviderUserID
	now := time.Now()
	user.LastSyncAt = &now
	// Omit columns with UNIQUE constraints (immutable after creation) — same
	// guard as server:1215. subject_id is the PK lookup key; username /
	// external_key have unique indexes that would conflict if Save tried to
	// re-write them with the same value under Postgres.
	if err := tx.Scopes(scope).Omit("subject_id", "username", "external_key").Save(&user).Error; err != nil {
		return err
	}
	return nil
}

// equalStringPtr is nil-safe *string equality. Used by the change-detection
// gate inside refreshUserProfileFromIdentitiesTx.
func equalStringPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
