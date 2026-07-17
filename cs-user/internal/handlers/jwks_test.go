// JWKS handler tests: 200 happy path, 503 when signer is nil.

package handlers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newSignerWithFreshKey builds a Signer with a freshly generated RSA-2048
// keypair. Used by every JWKS handler test so each test is independent
// (no shared fixture, no on-disk key file).
func newSignerWithFreshKey(t *testing.T) *auth.Signer {
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
	return s
}

// doJWKS spins up a gin engine with just the JWKS route + the supplied signer,
// performs GET /.well-known/jwks, returns the recorded response.
func doJWKS(t *testing.T, signer *auth.Signer) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	api := JWKSAPI{Signer: signer}
	r.GET("/.well-known/jwks", api.GetJWKS)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestJWKS_GetJWKS_HappyPath(t *testing.T) {
	signer := newSignerWithFreshKey(t)
	w := doJWKS(t, signer)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var got auth.JWKS
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode JWKS body: %v", err)
	}
	if len(got.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(got.Keys))
	}
	k := got.Keys[0]
	if k.Kty != "RSA" || k.Use != "sig" || k.Alg != "RS256" {
		t.Errorf("JWK fields: kty=%q use=%q alg=%q", k.Kty, k.Use, k.Alg)
	}
	if k.Kid != signer.KID() {
		t.Errorf("kid: got %q, want %q", k.Kid, signer.KID())
	}
	if k.N == "" || k.E == "" {
		t.Errorf("n and e must be non-empty: n=%q e=%q", k.N, k.E)
	}
}

func TestJWKS_GetJWKS_NilSignerReturns503(t *testing.T) {
	w := doJWKS(t, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if resp.Error == "" {
		t.Error("error message should be non-empty")
	}
}
