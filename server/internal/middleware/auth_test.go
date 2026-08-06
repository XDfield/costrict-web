package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

// performRequest executes a handler with the given request and returns the recorder.
func performRequest(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

const testInternalToken = "test-internal-token"

// stubCSUser spawns an httptest server that mimics cs-user's
// POST /api/internal/auth/verify endpoint. behavior describes what to return
// for each call (status code + JSON body). Returns the URL to feed into
// SetTokenVerifier; the server is auto-closed on test cleanup.
//
// The server also asserts that the X-Internal-Token header matches
// testInternalToken — a missing/wrong token gets a 401 so tests catching
// misconfigured wiring fail loudly rather than silently passing.
type stubCSUser struct {
	server   *httptest.Server
	calls    atomic.Int32
	bodies   atomic.Value // []string
	behavior func(w http.ResponseWriter, r *http.Request, call int32)
}

func newStubCSUser(t *testing.T, behavior func(w http.ResponseWriter, r *http.Request, call int32)) *stubCSUser {
	t.Helper()
	s := &stubCSUser{behavior: behavior}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != testInternalToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		call := s.calls.Add(1)
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		prev, _ := s.bodies.Load().([]string)
		s.bodies.Store(append(prev, string(body)))
		s.behavior(w, r, call)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// installVerifier configures SetTokenVerifier to point at the stub cs-user
// server, using the shared testInternalToken. Restores the previous verifier
// on cleanup so tests don't leak state into each other.
func installVerifier(t *testing.T, url string) {
	t.Helper()
	SetTokenVerifier(url, testInternalToken, 2*time.Second)
	t.Cleanup(func() {
		// Reset to disabled so the next test starts clean.
		SetTokenVerifier("", "", 0)
	})
}

// verifyOK builds the 200 response cs-user returns for a successful
// introspection. Mirrors the verifyTokenResponse JSON shape from
// cs-user/internal/handlers/auth.go.
func verifyOK(subject, tenantID, tenantSlug string) map[string]any {
	return map[string]any{
		"active":      true,
		"token_source": "cs-user",
		"sub":         subject,
		"universal_id": "uni-" + subject,
		"name":        subject + "-name",
		"email":       subject + "@example.test",
		"tenant_id":   tenantID,
		"tenant_slug": tenantSlug,
		"iss":         "cs-user",
	}
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
	c.Request.AddCookie(&http.Cookie{Name: "zgsmAdminToken", Value: "cookie-token-456"})

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
	c.Request.AddCookie(&http.Cookie{Name: "zgsmAdminToken", Value: "cookie-token"})

	token := ExtractToken(c)
	if token != "header-token" {
		t.Errorf("expected 'header-token', got %q", token)
	}
}

func TestExtractToken_BearerPrefixOnlyReturnsEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer ")

	token := ExtractToken(c)
	if token != "" {
		t.Errorf("expected empty token for 'Bearer ' header, got %q", token)
	}
}

func TestExtractToken_NonBearerAuthHeaderUseCookie(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	c.Request.AddCookie(&http.Cookie{Name: "zgsmAdminToken", Value: "fallback-cookie"})

	token := ExtractToken(c)
	if token != "fallback-cookie" {
		t.Errorf("expected 'fallback-cookie' when Authorization is not Bearer, got %q", token)
	}
}

// ===========================================================================
// 3. introspectToken (cs-user verify contract)
// ===========================================================================

// TestParseToken_ValidTokenMapsFields pins the response→VerifiedUserInfo field
// mapping: the contract between cs-user's verifyTokenResponse JSON and the
// middleware. Silent drift in either direction fails this test.
func TestParseToken_ValidTokenMapsFields(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active":       true,
			"token_source": "cs-user",
			"sub":          "usr_alice_001",
			"universal_id": "u-alice-001",
			"short_id":     "alice",
			"name":         "Alice Admin",
			"email":        "alice@corp.acme.test",
			"phone":        "+1-555-0100",
			"tenant_id":    "t-acme",
			"tenant_slug":  "acme",
			"iss":          "cs-user",
		})
	})
	installVerifier(t, stub.server.URL)

	info, err := ParseToken("valid-token-A")
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if info.Sub != "usr_alice_001" {
		t.Errorf("Sub = %q, want usr_alice_001", info.Sub)
	}
	if info.ID != "usr_alice_001" {
		t.Errorf("ID = %q, want usr_alice_001 (mirrors Sub)", info.ID)
	}
	if info.UniversalID != "u-alice-001" {
		t.Errorf("UniversalID = %q, want u-alice-001", info.UniversalID)
	}
	if info.Name != "Alice Admin" {
		t.Errorf("Name = %q, want Alice Admin", info.Name)
	}
	if info.PreferredUsername != "Alice Admin" {
		t.Errorf("PreferredUsername = %q, want Alice Admin (mirrors Name)", info.PreferredUsername)
	}
	if info.Email != "alice@corp.acme.test" {
		t.Errorf("Email = %q", info.Email)
	}
	if info.Phone != "+1-555-0100" {
		t.Errorf("Phone = %q", info.Phone)
	}
	if info.TenantID != "t-acme" {
		t.Errorf("TenantID = %q, want t-acme", info.TenantID)
	}
	if info.TenantSlug != "acme" {
		t.Errorf("TenantSlug = %q, want acme", info.TenantSlug)
	}
	if info.Issuer != "cs-user" {
		t.Errorf("Issuer = %q, want cs-user", info.Issuer)
	}
	// Platform-admin fields stay zero — server no longer trusts local JWT claims.
	if info.PlatformAdmin || info.PlatformScope != "" || info.TenantRoles != nil {
		t.Errorf("platform-admin fields must be zero, got admin=%v scope=%q roles=%v", info.PlatformAdmin, info.PlatformScope, info.TenantRoles)
	}
}

