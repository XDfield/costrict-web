package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/idp"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/oauth"
	"github.com/costrict/costrict-web/server/internal/tenant"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// stubMultiIdPUserWriter captures the calls made by runMultiIdPCallback so
// tests can assert provisioning happened with the right claims, without
// needing a real cs-user RPC backend or GORM DB.
type stubMultiIdPUserWriter struct {
	created   *userpkg.JWTClaims
	bound     *userpkg.JWTClaims
	mapped    string
	reissued  *userpkg.JWTClaims
	newToken  string
	reissueOK bool
}

func (s *stubMultiIdPUserWriter) GetOrCreateUser(ctx context.Context, c *userpkg.JWTClaims) (*models.User, error) {
	s.created = c
	return &models.User{SubjectID: "u-stub"}, nil
}
func (s *stubMultiIdPUserWriter) SyncUser(ctx context.Context, c *userpkg.JWTClaims) (*models.User, error) {
	return &models.User{SubjectID: "u-stub"}, nil
}
func (s *stubMultiIdPUserWriter) BindIdentityToUser(ctx context.Context, _ string, c *userpkg.JWTClaims, _ ...userpkg.BindIdentityOptions) error {
	s.bound = c
	return nil
}
func (s *stubMultiIdPUserWriter) TransferIdentityToUser(context.Context, string, string, string) error { return nil }
func (s *stubMultiIdPUserWriter) UnbindIdentityByProvider(context.Context, string, string) error      { return nil }
func (s *stubMultiIdPUserWriter) ApplyEnterpriseMapping(_ context.Context, _ string, provider string) error {
	s.mapped = provider
	return nil
}
func (s *stubMultiIdPUserWriter) ReissueToken(_ context.Context, _ string, c *userpkg.JWTClaims, _ []string) (string, time.Time, error) {
	s.reissued = c
	if !s.reissueOK {
		return "", time.Time{}, nil
	}
	return s.newToken, time.Now().Add(7 * 24 * time.Hour), nil
}

// setupMultiIdPTest wires the package-level multiIdP singleton against
// httptest-driven cs-user + OAuth provider stand-ins, returns the handler
// + cleanup func. Each test customizes the cs-user / OAuth handler bodies.
func setupMultiIdPTest(t *testing.T, csUserHandler http.HandlerFunc) (*MultiIdPHandler, *httptest.Server, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	csUserSrv := httptest.NewServer(http.HandlerFunc(csUserHandler))

	rpcClient := mustConfiguredRPCClient(t, csUserSrv.URL)

	writer := &stubMultiIdPUserWriter{newToken: "jwt-from-cs-user", reissueOK: true}

	h := &MultiIdPHandler{
		RPC:         rpcClient,
		OAuth:       oauth.NewClient(),
		UserWriter:  writer,
		StateSecret: "test-state-secret",
		JWTSignMode: config.JWTSignModeOff,
	}
	prevMultiIdP := multiIdP
	multiIdP = h
	prevDefault := multiIdPDefaultProvider
	multiIdPDefaultProvider = ""
	prevFrontendURL := defaultFrontendURL
	defaultFrontendURL = "http://localhost:3000"
	prevCookieSecure := cookieSecure
	cookieSecure = false

	cleanup := func() {
		csUserSrv.Close()
		multiIdP = prevMultiIdP
		multiIdPDefaultProvider = prevDefault
		defaultFrontendURL = prevFrontendURL
		cookieSecure = prevCookieSecure
	}
	return h, csUserSrv, cleanup
}

// mustConfiguredRPCClient builds an idp.RPCClient against the test cs-user
// URL with a non-empty internal token so Configured() returns true.
func mustConfiguredRPCClient(t *testing.T, baseURL string) *idp.RPCClient {
	t.Helper()
	c := idp.NewRPCClient(config.UserServiceConfig{
		BaseURL:       baseURL,
		InternalToken: "test-internal-token",
		TimeoutSec:    5,
	})
	if !c.Configured() {
		t.Fatalf("rpc client not configured for url=%s", baseURL)
	}
	return c
}

