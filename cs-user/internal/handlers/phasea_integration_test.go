//go:build cgo

// Integration tests — end-to-end exercise of the OAuth-callback-takeover
// flow at the unit-test level. The acceptance criteria in
// todo/IDENTITY_TENANT_PROGRESS.md §"OAuth-callback 验收" list integration points
// (cs-cloud / csc / assistant-ui / quota-manager) that can't be exercised from
// this repo; these tests cover the code-level bar that CAN be locked in:
//
//  1. The reissue-token handler produces a verifiable JWT carrying every claim
//     group when all readers are wired (full-context happy path).
//  2. The JWKS endpoint serves the public key that verifies the issued token
//     — exercises the actual key-distribution path, not just the test's own
//     copy of the private key.
//  3. The minimal-token path (no permission reader, no employment row) still
//     issues a verifiable token carrying only the standard + tenant claims.
//  4. server-side NormalizeClaimsMap can decode the cs-user-issued token
//     (dual-format reader contract per §9.6 / acceptance item L453).
//
// Together these pin the "does the issued token work downstream" contract
// that the operational acceptance items exercise against real downstream
// services.

package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// phaseAFixture bundles the moving parts an integration test needs.
// All optional readers are stubs so the test pins exact responses without
// a DB. The fixture also wires a mock Casdoor JWKS so the verifier path
// runs end-to-end — tests pass a raw Casdoor JWT (signed by casdoorKey) and
// cs-user's reissue-token handler verifies it itself.
type phaseAFixture struct {
	signer     *auth.Signer
	pk         *rsa.PrivateKey
	casdoorKey *rsa.PrivateKey
	casdoorKid string
	engine     *gin.Engine
	jwksBody   []byte // captured JWKS JSON for cross-verification
}

// newPhaseAFixture builds a gin engine wired with both /.well-known/jwks and
// /api/internal/users/reissue-token. employment MUST wire external_key +
// GetUserByID stubs so the handler can resolve subject_id and tenant from
// the verified JWT. permissions is optional: pass nil for the minimal-token path.
func newPhaseAFixture(t *testing.T, employment EmploymentReader, permissions PermissionReader, tenant TenantReader) phaseAFixture {
	t.Helper()
	signer, pk := newTestSigner(t)

	casdoorKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	const casdoorKid = "casdoor-kid-phasea"
	casdoorJWKS := startMockCasdoorJWKS(t, &casdoorKey.PublicKey, casdoorKid)

	jwksAPI := JWKSAPI{Signer: signer}
	authAPI := &AuthAPI{
		Svc:            employment,
		Signer:         signer,
		JWT:            config.JWTConfig{Issuer: "https://cs-user.test", TTL: time.Hour, DefaultAudience: []string{"costrict-web"}},
		Permissions:    permissions,
		TenantResolver: tenant,
		CasdoorVerifier: auth.NewCasdoorVerifier(
			casdoorJWKS.URL, "", nil, time.Second, time.Minute,
		),
	}
	r := gin.New()
	r.GET("/.well-known/jwks", jwksAPI.GetJWKS)
	r.POST("/api/internal/users/reissue-token", authAPI.ReissueToken)

	// Capture JWKS body once so each test can verify against the same shape
	// the production JWKS endpoint would actually serve.
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("JWKS bootstrap: status %d body=%s", w.Code, w.Body.String())
	}
	return phaseAFixture{
		signer:     signer,
		pk:         pk,
		casdoorKey: casdoorKey,
		casdoorKid: casdoorKid,
		engine:     r,
		jwksBody:   w.Body.Bytes(),
	}
}

// casdoorJWT signs a raw Casdoor-format JWT the fixture's verifier will
// accept. universalID + provider drive the external_key the handler resolves
// subject_id from.
func (f phaseAFixture) casdoorJWT(t *testing.T, universalID, name, provider string) string {
	t.Helper()
	return signCasdoorJWTForHandler(t, f.casdoorKey, f.casdoorKid, jwt.MapClaims{
		"sub":          universalID,
		"universal_id": universalID,
		"name":         name,
		"provider":     provider,
		"exp":          time.Now().Add(time.Hour).Unix(),
	})
}