// TestParseToken_EmptyTokenRejected guards the cheap path — an empty bearer
// never reaches cs-user.
func TestParseToken_EmptyTokenRejected(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		t.Errorf("cs-user must not be called for empty token")
	})
	installVerifier(t, stub.server.URL)

	_, err := ParseToken("")
	if !isErrInvalidToken(err) {
		t.Fatalf("empty token: err = %v, want errInvalidToken", err)
	}
}

// TestParseToken_RejectedByCSUser pins the 401 path: cs-user explicitly
// rejecting the token surfaces as errInvalidToken (NOT errVerifierUnavailable).
func TestParseToken_RejectedByCSUser(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	installVerifier(t, stub.server.URL)

	_, err := ParseToken("rejected-token")
	if !isErrInvalidToken(err) {
		t.Fatalf("rejected token: err = %v, want errInvalidToken", err)
	}
}

// TestParseToken_InactiveTokenRejected pins the active=false path — cs-user
// returned 200 but active=false means the token was revoked.
func TestParseToken_InactiveTokenRejected(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false, "sub": "usr_x"})
	})
	installVerifier(t, stub.server.URL)

	_, err := ParseToken("revoked-token")
	if !isErrInvalidToken(err) {
		t.Fatalf("inactive token: err = %v, want errInvalidToken", err)
	}
}

// TestParseToken_EmptySubjectRejected guards against a misbehaving cs-user
// returning active=true with no sub — we must not mint an auth context for
// an unknown subject.
func TestParseToken_EmptySubjectRejected(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": ""})
	})
	installVerifier(t, stub.server.URL)

	_, err := ParseToken("no-sub-token")
	if !isErrInvalidToken(err) {
		t.Fatalf("empty subject: err = %v, want errInvalidToken", err)
	}
}

// TestParseToken_CSUser5xxFailsClosed pins the fail-closed guarantee: a 5xx
// from cs-user MUST surface as errVerifierUnavailable (NOT errInvalidToken),
// so RequireAuth returns 503 and the request never silently degrades.
func TestParseToken_CSUser5xxFailsClosed(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.WriteHeader(http.StatusBadGateway)
	})
	installVerifier(t, stub.server.URL)

	_, err := ParseToken("some-token")
	if !isErrVerifierUnavailable(err) {
		t.Fatalf("5xx: err = %v, want errVerifierUnavailable", err)
	}
}

