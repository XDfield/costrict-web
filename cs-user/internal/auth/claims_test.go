// EnterpriseClaims tests: constructor validation, JSON serialization shape,
// jwt.Claims interface compliance, end-to-end sign + verify with Signer.

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/golang-jwt/jwt/v5"
)

// fixedNow is the pinned issuance time used across tests. 2026-07-17T12:00:00Z.
var fixedNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

// newRSAKey generates a fresh RSA-2048 key for sign+verify roundtrip tests.
func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return pk
}

// pemEncode wraps a signer test helper around the production Signer so we
// exercise the actual NewSignerFromPEM path. Returns the Signer + the
// underlying private key (so tests can verify with the matching public key).
func pemEncode(t *testing.T, pk *rsa.PrivateKey) *Signer {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s, err := NewSignerFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("NewSignerFromPEM: %v", err)
	}
	return s
}

func TestNewEnterpriseClaims_HappyPath(t *testing.T) {
	now := fixedNow
	emp := &models.EmploymentIdentity{
		EmployeeNumber: ptrStr("E-1001"),
		JobTitle:       ptrStr("Staff Engineer"),
		JobLevel:       ptrStr("L5"),
		CostCenter:     ptrStr("CC-300"),
	}
	identity := &models.JWTClaims{
		UniversalID: "uuid-alice",
		Name:        "Alice Lee",
		Email:       "alice@example.com",
		Provider:    "idtrust",
	}

	c, err := NewEnterpriseClaims(IssuanceParams{
		Issuer:     "https://cs-user.example.com",
		Subject:    "usr_alice",
		Audience:   []string{"costrict-web"},
		TTL:        time.Hour,
		JTI:        "jti-1",
		Identity:   identity,
		Employment: emp,
		TenantID:   "default",
	}, now)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}

	if c.Issuer != "https://cs-user.example.com" {
		t.Errorf("Issuer: got %q", c.Issuer)
	}
	if c.Subject != "usr_alice" {
		t.Errorf("Subject: got %q", c.Subject)
	}
	if c.Expiry == nil || !c.Expiry.Equal(now.Add(time.Hour)) {
		t.Errorf("Expiry: got %v, want %v", c.Expiry, now.Add(time.Hour))
	}
	if c.IssuedAt == nil || !c.IssuedAt.Equal(now) {
		t.Errorf("IssuedAt: got %v, want %v", c.IssuedAt, now)
	}
	if c.UniversalID != "uuid-alice" {
		t.Errorf("UniversalID: got %q", c.UniversalID)
	}
	if c.EmployeeNumber != "E-1001" {
		t.Errorf("EmployeeNumber: got %q", c.EmployeeNumber)
	}
	if c.JobTitle != "Staff Engineer" {
		t.Errorf("JobTitle: got %q", c.JobTitle)
	}
	if c.TenantID != "default" {
		t.Errorf("TenantID: got %q", c.TenantID)
	}
}

func TestNewEnterpriseClaims_EmptySubjectErrors(t *testing.T) {
	_, err := NewEnterpriseClaims(IssuanceParams{TTL: time.Hour}, fixedNow)
	if err != ErrEmptySubject {
		t.Errorf("expected ErrEmptySubject, got %v", err)
	}
}

func TestNewEnterpriseClaims_ZeroTTLErrors(t *testing.T) {
	_, err := NewEnterpriseClaims(IssuanceParams{Subject: "usr_x"}, fixedNow)
	if err != ErrZeroTTL {
		t.Errorf("expected ErrZeroTTL, got %v", err)
	}
}

// TestNewEnterpriseClaims_NilIdentityAndEmployment verifies the omitempty
// path: when both Identity and Employment are nil, no enterprise/identity
// fields leak into the marshalled JSON.
func TestNewEnterpriseClaims_NilIdentityAndEmployment(t *testing.T) {
	c, err := NewEnterpriseClaims(IssuanceParams{
		Subject: "usr_minimal",
		TTL:     time.Hour,
	}, fixedNow)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	bs, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsonStr := string(bs)
	// universal_id is ALWAYS emitted (falls back to Subject per §12.1
	// "三字段同值约定" — Casdoor OAuth-brokered users arrive with it empty).
	for _, banned := range []string{"employee_number", "job_title", "tenant_id"} {
		if strings.Contains(jsonStr, banned) {
			t.Errorf("nil-input JSON should omit %q; got %s", banned, jsonStr)
		}
	}
}