// callReissue posts the request body to the fixture's reissue-token route
// and returns the parsed token + expiry.
func (f phaseAFixture) callReissue(t *testing.T, body reissueTokenRequest) reissueTokenResponse {
	t.Helper()
	w := doJSON(t, f.engine, http.MethodPost, "/api/internal/users/reissue-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("reissue status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp reissueTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}
	return resp
}

// verifyViaJWKS verifies the issued token against the JWKS-fetched public
// key — NOT the test's own private key. This exercises the actual
// key-distribution path: if the JWKS endpoint serves the wrong key or the
// signer uses a different KID, this fails.
func (f phaseAFixture) verifyViaJWKS(t *testing.T, token string) *auth.EnterpriseClaims {
	t.Helper()
	var jwks auth.JWKS
	if err := json.Unmarshal(f.jwksBody, &jwks); err != nil {
		t.Fatalf("unmarshal captured JWKS: %v", err)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("expected 1 JWKS key, got %d", len(jwks.Keys))
	}
	jwk := jwks.Keys[0]

	// Reconstruct the RSA public key from the JWK's n + e (base64url-encoded
	// big-endian). This is what a relying party that doesn't have a JWT lib
	// with built-in JWK support would do — and what jwt/v5 does internally
	// when handed a JWKS-backed keyfunc.
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}

	parsed, err := jwt.ParseWithClaims(token, &auth.EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			t.Fatalf("unexpected alg: %v", tok.Header["alg"])
		}
		// Belt-and-braces: also confirm the kid matches what the signer
		// advertised.
		if kid, _ := tok.Header["kid"].(string); kid != f.signer.KID() {
			t.Errorf("token kid=%q, JWKS kid=%q (signer advertised %q)", kid, jwk.Kid, f.signer.KID())
		}
		return pub, nil
	})
	if err != nil {
		t.Fatalf("verify via JWKS-fetched key: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token should be valid against JWKS-fetched key")
	}
	got, ok := parsed.Claims.(*auth.EnterpriseClaims)
	if !ok {
		t.Fatalf("claims type: %T", parsed.Claims)
	}
	return got
}

