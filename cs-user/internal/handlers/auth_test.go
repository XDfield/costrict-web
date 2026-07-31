package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

// stubEmploymentReader lets handler tests pin service responses without a DB.
//
// externalKeyFn drives GetSubjectIDByExternalKey — the PRIMARY resolution
// path. userFn drives GetUserByID — the authoritative user row carrying
// tenant_id. fn drives GetEmploymentIdentity. applyCalls captures
// ApplyEnterpriseMapping invocations so tests can assert ExternalClaims
// forwarding.
type stubEmploymentReader struct {
	fn            func(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error)
	applyCalls    *[]user.EmploymentMappingParams
	externalKeyFn func(ctx context.Context, externalKey string) (string, error)
	userFn        func(ctx context.Context, subjectID string) (*models.User, error)
}

func (s stubEmploymentReader) GetEmploymentIdentity(ctx context.Context, id string) (*models.EmploymentIdentity, error) {
	return s.fn(ctx, id)
}

func (s stubEmploymentReader) ApplyEnterpriseMapping(_ context.Context, params user.EmploymentMappingParams) error {
	if s.applyCalls != nil {
		*s.applyCalls = append(*s.applyCalls, params)
	}
	return nil
}

func (s stubEmploymentReader) GetSubjectIDByExternalKey(ctx context.Context, extKey string) (string, error) {
	if s.externalKeyFn == nil {
		return "", nil
	}
	return s.externalKeyFn(ctx, extKey)
}

func (s stubEmploymentReader) GetUserByID(ctx context.Context, subjectID string) (*models.User, error) {
	if s.userFn == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return s.userFn(ctx, subjectID)
}

// stubPermissionReader lets handler tests pin GetPlatformAdmin +
// ListActiveTenantRoles responses without a DB. Phase C1.
type stubPermissionReader struct {
	platformFn    func(ctx context.Context, userSubjectID string) (*models.PlatformAdmin, error)
	tenantRolesFn func(ctx context.Context, userSubjectID, tenantID string) ([]string, error)
}

func (s stubPermissionReader) GetPlatformAdmin(ctx context.Context, id string) (*models.PlatformAdmin, error) {
	return s.platformFn(ctx, id)
}

func (s stubPermissionReader) ListActiveTenantRoles(ctx context.Context, userSubjectID, tenantID string) ([]string, error) {
	return s.tenantRolesFn(ctx, userSubjectID, tenantID)
}

// stubTenantReader pins TenantReader.ResolveBySlug responses.
type stubTenantReader struct {
	fn func(ctx context.Context, idOrSlug string) (*models.Tenant, error)
}

func (s stubTenantReader) ResolveBySlug(ctx context.Context, idOrSlug string) (*models.Tenant, error) {
	return s.fn(ctx, idOrSlug)
}

// newAuthAPI builds a minimal gin engine wired only with the reissue-token
// route. Returns the api + engine so each test injects its own stubs.
func newAuthAPI(svc EmploymentReader, signer *auth.Signer, jwtCfg config.JWTConfig) (*AuthAPI, *gin.Engine) {
	api := &AuthAPI{Svc: svc, Signer: signer, JWT: jwtCfg}
	r := gin.New()
	r.POST("/api/internal/users/reissue-token", api.ReissueToken)
	return api, r
}

// newTestSigner generates a fresh RSA-2048 key + constructs a *auth.Signer
// via the production NewSignerFromPEM path. Each test gets its own key.
func newTestSigner(t *testing.T) (*auth.Signer, *rsa.PrivateKey) {
	t.Helper()
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s, err := auth.NewSignerFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("NewSignerFromPEM: %v", err)
	}
	return s, pk
}

// defaultJWTCfg returns a valid JWTConfig for tests.
func defaultJWTCfg() config.JWTConfig {
	return config.JWTConfig{
		Issuer:          "test-issuer",
		TTL:             time.Hour,
		DefaultAudience: []string{"costrict-web"},
	}
}