// TestEnterpriseClaims_JSONShape verifies the marshalled JSON keys match the
// wire contract that server-side parsers will consume (snake_case field
// names, omitempty on zero values, RFC 7519 standard claim names).
func TestEnterpriseClaims_JSONShape(t *testing.T) {
	now := fixedNow
	emp := &models.EmploymentIdentity{
		EmployeeNumber: ptrStr("E-1001"),
		JobTitle:       ptrStr("Staff Engineer"),
	}
	c, _ := NewEnterpriseClaims(IssuanceParams{
		Issuer:     "iss-x",
		Subject:    "usr_x",
		Audience:   []string{"aud-1"},
		TTL:        time.Hour,
		Employment: emp,
	}, now)

	var raw map[string]any
	bs, _ := json.Marshal(c)
	if err := json.Unmarshal(bs, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantKeys := map[string]bool{
		"iss": true, "sub": true, "iat": true, "nbf": true, "exp": true,
		"aud": true, "employee_number": true, "job_title": true,
	}
	for k := range wantKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing expected key %q in JSON %s", k, string(bs))
		}
	}
	// aud must serialize as array per RFC 7519 §4.1.3.
	if aud, ok := raw["aud"].([]any); !ok || len(aud) != 1 || aud[0] != "aud-1" {
		t.Errorf("aud shape wrong: %v", raw["aud"])
	}
}

// TestEnterpriseClaims_SignAndVerifyWithSigner exercises the end-to-end
// integration: claims → Signer.SignJWT → public-key parse + exp enforcement.
// This is the closest unit test to the A7 OAuth-callback path. Uses real
// time.Now() so nbf doesn't drift into the future relative to the parser's
// clock — fixedNow would be flaky across wall-clock timezones.
func TestEnterpriseClaims_SignAndVerifyWithSigner(t *testing.T) {
	pk := newRSAKey(t)
	signer := pemEncode(t, pk)
	now := time.Now()
	c, err := NewEnterpriseClaims(IssuanceParams{
		Issuer:   "https://cs-user.test",
		Subject:  "usr_alice",
		Audience: []string{"costrict-web"},
		TTL:      time.Hour,
		Identity: &models.JWTClaims{Name: "Alice", Provider: "idtrust"},
		Employment: &models.EmploymentIdentity{
			EmployeeNumber: ptrStr("E-1"),
			JobTitle:       ptrStr("Eng"),
		},
	}, now)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}

	signed, err := signer.SignJWT(c, now)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}

	parsed, err := jwt.ParseWithClaims(signed, &EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
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
	got, ok := parsed.Claims.(*EnterpriseClaims)
	if !ok {
		t.Fatalf("parsed claims type: %T", parsed.Claims)
	}
	if got.Subject != "usr_alice" {
		t.Errorf("Subject: got %q", got.Subject)
	}
	if got.Name != "Alice" {
		t.Errorf("Name: got %q", got.Name)
	}
	if got.EmployeeNumber != "E-1" {
		t.Errorf("EmployeeNumber: got %q", got.EmployeeNumber)
	}
}

