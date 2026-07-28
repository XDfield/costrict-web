// JWKS wire types for /.well-known/jwks. Field names + ordering match the
// consumer at server/internal/middleware/jwks.go so the existing Casdoor
// fetcher can be repointed at cs-user without code changes.

package auth

// JWKS is the JSON Web Key Set shape exposed at /.well-known/jwks (RFC 7517).
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is a single JSON Web Key entry. Alg is RS256 (Phase A only).
type JWK struct {
	Kty string `json:"kty"` // key type: "RSA"
	Use string `json:"use"` // public key use: "sig"
	Kid string `json:"kid"` // key id: RFC 7638 thumbprint
	Alg string `json:"alg"` // algorithm: "RS256"
	N   string `json:"n"`   // RSA modulus, base64url-no-pad
	E   string `json:"e"`   // RSA exponent, base64url-no-pad
}