// startMockCasdoorJWKS spins up an httptest server that returns the public
// half of `key` under `kid` as a JWK. Close it yourself per test.
func startMockCasdoorJWKS(t *testing.T, key *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	nBytes := key.N.Bytes()
	eBytes := []byte{byte(key.E >> 24), byte(key.E >> 16), byte(key.E >> 8), byte(key.E)}
	for len(eBytes) > 0 && eBytes[0] == 0 {
		eBytes = eBytes[1:]
	}
	jwk := auth.JWK{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(nBytes),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(auth.JWKS{Keys: []auth.JWK{jwk}})
	}))
}

// signCasdoorJWTForHandler produces a raw RS256 JWT signed by `key` with the
// given kid + claims. Mirrors what Casdoor would actually emit.
func signCasdoorJWTForHandler(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return signed
}

// newVerifiedCasdoorBundle wires a Casdoor verifier backed by an httptest
// JWKS serving `key` under `kid`. Returns the verifier + cleanup func.
func newVerifiedCasdoorBundle(t *testing.T) (*auth.CasdoorVerifier, *rsa.PrivateKey, string, func()) {
	t.Helper()
	casdoorKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	const kid = "casdoor-kid-1"
	jwks := startMockCasdoorJWKS(t, &casdoorKey.PublicKey, kid)
	v := auth.NewCasdoorVerifier(jwks.URL, "", nil, time.Second, time.Minute)
	return v, casdoorKey, kid, jwks.Close
}

// defaultHappyStubs wires the canonical "user exists in default tenant"
// reader/tenant/permission stubs. subjectID = "usr_alice", tenant="default".
func defaultHappyStubs(t *testing.T, externalKey, subjectID, tenantID string) (stubEmploymentReader, stubTenantReader) {
	t.Helper()
	svc := stubEmploymentReader{
		fn: func(_ context.Context, id string) (*models.EmploymentIdentity, error) {
			if id != subjectID {
				t.Errorf("GetEmploymentIdentity id: got %q, want %q", id, subjectID)
			}
			return nil, nil
		},
		externalKeyFn: func(_ context.Context, k string) (string, error) {
			if k != externalKey {
				t.Errorf("external_key: got %q, want %q", k, externalKey)
			}
			return subjectID, nil
		},
		userFn: func(_ context.Context, id string) (*models.User, error) {
			if id != subjectID {
				t.Errorf("GetUserByID id: got %q, want %q", id, subjectID)
			}
			return &models.User{SubjectID: subjectID, TenantID: tenantID}, nil
		},
	}
	tenant := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			if idOrSlug != tenantID {
				t.Errorf("ResolveBySlug arg: got %q, want %q", idOrSlug, tenantID)
			}
			return &models.Tenant{TenantID: tenantID, Slug: "default"}, nil
		},
	}
	return svc, tenant
}

