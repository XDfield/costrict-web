// Package auth owns cs-user's JWT signing primitive and JWKS exposure.
//
// Phase A3 scope: load an RSA private key from a PEM file (operator-managed
// via k8s secret / docker secret), expose the public key at
// /.well-known/jwks, and provide SignJWT for downstream callers (A7 OAuth
// callback takeover). Key ID (kid) is derived as the RFC 7638 JWK
// thumbprint of the public key — deterministic from the key, so rotation
// is purely "swap file + restart pod".
//
// Not in scope for A3: token TTL/issuer wiring (A5 claims extension),
// actual issuance paths (A7 OAuth callback takeover), refresh-token
// rotation (Phase B).
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
func NewSignerFromPEM(pemBytes []byte) (*Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("auth: signing key is not valid PEM")
	}
	var pk *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: parse PKCS#1 RSA key: %w", err)
		}
		pk = parsed
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: parse PKCS#8 key: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("auth: PKCS#8 key is not RSA")
		}
		pk = rsaKey
	default:
		return nil, fmt.Errorf("auth: unsupported PEM type %q (want RSA PRIVATE KEY or PRIVATE KEY)", block.Type)
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
// RS256 — Phase A deliberately scopes down to a single alg so audit surface
// stays small. The kid header is populated so relying parties can route via
// JWKS lookup.
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

// JWKS returns the JSON-serializable key set exposing the public key.
// Phase A: single key. Phase B may add a previous-key overlap window so
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
