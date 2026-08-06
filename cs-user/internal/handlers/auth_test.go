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
// forwarding. getOrCreateUserFn drives the inline-provisioning fallback
// — fires when externalKeyFn returns "".
type stubEmploymentReader struct {
	fn               func(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error)
	applyCalls       *[]user.EmploymentMappingParams
	externalKeyFn    func(ctx context.Context, externalKey string) (string, error)
	userFn           func(ctx context.Context, subjectID string) (*models.User, error)
	getOrCreateUserFn func(ctx context.Context, claims *models.JWTClaims) (*models.User, bool, error)
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

func (s stubEmploymentReader) GetOrCreateUser(ctx context.Context, claims *models.JWTClaims) (*models.User, bool, error) {
	if s.getOrCreateUserFn == nil {
		panic("GetOrCreateUser invoked on stub without getOrCreateUserFn — test setup bug")
	}
	return s.getOrCreateUserFn(ctx, claims)
}

// stubPermissionReader lets handler tests pin GetPlatformAdmin +
// ListActiveTenantRoles responses without a DB.
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
		Token       string         `json:"token"`
		ExpiresAt   time.Time      `json:"expires_at"`
		ExternalKey string         `json:"external_key"`
		SubjectID   string         `json:"subject_id"`
		Profile     reissueProfile `json:"profile"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if resp.Token == "" {
		t.Fatal("token empty")
	}
	// ExternalKey / SubjectID / Profile are the fields server reads to
	// retire its own unverified JWT parse. Lock the contract here so a
	// regression surfaces before server depends on it.
	if resp.ExternalKey != "casdoor:idtrust:uuid-alice" {
		t.Errorf("ExternalKey: got %q, want casdoor:idtrust:uuid-alice", resp.ExternalKey)
	}
	if resp.SubjectID != subjectID {
		t.Errorf("SubjectID: got %q, want %q", resp.SubjectID, subjectID)
	}
	if resp.Profile.UniversalID != "uuid-alice" {
		t.Errorf("Profile.UniversalID: got %q, want uuid-alice", resp.Profile.UniversalID)
	}
	if resp.Profile.Name != "Alice Lee" {
		t.Errorf("Profile.Name: got %q, want Alice Lee", resp.Profile.Name)
	}
	if resp.Profile.Email != "alice@example.com" {
		t.Errorf("Profile.Email: got %q, want alice@example.com", resp.Profile.Email)
	}
	if resp.Profile.Provider != "idtrust" {
		t.Errorf("Profile.Provider: got %q, want idtrust", resp.Profile.Provider)
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

// TestReissueToken_PhoneLoginBuildsPhoneExternalKey is the handler-level
// regression for the reissue-token 404 bug. Casdoor phone-login JWTs carry
// `phone` but no top-level `provider` claim; if the verifier's NormalizeClaimsMap
// doesn't run the phone fallback, BuildExternalKey produces
// `casdoor:<universal_id>` while GetOrCreateUser (driven by @server's
// NormalizeClaimsMap) wrote `casdoor:phone:<universal_id>` — reissue-token
// returns 404 and @server's OAuth callback falls back to writing the raw
// Casdoor token into the cookie. This test pins the contract end-to-end:
// phone-only JWT → externalKeyFn must receive `casdoor:phone:<universal_id>`.
// defaultHappyStubs already asserts the exact key, so a regression in
// provider derivation surfaces here as a lookup-key mismatch.
func TestReissueToken_PhoneLoginBuildsPhoneExternalKey(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_phone"
	const tenantID = "default"
	// external_key shape MUST match what @server's GetOrCreateUser RPC writes
	// for the same JWT (server derives provider="phone" from phone field).
	svc, tenantR := defaultHappyStubs(t, "casdoor:phone:b74e5889-phonelogin", subjectID, tenantID)
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	// Mirrors the real Casdoor phone-login payload that triggered the bug:
	// `phone` present, top-level `provider` ABSENT, universal_id present.
	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "b74e5889-phonelogin",
		"universal_id": "b74e5889-phonelogin",
		"phone":        "15986746954",
		"name":         "陈烜42766",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	// defaultHappyStubs.externalKeyFn already asserted the key shape inline;
	// reaching here with 200 means the phone fallback produced the correct
	// `casdoor:phone:<universal_id>` and the lookup succeeded.
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

// TestReissueToken_VerifiedButUserNotFound_ProvisionsInline verifies the
// inline-provisioning contract: when verification passes but
// GetSubjectIDByExternalKey returns "" (no row yet), ReissueToken provisions
// the user inline via GetOrCreateUser and returns 200 with is_new=true —
// NOT the old 404 "GetOrCreate hasn't run yet" path. The OAuth callback no
// longer needs to call GetOrCreateUser beforehand.
func TestReissueToken_VerifiedButUserNotFound_ProvisionsInline(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const newSubject = "usr_provisioned_inline"
	const newTenant = "default"
	var gocCalled bool
	svc := stubEmploymentReader{
		fn: func(_ context.Context, id string) (*models.EmploymentIdentity, error) {
			if id != newSubject {
				t.Errorf("GetEmploymentIdentity id: got %q, want %q (must use provisioned subject_id)", id, newSubject)
			}
			return nil, nil
		},
		externalKeyFn: func(_ context.Context, k string) (string, error) {
			// Confirm the external_key shape made it through; then return ""
			// to trigger the inline-provisioning fallback.
			if !strings.HasPrefix(k, "casdoor:") {
				t.Errorf("external_key shape: got %q", k)
			}
			return "", nil
		},
		userFn: func(_ context.Context, id string) (*models.User, error) {
			if id != newSubject {
				t.Errorf("GetUserByID id: got %q, want %q (post-provisioning load)", id, newSubject)
			}
			return &models.User{SubjectID: newSubject, TenantID: newTenant}, nil
		},
		getOrCreateUserFn: func(_ context.Context, claims *models.JWTClaims) (*models.User, bool, error) {
			gocCalled = true
			if claims == nil || claims.UniversalID != "uuid-ghost" {
				t.Errorf("GetOrCreateUser claims.UniversalID: got %q, want uuid-ghost", claimsOrEmpty(claims))
			}
			return &models.User{SubjectID: newSubject, TenantID: newTenant}, true, nil
		},
	}
	tenantR := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			if idOrSlug != newTenant {
				t.Errorf("ResolveBySlug arg: got %q, want %q", idOrSlug, newTenant)
			}
			return &models.Tenant{TenantID: newTenant, Slug: "default"}, nil
		},
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-ghost",
		"universal_id": "uuid-ghost",
		"provider":     "idtrust",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	body := reissueTokenRequest{CasdoorJWT: raw}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (inline provisioning), body=%s", w.Code, w.Body.String())
	}
	if !gocCalled {
		t.Errorf("GetOrCreateUser was not invoked — inline provisioning path did not run")
	}

	var resp struct {
		SubjectID string `json:"subject_id"`
		IsNew     bool   `json:"is_new"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.SubjectID != newSubject {
		t.Errorf("response subject_id: got %q, want %q", resp.SubjectID, newSubject)
	}
	if !resp.IsNew {
		t.Errorf("response is_new: got false, want true (user was just provisioned inline)")
	}
}