// TestReissueToken_HappyPath verifies the full new flow: cs-user verifies
// the raw Casdoor JWT, resolves subject_id via external_key, loads the user
// row for tenant_id, joins tenant slug, then signs.
func TestReissueToken_HappyPath(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_alice"
	const tenantID = "default"
	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", subjectID, tenantID)
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"name":         "Alice Lee",
		"email":        "alice@example.com",
		"provider":     "idtrust",
		// idtrust provider in NormalizeClaimsMap overrides name = username =
		// firstNonEmpty(providerUserID, providerUsername). Without this the
		// name would fall back to stableNameFromSubject(universal_id). Mirror
		// of what a real Casdoor idtrust payload carries.
		"properties": map[string]any{
			"oauth_Custom_id": "Alice Lee",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if resp.Token == "" {
		t.Fatal("token empty")
	}
	parsed, err := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			t.Fatalf("unexpected alg: %v", tok.Header["alg"])
		}
		return &pk.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	got, ok := parsed.Claims.(*auth.EnterpriseClaims)
	if !ok {
		t.Fatalf("claims type: %T", parsed.Claims)
	}
	if got.Subject != subjectID {
		t.Errorf("Subject: got %q, want %q", got.Subject, subjectID)
	}
	if got.Issuer != "test-issuer" {
		t.Errorf("Issuer: got %q, want test-issuer", got.Issuer)
	}
	if got.Name != "Alice Lee" {
		t.Errorf("Name: got %q, want Alice Lee", got.Name)
	}
	if got.TenantID != tenantID {
		t.Errorf("TenantID: got %q, want %q", got.TenantID, tenantID)
	}
	if got.TenantSlug != "default" {
		t.Errorf("TenantSlug: got %q, want default", got.TenantSlug)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "costrict-web" {
		t.Errorf("Audience: got %v, want [costrict-web]", got.Audience)
	}
	if got.Expiry == nil || !got.Expiry.Equal(resp.ExpiresAt) {
		t.Errorf("ExpiresAt mismatch: response=%v claims=%v", resp.ExpiresAt, got.Expiry)
	}
}

// TestReissueToken_NoCasdoorJWT_Returns400 locks the new contract:
// casdoor_jwt is required; missing it surfaces as 400 (binding error), and
// the service is never called.
func TestReissueToken_NoCasdoorJWT_Returns400(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("reader should not be called when casdoor_jwt missing")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

// TestReissueToken_IgnoresServerSuppliedSubjectAndTenant is the load-bearing
// contract: server may still forward legacy user_subject_id / tenant_id /
// tenant_slug / identity fields in the JSON body (e.g. during the rolling
// deploy), but cs-user MUST NOT honor them — the issued token's subject /
// tenant / slug come exclusively from cs-user's own verification + reverse
// lookup. The request shape ignores those fields at unmarshal time; this
// test pins the behavior by stuffing bogus values into the raw JSON.
func TestReissueToken_IgnoresServerSuppliedSubjectAndTenant(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const realSubject = "usr_cs_authoritative"
	const realTenant = "t-real"
	// Stubs only honor the realSubject / realTenant; if the handler used
	// server-supplied "usr_evil" / "t-evil" the stubs would fail assertions.
	svc := stubEmploymentReader{
		fn: func(_ context.Context, id string) (*models.EmploymentIdentity, error) {
			if id != realSubject {
				t.Errorf("GetEmploymentIdentity called with server-supplied %q (forbidden)", id)
			}
			return nil, nil
		},
		externalKeyFn: func(_ context.Context, k string) (string, error) {
			if k != "casdoor:idtrust:uuid-real" {
				t.Errorf("external_key: got %q", k)
			}
			return realSubject, nil
		},
		userFn: func(_ context.Context, id string) (*models.User, error) {
			if id != realSubject {
				t.Errorf("GetUserByID called with server-supplied %q (forbidden)", id)
			}
			return &models.User{SubjectID: realSubject, TenantID: realTenant}, nil
		},
	}
	tenantR := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			if idOrSlug != realTenant {
				t.Errorf("ResolveBySlug arg: got %q, want %q (server-supplied tenant ignored)", idOrSlug, realTenant)
			}
			return &models.Tenant{TenantID: realTenant, Slug: "real-slug"}, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-real",
		"universal_id": "uuid-real",
		"name":         "Real User",
		"provider":     "idtrust",
		// idtrust override: name = username = providerUserID. See
		// TestReissueToken_HappyPath fixture note for context.
		"properties": map[string]any{
			"oauth_Custom_id": "Real User",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	// Hand-rolled JSON with bogus legacy fields to prove they don't influence
	// the outcome. struct urllib wouldn't marshal unknown fields.
	rawBody := `{"casdoor_jwt":` + jsonQuote(raw) + `,` +
		`"user_subject_id":"usr_evil",` +
		`"tenant_id":"t-evil",` +
		`"tenant_slug":"evil-slug",` +
		`"identity":{"universal_id":"uuid-evil","name":"Evil"}}`

	req := httptest.NewRequest(http.MethodPost, "/api/internal/users/reissue-token", strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rw.Code, rw.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &resp)
	parsed, _ := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	got, _ := parsed.Claims.(*auth.EnterpriseClaims)
	if got.Subject != realSubject {
		t.Errorf("Subject: got %q, want %q (server-supplied usr_evil ignored)", got.Subject, realSubject)
	}
	if got.TenantID != realTenant {
		t.Errorf("TenantID: got %q, want %q (server-supplied t-evil ignored)", got.TenantID, realTenant)
	}
	if got.TenantSlug != "real-slug" {
		t.Errorf("TenantSlug: got %q, want real-slug (server-supplied evil-slug ignored)", got.TenantSlug)
	}
	if got.Name != "Real User" {
		t.Errorf("Name: got %q, want Real User (server-supplied identity ignored)", got.Name)
	}
}

// TestReissueToken_VerifiedButUserNotFound_Returns404 verifies the contract
// that "verification passed but GetSubjectIDByExternalKey returned empty"
// surfaces as 404 — caller treats it as "GetOrCreate hasn't run yet", not a
// retryable auth failure.
func TestReissueToken_VerifiedButUserNotFound_Returns404(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("GetEmploymentIdentity must not be called when user is not provisioned")
			return nil, nil
		},
		externalKeyFn: func(context.Context, string) (string, error) { return "", nil },
		userFn: func(context.Context, string) (*models.User, error) {
			t.Fatal("GetUserByID must not be called when external_key misses")
			return nil, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-ghost",
		"universal_id": "uuid-ghost",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "user not provisioned") {
		t.Errorf("body: want 'user not provisioned', got %s", w.Body.String())
	}
}

// TestReissueToken_AppliesEnterpriseMappingFromExternalClaims verifies the
// reissue-token flow forwards ExternalClaims (harvested from the verified
// JWT's properties.oauth_Custom.* + signupApplication) to ApplyEnterpriseMapping.
func TestReissueToken_AppliesEnterpriseMappingFromExternalClaims(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_alice"
	const tenantID = "default"
	var applyCalls []user.EmploymentMappingParams
	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", subjectID, tenantID)
	svc.applyCalls = &applyCalls
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":                "uni-alice",
		"universal_id":       "uuid-alice",
		"provider":           "idtrust",
		"signupApplication":  "idtrust",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"properties": map[string]any{
			"oauth_Custom": map[string]any{
				"id":             "alice-idtrust-uid",
				"employeeNumber": "E-1001",
			},
		},
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(applyCalls) != 1 {
		t.Fatalf("ApplyEnterpriseMapping calls: got %d, want 1", len(applyCalls))
	}
	got := applyCalls[0]
	if got.UserSubjectID != subjectID {
		t.Errorf("UserSubjectID: got %q, want %q", got.UserSubjectID, subjectID)
	}
	if got.TenantID != tenantID {
		t.Errorf("TenantID: got %q, want %q", got.TenantID, tenantID)
	}
	if got.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust", got.Provider)
	}
	if _, ok := got.ExternalClaims["properties"]; !ok {
		t.Errorf("ExternalClaims forwarded without properties: %+v", got.ExternalClaims)
	}
	if _, ok := got.ExternalClaims["signupApplication"]; !ok {
		t.Errorf("ExternalClaims forwarded without signupApplication: %+v", got.ExternalClaims)
	}
}

// TestReissueToken_NoEnterpriseFieldsStillIssuesToken verifies the path
// where the verified Casdoor JWT carries no enterprise fields (no
// properties.*, no signupApplication, no top-level IdP fields): the field_map
// walker inside ApplyEnterpriseMapping finds nothing and produces no columns,
// but the handler still issues a valid token. ApplyEnterpriseMapping is
// invoked unconditionally — the new harvest spills the full payload, so
// ExternalClaims is always non-nil when a JWT verifies. Whether columns get
// written is the walker's concern (deeper-level tests cover that).
func TestReissueToken_NoEnterpriseFieldsStillIssuesToken(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_alice"
	const tenantID = "default"
	var applyCalls []user.EmploymentMappingParams
	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", subjectID, tenantID)
	svc.applyCalls = &applyCalls
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
		// no properties / signupApplication / top-level IdP fields — minimal
		// JWT. field_map walker will find nothing, no columns written.
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	// ApplyEnterpriseMapping IS called (full-spill harvest keeps ExternalClaims
	// non-nil for any verified JWT); the field_map walker simply writes no
	// columns. Asserting the call fires here locks the contract that the
	// reissue-token handler always re-evaluates enterprise fields on login.
	if len(applyCalls) != 1 {
		t.Errorf("ApplyEnterpriseMapping calls: got %d, want 1 (field_map walker decides columns, not handler)", len(applyCalls))
	}
}

// TestReissueToken_NilSignerMaps503 verifies the missing-signer path surfaces
// as 503, mirroring the JWKS endpoint's contract.
func TestReissueToken_NilSignerMaps503(t *testing.T) {
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("reader should not be called when signer is nil")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, nil, defaultJWTCfg())

	body := reissueTokenRequest{CasdoorJWT: "header.payload.sig"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "JWT signing not configured") {
		t.Errorf("body = %s, want substring 'JWT signing not configured'", w.Body.String())
	}
}

