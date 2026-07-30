package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// generateTestRSAKey makes a 2048-bit RSA key for the test Casdoor. Each
// test uses its own key — no global state.
func generateTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

// jwkFromPrivateKey builds the JWK wire shape the test JWKS stub returns.
// Mirrors signer.go's public-key → JWK logic but lives here so the test is
// self-contained.
func jwkFromPrivateKey(t *testing.T, key *rsa.PublicKey, kid string) JWK {
	t.Helper()
	nBytes := key.N.Bytes()
	eBytes := []byte{byte(key.E >> 24), byte(key.E >> 16), byte(key.E >> 8), byte(key.E)}
	// Trim leading zero bytes from e per JWK spec (minimal big-endian).
	for len(eBytes) > 0 && eBytes[0] == 0 {
		eBytes = eBytes[1:]
	}
	return JWK{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(nBytes),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}
}

// signTestCasdoorJWT signs a JWT with the test key — this is what Casdoor
// would emit. Claims are arbitrary; tests add exp/iss/aud as needed.
func signTestCasdoorJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return signed
}

func TestCasdoorVerifier_NilReceiverDisabled(t *testing.T) {
	var v *CasdoorVerifier
	_, err := v.Verify(context.Background(), "anything")
	if !errors.Is(err, ErrCasdoorVerifierDisabled) {
		t.Fatalf("nil receiver: want ErrCasdoorVerifierDisabled, got %v", err)
	}
}

func TestCasdoorVerifier_EmptyJWKSURLReturnsNil(t *testing.T) {
	if v := NewCasdoorVerifier("   ", "", nil, 0, 0); v != nil {
		t.Fatalf("empty JWKSURL: want nil verifier, got %+v", v)
	}
}

// TestCasdoorVerifier_VerifyHappyPath covers the standard success path:
// JWKS serves the right key, JWT signed with matching private key, exp in
// the future, iss matches when configured.
func TestCasdoorVerifier_VerifyHappyPath(t *testing.T) {
	key := generateTestRSAKey(t)
	const kid = "test-kid-1"
	jwk := jwkFromPrivateKey(t, &key.PublicKey, kid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwk}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "http://casdoor:8000", nil, time.Second, time.Minute)
	raw := signTestCasdoorJWT(t, key, kid, jwt.MapClaims{
		"sub":                "uni-1",
		"universal_id":       "uni-1",
		"name":               "Alice",
		"preferred_username": "alice",
		"iss":                "http://casdoor:8000",
		"exp":                time.Now().Add(time.Hour).Unix(),
	})

	claims, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: unexpected err: %v", err)
	}
	if claims.Sub != "uni-1" {
		t.Errorf("Sub: got %q, want uni-1", claims.Sub)
	}
	if claims.Name != "Alice" {
		t.Errorf("Name: got %q, want Alice", claims.Name)
	}
	if claims.PreferredUsername != "alice" {
		t.Errorf("PreferredUsername: got %q, want alice", claims.PreferredUsername)
	}
}

// TestCasdoorVerifier_BadSignature ensures a JWT signed by a different key
// than the JWKS serves fails verification. This is the core security check.
func TestCasdoorVerifier_BadSignature(t *testing.T) {
	jwksKey := generateTestRSAKey(t)
	signerKey := generateTestRSAKey(t) // different key
	const kid = "test-kid-1"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwk := jwkFromPrivateKey(t, &jwksKey.PublicKey, kid)
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwk}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "", nil, time.Second, time.Minute)
	raw := signTestCasdoorJWT(t, signerKey, kid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, ErrCasdoorJWTInvalid) {
		t.Fatalf("bad signature: want ErrCasdoorJWTInvalid, got %v", err)
	}
}

// TestCasdoorVerifier_Expired asserts exp enforcement — the most common
// real-world failure.
func TestCasdoorVerifier_Expired(t *testing.T) {
	key := generateTestRSAKey(t)
	const kid = "test-kid-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwkFromPrivateKey(t, &key.PublicKey, kid)}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "", nil, time.Second, time.Minute)
	raw := signTestCasdoorJWT(t, key, kid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(-time.Hour).Unix(),
	})

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, ErrCasdoorJWTInvalid) {
		t.Fatalf("expired: want ErrCasdoorJWTInvalid, got %v", err)
	}
}

// TestCasdoorVerifier_WrongIssuer locks the optional iss check. A token
// from the wrong IdP (or a misconfigured Casdoor pointing at a different
// issuer) must fail even if signed correctly.
func TestCasdoorVerifier_WrongIssuer(t *testing.T) {
	key := generateTestRSAKey(t)
	const kid = "test-kid-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwkFromPrivateKey(t, &key.PublicKey, kid)}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "http://casdoor:8000", nil, time.Second, time.Minute)
	raw := signTestCasdoorJWT(t, key, kid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
		"iss": "http://evil.example.com",
	})

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, ErrCasdoorJWTInvalid) {
		t.Fatalf("wrong iss: want ErrCasdoorJWTInvalid, got %v", err)
	}
}