// claimsOrEmpty returns claims.UniversalID or "<nil>" for cleaner test failure output.
func claimsOrEmpty(c *models.JWTClaims) string {
	if c == nil {
		return "<nil>"
	}
	return c.UniversalID
}

// TestReissueToken_InlineProvisioningFailure_Returns500 verifies that when
// inline GetOrCreateUser fails (DB outage, etc.), ReissueToken surfaces 500
// — not 200 with a partial token. Failures are real data-integrity issues,
// not retryable auth failures.
func TestReissueToken_InlineProvisioningFailure_Returns500(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("GetEmploymentIdentity must not be called when provisioning fails")
			return nil, nil
		},
		externalKeyFn: func(context.Context, string) (string, error) { return "", nil },
		userFn: func(context.Context, string) (*models.User, error) {
			t.Fatal("GetUserByID must not be called when provisioning fails")
			return nil, nil
		},
		getOrCreateUserFn: func(context.Context, *models.JWTClaims) (*models.User, bool, error) {
			return nil, false, errors.New("simulated DB outage")
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
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body=%s", w.Code, w.Body.String())
	}
	// Privacy: error body must not leak verified claims.
	if strings.Contains(w.Body.String(), "external_key") {
		t.Errorf("error body leaked external_key field: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "profile") {
		t.Errorf("error body leaked profile field: %s", w.Body.String())
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

// --- permission claims wiring ---

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
// degradation: when Permissions is nil (deployments that haven't wired the
// permission readers yet), the issued token simply omits the permission
// claims.
func TestReissueToken_NoPermissionReaderStillIssuesToken(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-alice", "usr_alice", "default")
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.CasdoorVerifier = v
	api.TenantResolver = tenantR
	// api.Permissions intentionally left nil — graceful degradation when PermissionReader isn't wired.

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

// newVerifyEngine wires a minimal gin engine with only the verify-token route.
// Mirrors newAuthAPI's pattern — handler-level testing without the full app
// router. CasdoorVerifier + Signer are passed in so each test picks the
// configured / unconfigured combination explicitly.
func newVerifyEngine(api *AuthAPI) *gin.Engine {
	r := gin.New()
	r.POST("/api/internal/auth/verify", api.VerifyToken)
	return r
}

// signEnterpriseJWT issues a cs-user-shaped JWT via the test signer —
// exercises the local-verification fast path. Returns the raw JWT string.
func signEnterpriseJWT(t *testing.T, s *auth.Signer, claims *auth.EnterpriseClaims) string {
	t.Helper()
	signed, err := s.SignJWT(claims, time.Now())
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	return signed
}

// TestVerifyToken_CSUserToken verifies that a cs-user-signed JWT validates
// via the local public key path (no JWKS round-trip) and surfaces
// token_source="cs-user".
func TestVerifyToken_CSUserToken(t *testing.T) {
	signer, _ := newTestSigner(t)
	now := time.Now()
	claims, err := auth.NewEnterpriseClaims(auth.IssuanceParams{
		Issuer:   "test-issuer",
		Subject:  "usr_alice",
		ShortID:  "u-alice",
		TenantID: "default",
		TTL:      time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	claims.Email = "alice@example.com"
	raw := signEnterpriseJWT(t, signer, claims)

	api := &AuthAPI{Signer: signer, JWT: defaultJWTCfg()}
	r := newVerifyEngine(api)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp verifyTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Active || resp.TokenSource != "cs-user" {
		t.Errorf("active/source = %v/%q, want true/cs-user", resp.Active, resp.TokenSource)
	}
	if resp.Subject != "usr_alice" {
		t.Errorf("sub = %q, want usr_alice", resp.Subject)
	}
	if resp.Email != "alice@example.com" {
		t.Errorf("email = %q", resp.Email)
	}
	// Fast path MUST NOT populate the reissued fields — the gateway only
	// overwrites the browser cookie when these are present, so a false
	// positive here would pointlessly churn cookies on every request.
	if resp.ReissuedToken != "" {
		t.Errorf("reissued_token = %q, want empty on cs-user JWT fast path", resp.ReissuedToken)
	}
	if !resp.ReissuedExpiresAt.IsZero() {
		t.Errorf("reissued_expires_at = %v, want zero on cs-user JWT fast path", resp.ReissuedExpiresAt)
	}
}

// TestVerifyToken_CasdoorJWT_KnownUser_Reissues pins the fallback happy
// path: a Casdoor-signed JWT (not a cs-user JWT) is accepted by the
// CasdoorVerifier, the external_key resolves to an existing user row, and
// cs-user reissues a fresh cs-user JWT. The reissued token surfaces in
// `reissued_token` / `reissued_expires_at` so the gateway knows to overwrite
// the browser cookie. The primary token field on the response is also set
// to the cs-user-signed value (so callers reading `sub` / `tenant_id` /
// `exp` see the reissued token's claims, not the upstream Casdoor claims).
func TestVerifyToken_CasdoorJWT_KnownUser_Reissues(t *testing.T) {
	signer, pk := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	const subjectID = "usr_bob"
	const tenantID = "default"
	svc, tenantR := defaultHappyStubs(t, "casdoor:idtrust:uuid-bob", subjectID, tenantID)
	api := &AuthAPI{Svc: svc, Signer: signer, JWT: defaultJWTCfg(), CasdoorVerifier: v, TenantResolver: tenantR}
	r := newVerifyEngine(api)

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-bob",
		"universal_id": "uuid-bob",
		"name":         "Bob",
		"email":        "bob@idp.example",
		"provider":     "idtrust",
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp verifyTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if !resp.Active {
		t.Errorf("active = false, want true")
	}
	if resp.TokenSource != "cs-user" {
		t.Errorf("token_source = %q, want cs-user (post-reissue canonical source)", resp.TokenSource)
	}
	if resp.Subject != subjectID {
		t.Errorf("sub = %q, want %q (cs-user subject_id, not Casdoor sub)", resp.Subject, subjectID)
	}
	if resp.TenantID != tenantID {
		t.Errorf("tenant_id = %q, want %q", resp.TenantID, tenantID)
	}
	if resp.ReissuedToken == "" {
		t.Fatal("reissued_token empty — gateway cannot overwrite the browser cookie")
	}
	if resp.ReissuedToken == raw {
		t.Fatal("reissued_token equals the input Casdoor JWT — takeover did not happen")
	}
	if resp.ReissuedExpiresAt.IsZero() {
		t.Fatal("reissued_expires_at zero — gateway cannot compute cookie MaxAge")
	}

	// The reissued token MUST be a cs-user-signed JWT verifiable by the
	// cs-user signer's public key. Confirms the takeover produced a real
	// cs-user token, not just a different string.
	parsed, err := jwt.ParseWithClaims(resp.ReissuedToken, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("reissued_token did not verify as cs-user JWT: %v", err)
	}
	got, _ := parsed.Claims.(*auth.EnterpriseClaims)
	if got.Subject != subjectID {
		t.Errorf("reissued_token sub = %q, want %q", got.Subject, subjectID)
	}
	if got.TenantID != tenantID {
		t.Errorf("reissued_token tenant_id = %q, want %q", got.TenantID, tenantID)
	}
	// reissued_expires_at must match the JWT's exp claim 1:1.
	if got.Expiry == nil || !got.Expiry.Equal(resp.ReissuedExpiresAt) {
		t.Errorf("reissued_expires_at mismatch: response=%v claims=%v", resp.ReissuedExpiresAt, got.Expiry)
	}
}

// TestVerifyToken_CasdoorJWT_UnknownUser_Rejected pins the load-bearing
// contract: a Casdoor JWT that verifies but maps to NO existing user row
// MUST be rejected with 401. VerifyToken never provisions inline —
// provisioning belongs to ReissueToken (the OAuth callback path). Without
// this rejection a stolen Casdoor token could bootstrap a cs-user account
// on the very first authenticated request, bypassing the OAuth consent
// screen; and every authenticated request would amplify DB write load.
func TestVerifyToken_CasdoorJWT_UnknownUser_Rejected(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	// externalKeyFn returns "" → user not found. The handler MUST reject
	// (401), not call GetOrCreateUser.
	svc := stubEmploymentReader{
		externalKeyFn: func(_ context.Context, _ string) (string, error) { return "", nil },
	}
	api := &AuthAPI{Svc: svc, Signer: signer, JWT: defaultJWTCfg(), CasdoorVerifier: v}
	r := newVerifyEngine(api)

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-ghost",
		"universal_id": "uuid-ghost",
		"name":         "Ghost",
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (unknown user must not be provisioned)", w.Code)
	}
	var resp struct {
		Active          bool   `json:"active"`
		Error           string `json:"error"`
		ReissuedToken   string `json:"reissued_token"`
		ReissuedExpires time.Time `json:"reissued_expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Active {
		t.Errorf("active = true, want false")
	}
	if resp.ReissuedToken != "" {
		t.Errorf("reissued_token = %q, want empty on rejection", resp.ReissuedToken)
	}
}

// TestVerifyToken_CasdoorJWT_VerifyFails_Rejected pins the rejection path
// when the Casdoor fallback verifier itself rejects the JWT (bad signature,
// expired, wrong issuer, malformed). Must surface as 401, NOT 200/active=false
// — the gateway's standard auth-failure pipeline kicks in on 401.
func TestVerifyToken_CasdoorJWT_VerifyFails_Rejected(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, _, _, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	// Sign with a DIFFERENT key than the JWKS serves. CasdoorVerifier.Verify
	// will fail signature verification.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	raw := signCasdoorJWTForHandler(t, otherKey, "wrong-kid", jwt.MapClaims{
		"sub":          "uni-forged",
		"universal_id": "uuid-forged",
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
	})

	// Svc intentionally has a panicking externalKeyFn — if the handler
	// proceeds past the verifier failure, the test fails loudly instead of
	// silently passing through the wrong branch.
	svc := stubEmploymentReader{
		externalKeyFn: func(context.Context, string) (string, error) {
			t.Fatal("externalKeyFn must NOT be called when Casdoor verify fails")
			return "", nil
		},
	}
	api := &AuthAPI{Svc: svc, Signer: signer, JWT: defaultJWTCfg(), CasdoorVerifier: v}
	r := newVerifyEngine(api)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestVerifyToken_CasdoorVerifierDisabled_Rejected covers the deployment
// that opted out of JWKS (CS_USER_CASDOOR_JWKS_URL unset → CasdoorVerifier
// nil). A token that fails cs-user verification can't fall back, so the
// handler returns plain 401 — no panic, no 500, no misleading
// "active=false" 200.
func TestVerifyToken_CasdoorVerifierDisabled_Rejected(t *testing.T) {
	signer, _ := newTestSigner(t)
	// No CasdoorVerifier on the API.
	api := &AuthAPI{Signer: signer, JWT: defaultJWTCfg()}
	r := newVerifyEngine(api)

	// A self-signed cs-user token from a DIFFERENT issuer/key fails the
	// fast path (signature mismatch) and there's no fallback to rescue it.
	otherSigner, _ := newTestSigner(t)
	claims, err := auth.NewEnterpriseClaims(auth.IssuanceParams{
		Issuer: "test-issuer", Subject: "usr_x", TTL: time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	raw := signEnterpriseJWT(t, otherSigner, claims)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no fallback available)", w.Code)
	}
	var resp struct {
		Active bool   `json:"active"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Active {
		t.Errorf("active = true, want false")
	}
	if resp.Error != "invalid token" {
		t.Errorf("error = %q, want \"invalid token\"", resp.Error)
	}
}

// TestVerifyToken_CasdoorFallbackDisabled_Rejected pins the kill-switch
// contract: when CasdoorVerifyFallbackDisabled is true, the VerifyToken
// handler MUST skip the Casdoor fallback entirely and return 401 — even
// though CasdoorVerifier is configured and would otherwise accept the
// token. Mirrors the verifier-disabled posture (same 401 + active=false
// body) so the gateway's auth-failure pipeline kicks in identically.
//
// The Svc intentionally panics in externalKeyFn: if the handler ignores
// the switch and proceeds into the fallback pipeline, the test fails
// loudly instead of silently passing through.
func TestVerifyToken_CasdoorFallbackDisabled_Rejected(t *testing.T) {
	signer, _ := newTestSigner(t)
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	// Svc would only be called if the switch is ignored — panic to fail
	// loudly in that case.
	svc := stubEmploymentReader{
		externalKeyFn: func(context.Context, string) (string, error) {
			t.Fatal("externalKeyFn must NOT be called when fallback is disabled")
			return "", nil
		},
	}
	api := &AuthAPI{
		Svc:                           svc,
		Signer:                        signer,
		JWT:                           defaultJWTCfg(),
		CasdoorVerifier:               v,
		CasdoorVerifyFallbackDisabled: true,
	}
	r := newVerifyEngine(api)

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"sub":          "uni-bob",
		"universal_id": "uuid-bob",
		"name":         "Bob",
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (fallback suppressed by switch)", w.Code)
	}
	var resp struct {
		Active        bool   `json:"active"`
		Error         string `json:"error"`
		ReissuedToken string `json:"reissued_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Active {
		t.Errorf("active = true, want false")
	}
	if resp.Error != "invalid token" {
		t.Errorf("error = %q, want \"invalid token\"", resp.Error)
	}
	if resp.ReissuedToken != "" {
		t.Errorf("reissued_token = %q, want empty when fallback is suppressed", resp.ReissuedToken)
	}
}

// TestVerifyToken_InvalidTokenReturns401 verifies the rejection contract: a
// token that fails cs-user verification AND has no Casdoor fallback available
// (no CasdoorVerifier wired) returns 401 (not introspection-style
// 200 + active=false) so the gateway's standard auth-failure pipeline kicks
// in. Body still carries active=false + error for callers that want
// structured detail alongside the status.
func TestVerifyToken_InvalidTokenReturns401(t *testing.T) {
	signer, _ := newTestSigner(t)

	api := &AuthAPI{Signer: signer, JWT: defaultJWTCfg()}
	r := newVerifyEngine(api)

	// Garbage that won't parse as a cs-user JWT.
	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{
		Token: "eyJhbGciOiJIUzI1NiJ9.not-a-real-token.badsignature",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	var resp struct {
		Active bool   `json:"active"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Active {
		t.Errorf("active = true, want false for invalid token")
	}
	if resp.Error != "invalid token" {
		t.Errorf("error = %q, want \"invalid token\"", resp.Error)
	}
}

// TestVerifyToken_NoVerifiersConfigured verifies the 503 path: when neither
// signer nor Casdoor verifier is configured, the endpoint refuses with
// "no token verifier configured" rather than pretending success.
func TestVerifyToken_NoVerifiersConfigured(t *testing.T) {
	api := &AuthAPI{JWT: defaultJWTCfg()} // Signer nil, CasdoorVerifier nil
	r := newVerifyEngine(api)
	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: "anything"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestVerifyToken_MissingTokenBody verifies the 400 contract.
func TestVerifyToken_MissingTokenBody(t *testing.T) {
	signer, _ := newTestSigner(t)
	api := &AuthAPI{Signer: signer, JWT: defaultJWTCfg()}
	r := newVerifyEngine(api)
	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestVerifyToken_ExpiredCSUserTokenReturns401 verifies that an expired
// cs-user token is rejected with 401 when no Casdoor fallback is wired. The
// parser enforces `exp` past the 10s leeway window; this matches the
// gateway's standard auth-failure contract.
func TestVerifyToken_ExpiredCSUserTokenReturns401(t *testing.T) {
	signer, _ := newTestSigner(t)

	// Token issued 2 hours ago with 1h TTL — already expired past the 10s
	// leeway window.
	past := time.Now().Add(-2 * time.Hour)
	claims, err := auth.NewEnterpriseClaims(auth.IssuanceParams{
		Issuer: "test-issuer", Subject: "usr_alice", TTL: time.Hour,
	}, past)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	raw := signEnterpriseJWT(t, signer, claims)

	api := &AuthAPI{Signer: signer, JWT: defaultJWTCfg()}
	r := newVerifyEngine(api)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/verify", verifyTokenRequest{Token: raw})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// newParseIdentityEngine wires a gin engine with only the parse-identity
// route registered. Tests inject CasdoorVerifier / Signer / etc. on the
// returned api pointer before serving requests.
func newParseIdentityEngine(api *AuthAPI) *gin.Engine {
	r := gin.New()
	r.POST("/api/internal/auth/parse-identity", api.ParseIdentity)
	return r
}

// TestParseIdentity_HappyPath_ReturnsVerifiedProfileAndExternalKey pins the
// parse-identity contract: server forwards a raw Casdoor JWT, cs-user verifies
// it against JWKS and returns (a) the verified profile fields server uses to
// drive BindIdentityToUser and (b) the canonical external_key cs-user
// computed from the SAME verified claims (replacing server's local
// BuildExternalKey).
func TestParseIdentity_HappyPath_ReturnsVerifiedProfileAndExternalKey(t *testing.T) {
	v, casdoorKey, kid, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	api := &AuthAPI{JWT: defaultJWTCfg(), CasdoorVerifier: v}
	r := newParseIdentityEngine(api)

	raw := signCasdoorJWTForHandler(t, casdoorKey, kid, jwt.MapClaims{
		"id":           "casdoor-id-1",
		"sub":          "uni-1",
		"universal_id": "uuid-1",
		"name":         "alice",
		"provider":     "github",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/parse-identity", map[string]any{"token": raw})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var resp parseIdentityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Profile == nil {
		t.Fatalf("Profile is nil")
	}
	if resp.Profile.ID != "casdoor-id-1" {
		t.Errorf("Profile.ID: got %q, want %q", resp.Profile.ID, "casdoor-id-1")
	}
	if resp.Profile.Sub != "uni-1" {
		t.Errorf("Profile.Sub: got %q, want %q", resp.Profile.Sub, "uni-1")
	}
	if resp.Profile.UniversalID != "uuid-1" {
		t.Errorf("Profile.UniversalID: got %q, want %q", resp.Profile.UniversalID, "uuid-1")
	}
	if resp.Profile.Provider != "github" {
		t.Errorf("Profile.Provider: got %q, want %q", resp.Profile.Provider, "github")
	}
	// ExternalKey must be the canonical casdoor:<provider>:<universal_id>,
	// computed by cs-user from the verified claims (replacing server's local
	// BuildExternalKey).
	if resp.ExternalKey != "casdoor:github:uuid-1" {
		t.Errorf("ExternalKey: got %q, want %q", resp.ExternalKey, "casdoor:github:uuid-1")
	}
}

// TestParseIdentity_VerifierNotConfigured_Returns503 pins the 503 contract:
// without a Casdoor verifier wired, cs-user refuses rather than pretending
// success. Mirrors ReissueToken's contract for the same state.
func TestParseIdentity_VerifierNotConfigured_Returns503(t *testing.T) {
	api := &AuthAPI{JWT: defaultJWTCfg()} // CasdoorVerifier nil
	r := newParseIdentityEngine(api)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/parse-identity", map[string]any{"token": "any-jwt"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
}

// TestParseIdentity_MissingTokenBody_Returns400 pins the 400 contract.
func TestParseIdentity_MissingTokenBody_Returns400(t *testing.T) {
	v, _, _, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	api := &AuthAPI{JWT: defaultJWTCfg(), CasdoorVerifier: v}
	r := newParseIdentityEngine(api)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/parse-identity", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

// TestParseIdentity_VerificationFails_Returns401 pins the trust boundary:
// a Casdoor JWT signed by an unknown key (or otherwise invalid) MUST be
// rejected with 401 — server cannot trust the claims in the body, so
// parse-identity refuses to surface them. This is the whole point of the
// cleanup: server's old unverified ParseJWTClaimsFromAccessToken would have
// accepted this token.
func TestParseIdentity_VerificationFails_Returns401(t *testing.T) {
	v, _, _, cleanup := newVerifiedCasdoorBundle(t)
	defer cleanup()

	// Sign with a freshly generated key that the JWKS doesn't serve — the
	// verifier cannot find a matching kid / public key and rejects.
	rogueKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	raw := signCasdoorJWTForHandler(t, rogueKey, "rogue-kid", jwt.MapClaims{
		"id":           "casdoor-id-rogue",
		"universal_id": "uuid-rogue",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	api := &AuthAPI{JWT: defaultJWTCfg(), CasdoorVerifier: v}
	r := newParseIdentityEngine(api)

	w := doJSON(t, r, http.MethodPost, "/api/internal/auth/parse-identity", map[string]any{"token": raw})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", w.Code, w.Body.String())
	}
}