// TestParseToken_CSUserUnreachableFailsClosed pins the network-error path:
// a closed stub connection surfaces as errVerifierUnavailable, never as
// errInvalidToken (which would let an outage masquerade as a bad token).
func TestParseToken_CSUserUnreachableFailsClosed(t *testing.T) {
	// Start then immediately close a server to get an unreachable URL.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	installVerifier(t, url)

	_, err := ParseToken("any-token")
	if !isErrVerifierUnavailable(err) {
		t.Fatalf("unreachable: err = %v, want errVerifierUnavailable", err)
	}
}

// TestParseToken_VerifierUnconfiguredFailsClosed verifies that with no
// SetTokenVerifier call, introspectToken fails closed rather than panicking.
func TestParseToken_VerifierUnconfiguredFailsClosed(t *testing.T) {
	SetTokenVerifier("", "", 0)
	_, err := ParseToken("any-token")
	if !isErrVerifierUnavailable(err) {
		t.Fatalf("unconfigured: err = %v, want errVerifierUnavailable", err)
	}
}

// TestParseToken_CachesSuccessfulLookups verifies the SHA-256-keyed cache:
// two ParseToken calls with the same token produce one cs-user round-trip.
func TestParseToken_CachesSuccessfulLookups(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("usr_cached", "default", "default"))
	})
	installVerifier(t, stub.server.URL)

	for i := 0; i < 2; i++ {
		info, err := ParseToken("same-token-twice")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if info.Sub != "usr_cached" {
			t.Fatalf("call %d: Sub = %q", i, info.Sub)
		}
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("expected cs-user called once (cached), got %d", got)
	}
}

// TestParseToken_InvalidateCacheForcesRefresh verifies
// InvalidateTokenCache drops the cache entry so the next call re-queries.
func TestParseToken_InvalidateCacheForcesRefresh(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("usr_invalidate", "default", "default"))
	})
	installVerifier(t, stub.server.URL)

	if _, err := ParseToken("invalidate-token"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	InvalidateTokenCache("invalidate-token")
	if _, err := ParseToken("invalidate-token"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Errorf("expected cs-user called twice after invalidate, got %d", got)
	}
}

// TestParseToken_DifferentTokensBypassCache verifies the cache is keyed by
// the token (not by some shared subject) so two distinct tokens both hit
// cs-user.
func TestParseToken_DifferentTokensBypassCache(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("usr_"+strconv.FormatInt(int64(call), 10), "default", "default"))
	})
	installVerifier(t, stub.server.URL)

	if _, err := ParseToken("token-one"); err != nil {
		t.Fatalf("token-one: %v", err)
	}
	if _, err := ParseToken("token-two"); err != nil {
		t.Fatalf("token-two: %v", err)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Errorf("expected cs-user called twice (distinct tokens), got %d", got)
	}
}

// ===========================================================================
// 4. RequireAuth middleware (integration-style via stub cs-user)
// ===========================================================================

func TestRequireAuth_NoTokenReturns401(t *testing.T) {
	stub := newStubCSUser(t, func(http.ResponseWriter, *http.Request, int32) {
		t.Errorf("cs-user must not be called without a token")
	})
	installVerifier(t, stub.server.URL)

	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	w := performRequest(router, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRequireAuth_ValidTokenSetsUserID(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("user-abc", "default", "default"))
	})
	installVerifier(t, stub.server.URL)
	SetSubjectResolver(nil)

	var capturedUserID, capturedUserName string
	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		capturedUserID = c.GetString(UserIDKey)
		capturedUserName = c.GetString(UserNameKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if capturedUserID != "user-abc" {
		t.Errorf("expected userId 'user-abc', got %q", capturedUserID)
	}
	if capturedUserName != "user-abc-name" {
		t.Errorf("expected userName 'user-abc-name', got %q", capturedUserName)
	}
}