// TestCasdoorVerifier_WrongAudience locks the optional aud check.
func TestCasdoorVerifier_WrongAudience(t *testing.T) {
	key := generateTestRSAKey(t)
	const kid = "test-kid-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwkFromPrivateKey(t, &key.PublicKey, kid)}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "", []string{"expected-aud"}, time.Second, time.Minute)
	raw := signTestCasdoorJWT(t, key, kid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
		"aud": "wrong-aud",
	})

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, ErrCasdoorJWTInvalid) {
		t.Fatalf("wrong aud: want ErrCasdoorJWTInvalid, got %v", err)
	}
}

// TestCasdoorVerifier_UnknownKidTriggersRefresh exercises the rotation path:
// the cache is primed with an OLD key (cold start), then a request with a
// NEW kid arrives. The verifier should refresh once and succeed when the
// second JWKS fetch returns the new key. The verifier MUST NOT loop refresh
// on a persistently-unknown kid (DoS vector), so a single refresh per
// Verify call is the contract.
func TestCasdoorVerifier_UnknownKidTriggersRefresh(t *testing.T) {
	oldKey := generateTestRSAKey(t)
	newKey := generateTestRSAKey(t)
	const oldKid = "old-kid"
	const newKid = "new-kid"

	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		if fetches == 1 {
			// Cold start: only the old key is published.
			_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwkFromPrivateKey(t, &oldKey.PublicKey, oldKid)}})
			return
		}
		// After rotation: both keys appear (Casdoor typically keeps the
		// retiring key for a grace period).
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{
			jwkFromPrivateKey(t, &oldKey.PublicKey, oldKid),
			jwkFromPrivateKey(t, &newKey.PublicKey, newKid),
		}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "", nil, time.Second, time.Minute)

	// Prime the cache with the old key (fetch 1).
	primeRaw := signTestCasdoorJWT(t, oldKey, oldKid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(context.Background(), primeRaw); err != nil {
		t.Fatalf("prime Verify: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("after prime: fetches=%d, want 1", fetches)
	}

	// New request with the rotated kid — must trigger refresh (fetch 2).
	rotatedRaw := signTestCasdoorJWT(t, newKey, newKid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(context.Background(), rotatedRaw); err != nil {
		t.Fatalf("rotated Verify: %v", err)
	}
	if fetches != 2 {
		t.Errorf("after rotation: fetches=%d, want 2 (prime + rotation refresh)", fetches)
	}
}

// TestCasdoorVerifier_PersistentlyUnknownKidDoesNotLoopRefresh locks the
// DoS-defense contract: an attacker sending a bogus kid must NOT cause
// repeated JWKS fetches. One refresh per Verify call, then fail.
func TestCasdoorVerifier_PersistentlyUnknownKidDoesNotLoopRefresh(t *testing.T) {
	key := generateTestRSAKey(t)
	const realKid = "real-kid"
	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(JWKS{Keys: []JWK{jwkFromPrivateKey(t, &key.PublicKey, realKid)}})
	}))
	defer srv.Close()

	v := NewCasdoorVerifier(srv.URL, "", nil, time.Second, time.Minute)
	// JWT signed by the right key but advertising a bogus kid in header —
	// signature would verify against realKid's key but kid lookup drives
	// the key choice, so verification fails on the missing kid. Refresh
	// fires once, second lookup still misses, fail. Total fetches = 1
	// (cold cache) + 1 (refresh) = 2 — but no more.
	raw := signTestCasdoorJWT(t, key, "bogus-kid", jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, ErrCasdoorJWTInvalid) {
		t.Fatalf("bogus kid: want ErrCasdoorJWTInvalid, got %v", err)
	}
	if fetches > 2 {
		t.Errorf("fetches=%d, want ≤2 (no loop on bogus kid)", fetches)
	}
}

// TestJwkToRSAPublicKey_RoundTrip sanity-checks the n/e decoder by feeding
// it a known key and verifying the reconstruction matches.
func TestJwkToRSAPublicKey_RoundTrip(t *testing.T) {
	key := generateTestRSAKey(t)
	jwk := jwkFromPrivateKey(t, &key.PublicKey, "test")
	pub, err := jwkToRSAPublicKey(jwk)
	if err != nil {
		t.Fatalf("jwkToRSAPublicKey: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 {
		t.Errorf("modulus mismatch")
	}
	if pub.E != key.PublicKey.E {
		t.Errorf("exponent: got %d, want %d", pub.E, key.PublicKey.E)
	}
}
