package models

// JWTClaims is cs-user's internal representation of a parsed + verified
// Casdoor JWT payload. cs-user IS the identity-trust boundary:
// CasdoorVerifier.Verify / Signer.VerifyJWT produce this type, and the
// issuance + employment-mapping pipelines consume it. Server no longer parses
// JWT payload itself, so this type is NOT a wire-type mirror of server's
// user.JWTClaims — both types just happen to share a JSON shape because they
// describe the same Casdoor claim set. Per JWT_VERIFY_CLEANUP_PLAN Q4 the
// symmetric decision applies: server's user.JWTClaims stays as server's
// internal context representation; cs-user's models.JWTClaims stays as cs-user's
// internal claims representation.
//
// Wire-type role on cs-user side (post-cleanup):
//   - New identity-authority RPCs (ReissueToken, ParseIdentity) take raw
//     `casdoor_jwt` strings and produce this type internally via CasdoorVerifier.
//   - Legacy upsert / bind / suggest RPCs (GetOrCreate, BindIdentity,
//     SuggestProfile) still accept a claims-shape JSON body — these RPCs
//     receive identity data the server already holds (from ParseIdentity's
//     response Profile, or from /api/userinfo), not a raw JWT to forward.
//     Migrating them to casdoor_jwt input is plan §2's terminal vision but is
//     out of Phase 6.1 scope.
//
// Field names use JSON snake_case (matching Casdoor's token format) via the
// json tags; Go identifiers stay PascalCase per convention.
type JWTClaims struct {
	ID                string `json:"id,omitempty"`
	Sub               string `json:"sub,omitempty"`
	UniversalID       string `json:"universal_id,omitempty"`
	Name              string `json:"name,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	Email             string `json:"email,omitempty"`
	Picture           string `json:"picture,omitempty"`
	Owner             string `json:"owner,omitempty"`
	Provider          string `json:"provider,omitempty"`
	ProviderUserID    string `json:"provider_user_id,omitempty"`
	Phone             string `json:"phone,omitempty"`
	// ExternalClaims carries the raw IdP userinfo fields (e.g. wxwork's
	// UserId/JobNumber, azure_ad's department) harvested by server's OAuth
	// callback. Consumed by ApplyEnterpriseMapping via the tenant's
	// employment_providers.field_map config to populate employment_identities
	// enterprise columns. Empty/nil → stub write path (enterprise fields stay
	// NULL), so legacy callers without enterprise mapping keep working.
	ExternalClaims map[string]any `json:"external_claims,omitempty"`
}

// BindIdentityOptions tunes BindIdentityToUser behavior. ForceRebind overrides
// an ExplicitlyUnbound marker on a prior identity — used when the user
// explicitly re-grants a provider they previously unbound.
type BindIdentityOptions struct {
	ForceRebind bool `json:"force_rebind,omitempty"`
}