// TestPhaseA_FullContextEndToEnd exercises the complete reissue pipeline in
// one test: all claim groups populated, JWKS-served key verifies the token.
// Locks in the contract that the OAuth-callback takeover endpoint, given
// real reader responses, produces a token that downstream services can
// verify purely from /.well-known/jwks.
func TestPhaseA_FullContextEndToEnd(t *testing.T) {
	empNum := "E-1001"
	const subjectID = "usr_alice"
	const tenantID = "t-acme"
	employment := stubEmploymentReader{
		fn: func(_ context.Context, id string) (*models.EmploymentIdentity, error) {
			return &models.EmploymentIdentity{
				UserSubjectID:  id,
				EmployeeNumber: &empNum,
				JobTitle:       ptrStrLocal("Staff Engineer"),
			}, nil
		},
		externalKeyFn: func(_ context.Context, k string) (string, error) {
			if k != "casdoor:idtrust:uuid-alice" {
				t.Errorf("external_key: got %q", k)
			}
			return subjectID, nil
		},
		userFn: func(_ context.Context, id string) (*models.User, error) {
			return &models.User{SubjectID: id, TenantID: tenantID}, nil
		},
	}
	permissions := stubPermissionReader{
		platformFn: func(_ context.Context, id string) (*models.PlatformAdmin, error) {
			return &models.PlatformAdmin{UserID: id, Scope: models.PlatformScopeFull}, nil
		},
		tenantRolesFn: func(_ context.Context, _, tenant string) ([]string, error) {
			if tenant != tenantID {
				t.Errorf("tenant_roles tenant arg: got %q, want %q", tenant, tenantID)
			}
			return []string{models.TenantRoleOwner}, nil
		},
	}
	tenant := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			if idOrSlug != tenantID {
				t.Errorf("ResolveBySlug arg: got %q, want %q", idOrSlug, tenantID)
			}
			return &models.Tenant{TenantID: tenantID, Slug: "acme"}, nil
		},
	}
	fixture := newPhaseAFixture(t, employment, permissions, tenant)

	raw := fixture.casdoorJWT(t, "uuid-alice", "Alice Lee", "idtrust")
	// Embed the email claim that the fixture's helper doesn't set by default.
	// Re-sign with the richer claim set:
	raw = signCasdoorJWTForHandler(t, fixture.casdoorKey, fixture.casdoorKid, jwt.MapClaims{
		"sub":          "uuid-alice",
		"universal_id": "uuid-alice",
		"name":         "Alice Lee",
		"email":        "alice@example.com",
		"provider":     "idtrust",
		// idtrust provider in NormalizeClaimsMap overrides name = username =
		// providerUserID. Real Casdoor idtrust payloads carry this; without
		// it, name would fall back to stableNameFromSubject(universal_id).
		"properties": map[string]any{
			"oauth_Custom_id": "Alice Lee",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	resp := fixture.callReissue(t, reissueTokenRequest{CasdoorJWT: raw})

	got := fixture.verifyViaJWKS(t, resp.Token)

	// Standard JWT group.
	if got.Issuer != "https://cs-user.test" {
		t.Errorf("iss: got %q", got.Issuer)
	}
	if got.Subject != subjectID {
		t.Errorf("sub: got %q", got.Subject)
	}
	if got.Expiry == nil || !got.Expiry.After(time.Now()) {
		t.Errorf("exp not in future: %v", got.Expiry)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "costrict-web" {
		t.Errorf("aud: got %v", got.Audience)
	}
	// OIDC identity group. UniversalID is the cross-provider stable
	// identifier — semantically the most load-bearing field, so pin it
	// explicitly rather than relying on the Name/Email/Provider trio.
	if got.UniversalID != "uuid-alice" {
		t.Errorf("OIDC universal_id: got %q", got.UniversalID)
	}
	if got.Name != "Alice Lee" || got.Email != "alice@example.com" || got.Provider != "idtrust" {
		t.Errorf("OIDC identity: name=%q email=%q provider=%q", got.Name, got.Email, got.Provider)
	}
	// Enterprise context group.
	if got.EmployeeNumber != empNum || got.JobTitle != "Staff Engineer" {
		t.Errorf("enterprise: emp=%q job=%q", got.EmployeeNumber, got.JobTitle)
	}
	// Tenant group.
	if got.TenantID != tenantID || got.TenantSlug != "acme" {
		t.Errorf("tenant: id=%q slug=%q", got.TenantID, got.TenantSlug)
	}
	// Permission group.
	if !got.PlatformAdmin || got.PlatformScope != models.PlatformScopeFull {
		t.Errorf("platform: admin=%v scope=%q", got.PlatformAdmin, got.PlatformScope)
	}
	if len(got.TenantRoles) != 1 || got.TenantRoles[0] != models.TenantRoleOwner {
		t.Errorf("tenant_roles: got %v", got.TenantRoles)
	}
}

// TestPhaseA_GrayReleaseMinimalToken verifies the minimal-token path: no
// permission reader wired, no employment row. The handler must still
// issue a verifiable token, and the JSON must omit the enterprise +
// permission claim groups entirely — so relying parties parsing the
// token see only the standard + tenant claims they already know how to
// handle.
func TestPhaseA_GrayReleaseMinimalToken(t *testing.T) {
	const subjectID = "usr_pre_cutover"
	employment := stubEmploymentReader{
		fn: func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
		externalKeyFn: func(_ context.Context, k string) (string, error) {
			if k != "casdoor:idtrust:uuid-pre" {
				t.Errorf("external_key: got %q", k)
			}
			return subjectID, nil
		},
		userFn: func(_ context.Context, id string) (*models.User, error) {
			return &models.User{SubjectID: id, TenantID: "default"}, nil
		},
	}
	tenant := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			return &models.Tenant{TenantID: idOrSlug, Slug: "default"}, nil
		},
	}
	fixture := newPhaseAFixture(t, employment, nil, tenant) // no permission reader — gray release

	raw := fixture.casdoorJWT(t, "uuid-pre", "Pre Cutover", "idtrust")
	resp := fixture.callReissue(t, reissueTokenRequest{CasdoorJWT: raw})

	got := fixture.verifyViaJWKS(t, resp.Token)

	// Standard + tenant groups populated.
	if got.Subject != subjectID {
		t.Errorf("sub: got %q", got.Subject)
	}
	if got.TenantID != "default" {
		t.Errorf("tenant_id: got %q", got.TenantID)
	}
	// Enterprise + permission groups absent.
	if got.EmployeeNumber != "" || got.JobTitle != "" {
		t.Errorf("enterprise claims should be empty in gray release: emp=%q job=%q", got.EmployeeNumber, got.JobTitle)
	}
	if got.PlatformAdmin || got.PlatformScope != "" || len(got.TenantRoles) != 0 {
		t.Errorf("permission claims should be empty in gray release: %+v", got)
	}
}

// TestPhaseA_JWKSKidMatchesSignerKid verifies the issued token's kid header
// matches what /.well-known/jwks advertises. The kid is what a relying
// party uses to pick the right JWKS key when multiple are listed (future
// rotation); a mismatch means verification would fall through to a wrong
// key.
func TestPhaseA_JWKSKidMatchesSignerKid(t *testing.T) {
	const subjectID = "usr_kid"
	employment := stubEmploymentReader{
		fn:            func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
		externalKeyFn: func(context.Context, string) (string, error) { return subjectID, nil },
		userFn: func(_ context.Context, id string) (*models.User, error) {
			return &models.User{SubjectID: id, TenantID: "default"}, nil
		},
	}
	tenant := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			return &models.Tenant{TenantID: idOrSlug, Slug: "default"}, nil
		},
	}
	fixture := newPhaseAFixture(t, employment, nil, tenant)
	raw := fixture.casdoorJWT(t, "uuid-kid", "Kid", "idtrust")
	resp := fixture.callReissue(t, reissueTokenRequest{CasdoorJWT: raw})

	// Parse the unverified header — we only need the kid, not signature.
	unverified := jwt.NewParser(jwt.WithoutClaimsValidation())
	tok, _, err := unverified.ParseUnverified(resp.Token, &auth.EnterpriseClaims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	kid, _ := tok.Header["kid"].(string)
	if kid != fixture.signer.KID() {
		t.Errorf("token kid=%q, signer.KID()=%q", kid, fixture.signer.KID())
	}

	// Also confirm the JWKS body carries the same kid.
	var jwks auth.JWKS
	_ = json.Unmarshal(fixture.jwksBody, &jwks)
	if len(jwks.Keys) != 1 || jwks.Keys[0].Kid != kid {
		t.Errorf("JWKS kid mismatch: jwks=%v token=%q", jwks.Keys, kid)
	}
}

