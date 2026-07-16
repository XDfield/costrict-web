// Package user — claims.go ports the JWTClaims normalization + external-key
// derivation helpers from server/internal/user/service.go verbatim.
//
// cs-user does NOT verify the JWT signature — it trusts the X-Internal-Token
// middleware (cs-user/internal/middleware/internal_auth.go). These helpers
// operate purely on the parsed claim payload arriving over the internal RPC
// surface. The wire shape mirrors server's JWTClaims 1:1 (see
// cs-user/internal/models/jwt_claims.go), so a faithful port produces
// byte-identical external keys on both sides — load-bearing for the P0-8b
// dual-write canary, where a key mismatch would split a user's identities
// across DBs.
package user

import (
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// normalizeJWTClaims backfills missing Name / PreferredUsername fields so the
// downstream writer always has at least one human-readable handle to persist.
// Server mutates the claim in place — we do the same to preserve the contract.
func normalizeJWTClaims(claims *models.JWTClaims) *models.JWTClaims {
	if claims == nil {
		return nil
	}
	if claims.PreferredUsername == "" {
		claims.PreferredUsername = claims.Name
	}
	if claims.Name == "" && claims.PreferredUsername != "" {
		claims.Name = claims.PreferredUsername
	}
	if claims.Name == "" {
		if claims.Phone != "" {
			claims.Name = "phone_" + claims.Phone
		} else if claims.ProviderUserID != "" {
			claims.Name = claims.ProviderUserID
		}
	}
	return claims
}

// buildExternalKey derives the durable identity handle from a claim set. The
// format MUST stay byte-identical with server's buildExternalKey (server:1029)
// — provider-prefixed casdoor:<provider>:<universal_id> when the universal id
// is present, falling back to sub / id-only forms. RPCWriter (P0-8b) and
// server's local writer must produce the same key for the same claim or
// bind/transfer lookups will silently miss across DBs.
func buildExternalKey(claims *models.JWTClaims) string {
	if claims == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(claims.Provider))
	if claims.UniversalID != "" {
		if provider != "" {
			return "casdoor:" + provider + ":" + claims.UniversalID
		}
		return "casdoor:" + claims.UniversalID
	}
	if claims.Sub != "" {
		if provider != "" {
			return "casdoor-sub:" + provider + ":" + claims.Sub
		}
		return "casdoor-sub:" + claims.Sub
	}
	if claims.ID != "" {
		return "casdoor-id:" + claims.ID
	}
	return ""
}

// BuildExternalKey is the exported wrapper for tests / RPCWriter consumers
// that need to derive a key without invoking the full write path.
func BuildExternalKey(claims *models.JWTClaims) string {
	return buildExternalKey(claims)
}

// legacyExternalKey matches the pre-provider-keyed format used by historical
// rows in production. Used during the identity transfer lookup chain so an
// identity bound before the provider prefix shipped can still be matched by
// universal_id alone.
func legacyExternalKey(claims *models.JWTClaims) string {
	if claims == nil {
		return ""
	}
	if claims.UniversalID != "" {
		return "casdoor:" + claims.UniversalID
	}
	if claims.Sub != "" {
		return "casdoor-sub:" + claims.Sub
	}
	return ""
}

// buildUserAuthIdentity constructs a UserAuthIdentity row from a claim set,
// ready for Create within a bind/get-or-create tx. Mirrors server:1070 line
// for line; LastLoginAt is stamped at row construction time, matching the
// server's invariant (an identity's first write is also its first login).
func buildUserAuthIdentity(userSubjectID string, claims *models.JWTClaims) models.UserAuthIdentity {
	now := time.Now()
	externalKey := buildExternalKey(claims)
	provider := strings.ToLower(strings.TrimSpace(claims.Provider))
	if provider == "" {
		provider = "casdoor"
	}
	return models.UserAuthIdentity{
		UserSubjectID:   userSubjectID,
		Provider:        provider,
		ExternalKey:     externalKey,
		ExternalSubject: stringPtr(firstNonEmptyString(claims.UniversalID, claims.Sub)),
		ExternalUserID:  stringPtr(claims.ID),
		ProviderUserID:  stringPtr(claims.ProviderUserID),
		DisplayName:     stringPtr(claims.PreferredUsername),
		Email:           stringPtr(claims.Email),
		Phone:           stringPtr(claims.Phone),
		AvatarURL:       stringPtr(claims.Picture),
		Organization:    stringPtr(claims.Owner),
		LastLoginAt:     &now,
	}
}

// buildIdentityUpdates returns the column→value map that should be applied to
// an existing identity row when the same claim re-binds with refreshed data.
// Only non-empty claim fields trigger an update, and only when they differ
// from the persisted value — keeps the write set minimal on repeat logins.
func buildIdentityUpdates(existing *models.UserAuthIdentity, claims *models.JWTClaims) map[string]interface{} {
	updates := make(map[string]interface{})

	if claims.PreferredUsername != "" && (existing.DisplayName == nil || *existing.DisplayName != claims.PreferredUsername) {
		updates["display_name"] = claims.PreferredUsername
	}
	if claims.Email != "" && (existing.Email == nil || *existing.Email != claims.Email) {
		updates["email"] = claims.Email
	}
	if claims.Phone != "" && (existing.Phone == nil || *existing.Phone != claims.Phone) {
		updates["phone"] = claims.Phone
	}
	if claims.Picture != "" && (existing.AvatarURL == nil || *existing.AvatarURL != claims.Picture) {
		updates["avatar_url"] = claims.Picture
	}
	if claims.Owner != "" && (existing.Organization == nil || *existing.Organization != claims.Owner) {
		updates["organization"] = claims.Owner
	}
	if claims.ProviderUserID != "" && (existing.ProviderUserID == nil || *existing.ProviderUserID != claims.ProviderUserID) {
		updates["provider_user_id"] = claims.ProviderUserID
	}
	if claims.UniversalID != "" && (existing.ExternalSubject == nil || *existing.ExternalSubject != claims.UniversalID) {
		updates["external_subject"] = claims.UniversalID
	}

	return updates
}

// --- string helpers (shared between claims.go and identity.go) ---

// firstNonEmptyString returns the first trimmed-non-empty value. Used to
// collapse universal_id-then-sub fallback chains into a single column.
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ptrString safely dereferences a *string, returning "" for nil. The trim
// matches server's behaviour — leading/trailing whitespace on stored values
// is treated as missing for comparison purposes.
func ptrString(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// stringPtr returns a nil pointer for the empty string. Keeps the column
// nullable instead of polluting it with "" — load-bearing for unique indexes
// that need to permit multiple rows with no value.
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