// TestEnterpriseClaims_ExpiredTokenRejected verifies exp is enforced via the
// jwt.Claims interface methods. NotBefore is cleared on the constructed
// claims so the exp gate is what fails — otherwise a past-time issuance
// would trip nbf first ("not valid yet") and mask the expired check.
func TestEnterpriseClaims_ExpiredTokenRejected(t *testing.T) {
	pk := newRSAKey(t)
	signer := pemEncode(t, pk)
	// Issue 2 hours ago, 1h TTL → already expired 1 hour ago.
	past := time.Now().Add(-2 * time.Hour)
	c, err := NewEnterpriseClaims(IssuanceParams{
		Subject: "usr_x",
		TTL:     time.Hour,
	}, past)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	c.NotBefore = nil // isolate the exp gate

	signed, err := signer.SignJWT(c, past)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}

	_, err = jwt.ParseWithClaims(signed, &EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	if err == nil {
		t.Fatal("expected expiry error, got nil")
	}
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

// TestEnterpriseClaims_NilReceiverInterfaceSafety verifies the Get* methods
// don't nil-deref when called on a nil claims pointer. jwt/v5 may invoke
// these during parsing edge cases; defensive nil-handling keeps panic-free.
func TestEnterpriseClaims_NilReceiverInterfaceSafety(t *testing.T) {
	var c *EnterpriseClaims
	if _, err := c.GetIssuer(); err != nil {
		t.Errorf("nil GetIssuer: %v", err)
	}
	if _, err := c.GetSubject(); err != nil {
		t.Errorf("nil GetSubject: %v", err)
	}
	if _, err := c.GetAudience(); err != nil {
		t.Errorf("nil GetAudience: %v", err)
	}
	if _, err := c.GetExpirationTime(); err != nil {
		t.Errorf("nil GetExpirationTime: %v", err)
	}
	if _, err := c.GetIssuedAt(); err != nil {
		t.Errorf("nil GetIssuedAt: %v", err)
	}
	if _, err := c.GetNotBefore(); err != nil {
		t.Errorf("nil GetNotBefore: %v", err)
	}
}

// ptrStr is a test-only helper that takes a string literal and returns a
// pointer to a copy. Mirrors the nullable enterprise column shape.
func ptrStr(s string) *string { return &s }

// --- Permission claims (TenantRoles / PlatformAdmin / PlatformScope) ---

// TestNewEnterpriseClaims_PermissionFieldsRoundTrip verifies the constructor
// carries the new permission fields straight through to the claims struct.
// tenant_admins / platform_admins rows are translated into these claims at
// reissue-token time.
func TestNewEnterpriseClaims_UserOverridesIdentity(t *testing.T) {
	// cs-user's own user record is the canonical source for profile-shaped
	// claims. Identity (forwarded by server from Casdoor) is fallback only —
	// Casdoor's `name` drifts (phone-registered users get name=phone) and
	// must not bleed into relying-party JWTs. This test pins the override
	// contract so callers can rely on User being authoritative when set.
	strPtr := func(s string) *string { return &s }
	identity := &models.JWTClaims{
		UniversalID:       "casdoor-uuid-alice",
		Name:              "15986746954", // Casdoor's name=phone for phone-registered users
		PreferredUsername: "ph_15986746954",
		Email:             "stale@casdoor.example",
		Phone:             "15986746954",
		Picture:           "https://casdoor.example/old.png",
	}
	user := &models.User{
		SubjectID:   "b74e5889-546e-4cc9-9ec5-a0f3de1874c0",
		ShortID:     "u-5Fc4PIuH",
		Username:    "alice",
		DisplayName: strPtr("陈烜42766"),
		Email:       strPtr("alice@costrict.example"),
		Phone:       strPtr("15986746954"),
		AvatarURL:   strPtr("https://costrict.example/alice.png"),
	}
	c, err := NewEnterpriseClaims(IssuanceParams{
		Issuer:   "https://cs-user",
		Subject:  user.SubjectID,
		TTL:      time.Hour,
		Identity: identity,
		User:     user,
	}, fixedNow)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	if c.Name != "陈烜42766" {
		t.Errorf("Name: want %q (cs-user display_name), got %q", "陈烜42766", c.Name)
	}
	if c.PreferredUsername != "alice" {
		t.Errorf("PreferredUsername: want %q (cs-user username), got %q", "alice", c.PreferredUsername)
	}
	if c.ShortID != "u-5Fc4PIuH" {
		t.Errorf("ShortID: want %q (cs-user short_id), got %q", "u-5Fc4PIuH", c.ShortID)
	}
	if c.Email != "alice@costrict.example" {
		t.Errorf("Email: want cs-user email, got %q", c.Email)
	}
	if c.Picture != "https://costrict.example/alice.png" {
		t.Errorf("Picture: want cs-user avatar, got %q", c.Picture)
	}
	// universal_id stays sourced from Identity — it's the cross-IdP join
	// key, not a profile field, and cs-user doesn't own its value.
	if c.UniversalID != "casdoor-uuid-alice" {
		t.Errorf("UniversalID: want %q (Identity-sourced), got %q", "casdoor-uuid-alice", c.UniversalID)
	}
}

func TestNewEnterpriseClaims_UserDisplayNameNilFallsBackToUsername(t *testing.T) {
	// When cs-user has no display_name set, `name` should fall back to
	// cs-user's username rather than Casdoor's stale name=phone.
	user := &models.User{
		SubjectID:   "usr_x",
		Username:    "bob",
		DisplayName: nil,
	}
	identity := &models.JWTClaims{Name: "15999999999"}
	c, err := NewEnterpriseClaims(IssuanceParams{
		Subject:  "usr_x",
		TTL:      time.Hour,
		Identity: identity,
		User:     user,
	}, fixedNow)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	if c.Name != "bob" {
		t.Errorf("Name: want %q (username fallback), got %q", "bob", c.Name)
	}
}

func TestNewEnterpriseClaims_PermissionFieldsRoundTrip(t *testing.T) {
	c, err := NewEnterpriseClaims(IssuanceParams{
		Subject:       "usr_perm",
		TTL:           time.Hour,
		TenantID:      "t-acme",
		TenantRoles:   []string{"tenant_admin", "owner"},
		PlatformAdmin: true,
		PlatformScope: "full",
	}, fixedNow)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	if len(c.TenantRoles) != 2 || c.TenantRoles[0] != "tenant_admin" || c.TenantRoles[1] != "owner" {
		t.Errorf("TenantRoles: got %v", c.TenantRoles)
	}
	if !c.PlatformAdmin {
		t.Errorf("PlatformAdmin: got false, want true")
	}
	if c.PlatformScope != "full" {
		t.Errorf("PlatformScope: got %q, want %q", c.PlatformScope, "full")
	}
}

// TestEnterpriseClaims_PermissionJSONShape verifies the marshalled JSON
// carries the new permission keys (snake_case) so server-side parsers can
// read them post-JWT-decode.
func TestEnterpriseClaims_PermissionJSONShape(t *testing.T) {
	c, _ := NewEnterpriseClaims(IssuanceParams{
		Subject:       "usr_shape",
		TTL:           time.Hour,
		TenantID:      "t-acme",
		TenantRoles:   []string{"tenant_admin"},
		PlatformAdmin: true,
		PlatformScope: "support",
	}, fixedNow)

	var raw map[string]any
	bs, _ := json.Marshal(c)
	if err := json.Unmarshal(bs, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roles, ok := raw["tenant_roles"].([]any); !ok || len(roles) != 1 || roles[0] != "tenant_admin" {
		t.Errorf("tenant_roles shape wrong: %v", raw["tenant_roles"])
	}
	if pa, ok := raw["platform_admin"].(bool); !ok || !pa {
		t.Errorf("platform_admin shape wrong: %v", raw["platform_admin"])
	}
	if scope, ok := raw["platform_scope"].(string); !ok || scope != "support" {
		t.Errorf("platform_scope shape wrong: %v", raw["platform_scope"])
	}
}

// TestEnterpriseClaims_PermissionOmitempty verifies that a regular tenant
// member (no roles, not platform_admin) does NOT carry the permission keys
// in the marshalled JSON — keeps the token minimal and avoids leaking empty
// arrays/fields that downstream parsers might mishandle.
func TestEnterpriseClaims_PermissionOmitempty(t *testing.T) {
	c, _ := NewEnterpriseClaims(IssuanceParams{
		Subject:  "usr_regular",
		TTL:      time.Hour,
		TenantID: "t-acme",
		// TenantRoles nil/empty, PlatformAdmin false, PlatformScope empty
	}, fixedNow)
	bs, _ := json.Marshal(c)
	jsonStr := string(bs)
	for _, banned := range []string{"tenant_roles", "platform_admin", "platform_scope"} {
		if strings.Contains(jsonStr, banned) {
			t.Errorf("regular-user JSON should omit %q; got %s", banned, jsonStr)
		}
	}
}

// TestEnterpriseClaims_PermissionFieldsSignAndVerify ensures the new claims
// survive the sign+parse roundtrip via RSA JWT — middleware on server side
// reads them out of the parsed token.
func TestEnterpriseClaims_PermissionFieldsSignAndVerify(t *testing.T) {
	pk := newRSAKey(t)
	signer := pemEncode(t, pk)
	now := time.Now()
	c, err := NewEnterpriseClaims(IssuanceParams{
		Subject:       "usr_perm",
		TTL:           time.Hour,
		TenantID:      "t-acme",
		TenantRoles:   []string{"owner"},
		PlatformAdmin: true,
		PlatformScope: "full",
	}, now)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	signed, err := signer.SignJWT(c, now)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	parsed, err := jwt.ParseWithClaims(signed, &EnterpriseClaims{}, func(tok *jwt.Token) (any, error) {
		return &pk.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("ParseWithClaims: %v", err)
	}
	got, ok := parsed.Claims.(*EnterpriseClaims)
	if !ok {
		t.Fatalf("parsed claims type: %T", parsed.Claims)
	}
	if len(got.TenantRoles) != 1 || got.TenantRoles[0] != "owner" {
		t.Errorf("TenantRoles after parse: got %v", got.TenantRoles)
	}
	if !got.PlatformAdmin || got.PlatformScope != "full" {
		t.Errorf("PlatformAdmin/Scope after parse: %v / %q", got.PlatformAdmin, got.PlatformScope)
	}
}

// --- Gitea fork binding key: user_id (mirrors sub) ---

// TestNewEnterpriseClaims_UserIDMirrorsSubject pins the duplicate-by-design
// relationship between `user_id` and `sub`. The Gitea fork's CoStrictJWT
// middleware reads `user_id` (and only `user_id`) as the key for its
// binding-status lookup, which our side answers out of user_git_binding —
// whose primary key is (user_subject_id, tenant_id). So the value must be the
// subject_id, not universal_id / short_id / employee number.
func TestNewEnterpriseClaims_UserIDMirrorsSubject(t *testing.T) {
	const subject = "b74e5889-546e-4cc9-9ec5-a0f3de1874c0"
	c, err := NewEnterpriseClaims(IssuanceParams{
		Issuer:  "https://cs-user",
		Subject: subject,
		ShortID: "u-5Fc4PIuH",
		TTL:     time.Hour,
		// Identity carries a DIFFERENT universal_id so a mix-up between the
		// two identifiers would surface here rather than silently pass.
		Identity: &models.JWTClaims{UniversalID: "casdoor-uuid-alice"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	if c.UserID != subject {
		t.Errorf("UserID: want %q (subject_id), got %q", subject, c.UserID)
	}
	if c.UserID != c.Subject {
		t.Errorf("UserID must mirror Subject: user_id=%q sub=%q", c.UserID, c.Subject)
	}
	if c.UserID == c.UniversalID {
		t.Errorf("UserID must be the subject_id, not universal_id (%q)", c.UniversalID)
	}
	if c.UserID == c.ShortID {
		t.Errorf("UserID must be the subject_id, not short_id (%q)", c.ShortID)
	}
}

// TestEnterpriseClaims_UserIDInMarshalledJSON asserts on the marshalled JSON,
// not the struct field. A struct-level assertion cannot catch a missing /
// misspelled json tag, and `omitempty` means a wrong source value would drop
// the key entirely — which is exactly the failure that locks every user out of
// Gitea with a 503 while cs-user reports success.
func TestEnterpriseClaims_UserIDInMarshalledJSON(t *testing.T) {
	const subject = "usr_binding_key"
	c, err := NewEnterpriseClaims(IssuanceParams{
		Subject: subject,
		TTL:     time.Hour,
	}, fixedNow)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}

	bs, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(bs, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := raw["user_id"].(string)
	if !ok {
		t.Fatalf("marshalled JSON is missing the %q key: %s", "user_id", string(bs))
	}
	if got != subject {
		t.Errorf("user_id: want %q, got %q", subject, got)
	}
	if sub, _ := raw["sub"].(string); sub != got {
		t.Errorf("user_id (%q) must equal sub (%q) in the wire payload", got, sub)
	}
}

// TestEnterpriseClaims_UserIDSurvivesSigning decodes the signed token's own
// payload segment — the exact bytes the Gitea fork parses. This is the only
// assertion that proves the claim reaches the relying party; marshalling the
// struct in isolation would still pass if signing dropped it.
func TestEnterpriseClaims_UserIDSurvivesSigning(t *testing.T) {
	const subject = "usr_signed_binding_key"
	pk := newRSAKey(t)
	signer := pemEncode(t, pk)
	now := time.Now()
	c, err := NewEnterpriseClaims(IssuanceParams{
		Issuer:  "https://cs-user.test",
		Subject: subject,
		ShortID: "u-e2es7",
		TTL:     time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("NewEnterpriseClaims: %v", err)
	}
	signed, err := signer.SignJWT(c, now)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}

	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload segment: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, _ := raw["user_id"].(string); got != subject {
		t.Errorf("signed payload user_id: want %q, got %v (payload: %s)", subject, raw["user_id"], string(payload))
	}
	// short_id and user_id are two distinct claims serving two distinct
	// lookups in the fork; assert both are present and different.
	if got, _ := raw["short_id"].(string); got != "u-e2es7" {
		t.Errorf("signed payload short_id: want %q, got %v", "u-e2es7", raw["short_id"])
	}
}

// ===========================================================================
// Contract lock — reflection-based JSON tag vocabulary test.
//
// Per-key tests above prove individual fields serialize correctly, but a
// silent rename or new field wouldn't be caught. This test enumerates the
// COMPLETE expected JSON tag set on EnterpriseClaims via reflection and
// fails on any addition/removal/rename. Update the want set deliberately
// when adding fields, and bump the corresponding server-side consumer
// test (TestParseJWTToken_EnterpriseClaimsRoundTrip).
// ===========================================================================

// TestEnterpriseClaims_JSONTagVocabularyLock is the canonical registry of
// every JWT claim cs-user emits. Drift here breaks server's JWT consumer
// (server/internal/middleware/auth_test.go
// TestParseJWTToken_EnterpriseClaimsRoundTrip) — keep the two in sync.
func TestEnterpriseClaims_JSONTagVocabularyLock(t *testing.T) {
	want := map[string]string{
		// Standard JWT (RFC 7519)
		"iss": "Issuer", "sub": "Subject", "iat": "IssuedAt",
		"nbf": "NotBefore", "exp": "Expiry", "aud": "Audience", "jti": "JTI",

		// OIDC identity (mirrors models.JWTClaims)
		"user_id":            "UserID",
		"universal_id":       "UniversalID",
		"name":               "Name",
		"preferred_username": "PreferredUsername",
		"short_id":           "ShortID",
		"email":              "Email",
		"picture":            "Picture",
		"owner":              "Owner",
		"provider":           "Provider",
		"provider_user_id":   "ProviderUserID",
		"phone":              "Phone",

		// Enterprise context (from employment_identities)
		"enterprise_uid":  "EnterpriseUID",
		"display_name":    "DisplayName",
		"employee_number": "EmployeeNumber",
		"job_title":       "JobTitle",
		"job_level":       "JobLevel",
		"employment_type": "EmploymentType",
		"cost_center":     "CostCenter",
		"org_path":        "OrgPath",
		"work_location":   "WorkLocation",

		// Tenant
		"tenant_id":   "TenantID",
		"tenant_slug": "TenantSlug",

		// Permission
		"tenant_roles":   "TenantRoles",
		"platform_admin": "PlatformAdmin",
		"platform_scope": "PlatformScope",
	}

	got := map[string]string{}
	tp := reflect.TypeOf(EnterpriseClaims{})
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// json tag is "key,omitempty" — strip options.
		key := tag
		if idx := strings.IndexByte(key, ','); idx >= 0 {
			key = key[:idx]
		}
		got[key] = f.Name
	}

	// Detect additions / renames in cs-user not present in want.
	for k, fname := range got {
		wantFname, ok := want[k]
		if !ok {
			t.Errorf("EnterpriseClaims has unexpected JSON tag %q (field %s) — add to want set deliberately if new, and update server's TestParseJWTToken_EnterpriseClaimsRoundTrip", k, fname)
			continue
		}
		if wantFname != fname {
			t.Errorf("JSON tag %q maps to field %s, want %s", k, fname, wantFname)
		}
	}
	// Detect removals — claim key expected but no longer on the struct.
	for k, fname := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("EnterpriseClaims is MISSING JSON tag %q (expected on field %s) — removing a claim breaks downstream consumers; update want set + server consumer test if removal is intentional", k, fname)
		}
	}
}