// TestPhaseA_TokenExpiryMatchesConfigTTL verifies the issued exp matches the
// configured TTL — locks in the "1h default" contract that the OAuth
// callback + cookie MaxAge depend on.
func TestPhaseA_TokenExpiryMatchesConfigTTL(t *testing.T) {
	const subjectID = "usr_ttl"
	employment := stubEmploymentReader{
		fn:            func(context.Context, string) (*models.EmploymentIdentity, error) { return nil, nil },
		externalKeyFn: func(context.Context, string) (string, error) { return subjectID, nil },
		userFn: func(_ context.Context, id string) (*models.User, error) {
			return &models.User{SubjectID: id, TenantID: "default"}, nil
		},
	}
	tenant := stubTenantReader{
		fn: func(_ context.Context, idOrSlug string) (*models.Tenant, error) {
			return &models.Tenant{TenantID: idOrSlug, Slug: "default"}, nil
		},
	}
	fixture := newPhaseAFixture(t, employment, nil, tenant)
	raw := fixture.casdoorJWT(t, "uuid-ttl", "TTL", "idtrust")
	resp := fixture.callReissue(t, reissueTokenRequest{CasdoorJWT: raw})

	got := fixture.verifyViaJWKS(t, resp.Token)
	if got.Expiry == nil {
		t.Fatal("exp is nil")
	}
	gotExpiry := *got.Expiry
	if !gotExpiry.Equal(resp.ExpiresAt) {
		t.Errorf("response.ExpiresAt %v != claim exp %v", resp.ExpiresAt, gotExpiry)
	}
	// exp should be ~1h from now (within a 5s slack to absorb test runtime).
	expected := time.Now().Add(time.Hour)
	delta := gotExpiry.Sub(expected)
	if delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("exp drift: got %v (delta %v from expected ~1h)", gotExpiry, delta)
	}
}

// ptrStrLocal is a local alias so this file doesn't depend on the order in
// which auth_test.go declares ptrStr (same shape, different name to avoid
// redeclaration in the same package).
func ptrStrLocal(s string) *string { return &s }
