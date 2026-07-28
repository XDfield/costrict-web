// Signer tests: PEM loading (PKCS#1 + PKCS#8), KID determinism (RFC 7638),
// SignJWT roundtrip with public-key verification, JWKS shape.

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// newTestRSAKey generates a deterministic-enough RSA key for tests. 2048-bit
// is the production floor; tests use 2048 (not smaller) so RFC 7638 thumbprint
// math runs the same code path as real keys.
func newTestRSAKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	pk, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return pk
}

// encodePKCS1PEM and encodePKCS8PEM emit the two PEM shapes NewSignerFromPEM
// must accept. Useful for fixture generation without checked-in key files.
func encodePKCS1PEM(t *testing.T, pk *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(pk),
	})
}

func encodePKCS8PEM(t *testing.T, pk *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})
}

func TestNewSignerFromPEM_PKCS1(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	s, err := NewSignerFromPEM(encodePKCS1PEM(t, pk))
	if err != nil {
		t.Fatalf("NewSignerFromPEM PKCS#1: %v", err)
	}
	if s.KID() == "" {
		t.Error("KID should not be empty")
	}
}

func TestNewSignerFromPEM_PKCS8(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	s, err := NewSignerFromPEM(encodePKCS8PEM(t, pk))
	if err != nil {
		t.Fatalf("NewSignerFromPEM PKCS#8: %v", err)
	}
	if s.KID() == "" {
		t.Error("KID should not be empty")
	}
}

func TestNewSignerFromPEM_MalformedBytes(t *testing.T) {
	cases := []struct {
		name string
		pem  []byte
		want string
	}{
		{"empty", []byte(""), "not valid PEM"},
		{"not pem", []byte("just a string"), "not valid PEM"},
		// Valid PEM envelope but garbage DER inside — exercises the x509 parse
		// path, not the pem.Decode nil path. "bm9zZQ==" is base64 of "nose".
		{"valid-pem-bad-der", []byte("-----BEGIN PRIVATE KEY-----\nbm9zZQ==\n-----END PRIVATE KEY-----"), "parse"},
		{"wrong pem type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), "unsupported PEM type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSignerFromPEM(c.pem)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestNewSignerFromPEMPath(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, encodePKCS8PEM(t, pk), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s, err := NewSignerFromPEMPath(path)
	if err != nil {
		t.Fatalf("NewSignerFromPEMPath: %v", err)
	}
	if s.KID() == "" {
		t.Error("KID should not be empty")
	}
}

func TestNewSignerFromPEMPath_EmptyPath(t *testing.T) {
	_, err := NewSignerFromPEMPath("")
	if err == nil || !strings.Contains(err.Error(), "empty signing key path") {
		t.Errorf("expected empty-path error, got %v", err)
	}
}

func TestNewSignerFromPEMPath_MissingFile(t *testing.T) {
	_, err := NewSignerFromPEMPath(filepath.Join(t.TempDir(), "does-not-exist.pem"))
	if err == nil || !strings.Contains(err.Error(), "read signing key") {
		t.Errorf("expected read error, got %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist in chain, got %v", err)
	}
}

// TestKID_DeterministicAndDistinct verifies the RFC 7638 thumbprint property:
// same key → same kid; different keys → different kids.
func TestKID_DeterministicAndDistinct(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	s1, err := NewSignerFromPEM(encodePKCS8PEM(t, pk))
	if err != nil {
		t.Fatalf("first signer: %v", err)
	}
	s2, err := NewSignerFromPEM(encodePKCS8PEM(t, pk))
	if err != nil {
		t.Fatalf("second signer (same key): %v", err)
	}
	if s1.KID() != s2.KID() {
		t.Errorf("same key produced different kids: %s vs %s", s1.KID(), s2.KID())
	}

	other := newTestRSAKey(t, 2048)
	s3, err := NewSignerFromPEM(encodePKCS8PEM(t, other))
	if err != nil {
		t.Fatalf("third signer (different key): %v", err)
	}
	if s1.KID() == s3.KID() {
		t.Errorf("different keys produced same kid: %s", s1.KID())
	}
}

// TestKID_CrossValidatesWithIndependentRFC7638Impl verifies our kid derivation
// against a tiny inline RFC 7638 implementation using only stdlib. The inline
// version builds the canonical JSON `{"e":"...","kty":"RSA","n":"..."}` via
// json.Marshal (sorted map keys → lexicographic order, matching the RFC) and
// hashes with sha256. If our hand-rolled string concat in kidFor drifts from
// RFC (wrong field order, extra whitespace, wrong quoting), this test catches
// it without depending on a brittle hardcoded reference vector.
func TestKID_CrossValidatesWithIndependentRFC7638Impl(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	got := kidFor(pk.PublicKey)

	eStr := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pk.PublicKey.E)).Bytes())
	nStr := base64.RawURLEncoding.EncodeToString(pk.PublicKey.N.Bytes())
	// json.Marshal on a map[string]string emits keys in sorted order — which
	// for {e, kty, n} is exactly the RFC 7638 §3.2 lexicographic order.
	canonical, err := json.Marshal(map[string]string{
		"e":   eStr,
		"kty": "RSA",
		"n":   nStr,
	})
	if err != nil {
		t.Fatalf("marshal canonical JWK: %v", err)
	}
	sum := sha256.Sum256(canonical)
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got != want {
		t.Errorf("kidFor %q != independent RFC 7638 thumbprint %q", got, want)
	}
}