// TestListIdPs_DisabledByConfig — when no RPC client is wired, the
// endpoint returns 503 so the frontend can fall back gracefully.
func TestListIdPs_DisabledByConfig(t *testing.T) {
	prev := multiIdP
	multiIdP = nil
	defer func() { multiIdP = prev }()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/idps", nil)

	ListIdPs(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestListIdPs_RedactsSecrets — secrets in the IdP config must not appear
// in the response; the endpoint only exposes provider / display metadata.
func TestListIdPs_RedactsSecrets(t *testing.T) {
	csUserHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]idp.InternalIdPSourceView{
			{
				Provider: "github",
				Config: map[string]interface{}{
					"client_id":     "id-gh",
					"client_secret": "shh-gh",
					"display_name":  "GitHub Enterprise",
				},
				Enabled:  true,
				Priority: 100,
			},
			{
				Provider: "google",
				Config: map[string]interface{}{
					"client_id":     "id-goog",
					"client_secret": "shh-goog",
				},
				Enabled:  false, // filtered out
				Priority: 50,
			},
		})
	}

	_, _, cleanup := setupMultiIdPTest(t, csUserHandler)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/idps", nil)
	c.Request = c.Request.WithContext(tenant.WithSlug(c.Request.Context(), "acme"))
	c.Set("tenant_slug", "acme")

	ListIdPs(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "shh-gh") || strings.Contains(body, "shh-goog") {
		t.Errorf("secret leaked: %s", body)
	}
	if !strings.Contains(body, "github") {
		t.Errorf("github provider missing: %s", body)
	}
	if strings.Contains(body, "google") {
		t.Errorf("disabled provider should be filtered: %s", body)
	}
	if !strings.Contains(body, "GitHub Enterprise") {
		t.Errorf("display_name missing: %s", body)
	}
}

// TestListIdPs_EmptyTenant — when no tenant is resolved, return an empty
// list rather than querying cs-user with an empty tenant_id.
func TestListIdPs_EmptyTenant(t *testing.T) {
	called := false
	csUserHandler := func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte("[]"))
	}
	_, _, cleanup := setupMultiIdPTest(t, csUserHandler)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/idps", nil)

	ListIdPs(c)

	if called {
		t.Error("cs-user should not be called when tenant is empty")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "[]" {
		t.Errorf("expected empty JSON array, got %s", w.Body.String())
	}
}

// TestLoginMultiIdP_BuildsAuthorizeURL — with `?idp=github` the handler
// 302s to the OAuth provider's authorize URL carrying client_id, state,
// and the configured scopes.
func TestLoginMultiIdP_BuildsAuthorizeURL(t *testing.T) {
	csUserHandler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/acme/github") {
			t.Errorf("cs-user path unexpected: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.InternalIdPSourceView{
			Provider: "github",
			Config: map[string]interface{}{
				"authorization_url": "https://github.com/login/oauth/authorize",
				"token_url":         "https://github.com/login/oauth/access_token",
				"userinfo_url":      "https://api.github.com/user",
				"client_id":         "cid-gh",
				"client_secret":     "sec-gh",
				"scopes":            []interface{}{"read:user", "user:email"},
			},
			Enabled: true,
		})
	}
	_, _, cleanup := setupMultiIdPTest(t, csUserHandler)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/login?idp=github&redirect_to=/console", nil)
	c.Request = c.Request.WithContext(tenant.WithSlug(c.Request.Context(), "acme"))
	c.Set("tenant_slug", "acme")

	LoginMultiIdP(c)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body=%s)", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") {
		t.Errorf("expected github authorize URL, got %s", loc)
	}
	if !strings.Contains(loc, "client_id=cid-gh") {
		t.Errorf("client_id missing: %s", loc)
	}
	if !strings.Contains(loc, "scope=read") {
		t.Errorf("scope missing: %s", loc)
	}
	if !strings.Contains(loc, "state=midp.") {
		t.Errorf("state should carry midp. prefix: %s", loc)
	}
}

// TestLoginMultiIdP_LegacyFallback — when multi-IdP is disabled (nil
// singleton), the wrapper delegates to the legacy Casdoor Login.
func TestLoginMultiIdP_LegacyFallback(t *testing.T) {
	prev := multiIdP
	multiIdP = nil
	defer func() { multiIdP = prev }()

	// Construct a real CasdoorClient against a fake endpoint so the
	// legacy Login path doesn't crash. We assert only that the resulting
	// redirect points at Casdoor — confirming the wrapper delegated.
	prevClient := CasdoorClient
	CasdoorClient = casdoor.NewClient(&config.CasdoorConfig{
		Endpoint:    "http://casdoor.test/",
		ClientID:    "cid",
		Secret:      "sec",
		CallbackURL: "http://localhost:8080/api/auth/callback",
		Organization: "org",
	})
	defer func() { CasdoorClient = prevClient }()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/login", nil)

	LoginMultiIdP(c)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body=%s)", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://casdoor.test/") {
		t.Errorf("legacy Casdoor flow expected, got %s", loc)
	}
}

