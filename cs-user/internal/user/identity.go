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

// refreshUserProfileFromIdentitiesTx recomputes the user row's denormalized
// fields (auth_provider / external_key / display_name / email / phone / etc.)
// from the best-primary identity and writes back iff something actually
// changed. Server:1147 — the change detection avoids touching updated_at on
// repeat logins, which would mask real drift in ops dashboards.
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

	// Compute new values from primary identity.
	newAuthProvider := stringPtr(primary.Provider)
	newExternalKey := stringPtr(primary.ExternalKey)
	newProviderUserID := primary.ProviderUserID
	newDisplayName := firstNonNilStringPtr(primary.DisplayName, bestIdentityString(identities, func(i *models.UserAuthIdentity) *string { return i.DisplayName }))
	newAvatarURL := firstNonNilStringPtr(primary.AvatarURL, githubAvatar(identities), bestIdentityString(identities, func(i *models.UserAuthIdentity) *string { return i.AvatarURL }))
	newEmail := validEmailPtr(primary.Email, identities)
	newPhone := preferredPhonePtr(primary, identities)
	newOrganization := firstNonNilStringPtr(primary.Organization, bestIdentityString(identities, func(i *models.UserAuthIdentity) *string { return i.Organization }))
	var newUsername string
	if primaryUsername := firstNonEmptyString(ptrString(primary.ProviderUserID), ptrString(primary.DisplayName)); primaryUsername != "" {
		newUsername = primaryUsername
	}

	// Check if any field actually changed before writing.
	changed := !equalStringPtr(user.AuthProvider, newAuthProvider) ||
		!equalStringPtr(user.ExternalKey, newExternalKey) ||
		!equalStringPtr(user.ProviderUserID, newProviderUserID) ||
		!equalStringPtr(user.DisplayName, newDisplayName) ||
		!equalStringPtr(user.AvatarURL, newAvatarURL) ||
		!equalStringPtr(user.Email, newEmail) ||
		!equalStringPtr(user.Phone, newPhone) ||
		!equalStringPtr(user.Organization, newOrganization) ||
		(newUsername != "" && user.Username != newUsername)

	if !changed {
		return nil
	}

	user.AuthProvider = newAuthProvider
	user.ExternalKey = newExternalKey
	user.ProviderUserID = newProviderUserID
	user.DisplayName = newDisplayName
	user.AvatarURL = newAvatarURL
	user.Email = newEmail
	user.Phone = newPhone
	user.Organization = newOrganization
	if newUsername != "" {
		user.Username = newUsername
	}
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

// --- string-ptr helpers used by refreshUserProfileFromIdentitiesTx ---

// firstNonNilStringPtr returns the first non-nil, non-empty (after trim)
// pointer. The returned pointer is a fresh allocation holding the trimmed
// value — never aliases the input — so callers can mutate without surprise.
func firstNonNilStringPtr(values ...*string) *string {
	for _, v := range values {
		if v != nil && strings.TrimSpace(*v) != "" {
			trimmed := strings.TrimSpace(*v)
			return &trimmed
		}
	}
	return nil
}

// bestIdentityString picks the string from the highest-rank identity that has
// a non-empty value for `getter`. Used to fall back to secondary identities
// when the primary lacks a field (e.g. phone primary has no avatar → fall
// back to github's).
func bestIdentityString(identities []*models.UserAuthIdentity, getter func(*models.UserAuthIdentity) *string) *string {
	var best *models.UserAuthIdentity
	for _, identity := range identities {
		candidate := getter(identity)
		if candidate == nil || strings.TrimSpace(*candidate) == "" {
			continue
		}
		if best == nil || providerRank(identity.Provider) > providerRank(best.Provider) {
			best = identity
		}
	}
	if best == nil {
		return nil
	}
	return getter(best)
}

// githubAvatar returns the first github-identity avatar URL, ignoring empties.
// Github avatars are stable across the user's lifetime — preferred over
// casdoor's ephemeral one when both are present.
func githubAvatar(identities []*models.UserAuthIdentity) *string {
	for _, identity := range identities {
		if strings.EqualFold(identity.Provider, "github") && identity.AvatarURL != nil && strings.TrimSpace(*identity.AvatarURL) != "" {
			return identity.AvatarURL
		}
	}
	return nil
}

// validEmailPtr returns the primary's email if it contains "@", else the
// first identity with a valid email shape. Prevents junk like "alice" from
// being persisted when a provider sends a placeholder.
func validEmailPtr(primary *string, identities []*models.UserAuthIdentity) *string {
	if primary != nil && strings.Contains(strings.TrimSpace(*primary), "@") {
		return firstNonNilStringPtr(primary)
	}
	for _, identity := range identities {
		if identity.Email != nil && strings.Contains(strings.TrimSpace(*identity.Email), "@") {
			return firstNonNilStringPtr(identity.Email)
		}
	}
	return nil
}

// preferredPhonePtr returns the phone-identity's phone if present, else the
// primary's, else any identity's. Phone-identity rows own the canonical phone
// even when they aren't the primary (phone login is often secondary to a
// github primary).
func preferredPhonePtr(primary *models.UserAuthIdentity, identities []*models.UserAuthIdentity) *string {
	for _, identity := range identities {
		if strings.EqualFold(identity.Provider, "phone") && identity.Phone != nil && strings.TrimSpace(*identity.Phone) != "" {
			return firstNonNilStringPtr(identity.Phone)
		}
	}
	if primary != nil && primary.Phone != nil && strings.TrimSpace(*primary.Phone) != "" {
		return firstNonNilStringPtr(primary.Phone)
	}
	for _, identity := range identities {
		if identity.Phone != nil && strings.TrimSpace(*identity.Phone) != "" {
			return firstNonNilStringPtr(identity.Phone)
		}
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
