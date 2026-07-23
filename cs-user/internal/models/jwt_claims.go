package models

// JWTClaims represents the parsed JWT payload from Casdoor, transmitted over
// the internal RPC surface. cs-user does NOT verify the JWT signature — it
// trusts the X-Internal-Token header on the request. Verification stays in
// server. Field set mirrors server/internal/user/service.go JWTClaims 1:1 so
// the wire format is identical regardless of which side serializes.
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
