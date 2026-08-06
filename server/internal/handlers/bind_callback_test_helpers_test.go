package handlers

import (
	"context"
	"fmt"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/golang-jwt/jwt/v5"
)

// jwtParsingStubWriter wraps the local *user.UserService for bind-callback
// tests, intercepting ParseIdentity to parse test-signed JWTs inline. The
// production ParseIdentity path forwards the raw JWT to cs-user's
// /api/internal/auth/parse-identity RPC; under USER_SERVICE_BACKEND=local —
// which is what these tests configure — the local UserService stub returns
// ErrSelfSignUnavailable. Without this shim, every bind-callback test would
// 401 at parseIdentityClaims and we'd lose coverage of the bind flow
// (provider mismatch, identity_already_bound, success path).
//
// The shim parses the JWT WITHOUT signature verification. That's safe here
// because the test fixture signs the JWT itself — the goal is just to
// recover the claims the test set up. The production cs-user RPC verifies
// the signature; that contract is pinned in cs-user's TestParseIdentity_*
// tests, not here.
//
// All other UserWriter methods delegate to the embedded *UserService so the
// bind flow's GetOrCreateUser / BindIdentityToUser calls land in the real
// in-memory DB and the test asserts on the resulting rows.
type jwtParsingStubWriter struct {
	*userpkg.UserService
}

func (w *jwtParsingStubWriter) ParseIdentity(_ context.Context, rawJWT string) (*userpkg.ParseIdentityResult, error) {
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := jwt.MapClaims{}
	_, _, err := parser.ParseUnverified(rawJWT, claims)
	if err != nil {
		return nil, fmt.Errorf("bind test stub: parse jwt: %w", err)
	}
	profile := mapClaimsToReissueProfile(claims)
	if profile == nil {
		return nil, fmt.Errorf("bind test stub: claims carry no identity fields")
	}
	return &userpkg.ParseIdentityResult{
		ExternalKey: buildTestExternalKey(profile),
		Profile:     profile,
	}, nil
}

// mapClaimsToReissueProfile mirrors cs-user's NormalizeClaimsMap output for
// the subset of fields the bind flow reads. Sufficient for tests; not a
// general-purpose normalizer.
func mapClaimsToReissueProfile(claims jwt.MapClaims) *userpkg.ReissueProfile {
	p := &userpkg.ReissueProfile{}
	hasAny := false
	if v, ok := claims["id"].(string); ok && v != "" {
		p.ID = v
		hasAny = true
	}
	if v, ok := claims["sub"].(string); ok && v != "" {
		p.Sub = v
		hasAny = true
	}
	if v, ok := claims["universal_id"].(string); ok && v != "" {
		p.UniversalID = v
		hasAny = true
	}
	if v, ok := claims["name"].(string); ok && v != "" {
		p.Name = v
	}
	if v, ok := claims["preferred_username"].(string); ok && v != "" {
		p.PreferredUsername = v
	}
	if v, ok := claims["email"].(string); ok && v != "" {
		p.Email = v
	}
	if v, ok := claims["phone_number"].(string); ok && v != "" {
		p.Phone = v
	}
	if v, ok := claims["picture"].(string); ok && v != "" {
		p.Picture = v
	}
	if v, ok := claims["owner"].(string); ok && v != "" {
		p.Owner = v
	}
	if v, ok := claims["provider"].(string); ok && v != "" {
		p.Provider = v
	}
	// Mirror cs-user's NormalizeClaimsMap precedence: oauth_<Provider>_id
	// → provider_user_id.
	if props, ok := claims["properties"].(map[string]any); ok {
		if pv, ok := props["oauth_Custom_id"].(string); ok && pv != "" {
			p.ProviderUserID = pv
		}
		if pv, ok := props["oauth_GitHub_id"].(string); ok && pv != "" {
			p.ProviderUserID = pv
		}
	}
	if !hasAny {
		return nil
	}
	return p
}

// buildTestExternalKey mirrors user.UserService.buildExternalKey for the
// happy path (casdoor:<provider>:<universal_id>). Tests don't exercise the
// legacy fallback paths, so this stays minimal.
func buildTestExternalKey(p *userpkg.ReissueProfile) string {
	if p == nil || p.UniversalID == "" {
		return ""
	}
	if p.Provider != "" {
		return "casdoor:" + p.Provider + ":" + p.UniversalID
	}
	return "casdoor:" + p.UniversalID
}

// Compile-time interface check — guarantees the stub satisfies UserWriter.
// If a future UserWriter method isn't delegated here, this fails the build.
var _ userpkg.UserWriter = (*jwtParsingStubWriter)(nil)
