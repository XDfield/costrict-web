package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func init() {
	gin.SetMode(gin.TestMode)
}

func TestMain(m *testing.M) {
	SetSubjectResolver(nil)
	m.Run()
}

// generateTestRSAKey generates a 2048-bit RSA key pair for testing.
func generateTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

// signTestJWT creates an RS256-signed JWT with the given claims and key ID.
func signTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	tokenStr, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return tokenStr
}

// newTestJWKSProvider creates a JWKSProvider with pre-cached keys (no HTTP needed).
func newTestJWKSProvider(keys map[string]*rsa.PublicKey) *JWKSProvider {
	return &JWKSProvider{
		jwksURL:    "http://localhost:0/.well-known/jwks", // won't be called
		keys:       keys,
		minRefresh: 5 * time.Minute,
		lastFetch:  time.Now(), // mark as recently fetched so refresh is skipped
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// performRequest executes a handler with the given request and returns the recorder.
func performRequest(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// rsaPubKeyToJWKParams encodes an RSA public key to base64url n and e values for JWK.
func rsaPubKeyToJWKParams(pub *rsa.PublicKey) (n, e string) {
	n = base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	eBig := big.NewInt(int64(pub.E))
	e = base64.RawURLEncoding.EncodeToString(eBig.Bytes())
	return
}

// ===========================================================================
// 1. InternalAuth middleware
// ===========================================================================

func TestInternalAuth_EmptySecretRejectsAll(t *testing.T) {
	router := gin.New()
	router.Use(InternalAuth(""))
	router.GET("/internal", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/internal", nil)
	req.Header.Set(InternalSecretHeader, "anything")
	w := performRequest(router, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestInternalAuth_MissingHeaderRejects(t *testing.T) {
	router := gin.New()
	router.Use(InternalAuth("my-secret"))
	router.GET("/internal", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/internal", nil)
	// No header set
	w := performRequest(router, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestInternalAuth_WrongSecretRejects(t *testing.T) {
	router := gin.New()
	router.Use(InternalAuth("correct-secret"))
	router.GET("/internal", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/internal", nil)
	req.Header.Set(InternalSecretHeader, "wrong-secret")
	w := performRequest(router, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestInternalAuth_CorrectSecretPasses(t *testing.T) {
	router := gin.New()
	router.Use(InternalAuth("correct-secret"))
	router.GET("/internal", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/internal", nil)
	req.Header.Set(InternalSecretHeader, "correct-secret")
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ===========================================================================
// 2. ExtractToken
// ===========================================================================

func TestExtractToken_FromBearerHeader(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer my-token-123")

	token := ExtractToken(c)
	if token != "my-token-123" {
		t.Errorf("expected 'my-token-123', got %q", token)
	}
}

func TestExtractToken_FromCookie(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.AddCookie(&http.Cookie{Name: "auth_token", Value: "cookie-token-456"})

	token := ExtractToken(c)
	if token != "cookie-token-456" {
		t.Errorf("expected 'cookie-token-456', got %q", token)
	}
}

func TestExtractToken_ReturnsEmptyWhenNeitherPresent(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)

	token := ExtractToken(c)
	if token != "" {
		t.Errorf("expected empty string, got %q", token)
	}
}

func TestExtractToken_BearerHeaderTakesPriorityOverCookie(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer header-token")
	c.Request.AddCookie(&http.Cookie{Name: "auth_token", Value: "cookie-token"})

	token := ExtractToken(c)
	if token != "header-token" {
		t.Errorf("expected 'header-token', got %q", token)
	}
}

// ===========================================================================
// 3. parseJWTToken
// ===========================================================================

func TestParseJWTToken_ValidRS256(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid-1"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"sub":                "user-123",
		"name":               "Test User",
		"preferred_username": "testuser",
		"email":              "test@example.com",
		"exp":                time.Now().Add(1 * time.Hour).Unix(),
	})

	info, err := parseJWTToken(tokenStr, jwks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Sub != "user-123" {
		t.Errorf("expected sub 'user-123', got %q", info.Sub)
	}
	if info.Name != "Test User" {
		t.Errorf("expected name 'Test User', got %q", info.Name)
	}
	if info.PreferredUsername != "testuser" {
		t.Errorf("expected preferred_username 'testuser', got %q", info.PreferredUsername)
	}
	if info.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", info.Email)
	}
}

func TestParseJWTToken_ForgedUnsignedJWT(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid-1"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	// Create a token with "none" algorithm by manually constructing it
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"hacker","name":"Evil","exp":` +
		fmt.Sprintf("%d", time.Now().Add(1*time.Hour).Unix()) + `}`))
	forgedToken := header + "." + payload + "."

	_, err := parseJWTToken(forgedToken, jwks)
	if err == nil {
		t.Fatal("expected error for forged unsigned JWT, got nil")
	}
}

func TestParseJWTToken_WrongKeyRejects(t *testing.T) {
	signingKey := generateTestRSAKey(t)
	wrongKey := generateTestRSAKey(t)
	kid := "test-kid-1"

	// JWKS has the wrong public key
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &wrongKey.PublicKey})

	tokenStr := signTestJWT(t, signingKey, kid, jwt.MapClaims{
		"sub": "user-123",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	_, err := parseJWTToken(tokenStr, jwks)
	if err == nil {
		t.Fatal("expected error for JWT signed with wrong key, got nil")
	}
}

func TestParseJWTToken_MissingSubClaim(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid-1"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	// Token with no "sub" and no "universal_id"
	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"name": "No Sub User",
		"exp":  time.Now().Add(1 * time.Hour).Unix(),
	})

	_, err := parseJWTToken(tokenStr, jwks)
	if err == nil {
		t.Fatal("expected error for missing sub claim, got nil")
	}
	if got := err.Error(); got != "no id, sub or universal_id in token" {
		t.Errorf("expected 'no id, sub or universal_id in token', got %q", got)
	}
}

func TestParseJWTToken_UniversalIDFallback(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid-1"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	// Token with "universal_id" instead of "sub"
	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"universal_id": "uid-456",
		"name":         "Fallback User",
		"exp":          time.Now().Add(1 * time.Hour).Unix(),
	})

	info, err := parseJWTToken(tokenStr, jwks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Sub != "uid-456" {
		t.Errorf("expected sub 'uid-456' from universal_id fallback, got %q", info.Sub)
	}
}

func TestParseJWTToken_NilJWKSProvider(t *testing.T) {
	_, err := parseJWTToken("some.jwt.token", nil)
	if err == nil {
		t.Fatal("expected error for nil JWKS provider, got nil")
	}
	if got := err.Error(); got != "JWKS provider not configured" {
		t.Errorf("expected 'JWKS provider not configured', got %q", got)
	}
}

func TestParseJWTToken_HS256Rejected(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid-1"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	// Create a token signed with HS256 (HMAC, not RSA)
	hmacToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-123",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	hmacToken.Header["kid"] = kid
	tokenStr, err := hmacToken.SignedString([]byte("hmac-secret"))
	if err != nil {
		t.Fatalf("sign HMAC JWT: %v", err)
	}

	_, err = parseJWTToken(tokenStr, jwks)
	if err == nil {
		t.Fatal("expected error for HS256-signed JWT, got nil")
	}
}

func TestParseJWTToken_PreferredUsernameFallsBackToName(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid-1"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	// Token without preferred_username
	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"sub":  "user-789",
		"name": "Fallback Name",
		"exp":  time.Now().Add(1 * time.Hour).Unix(),
	})

	info, err := parseJWTToken(tokenStr, jwks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.PreferredUsername != "Fallback Name" {
		t.Errorf("expected preferred_username to fall back to 'Fallback Name', got %q", info.PreferredUsername)
	}
}

// ===========================================================================
// 4. JWKSProvider
// ===========================================================================

func TestJWKSProvider_GetKeyCached(t *testing.T) {
	key := generateTestRSAKey(t)
	provider := newTestJWKSProvider(map[string]*rsa.PublicKey{"kid-1": &key.PublicKey})

	pub, err := provider.GetKey("kid-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 || pub.E != key.PublicKey.E {
		t.Error("returned key does not match cached key")
	}
}

func TestJWKSProvider_GetKeyEmptyKidReturnsFirstAvailable(t *testing.T) {
	key := generateTestRSAKey(t)
	provider := newTestJWKSProvider(map[string]*rsa.PublicKey{"some-kid": &key.PublicKey})

	pub, err := provider.GetKey("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 || pub.E != key.PublicKey.E {
		t.Error("returned key does not match the only available key")
	}
}

func TestJWKSProvider_GetKeyFetchesFromRemoteOnCacheMiss(t *testing.T) {
	key := generateTestRSAKey(t)
	nStr, eStr := rsaPubKeyToJWKParams(&key.PublicKey)

	fetchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		resp := jwksResponse{
			Keys: []jwkKey{
				{
					Kty: "RSA",
					Use: "sig",
					Kid: "remote-kid",
					Alg: "RS256",
					N:   nStr,
					E:   eStr,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := &JWKSProvider{
		jwksURL:    server.URL,
		keys:       make(map[string]*rsa.PublicKey), // empty cache
		minRefresh: 0,                               // no rate limiting for test
		httpClient: server.Client(),
	}

	pub, err := provider.GetKey("remote-kid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 {
		t.Error("fetched key does not match expected key")
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 fetch, got %d", fetchCount)
	}
}

func TestJWKSProvider_RateLimitingPreventsExcessiveFetches(t *testing.T) {
	key := generateTestRSAKey(t)
	nStr, eStr := rsaPubKeyToJWKParams(&key.PublicKey)

	var mu sync.Mutex
	fetchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetchCount++
		mu.Unlock()
		resp := jwksResponse{
			Keys: []jwkKey{
				{
					Kty: "RSA",
					Use: "sig",
					Kid: "rate-kid",
					Alg: "RS256",
					N:   nStr,
					E:   eStr,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := &JWKSProvider{
		jwksURL:    server.URL,
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 1 * time.Hour, // very long interval
		httpClient: server.Client(),
	}

	// First call: should fetch
	_, err := provider.GetKey("rate-kid")
	if err != nil {
		t.Fatalf("first GetKey: %v", err)
	}

	// Second call with unknown kid: should NOT fetch again due to rate limit
	_, err = provider.GetKey("unknown-kid")
	// This should fail because the key is not found and rate limit prevents refresh
	if err == nil {
		t.Error("expected error for unknown-kid when rate limited, got nil")
	}

	mu.Lock()
	if fetchCount != 1 {
		t.Errorf("expected exactly 1 fetch (rate limited), got %d", fetchCount)
	}
	mu.Unlock()
}

func TestParseRSAPublicKey_CorrectlyParsesJWK(t *testing.T) {
	key := generateTestRSAKey(t)
	nStr, eStr := rsaPubKeyToJWKParams(&key.PublicKey)

	jwk := jwkKey{
		Kty: "RSA",
		N:   nStr,
		E:   eStr,
	}

	pub, err := parseRSAPublicKey(jwk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 {
		t.Error("parsed N does not match original")
	}
	if pub.E != key.PublicKey.E {
		t.Errorf("parsed E=%d does not match original E=%d", pub.E, key.PublicKey.E)
	}
}

func TestParseRSAPublicKey_InvalidBase64(t *testing.T) {
	jwk := jwkKey{
		Kty: "RSA",
		N:   "!!!invalid-base64!!!",
		E:   "AQAB",
	}
	_, err := parseRSAPublicKey(jwk)
	if err == nil {
		t.Fatal("expected error for invalid base64 N, got nil")
	}
}

func TestJWKSProvider_EmptyKidUsesDefaultKey(t *testing.T) {
	// Simulate what happens when JWKS contains a key with empty kid —
	// the refresh() code maps it to "_default".
	key := generateTestRSAKey(t)
	nStr, eStr := rsaPubKeyToJWKParams(&key.PublicKey)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jwksResponse{
			Keys: []jwkKey{
				{
					Kty: "RSA",
					Use: "sig",
					Kid: "", // empty kid
					Alg: "RS256",
					N:   nStr,
					E:   eStr,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := &JWKSProvider{
		jwksURL:    server.URL,
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: server.Client(),
	}

	// GetKey with empty kid should get the key mapped to "_default"
	pub, err := provider.GetKey("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 {
		t.Error("fetched key does not match expected key for empty kid")
	}
}

func TestJWKSProvider_RemoteServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer server.Close()

	provider := &JWKSProvider{
		jwksURL:    server.URL,
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: server.Client(),
	}

	_, err := provider.GetKey("any-kid")
	if err == nil {
		t.Fatal("expected error when remote server returns 500, got nil")
	}
}

// ===========================================================================
// 5. RequireAuth middleware (integration-style)
// ===========================================================================

func TestRequireAuth_NoTokenReturns401(t *testing.T) {
	router := gin.New()
	router.Use(RequireAuth("http://localhost:0", nil))
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	w := performRequest(router, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRequireAuth_ValidJWTSetsUserID(t *testing.T) {
	SetSubjectResolver(nil)
	key := generateTestRSAKey(t)
	kid := "test-kid"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"sub":                "user-abc",
		"name":               "Test User",
		"preferred_username": "testuser",
		"exp":                time.Now().Add(1 * time.Hour).Unix(),
	})

	var capturedUserID string
	var capturedUserName string

	router := gin.New()
	router.Use(RequireAuth("http://localhost:0", jwks))
	router.GET("/protected", func(c *gin.Context) {
		if uid, ok := c.Get(UserIDKey); ok {
			capturedUserID = uid.(string)
		}
		if uname, ok := c.Get(UserNameKey); ok {
			capturedUserName = uname.(string)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if capturedUserID != "user-abc" {
		t.Errorf("expected userId 'user-abc', got %q", capturedUserID)
	}
	if capturedUserName != "testuser" {
		t.Errorf("expected userName 'testuser', got %q", capturedUserName)
	}
}

func TestRequireAuth_InvalidJWTFallsBackToCasdoor(t *testing.T) {
	SetSubjectResolver(nil)
	// Mock Casdoor server that returns user info
	casdoorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/userinfo" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Verify the token is forwarded
		auth := r.Header.Get("Authorization")
		if auth != "Bearer invalid-jwt-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(casdoorUserinfoResponse{
			Sub:  "casdoor-user-999",
			Name: "Casdoor User",
		})
	}))
	defer casdoorServer.Close()

	// JWKS provider with no matching keys — JWT parsing will fail, triggering fallback
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{})

	var capturedUserID string

	router := gin.New()
	router.Use(RequireAuth(casdoorServer.URL, jwks))
	router.GET("/protected", func(c *gin.Context) {
		if uid, ok := c.Get(UserIDKey); ok {
			capturedUserID = uid.(string)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer invalid-jwt-token")
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (Casdoor fallback), got %d", w.Code)
	}
	if capturedUserID != "casdoor-user-999" {
		t.Errorf("expected userId 'casdoor-user-999', got %q", capturedUserID)
	}
}

func TestRequireAuth_InvalidJWTAndCasdoorFailureReturns401(t *testing.T) {
	// Mock Casdoor server that always fails
	casdoorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","msg":"invalid token"}`))
	}))
	defer casdoorServer.Close()

	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{})

	router := gin.New()
	router.Use(RequireAuth(casdoorServer.URL, jwks))
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	w := performRequest(router, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ===========================================================================
// 6. OptionalAuth middleware (integration-style)
// ===========================================================================

func TestOptionalAuth_NoTokenPassesThroughWithoutUserID(t *testing.T) {
	var hasUserID bool

	router := gin.New()
	router.Use(OptionalAuth("http://localhost:0", nil))
	router.GET("/optional", func(c *gin.Context) {
		_, hasUserID = c.Get(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if hasUserID {
		t.Error("expected userId NOT to be set when no token provided")
	}
}

func TestOptionalAuth_ValidJWTSetsUserID(t *testing.T) {
	SetSubjectResolver(nil)
	key := generateTestRSAKey(t)
	kid := "test-kid"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"sub":                "optional-user-123",
		"name":               "Opt User",
		"preferred_username": "optuser",
		"exp":                time.Now().Add(1 * time.Hour).Unix(),
	})

	var capturedUserID string
	var hasUserID bool

	router := gin.New()
	router.Use(OptionalAuth("http://localhost:0", jwks))
	router.GET("/optional", func(c *gin.Context) {
		if uid, ok := c.Get(UserIDKey); ok {
			capturedUserID = uid.(string)
			hasUserID = true
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !hasUserID {
		t.Fatal("expected userId to be set")
	}
	if capturedUserID != "optional-user-123" {
		t.Errorf("expected userId 'optional-user-123', got %q", capturedUserID)
	}
}

func TestOptionalAuth_InvalidJWTAndCasdoorFailureStillPassesThrough(t *testing.T) {
	// Mock Casdoor server that always fails
	casdoorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","msg":"invalid token"}`))
	}))
	defer casdoorServer.Close()

	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{})

	var hasUserID bool

	router := gin.New()
	router.Use(OptionalAuth(casdoorServer.URL, jwks))
	router.GET("/optional", func(c *gin.Context) {
		_, hasUserID = c.Get(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	req.Header.Set("Authorization", "Bearer invalid-jwt")
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (optional auth should pass through), got %d", w.Code)
	}
	if hasUserID {
		t.Error("expected userId NOT to be set when auth fails in optional mode")
	}
}

func TestOptionalAuth_InvalidJWTFallsBackToCasdoorSuccess(t *testing.T) {
	SetSubjectResolver(nil)
	// Mock Casdoor server that returns user info
	casdoorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/userinfo" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(casdoorUserinfoResponse{
			Sub:  "casdoor-opt-user",
			Name: "Casdoor Opt User",
		})
	}))
	defer casdoorServer.Close()

	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{})

	var capturedUserID string

	router := gin.New()
	router.Use(OptionalAuth(casdoorServer.URL, jwks))
	router.GET("/optional", func(c *gin.Context) {
		if uid, ok := c.Get(UserIDKey); ok {
			capturedUserID = uid.(string)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	req.Header.Set("Authorization", "Bearer some-opaque-token")
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if capturedUserID != "casdoor-opt-user" {
		t.Errorf("expected userId 'casdoor-opt-user', got %q", capturedUserID)
	}
}

func TestRequireAuth_UsesResolvedSubjectID(t *testing.T) {
	defer SetSubjectResolver(nil)
	SetSubjectResolver(func(claims AuthClaims) (string, string, error) {
		if claims.UniversalID != "universal-123" {
			t.Fatalf("expected universal id universal-123, got %+v", claims)
		}
		return "subject-123", "resolved-user", nil
	})

	key := generateTestRSAKey(t)
	kid := "subject-kid"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})
	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"id":                 "legacy-id",
		"sub":                "legacy-sub",
		"universal_id":       "universal-123",
		"preferred_username": "legacy-name",
		"exp":                time.Now().Add(1 * time.Hour).Unix(),
	})

	var capturedUserID string
	var capturedUserName string
	router := gin.New()
	router.Use(RequireAuth("http://localhost:0", jwks))
	router.GET("/protected", func(c *gin.Context) {
		capturedUserID = c.GetString(UserIDKey)
		capturedUserName = c.GetString(UserNameKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedUserID != "subject-123" {
		t.Fatalf("expected resolved subject id, got %q", capturedUserID)
	}
	if capturedUserName != "resolved-user" {
		t.Fatalf("expected resolved user name, got %q", capturedUserName)
	}
}

// ===========================================================================
// Additional edge-case tests
// ===========================================================================

func TestParseJWTToken_ExpiredTokenRejected(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "test-kid"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"sub": "user-expired",
		"exp": time.Now().Add(-1 * time.Hour).Unix(), // expired 1 hour ago
	})

	_, err := parseJWTToken(tokenStr, jwks)
	if err == nil {
		t.Fatal("expected error for expired JWT, got nil")
	}
}

func TestParseJWTToken_NoKidUsesFirstAvailableKey(t *testing.T) {
	key := generateTestRSAKey(t)
	// Cache the key under a specific kid
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{"any-kid": &key.PublicKey})

	// Sign a token WITHOUT a kid header — the keyfunc will call GetKey("") which
	// returns the first available key from the cache.
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user-no-kid",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	// Deliberately don't set kid in header
	tokenStr, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	info, err := parseJWTToken(tokenStr, jwks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Sub != "user-no-kid" {
		t.Errorf("expected sub 'user-no-kid', got %q", info.Sub)
	}
}

func TestRequireAuth_TokenFromCookie(t *testing.T) {
	key := generateTestRSAKey(t)
	kid := "cookie-kid"
	jwks := newTestJWKSProvider(map[string]*rsa.PublicKey{kid: &key.PublicKey})

	tokenStr := signTestJWT(t, key, kid, jwt.MapClaims{
		"sub":  "cookie-user",
		"name": "Cookie User",
		"exp":  time.Now().Add(1 * time.Hour).Unix(),
	})

	var capturedUserID string

	router := gin.New()
	router.Use(RequireAuth("http://localhost:0", jwks))
	router.GET("/protected", func(c *gin.Context) {
		if uid, ok := c.Get(UserIDKey); ok {
			capturedUserID = uid.(string)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: tokenStr})
	w := performRequest(router, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if capturedUserID != "cookie-user" {
		t.Errorf("expected userId 'cookie-user', got %q", capturedUserID)
	}
}

func TestJWKSProvider_NoValidRSAKeysInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := jwksResponse{
			Keys: []jwkKey{
				{
					Kty: "EC", // Not RSA — should be skipped
					Kid: "ec-key",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := &JWKSProvider{
		jwksURL:    server.URL,
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 0,
		httpClient: server.Client(),
	}

	_, err := provider.GetKey("ec-key")
	if err == nil {
		t.Fatal("expected error when no valid RSA keys in response, got nil")
	}
}

func TestExtractToken_BearerPrefixOnlyReturnsEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer ")

	token := ExtractToken(c)
	// "Bearer " with TrimPrefix yields "", so should return empty
	// But the code checks HasPrefix("Bearer ") first which is true,
	// then TrimPrefix returns "".
	if token != "" {
		t.Errorf("expected empty token for 'Bearer ' header, got %q", token)
	}
}

func TestExtractToken_NonBearerAuthHeaderUseCookie(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // Basic auth, not Bearer
	c.Request.AddCookie(&http.Cookie{Name: "auth_token", Value: "fallback-cookie"})

	token := ExtractToken(c)
	if token != "fallback-cookie" {
		t.Errorf("expected 'fallback-cookie' when Authorization is not Bearer, got %q", token)
	}
}