// TestCallbackMultiIdP_FullFlow — happy path: state validates, code
// exchanges, userinfo fetches, user is provisioned + bound + mapped, and
// a JWT is reissued as the cookie value.
func TestCallbackMultiIdP_FullFlow(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauth.TokenResponse{AccessToken: "atok-gh", TokenType: "bearer"})
	}))
	userinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer atok-gh" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"login":"octo","name":"Octo Cat","email":"octo@gh.com","avatar_url":"http://a/u"}`))
	}))
	defer tokenSrv.Close()
	defer userinfoSrv.Close()

	csUserHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.InternalIdPSourceView{
			Provider: "github",
			Config: map[string]interface{}{
				"authorization_url": "https://github.com/login/oauth/authorize",
				"token_url":         tokenSrv.URL,
				"userinfo_url":      userinfoSrv.URL,
				"client_id":         "cid",
				"client_secret":     "sec",
				"scopes":            []string{"read:user"},
			},
			Enabled: true,
		})
	}

	_, _, cleanup := setupMultiIdPTest(t, csUserHandler)
	defer cleanup()

	// Force JWTSignMode=dual so ReissueToken fires.
	multiIdP.JWTSignMode = config.JWTSignModeDual

	state, err := multiIdP.encodeMultiIdPState(multiIdPStatePayload{
		TenantID:    "acme",
		Provider:    "github",
		RedirectTo:  "/console",
		CallbackURL: "http://localhost:8080/api/auth/callback",
	})
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)

	CallbackMultiIdP(c)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body=%s)", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/console") {
		t.Errorf("expected redirect to /console, got %s", loc)
	}

	stub, ok := multiIdP.UserWriter.(*stubMultiIdPUserWriter)
	if !ok {
		t.Fatalf("writer not stub: %T", multiIdP.UserWriter)
	}
	if stub.created == nil {
		t.Fatal("GetOrCreateUser not called")
	}
	if stub.created.Sub != "42" {
		t.Errorf("subject: got %q want 42", stub.created.Sub)
	}
	if stub.created.Email != "octo@gh.com" {
		t.Errorf("email: got %q", stub.created.Email)
	}
	if stub.created.Provider != "github" {
		t.Errorf("provider: got %q", stub.created.Provider)
	}
	if stub.bound == nil {
		t.Error("BindIdentityToUser not called")
	}
	// Slice 2: enterprise mapping is now auto-triggered inside cs-user's
	// GetOrCreateUser (not via a separate server-side ApplyEnterpriseMapping
	// call), so stub.mapped is no longer set here. The ExternalClaims payload
	// is verified separately by TestCallbackMultiIdP_ForwardsExternalClaims.
	if stub.reissued == nil {
		t.Error("ReissueToken not called (mode=dual should trigger it)")
	}

	// Cookie should carry the reissued JWT, not the OAuth access token.
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "zgsmAdminToken=jwt-from-cs-user") {
		t.Errorf("cookie should carry reissued JWT: %s", setCookie)
	}
	if strings.Contains(setCookie, "zgsmAdminToken=atok-gh") {
		t.Errorf("cookie should NOT carry the raw OAuth token: %s", setCookie)
	}
}

