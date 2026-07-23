package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
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
)

// stubEmploymentReader lets handler tests pin GetEmploymentIdentity responses
// without a DB. Mirrors the stubUserService pattern in users_test.go.
//
// applyCalls captures ApplyEnterpriseMapping invocations so tests can assert
// the reissue-token flow forwarded the right ExternalClaims / Provider. Nil
// slice = either no call or no capture — tests opt in by reading the slice.
type stubEmploymentReader struct {
	fn         func(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error)
	applyCalls *[]user.EmploymentMappingParams
}

func (s stubEmploymentReader) GetEmploymentIdentity(ctx context.Context, id string) (*models.EmploymentIdentity, error) {
	return s.fn(ctx, id)
}

// ApplyEnterpriseMapping records the call (when applyCalls is non-nil) so
// the reissue-token wiring test can verify ExternalClaims forwarding. Return
// value is irrelevant — the handler swallows it.
func (s stubEmploymentReader) ApplyEnterpriseMapping(_ context.Context, params user.EmploymentMappingParams) error {
	if s.applyCalls != nil {
		*s.applyCalls = append(*s.applyCalls, params)
	}
	return nil
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

// newAuthAPI builds a minimal gin engine wired only with the reissue-token
// route. Returns the api + engine so each test injects its own
// stubEmploymentReader + signer.
func newAuthAPI(svc EmploymentReader, signer *auth.Signer, jwtCfg config.JWTConfig) (*AuthAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	api := &AuthAPI{Svc: svc, Signer: signer, JWT: jwtCfg}
	r := gin.New()
	r.POST("/api/internal/users/reissue-token", api.ReissueToken)
	return api, r
}

// newTestSigner generates a fresh RSA-2048 key + constructs a *auth.Signer
// via the production NewSignerFromPEM path. Each test gets its own key so
// parallel runs don't share state. Tests use this over hand-rolled signers
// to exercise the actual PEM decode + KID derivation code paths.
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

// defaultJWTCfg returns a valid JWTConfig for tests. Production-like
// defaults so the handler exercises the realistic issuance path.
func defaultJWTCfg() config.JWTConfig {
	return config.JWTConfig{
		Issuer:          "test-issuer",
		TTL:             time.Hour,
		DefaultAudience: []string{"costrict-web"},
	}
}

func TestReissueToken_HappyPath(t *testing.T) {
	signer, pk := newTestSigner(t)
	empNum := "E-1001"
	jobTitle := "Staff Engineer"
	svc := stubEmploymentReader{
		fn: func(_ context.Context, id string) (*models.EmploymentIdentity, error) {
			if id != "usr_alice" {
				t.Errorf("passed id=%q, want usr_alice", id)
			}
			return &models.EmploymentIdentity{
				UserSubjectID:  "usr_alice",
				EmployeeNumber: &empNum,
				JobTitle:       &jobTitle,
			}, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{
		UserSubjectID: "usr_alice",
		Identity: &models.JWTClaims{
			UniversalID: "uuid-alice",
			Name:        "Alice Lee",
			Email:       "alice@example.com",
			Provider:    "idtrust",
		},
	}
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
	// Verify the token parses + carries the expected claims.
	parsed, err := jwt.ParseWithClaims(resp.Token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			t.Fatalf("unexpected alg: %v", tok.Header["alg"])
		}
		return &pk.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token should be valid")
	}
	got, ok := parsed.Claims.(*auth.EnterpriseClaims)
	if !ok {
		t.Fatalf("claims type: %T", parsed.Claims)
	}
	if got.Subject != "usr_alice" {
		t.Errorf("Subject: got %q", got.Subject)
	}
	if got.Issuer != "test-issuer" {
		t.Errorf("Issuer: got %q, want test-issuer", got.Issuer)
	}
	if got.Name != "Alice Lee" {
		t.Errorf("Name: got %q", got.Name)
	}
	if got.EmployeeNumber != empNum {
		t.Errorf("EmployeeNumber: got %q, want %q", got.EmployeeNumber, empNum)
	}
	if got.JobTitle != jobTitle {
		t.Errorf("JobTitle: got %q, want %q", got.JobTitle, jobTitle)
	}
	if got.TenantID != "default" {
		t.Errorf("TenantID default: got %q, want default", got.TenantID)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "costrict-web" {
		t.Errorf("Audience: got %v, want [costrict-web]", got.Audience)
	}

	// ExpiresAt should match the claim's exp.
	if got.Expiry == nil || !got.Expiry.Equal(resp.ExpiresAt) {
		t.Errorf("ExpiresAt mismatch: response=%v claims=%v", resp.ExpiresAt, got.Expiry)
	}
}

// TestReissueToken_AppliesEnterpriseMappingFromExternalClaims verifies the
// Slice 1.6 wiring: reissue-token must forward Identity.ExternalClaims to
// ApplyEnterpriseMapping BEFORE reading the snapshot back, so freshly-arrived
// Casdoor JWT payloads (properties.oauth_Custom.*, signupApplication, ...)
// drive the field_map upsert. Without this call the snapshot stays NULL and
// the signed JWT never carries enterprise claims.
func TestReissueToken_AppliesEnterpriseMappingFromExternalClaims(t *testing.T) {
	signer, _ := newTestSigner(t)
	var applyCalls []user.EmploymentMappingParams
	svc := stubEmploymentReader{
		fn: func(_ context.Context, _ string) (*models.EmploymentIdentity, error) {
			return nil, nil // read path returns "no snapshot" — still issues token
		},
		applyCalls: &applyCalls,
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	externalClaims := map[string]any{
		"signupApplication": "idtrust",
		"properties": map[string]any{
			"oauth_Custom": map[string]any{
				"id":           "alice-idtrust-uid",
				"employeeNumber": "E-1001",
			},
		},
	}
	body := reissueTokenRequest{
		UserSubjectID: "usr_alice",
		TenantID:      "default",
		Identity: &models.JWTClaims{
			Provider:        "idtrust",
			ExternalClaims:  externalClaims,
		},
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(applyCalls) != 1 {
		t.Fatalf("ApplyEnterpriseMapping calls: got %d, want 1", len(applyCalls))
	}
	got := applyCalls[0]
	if got.UserSubjectID != "usr_alice" {
		t.Errorf("UserSubjectID: got %q, want usr_alice", got.UserSubjectID)
	}
	if got.TenantID != "default" {
		t.Errorf("TenantID: got %q, want default", got.TenantID)
	}
	if got.Provider != "idtrust" {
		t.Errorf("Provider: got %q, want idtrust", got.Provider)
	}
	if len(got.ExternalClaims) != len(externalClaims) {
		t.Errorf("ExternalClaims forwarded: got %d keys, want %d", len(got.ExternalClaims), len(externalClaims))
	}
}

// TestReissueToken_NoExternalClaimsSkipsApplyMapping verifies the skip path:
// when Identity.ExternalClaims is empty (refresh-token / cached identity
// flows), the handler MUST NOT call ApplyEnterpriseMapping — there's nothing
// new to extract, and calling it with an empty map would write a NULL row on
// every reissue.
func TestReissueToken_NoExternalClaimsSkipsApplyMapping(t *testing.T) {
	signer, _ := newTestSigner(t)
	var applyCalls []user.EmploymentMappingParams
	svc := stubEmploymentReader{
		fn: func(_ context.Context, _ string) (*models.EmploymentIdentity, error) {
			return nil, nil
		},
		applyCalls: &applyCalls,
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{
		UserSubjectID: "usr_alice",
		Identity: &models.JWTClaims{
			Provider: "idtrust",
			// No ExternalClaims — refresh flow.
		},
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(applyCalls) != 0 {
		t.Errorf("ApplyEnterpriseMapping calls: got %d, want 0 (no ExternalClaims to forward)", len(applyCalls))
	}
}

// TestReissueToken_NilSignerMaps503 verifies the missing-signer path surfaces
// as 503, mirroring the JWKS endpoint's contract. Operators get a distinct
// status code that says "config incomplete" rather than 500 (bug).
func TestReissueToken_NilSignerMaps503(t *testing.T) {
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("reader should not be called when signer is nil")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, nil, defaultJWTCfg())

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "JWT signing not configured") {
		t.Errorf("body = %s, want substring 'JWT signing not configured'", w.Body.String())
	}
}

// TestReissueToken_MissingSubjectIDRejected verifies the binding:"required"
// tag catches the empty-subject case before the service is called.
func TestReissueToken_MissingSubjectIDRejected(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("reader should not be called on validation failure")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

// TestReissueToken_EmptySubjectFromServiceMaps400 verifies the case where
// the service-level guard (user.ErrEmptySubjectID) fires. Belt-and-braces:
// gin's binding:"required" already catches the empty case, but if the
// service emits the sentinel for any other reason the handler still maps
// it to 400.
func TestReissueToken_EmptySubjectFromServiceMaps400(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			return nil, user.ErrEmptySubjectID
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestReissueToken_ServiceErrorMaps500 verifies a non-validation service
// error surfaces as 500 — the caller did nothing wrong, this is a real
// server-side fault.
func TestReissueToken_ServiceErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			return nil, errors.New("db dead")
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "db dead") {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

// TestReissueToken_NoEmploymentRowStillIssuesToken verifies graceful
// degradation: a user without an employment_identities row (provider not
// enabled, never synced) still gets a valid token — just without enterprise
// claims. This is critical for the 灰度 rollout: tokens must work even
// before enterprise mapping is wired.
func TestReissueToken_NoEmploymentRowStillIssuesToken(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
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
// override wins over the config default. Server uses this to target
// specific relying parties (e.g. csc CLI vs. costrict-web frontend).
func TestReissueToken_AudienceOverride(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{
		UserSubjectID: "usr_alice",
		Audience:      []string{"csc-cli", "ops-portal"},
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

// TestReissueToken_TenantIDForwarded verifies a non-empty tenant_id in the
// request is honored by the handler. Phase A callers always pass "default"
// (or omit), but Phase B will route real tenant IDs through this field.
func TestReissueToken_TenantIDForwarded(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{
		UserSubjectID: "usr_alice",
		TenantID:      "acme-corp",
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
	if got.TenantID != "acme-corp" {
		t.Errorf("TenantID: got %q, want acme-corp", got.TenantID)
	}
}

// TestReissueToken_TenantSlugForwarded verifies the request-level tenant_slug
// is embedded verbatim into the signed JWT (Phase B / A7 unblock). Server's
// TenantMatch middleware reads this claim to compare against the runtime-
// resolved slug for cross-tenant detection (B3b.2c). Empty request slug →
// empty claim (graceful pre-cutover behavior).
func TestReissueToken_TenantSlugForwarded(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{
		UserSubjectID: "usr_alice",
		TenantSlug:    "acme",
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
	if got.TenantSlug != "acme" {
		t.Errorf("TenantSlug: got %q, want acme", got.TenantSlug)
	}
}

// TestReissueToken_EmptyTenantSlugOmitted verifies the omitempty behavior —
// when the request omits tenant_slug, the claim is absent (not an empty
// string) in the issued JWT. This is what enables server's TenantMatch
// middleware to distinguish "cs-user-signed but no slug signal" from "Casdoor
// token, never had the claim" — both surface as empty string post-decode.
func TestReissueToken_EmptyTenantSlugOmitted(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
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
	if got.TenantSlug != "" {
		t.Errorf("TenantSlug: got %q, want empty (omitted)", got.TenantSlug)
	}
}

// TestReissueToken_NilIdentityStillWorks verifies the optional Identity
// path. Refresh-token flows (Phase B) may pass nil; the issued token then
// carries only standard JWT claims + enterprise (if any).
func TestReissueToken_NilIdentityStillWorks(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
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
	if got.Name != "" {
		t.Errorf("Name should be empty when Identity is nil, got %q", got.Name)
	}
	if got.Email != "" {
		t.Errorf("Email should be empty when Identity is nil, got %q", got.Email)
	}
}

// TestReissueToken_BadJSONMaps400 verifies a malformed body surfaces as 400
// before the service is called.
func TestReissueToken_BadJSONMaps400(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) {
			t.Fatal("reader should not be called on bad JSON")
			return nil, nil
		},
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())

	// Non-empty body that fails JSON decode — can't use doJSON since it
	// encodes a struct; emit raw bytes via httptest directly.
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

// noPermReader is the canonical "user is a regular tenant member" stub:
// both lookups return empty/nil. Used to verify the omitempty path.
func noPermReader() stubPermissionReader {
	return stubPermissionReader{
		platformFn:    func(context.Context, string) (*models.PlatformAdmin, error) { return nil, nil },
		tenantRolesFn: func(context.Context, string, string) ([]string, error) { return nil, nil },
	}
}

// TestReissueToken_PermissionClaimsPopulated verifies the happy path: when
// Permissions is wired and the user has both platform_admin + tenant_admin
// rows, the corresponding JWT claims surface in the signed token.
func TestReissueToken_PermissionClaimsPopulated(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.Permissions = stubPermissionReader{
		platformFn: func(_ context.Context, id string) (*models.PlatformAdmin, error) {
			if id != "usr_root" {
				t.Errorf("platform lookup id: got %q, want usr_root", id)
			}
			return &models.PlatformAdmin{UserID: id, Scope: models.PlatformScopeFull}, nil
		},
		tenantRolesFn: func(_ context.Context, id, tenant string) ([]string, error) {
			if id != "usr_root" || tenant != "default" {
				t.Errorf("tenant_roles lookup args: id=%q tenant=%q", id, tenant)
			}
			return []string{models.TenantRoleOwner}, nil
		},
	}

	body := reissueTokenRequest{UserSubjectID: "usr_root"}
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
// degradation: when Permissions is nil (灰度 rollout — caller hasn't wired
// the new readers), the issued token simply omits the permission claims.
// The handler must NOT fail.
func TestReissueToken_NoPermissionReaderStillIssuesToken(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	_, r := newAuthAPI(svc, signer, defaultJWTCfg())
	// api.Permissions intentionally left nil — Phase A 灰度 mode.

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
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
// the wire level: a regular tenant member (no admin rows) gets a token that
// does NOT carry tenant_roles / platform_admin / platform_scope keys. Locks
// in the contract that downstream parsers can distinguish "regular member"
// from "admin" purely by claim presence.
func TestReissueToken_RegularMemberOmitsClaims(t *testing.T) {
	signer, pk := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.Permissions = noPermReader()

	body := reissueTokenRequest{UserSubjectID: "usr_regular"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	// Re-issue via parse + marshal to inspect JSON shape. The signer's own
	// public key (held by the test) is used so signature verification passes.
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
// platform-admin lookup surface as 500 (server fault), distinguishing from
// the (nil,nil) graceful-degradation path.
func TestReissueToken_PlatformLookupErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.Permissions = stubPermissionReader{
		platformFn:    func(context.Context, string) (*models.PlatformAdmin, error) { return nil, errors.New("db dead") },
		tenantRolesFn: func(context.Context, string, string) ([]string, error) { return nil, nil },
	}

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "db dead") {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

// TestReissueToken_TenantRolesLookupErrorMaps500 mirrors the above for the
// tenant-roles path.
func TestReissueToken_TenantRolesLookupErrorMaps500(t *testing.T) {
	signer, _ := newTestSigner(t)
	svc := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
	}
	api, r := newAuthAPI(svc, signer, defaultJWTCfg())
	api.Permissions = stubPermissionReader{
		platformFn:    func(context.Context, string) (*models.PlatformAdmin, error) { return nil, nil },
		tenantRolesFn: func(context.Context, string, string) ([]string, error) { return nil, errors.New("db dead") },
	}

	body := reissueTokenRequest{UserSubjectID: "usr_alice"}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
