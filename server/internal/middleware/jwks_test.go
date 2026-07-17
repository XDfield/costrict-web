package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- Phase A8: multi-source JWKSProvider ---

// jwksHandler returns an httptest.Handler that serves the given keys as a
// JWKS response. Each key is exposed with its kid and the RSA public key's
// n/e parameters (RFC 7517 wire shape).
func jwksHandler(t *testing.T, keys map[string]*rsa.PublicKey) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jwksResponse{}
		for kid, pub := range keys {
			resp.Keys = append(resp.Keys, jwkKey{
				Kty: "RSA",
				Use: "sig",
				Kid: kid,
				Alg: "RS256",
				N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				E:   base64.RawURLEncoding.EncodeToString(intToBytes(pub.E)),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func intToBytes(n int) []byte {
	b := big.NewInt(int64(n)).Bytes()
	if len(b) == 0 {
		return []byte{0}
	}
	return b
}

// TestNewMultiJWKSProvider_DedupesAndDropsEmpty verifies the constructor
// sanitizes input: empty entries dropped, duplicates collapsed (so passing
// the same cs-user URL twice doesn't double-fetch).
func TestNewMultiJWKSProvider_DedupesAndDropsEmpty(t *testing.T) {
	p := NewMultiJWKSProvider([]string{
		"http://cs-user:8080",
		"http://cs-user:8080 ", // duplicate after trim
		"  ",
		"",
		"http://casdoor:8000",
	})
	if got, want := len(p.sources), 2; got != want {
		t.Fatalf("len(sources) = %d, want %d (cs-user + casdoor after dedup)", got, want)
	}
}

// TestNewMultiJWKSProvider_AllEmptyYieldsNoSources verifies that a fully-empty
// input produces a zero-source provider (which then fails refresh with
// "no JWKS sources configured" rather than panicking).
func TestNewMultiJWKSProvider_AllEmptyYieldsNoSources(t *testing.T) {
	p := NewMultiJWKSProvider([]string{"", "  ", ""})
	if len(p.sources) != 0 {
		t.Fatalf("expected 0 sources, got %d", len(p.sources))
	}
}

// TestJWKSProvider_MultiSource_MergesKeysFromBothURLs covers the dual-sign
// happy path: a key from cs-user and a key from Casdoor both end up in the
// merged map, looked up by their respective kids.
func TestJWKSProvider_MultiSource_MergesKeysFromBothURLs(t *testing.T) {
	csUserKey := mustRSA(t)
	casdoorKey := mustRSA(t)

	csUser := httptest.NewServer(jwksHandler(t, map[string]*rsa.PublicKey{
		"cs-user-kid": &csUserKey.PublicKey,
	}))
	defer csUser.Close()

	casdoor := httptest.NewServer(jwksHandler(t, map[string]*rsa.PublicKey{
		"casdoor-kid": &casdoorKey.PublicKey,
	}))
	defer casdoor.Close()

	p := &JWKSProvider{
		sources:    []string{csUser.URL + "/.well-known/jwks", casdoor.URL + "/.well-known/jwks"},
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: csUser.Client(),
	}

	if err := p.refresh(); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}

	if got, err := p.GetKey("cs-user-kid"); err != nil || got == nil {
		t.Errorf("GetKey(cs-user-kid): got %v, err %v — cs-user key must be reachable", got, err)
	}
	if got, err := p.GetKey("casdoor-kid"); err != nil || got == nil {
		t.Errorf("GetKey(casdoor-kid): got %v, err %v — casdoor key must be reachable", got, err)
	}
}

// TestJWKSProvider_MultiSource_ToleratesOneSourceFailing covers the dual-sign
// resilience contract: when cs-user is briefly unavailable during 灰度, the
// Casdoor source still populates the cache so existing Casdoor sessions keep
// working. The failed source is logged but not fatal.
func TestJWKSProvider_MultiSource_ToleratesOneSourceFailing(t *testing.T) {
	casdoorKey := mustRSA(t)

	// cs-user returns 503 the whole time
	csUser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer csUser.Close()

	casdoor := httptest.NewServer(jwksHandler(t, map[string]*rsa.PublicKey{
		"casdoor-kid": &casdoorKey.PublicKey,
	}))
	defer casdoor.Close()

	p := &JWKSProvider{
		sources:    []string{csUser.URL + "/.well-known/jwks", casdoor.URL + "/.well-known/jwks"},
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: csUser.Client(),
	}

	if err := p.refresh(); err != nil {
		t.Fatalf("refresh should tolerate cs-user 503 (only Casdoor must succeed): %v", err)
	}
	if got, err := p.GetKey("casdoor-kid"); err != nil || got == nil {
		t.Errorf("GetKey(casdoor-kid) after partial refresh: got %v, err %v — Casdoor key must be reachable", got, err)
	}
	if _, err := p.GetKey("cs-user-kid"); err == nil {
		t.Errorf("GetKey(cs-user-kid) should fail since cs-user returned 503")
	}
}

// TestJWKSProvider_MultiSource_AllFailuresIsError verifies that if EVERY
// source fails, refresh returns an error rather than silently producing an
// empty key map (which would let every token through cache-miss + fetch-loop).
func TestJWKSProvider_MultiSource_AllFailuresIsError(t *testing.T) {
	dead1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead1.Close()
	dead2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer dead2.Close()

	p := &JWKSProvider{
		sources:    []string{dead1.URL + "/.well-known/jwks", dead2.URL + "/.well-known/jwks"},
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: dead1.Client(),
	}

	if err := p.refresh(); err == nil {
		t.Fatalf("refresh with all dead sources should return an error")
	}
}

// TestJWKSProvider_SingleSource_StillWorksAfterRefactor verifies that the
// existing single-source path (used by NewJWKSProvider in off / single modes)
// still works after the field rename from jwksURL to sources.
func TestJWKSProvider_SingleSource_StillWorksAfterRefactor(t *testing.T) {
	key := mustRSA(t)
	srv := httptest.NewServer(jwksHandler(t, map[string]*rsa.PublicKey{
		"single-kid": &key.PublicKey,
	}))
	defer srv.Close()

	p := &JWKSProvider{
		sources:    []string{srv.URL + "/.well-known/jwks"},
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: srv.Client(),
	}

	if err := p.refresh(); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if got, err := p.GetKey("single-kid"); err != nil || got == nil {
		t.Errorf("GetKey(single-kid): got %v, err %v", got, err)
	}
}

// TestJWKSProvider_NoSources_ReturnsError verifies that a zero-source
// provider (which only NewMultiJWKSProvider with all-empty input can produce)
// fails refresh cleanly rather than mutating state.
func TestJWKSProvider_NoSources_ReturnsError(t *testing.T) {
	p := &JWKSProvider{
		sources:    nil,
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: &http.Client{Timeout: 2 * time.Second},
	}
	if err := p.refresh(); err == nil {
		t.Fatalf("refresh with no sources should error")
	}
}

// TestNewJWKSProvider_AppendsJWKSPath verifies the suffix convention so that
// passing a base URL like "http://cs-user:8080" yields the right fetch URL
// (mirrors the pre-refactor behavior).
func TestNewJWKSProvider_AppendsJWKSPath(t *testing.T) {
	p := NewJWKSProvider("http://cs-user:8080")
	if got, want := len(p.sources), 1; got != want {
		t.Fatalf("len(sources) = %d, want %d", got, want)
	}
	if got, want := p.sources[0], "http://cs-user:8080/.well-known/jwks"; got != want {
		t.Errorf("source URL = %q, want %q", got, want)
	}
}

// mustRSA is a shared helper for the A8 jwks tests.
func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}