// RequireAuth must accept the token from ?token= for a browser-native
// WebSocket handshake, which cannot set an Authorization header. This is the
// cross-origin path used when the SameSite=Lax session cookie is not sent.
func TestRequireAuth_TokenFromQueryForWebSocketUpgrade(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("user-ws", "default", "default"))
	})
	installVerifier(t, stub.server.URL)
	SetSubjectResolver(nil)

	var capturedUserID string
	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		capturedUserID = c.GetString(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected?token=ws-token", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if capturedUserID != "user-ws" {
		t.Errorf("expected userID 'user-ws', got %q", capturedUserID)
	}
}

// RequireAuth must accept ?token= for an EventSource (SSE) handshake, which
// also cannot set an Authorization header.
func TestRequireAuth_TokenFromQueryForSSE(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("user-sse", "default", "default"))
	})
	installVerifier(t, stub.server.URL)
	SetSubjectResolver(nil)

	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected?token=sse-token", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// A plain HTTP request that carries the token only in ?token= must NOT be
// authenticated — the query fallback is reserved for WS/SSE handshakes so
// tokens don't leak into URLs/access logs for ordinary requests.
func TestRequireAuth_QueryTokenRejectedForPlainHTTP(t *testing.T) {
	stub := newStubCSUser(t, func(http.ResponseWriter, *http.Request, int32) {
		t.Errorf("cs-user must not be called for plain-HTTP query token")
	})
	installVerifier(t, stub.server.URL)

	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected?token=plain-token", nil)
	w := performRequest(router, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for plain HTTP with query-only token, got %d", w.Code)
	}
}

// TestRequireAuth_RejectedTokenReturns401AndClearsCookie pins the security
// path: cs-user rejecting the token (401/400/inactive) yields 401 and
// clears the auth cookie. This is the original CVE fix — forged unsigned
// tokens, alg=none tokens, and revoked tokens are all rejected.
func TestRequireAuth_RejectedTokenReturns401AndClearsCookie(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	installVerifier(t, stub.server.URL)

	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer forged.jwt.token")
	w := performRequest(router, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	// Cookie should be cleared via Set-Cookie with MaxAge=-1.
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Max-Age=0") && !strings.Contains(setCookie, "zgsmAdminToken=") {
		t.Errorf("expected Set-Cookie clearing zgsmAdminToken, got %q", setCookie)
	}
}

// TestRequireAuth_CSUser5xxReturns503 pins the fail-closed contract: when
// cs-user is unavailable (5xx), RequireAuth returns 503 — NOT 401, and NOT
// a successful pass-through. There is no fallback to local unverified
// decoding; that was the original CVE.
func TestRequireAuth_CSUser5xxReturns503(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	installVerifier(t, stub.server.URL)

	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	w := performRequest(router, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (fail closed), got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestRequireAuth_TokenFromCookie covers the cookie path that production
// browsers hit on every non-API navigation.
func TestRequireAuth_TokenFromCookie(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("cookie-user", "default", "default"))
	})
	installVerifier(t, stub.server.URL)
	SetSubjectResolver(nil)

	var capturedUserID string
	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		capturedUserID = c.GetString(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "zgsmAdminToken", Value: "cookie-token"})
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedUserID != "cookie-user" {
		t.Errorf("expected userId 'cookie-user', got %q", capturedUserID)
	}
}