// TestSignJWT_RoundtripWithPublicKey verifies SignJWT produces a token the
// standard parser verifies with the corresponding public key — i.e. the JWS
// is correct, not just structurally well-formed. Also asserts kid header +
// RS256 alg.
func TestSignJWT_RoundtripWithPublicKey(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	s, err := NewSignerFromPEM(encodePKCS8PEM(t, pk))
	if err != nil {
		t.Fatalf("NewSignerFromPEM: %v", err)
	}

	claims := jwt.MapClaims{
		"sub": "usr_alice",
		"iss": "cs-user",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	signed, err := s.SignJWT(claims, time.Now())
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	if !strings.Contains(signed, ".") || strings.Count(signed, ".") != 2 {
		t.Errorf("signed token should have 3 dot-separated parts, got %q", signed)
	}

	// Parse + verify with the public key.
	parsed, err := jwt.Parse(signed, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			t.Fatalf("unexpected signing method: %v", tok.Header["alg"])
		}
		return &pk.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("verify signed token: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("parsed token should be valid")
	}
	if kid, _ := parsed.Header["kid"].(string); kid != s.KID() {
		t.Errorf("kid header: got %q, want %q", kid, s.KID())
	}
	if alg, _ := parsed.Header["alg"].(string); alg != "RS256" {
		t.Errorf("alg header: got %q, want RS256", alg)
	}
}

func TestSignJWT_NilSigner(t *testing.T) {
	var s *Signer
	_, err := s.SignJWT(jwt.MapClaims{}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "signer not configured") {
		t.Errorf("expected nil-signer error, got %v", err)
	}
}

// TestJWKS_Shape verifies the JWKS JSON shape matches what
// server/internal/middleware/jwks.go expects (kty/use/kid/alg/n/e fields,
// base64url-no-pad encoding, single key in Phase A).
func TestJWKS_Shape(t *testing.T) {
	pk := newTestRSAKey(t, 2048)
	s, err := NewSignerFromPEM(encodePKCS8PEM(t, pk))
	if err != nil {
		t.Fatalf("NewSignerFromPEM: %v", err)
	}

	jwks := s.JWKS()
	if len(jwks.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(jwks.Keys))
	}
	k := jwks.Keys[0]
	if k.Kty != "RSA" {
		t.Errorf("Kty: got %q, want RSA", k.Kty)
	}
	if k.Use != "sig" {
		t.Errorf("Use: got %q, want sig", k.Use)
	}
	if k.Alg != "RS256" {
		t.Errorf("Alg: got %q, want RS256", k.Alg)
	}
	if k.Kid != s.KID() {
		t.Errorf("Kid: JWKS %q != signer KID() %q", k.Kid, s.KID())
	}

	// n and e must round-trip to the original public key.
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	gotN := new(big.Int).SetBytes(nBytes)
	if gotN.Cmp(pk.PublicKey.N) != 0 {
		t.Error("modulus mismatch between JWKS and source key")
	}
	exp := 0
	for _, b := range eBytes {
		exp = exp<<8 | int(b)
	}
	if exp != pk.PublicKey.E {
		t.Errorf("exponent: got %d, want %d", exp, pk.PublicKey.E)
	}
}

// TestJWKS_NilSignerReturnsEmptyKeyset verifies the nil-receiver path keeps
// the handler's 503 logic honest (no nil-deref).
func TestJWKS_NilSignerReturnsEmptyKeyset(t *testing.T) {
	var s *Signer
	jwks := s.JWKS()
	if len(jwks.Keys) != 0 {
		t.Errorf("nil signer should return empty keyset, got %d keys", len(jwks.Keys))
	}
}