// TestReissueToken_CasdoorJWTButVerifierUnconfiguredReturns503 locks the
// operational misconfig path: a caller forwards casdoor_jwt but the
// deployment hasn't set CS_USER_CASDOOR_JWKS_URL. Surface 503 (not 500).
func TestReissueToken_CasdoorJWTButVerifierUnconfiguredReturns503(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("GetEmploymentIdentity must not be called on verifier-missing path")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())
	// CasdoorVerifier intentionally left nil.

	body := reissueTokenRequest{CasdoorJWT: "header.payload.sig"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
}

// TestReissueToken_VerifiesCasdoorJWT_InvalidReturns401 asserts a JWT signed
// by the wrong key fails the verifier and the handler surfaces 401. This is
// the load-bearing security contract — a forged token must never reach the
// signing path.
func TestReissueToken_VerifiesCasdoorJWT_InvalidReturns401(t *testing.T) {
	signer, _ := newTestSigner(t)
	jwksKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	signerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	const kid = "casdoor-kid-1"
	jwks := startMockCasdoorJWKS(t, &jwksKey.PublicKey, kid)
	defer jwks.Close()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("GetEmploymentIdentity must not be called when verification fails")
			return nil, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = auth.NewCasdoorVerifier(jwks.URL, "", nil, time.Second, time.Minute)

	raw := signCasdoorJWTForHandler(t, signerKey, kid, jwt.MapClaims{
		"sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid casdoor jwt") {
		t.Errorf("body: want 'invalid casdoor jwt', got %s", w.Body.String())
	}
}

// TestReissueToken_GetSubjectIDByExternalKeyErrorMaps500 verifies a DB-side
// fault during the authoritative subject_id lookup surfaces as 500.
func TestReissueToken_GetSubjectIDByExternalKeyErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("GetEmploymentIdentity should not be called when subject lookup errors")
			return nil, nil
		},
		externalKeyFn: func(context.Context, string) (string, error) {
			return "", errors.New("db dead")
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-x",
		"universal_id": "uuid-x",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "db dead") {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

// TestReissueToken_GetUserByIDErrorMaps500 verifies a DB-side fault on
// GetUserByID (non-RecordNotFound) surfaces as 500.
func TestReissueToken_GetUserByIDErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("GetEmploymentIdentity should not be called when user load errors")
			return nil, nil
		},
		externalKeyFn: func(context.Context, string) (string, error) {
			return "usr_x", nil
		},
		userFn: func(context.Context, string) (*models.User, error) {
			return nil, errors.New("db dead")
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-x",
		"universal_id": "uuid-x",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestReissueToken_EmptyTenantIDOnUserMaps500 locks the data-integrity guard:
// a user row with empty tenant_id signals backfill gap / schema drift.
// Surface as 500 (loud) — never silently sign a tenant-less token.
func TestReissueToken_EmptyTenantIDOnUserMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			return nil, nil
		},
		externalKeyFn: func(context.Context, string) (string, error) { return "usr_x", nil },
		userFn: func(context.Context, string) (*models.User, error) {
			return &models.User{SubjectID: "usr_x", TenantID: ""}, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-x",
		"universal_id": "uuid-x",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestReissueToken_TenantLookupErrorMaps500 verifies a tenant lookup fault
// surfaces as 500.
func TestReissueToken_TenantLookupErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn:            func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
		externalKeyFn: func(context.Context, string) (string, error) { return "usr_x", nil },
		userFn: func(context.Context, string) (*models.User, error) {
			return &models.User{SubjectID: "usr_x", TenantID: "default"}, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = stubTenantReader{
		fn: func(context.Context, string) (*models.Tenant, error) {
			return nil, errors.New("db dead")
		},
	}

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-x",
		"universal_id": "uuid-x",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestReissueToken_GetEmploymentIdentityErrorMaps500 verifies a DB-side fault
// on the employment read surfaces as 500.
func TestReissueToken_GetEmploymentIdentityErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			return nil, errors.New("db dead")
		},
		externalKeyFn: func(context.Context, string) (string, error) { return "usr_x", nil },
		userFn: func(context.Context, string) (*models.User, error) {
			return &models.User{SubjectID: "usr_x", TenantID: "default"}, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			return &models.Tenant{TenantID: idOrSlug, Slug: "default"}, nil
		},
	}

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-x",
		"universal_id": "uuid-x",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "db dead") {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

// TestReissueToken_NoEmploymentRowStillIssuesToken verifies graceful
// degradation: a user without an employment_identities row still gets a valid
// token — just without enterprise claims.
func TestReissueToken_NoEmploymentRowStillIssuesToken(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_alice"
	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", subjectID, "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("token should still be issued")
	}
}