// TestRequireAuth_IgnoresSubjectResolver_PostPhase52 pins the contract:
// middleware's setAuthContext never calls subjectResolver regardless of
// what issuer the cs-user verify response carries. cs-user is the sole
// identity authority and signs every token, so the resolver branch is
// dead in the HTTP-request path. subjectResolver is still wired in
// main.go but only consumed by authz.Service.VerifyTokenWithUser on the
// internal /auth/verify path — NOT by HTTP request middleware.
//
// Regression guard: if a future change reintroduces a resolver call in
// setAuthContext, this test fails loudly. The stub returns a non-default
// issuer ("https://casdoor.example") specifically to defeat any future
// "iss == cs-user" predicate — there must be no issuer-based branching.
func TestRequireAuth_IgnoresSubjectResolver_PostPhase52(t *testing.T) {
	resolverCalled := false
	defer SetSubjectResolver(nil)
	SetSubjectResolver(func(claims AuthClaims) (string, string, error) {
		resolverCalled = true
		return "should-not-be-used", "should-not-be-used", nil
	})

	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		// Intentionally a non-default issuer to prove no issuer-based
		// branching exists downstream.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active":       true,
			"sub":          "usr_trusted_sub",
			"universal_id": "u-legacy-001",
			"name":         "Trusted",
			"iss":          "https://casdoor.example",
		})
	})
	installVerifier(t, stub.server.URL)

	var capturedUserID, capturedUserName string
	router := gin.New()
	router.Use(RequireAuth())
	router.GET("/protected", func(c *gin.Context) {
		capturedUserID = c.GetString(UserIDKey)
		capturedUserName = c.GetString(UserNameKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if resolverCalled {
		t.Errorf("subjectResolver must NOT be called from middleware setAuthContext (post-Phase-5.2)")
	}
	if capturedUserID != "usr_trusted_sub" {
		t.Errorf("UserIDKey: got %q, want raw sub from cs-user response (no resolver override)", capturedUserID)
	}
	if capturedUserName != "Trusted" {
		t.Errorf("UserNameKey: got %q, want raw name from cs-user response", capturedUserName)
	}
}

// ===========================================================================
// 5. OptionalAuth middleware (integration-style via stub cs-user)
// ===========================================================================

func TestOptionalAuth_NoTokenPassesThroughWithoutUserID(t *testing.T) {
	stub := newStubCSUser(t, func(http.ResponseWriter, *http.Request, int32) {
		t.Errorf("cs-user must not be called for anonymous request")
	})
	installVerifier(t, stub.server.URL)

	var hasUserID bool
	router := gin.New()
	router.Use(OptionalAuth())
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
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		_ = json.NewEncoder(w).Encode(verifyOK("optional-user-123", "default", "default"))
	})
	installVerifier(t, stub.server.URL)
	SetSubjectResolver(nil)

	var capturedUserID string
	var hasUserID bool
	router := gin.New()
	router.Use(OptionalAuth())
	router.GET("/optional", func(c *gin.Context) {
		capturedUserID = c.GetString(UserIDKey)
		_, hasUserID = c.Get(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	req.Header.Set("Authorization", "Bearer opt-token")
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

// TestOptionalAuth_RejectedTokenStillPassesThrough pins the optional
// semantics: a rejected token does NOT 401 — the request proceeds as
// anonymous. Public routes (swagger, health, etc.) stay reachable.
func TestOptionalAuth_RejectedTokenStillPassesThrough(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	installVerifier(t, stub.server.URL)

	var hasUserID bool
	router := gin.New()
	router.Use(OptionalAuth())
	router.GET("/optional", func(c *gin.Context) {
		_, hasUserID = c.Get(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	req.Header.Set("Authorization", "Bearer rejected-token")
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (optional auth should pass through), got %d", w.Code)
	}
	if hasUserID {
		t.Error("expected userId NOT to be set when auth fails in optional mode")
	}
}

// TestOptionalAuth_CSUser5xxStillPassesThrough verifies that even when
// cs-user is unavailable, OptionalAuth degrades to anonymous rather than
// 503. Required routes fail closed (503); optional routes fail open.
func TestOptionalAuth_CSUser5xxStillPassesThrough(t *testing.T) {
	stub := newStubCSUser(t, func(w http.ResponseWriter, r *http.Request, call int32) {
		w.WriteHeader(http.StatusBadGateway)
	})
	installVerifier(t, stub.server.URL)

	var hasUserID bool
	router := gin.New()
	router.Use(OptionalAuth())
	router.GET("/optional", func(c *gin.Context) {
		_, hasUserID = c.Get(UserIDKey)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/optional", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	w := performRequest(router, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (optional degrades to anonymous on cs-user outage), got %d", w.Code)
	}
	if hasUserID {
		t.Error("expected no userId when cs-user is unavailable")
	}
}

// ---------------------------------------------------------------------------
// helpers (errors.Is wrapper so test sites read clearly)
// ---------------------------------------------------------------------------

func isErrInvalidToken(err error) bool {
	return err == errInvalidToken
}

func isErrVerifierUnavailable(err error) bool {
	return err == errVerifierUnavailable
}