// TestCallbackMultiIdP_ForwardsExternalClaims verifies Slice 2 plumbing: the
// raw IdP userinfo map (profile.Raw) is forwarded as JWTClaims.ExternalClaims
// when GetOrCreateUser is called. cs-user's GetOrCreateUser auto-triggers
// ApplyEnterpriseMapping using this payload + the tenant's field_map.
func TestCallbackMultiIdP_ForwardsExternalClaims(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauth.TokenResponse{AccessToken: "atok-wx", TokenType: "bearer"})
	}))
	// wxwork-style userinfo: IdP-specific fields beyond standard OIDC.
	userinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"UserId":"wx_alice_001","JobNumber":"E-10042","Department":"R&D","Name":"alice","Email":"alice@wx.com"}`))
	}))
	defer tokenSrv.Close()
	defer userinfoSrv.Close()

	csUserHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.InternalIdPSourceView{
			Provider: "wxwork",
			Config: map[string]interface{}{
				"authorization_url": "https://example/authorize",
				"token_url":         tokenSrv.URL,
				"userinfo_url":      userinfoSrv.URL,
				"client_id":         "cid",
				"client_secret":     "sec",
				"scopes":            []string{"user_info"},
			},
			Enabled: true,
		})
	}

	_, _, cleanup := setupMultiIdPTest(t, csUserHandler)
	defer cleanup()

	state, err := multiIdP.encodeMultiIdPState(multiIdPStatePayload{
		TenantID:    "acme",
		Provider:    "wxwork",
		RedirectTo:  "/console",
		CallbackURL: "http://localhost:8080/api/auth/callback",
	})
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)

	CallbackMultiIdP(c)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body=%s)", w.Code, w.Body.String())
	}

	stub, ok := multiIdP.UserWriter.(*stubMultiIdPUserWriter)
	if !ok {
		t.Fatalf("writer not stub: %T", multiIdP.UserWriter)
	}
	if stub.created == nil {
		t.Fatal("GetOrCreateUser not called")
	}
	if stub.created.Provider != "wxwork" {
		t.Errorf("Provider: got %q, want wxwork", stub.created.Provider)
	}
	if stub.created.ExternalClaims == nil {
		t.Fatal("ExternalClaims: got nil, want raw userinfo map")
	}
	// Verify each IdP-specific field survived the profile.Raw → JWTClaims hop.
	if got := stub.created.ExternalClaims["UserId"]; got != "wx_alice_001" {
		t.Errorf("ExternalClaims[UserId]: got %v, want wx_alice_001", got)
	}
	if got := stub.created.ExternalClaims["JobNumber"]; got != "E-10042" {
		t.Errorf("ExternalClaims[JobNumber]: got %v, want E-10042", got)
	}
	if got := stub.created.ExternalClaims["Department"]; got != "R&D" {
		t.Errorf("ExternalClaims[Department]: got %v, want R&D", got)
	}
}

// TestCallbackMultiIdP_RejectsTamperedState — HMAC mismatch → 400.
func TestCallbackMultiIdP_RejectsTamperedState(t *testing.T) {
	noop := func(http.ResponseWriter, *http.Request) {}
	_, _, cleanup := setupMultiIdPTest(t, noop)
	defer cleanup()

	state, _ := multiIdP.encodeMultiIdPState(multiIdPStatePayload{
		TenantID: "acme", Provider: "github",
	})
	// Tamper: flip last char of the signature.
	tampered := state[:len(state)-1]
	if state[len(state)-1] == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+tampered, nil)

	CallbackMultiIdP(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for tampered state, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestCallbackMultiIdP_RejectsExpiredState — IssuedAt beyond TTL → 400.
func TestCallbackMultiIdP_RejectsExpiredState(t *testing.T) {
	noop := func(http.ResponseWriter, *http.Request) {}
	_, _, cleanup := setupMultiIdPTest(t, noop)
	defer cleanup()

	payload := multiIdPStatePayload{
		TenantID: "acme",
		Provider: "github",
		IssuedAt: time.Now().Add(-2 * multiIdPStateTTL).Unix(),
	}
	body, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	h := multiIdP
	st := "midp." + encoded + "." + h.signState(encoded)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+st, nil)

	CallbackMultiIdP(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for expired state, got %d", w.Code)
	}
}

// TestCallbackMultiIdP_DisabledIdPRejected — if cs-user marks the IdP
// disabled mid-flow, the callback bails out rather than provisioning.
func TestCallbackMultiIdP_DisabledIdPRejected(t *testing.T) {
	csUserHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idp.InternalIdPSourceView{
			Provider: "github",
			Config: map[string]interface{}{
				"authorization_url": "x", "token_url": "y", "userinfo_url": "z",
				"client_id": "cid", "client_secret": "sec",
			},
			Enabled: false,
		})
	}
	_, _, cleanup := setupMultiIdPTest(t, csUserHandler)
	defer cleanup()

	state, _ := multiIdP.encodeMultiIdPState(multiIdPStatePayload{
		TenantID: "acme", Provider: "github",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)

	CallbackMultiIdP(c)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 (error redirect), got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=idp_disabled") {
		t.Errorf("expected idp_disabled error redirect, got %s", loc)
	}
}