// TestReissueToken_AudienceOverride verifies the request-level audience
// override wins over the config default.
func TestReissueToken_AudienceOverride(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", "usr_alice", "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{
		CasdoorJWT: raw,
		Audience:   []string{"csc-cli", "ops-portal"},
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	parsed, _ := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	got, _ := parsed.Claims.(*auth.EnterpriseClaims)
	if len(got.Audience) != 2 || got.Audience[0] != "csc-cli" || got.Audience[1] != "ops-portal" {
		t.Errorf("Audience override: got %v, want [csc-cli ops-portal]", got.Audience)
	}
}

// TestReissueToken_BadJSONMaps400 verifies a malformed body surfaces as 400
// before the verifier is consulted.
func TestReissueToken_BadJSONMaps400(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("reader should not be called on bad JSON")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	req := httptest.NewRequest(http.MethodPost, "/api/internal/users/reissue-token",
		strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rw.Code)
	}
}

// --- Phase C1: permission claims wiring ---

func noPermReader() stubPermissionReader {
	return stubPermissionReader{
		platformFn:    func(context.Context, string) (*models.PlatformAdmin, error) { return nil, nil },
		tenantRolesFn: func(context.Context, string, string) ([]string, error) { return nil, nil },
	}
}

// TestReissueToken_PermissionClaimsPopulated verifies the happy path: when
// Permissions is wired and the user has both platform_admin + tenant_admin
// rows, the corresponding JWT claims surface in the signed token. Roles
// lookup runs against the cs-user-resolved tenant_id, never a server value.
func TestReissueToken_PermissionClaimsPopulated(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_root"
	const tenantID = "default"
	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-root", subjectID, tenantID)
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR
	api.Permissions = stubPermissionReader{
		platformFn: func(_ context.Context, id string) (*models.PlatformAdmin, error) {
			if id != subjectID {
				t.Errorf("platform lookup id: got %q, want %q", id, subjectID)
			}
			return &models.PlatformAdmin{UserID: id, Scope: models.PlatformScopeFull}, nil
		},
		tenantRolesFn: func(_ context.Context, id, tenant string) ([]string, error) {
			if id != subjectID || tenant != tenantID {
				t.Errorf("tenant_roles lookup args: id=%q tenant=%q", id, tenant)
			}
			return []string{models.TenantRoleOwner}, nil
		},
	}

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-root",
		"universal_id": "uuid-root",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	parsed, _ := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	got, _ := parsed.Claims.(*auth.EnterpriseClaims)
	if !got.PlatformAdmin {
		t.Errorf("PlatformAdmin: got false, want true")
	}
	if got.PlatformScope != models.PlatformScopeFull {
		t.Errorf("PlatformScope: got %q, want %q", got.PlatformScope, models.PlatformScopeFull)
	}
	if len(got.TenantRoles) != 1 || got.TenantRoles[0] != models.TenantRoleOwner {
		t.Errorf("TenantRoles: got %v, want [owner]", got.TenantRoles)
	}
}

