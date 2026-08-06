// Package auth owns cs-user's JWT signing primitive and JWKS exposure.
//
// Scope: load an RSA private key from a PEM file (operator-managed via k8s
// secret / docker secret), expose the public key at /.well-known/jwks, and
// provide SignJWT for downstream callers (the OAuth-callback takeover path).
// Key ID (kid) is derived as the RFC 7638 JWK thumbprint of the public key —
// deterministic from the key, so rotation is purely "swap file + restart
// pod".
//
// Token TTL/issuer wiring, issuance paths, and refresh-token rotation are
// owned by the callers and downstream evolution of this package.
package auth

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrSignerDisabled is returned by VerifyJWT when the receiver is nil or has
// no private key loaded — handlers surface this as 503 (config missing),
// mirroring the Signer-not-configured branch of the JWKS handler.
var ErrSignerDisabled = errors.New("auth: signer not configured")

// Signer is the cs-user JWT signing primitive. Construct once at startup;
// share across handlers. The RSA private key never leaves the struct.
type Signer struct {
	privateKey *rsa.PrivateKey
	kid        string
}

// NewSignerFromPEMPath reads a PEM file from disk. The path must be supplied
// by the operator via CS_USER_JWT_SIGNING_KEY_PATH — typically a k8s secret
// mount. Returns a descriptive error if the path is empty, the file is
// unreadable, or the PEM is malformed.
func NewSignerFromPEMPath(path string) (*Signer, error) {
	if path == "" {
		return nil, errors.New("auth: empty signing key path")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read signing key %s: %w", path, err)
	}
	return NewSignerFromPEM(pemBytes)
}

// NewSignerFromPEM constructs a Signer from in-memory PEM bytes. Test seam
// — production paths should use NewSignerFromPEMPath.
//
// Accepts both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") PEM
// blocks; the latter is what `openssl genpkey -algorithm RSA` produces and
// what most k8s secret scaffolding emits.
//
// The PEM block.Type header is treated as a HINT, not a contract: if the
// header says "RSA PRIVATE KEY" but the underlying ASN.1 is actually PKCS#8
// (seen in practice with some secret tooling and OpenSSL 3.x edge cases),
// we fall back to the PKCS#8 parser. Same in reverse.
func NewSignerFromPEM(pemBytes []byte) (*Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("auth: signing key is not valid PEM")
	}

	parseAsRSA := func(b []byte) (*rsa.PrivateKey, error) {
		// Try PKCS#1 first (traditional RSA PRIVATE KEY).
		if k, err := x509.ParsePKCS1PrivateKey(b); err == nil {
			return k, nil
		}
		// Fall back to PKCS#8 — many tools emit PKCS#8 bytes under a
		// "RSA PRIVATE KEY" header, which Go's strict PKCS#1 parser
		// rejects with "use ParsePKCS8PrivateKey instead".
		k, err := x509.ParsePKCS8PrivateKey(b)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("PKCS#8 key is not RSA")
		}
		return rsaKey, nil
	}

	var pk *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY", "PRIVATE KEY":
		parsed, err := parseAsRSA(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: parse RSA key (header=%q): %w", block.Type, err)
		}
		pk = parsed
	default:
		// Last-ditch: try both parsers regardless of header.
		parsed, err := parseAsRSA(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: unsupported PEM type %q (want RSA PRIVATE KEY or PRIVATE KEY): %w", block.Type, err)
		}
		pk = parsed
	}
	return &Signer{
		privateKey: pk,
		kid:        kidFor(pk.PublicKey),
	}, nil
}

// KID returns the key id used in JWT headers and JWKS entries. Stable for a
// given key — operators can grep JWKS for it after rotation.
func (s *Signer) KID() string { return s.kid }

// SignJWT issues a signed JWT string for the given claims. alg is fixed to
// RS256 — scoping down to a single alg keeps the audit surface small. The
// kid header is populated so relying parties can route via JWKS lookup.
//
// `now` is taken as a parameter (not read from time.Now internally) so tests
// can pin issuance time; production callers pass time.Now().
func (s *Signer) SignJWT(claims jwt.Claims, _ time.Time) (string, error) {
	if s == nil || s.privateKey == nil {
		return "", errors.New("auth: signer not configured")
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("auth: sign JWT: %w", err)
	}
	return signed, nil
}

// VerifyJWT validates a cs-user-issued JWT against the signer's own public
// key. Used by the gateway-facing /api/internal/auth/verify endpoint to
// authenticate "new" (post-A7 takeover) tokens without round-tripping
// through JWKS — the in-process public key is the same one exposed at
// /.well-known/jwks, but local verification saves an HTTP hop and is
// independent of JWKS cache TTL.
//
// issuer, when non-empty, is enforced via jwt.WithIssuer — typically the
// caller passes cfg.JWT.Issuer so tokens from a different iss are rejected
// even if the signature happens to validate (defence-in-depth against
// cross-issuer confusion if a key is ever shared).
//
// Returns the parsed EnterpriseClaims on success. A nil receiver / missing
// key returns ErrSignerDisabled (handler maps to 503); any verification
// failure (bad signature, expired, wrong alg, malformed, iss mismatch)
// returns the underlying jwt/v5 error so callers can fall through to a
// legacy-verifier path.
func (s *Signer) VerifyJWT(rawJWT, issuer string) (*EnterpriseClaims, error) {
	if s == nil || s.privateKey == nil {
		return nil, ErrSignerDisabled
	}
	if rawJWT == "" {
		return nil, errors.New("empty token")
	}
	claims := &EnterpriseClaims{}
	pub := &s.privateKey.PublicKey
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(10 * time.Second),
	}
	if issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(issuer))
	}
	_, err := jwt.ParseWithClaims(rawJWT, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected alg %v", t.Header["alg"])
		}
		// kid is informational only — cs-user has a single key, so we
		// always verify against it regardless of the kid header value.
		// A mismatched kid on a valid signature is not a security event.
		return pub, nil
	}, parserOpts...)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// JWKS returns the JSON-serializable key set exposing the public key.
// Single key. A future evolution may add a previous-key overlap window so
// in-flight tokens remain valid through rotation.
func (s *Signer) JWKS() JWKS {
	if s == nil {
		return JWKS{Keys: []JWK{}}
	}
	pub := &s.privateKey.PublicKey
	return JWKS{
		Keys: []JWK{{
			Kty: "RSA",
			Use: "sig",
			Kid: s.kid,
			Alg: "RS256",
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
}

// kidFor computes the RFC 7638 JWK thumbprint (SHA-256 over the canonical
// JSON `{"e":"...","kty":"RSA","n":"..."}`, base64url-no-pad) of the RSA
// public key. Same key always yields the same kid, so rotation needs no
// separate kid config — the new key gets a new kid naturally, and relying
// parties refresh via JWKS.
func kidFor(pub rsa.PublicKey) string {
	// RFC 7638 §3.2: lexicographic order e, kty, n. No whitespace.
	eStr := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	nStr := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	canonical := `{"e":"` + eStr + `","kty":"RSA","n":"` + nStr + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
