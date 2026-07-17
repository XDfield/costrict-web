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
type stubEmploymentReader struct {
	fn func(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error)
}

func (s stubEmploymentReader) GetEmploymentIdentity(ctx context.Context, id string) (*models.EmploymentIdentity, error) {
	return s.fn(ctx, id)
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