// TestReissueToken_NoPermissionReaderStillIssuesToken verifies graceful
// degradation: when Permissions is nil (灰度 rollout), the issued token
// simply omits the permission claims.
func TestReissueToken_NoPermissionReaderStillIssuesToken(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", "usr_alice", "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR
	// api.Permissions intentionally left nil — Phase A 灰度 mode.

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	parsed, _ := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	got, _ := parsed.Claims.(*auth.EnterpriseClaims)
	if got.PlatformAdmin {
		t.Errorf("PlatformAdmin should be false when Permissions is nil")
	}
	if got.PlatformScope != "" {
		t.Errorf("PlatformScope should be empty when Permissions is nil, got %q", got.PlatformScope)
	}
	if len(got.TenantRoles) != 0 {
		t.Errorf("TenantRoles should be empty when Permissions is nil, got %v", got.TenantRoles)
	}
}

// TestReissueToken_RegularMemberOmitsClaims verifies the omitempty path at
// the wire level: a regular tenant member gets a token that does NOT carry
// tenant_roles / platform_admin / platform_scope keys.
func TestReissueToken_RegularMemberOmitsClaims(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-regular", "usr_regular", "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR
	api.Permissions = noPermReader()

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-regular",
		"universal_id": "uuid-regular",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	parsed, _ := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	if parsed == nil {
		t.Fatal("token did not parse")
	}
	got, _ := parsed.Claims.(*auth.EnterpriseClaims)
	if got.PlatformAdmin || got.PlatformScope != "" || len(got.TenantRoles) != 0 {
		t.Errorf("regular member should not carry permission claims: %+v", got)
	}
}

// TestReissueToken_PlatformLookupErrorMaps500 verifies DB-side faults in the
// platform-admin lookup surface as 500.
func TestReissueToken_PlatformLookupErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", "usr_alice", "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR
	api.Permissions = stubPermissionReader{
		platformFn:    func(context.Context, string) (*models.PlatformAdmin, error) { return nil, errors.New("db dead") },
		tenantRolesFn: func(context.Context, string, string) ([]string, error) { return nil, nil },
	}

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestReissueToken_TenantRolesLookupErrorMaps500 mirrors the above for the
// tenant-roles path.
func TestReissueToken_TenantRolesLookupErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", "usr_alice", "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR
	api.Permissions = stubPermissionReader{
		platformFn:    func(context.Context, string) (*models.PlatformAdmin, error) { return nil, nil },
		tenantRolesFn: func(context.Context, string, string) ([]string, error) { return nil, errors.New("db dead") },
	}

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-alice",
		"universal_id": "uuid-alice",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// jsonQuote is a tiny helper to embed a JSON-string-safe literal in raw test
// bodies. Uses encoding/json so escaping matches what server would emit.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
