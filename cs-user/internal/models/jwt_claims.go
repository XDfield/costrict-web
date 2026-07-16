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
}

// BindIdentityOptions tunes BindIdentityToUser behavior. ForceRebind overrides
// an ExplicitlyUnbound marker on a prior identity — used when the user
// explicitly re-grants a provider they previously unbound.
type BindIdentityOptions struct {
	ForceRebind bool `json:"force_rebind,omitempty"`
}
