// Local UserService stubs for ApplyEnterpriseMapping / ReissueToken /
// ParseIdentity. The server's local DB has no employment_identities table
// (that table is cs-user's exclusive ownership — see
// cs-user/migrations/20260716150000). When the server runs in Backend=local
// (default) there is nothing to write; the call must be a no-op so the OAuth
// callback's hook fires harmlessly on every deployment posture:
//
//   - Backend=local, WriteMode=local  → this no-op (local has no table)
//   - Backend=rpc,   WriteMode=local  → DualWriter: this no-op (Primary) +
//                                       RPCWriter (Secondary)
//   - Backend=rpc,   WriteMode=readonly → RPCWriter only (writer.go skips svc)
//
// ReissueToken / ParseIdentity stubs: server has no RSA signing key nor a
// Casdoor JWKS client locally; both calls always return
// ErrSelfSignUnavailable. Callers must gate on USER_SERVICE_BACKEND=rpc
// before invoking.

package user

import (
	"context"
)

// ApplyEnterpriseMapping is the local-backend stub. Server has no
// employment_identities table; employment mapping only takes effect once the
// deployment cutover to cs-user (USER_SERVICE_BACKEND=rpc). Returns nil
// unconditionally so callers can fire it from best-effort hooks without a
// local-mode conditional.
func (s *UserService) ApplyEnterpriseMapping(ctx context.Context, userSubjectID, provider string) error {
	_ = ctx
	_ = userSubjectID
	_ = provider
	return nil
}

// ReissueToken is the local-backend stub. Server has no RSA signing key
// configured — JWT self-signing requires USER_SERVICE_BACKEND=rpc so the call
// routes through RPCWriter → cs-user. Returns (nil, ErrSelfSignUnavailable)
// unconditionally so callers can detect this path. DualWriter bypasses
// Primary entirely; the OAuth callback treats ErrSelfSignUnavailable as a
// signal to fall back to the raw Casdoor access token (lossy — only
// /api/userinfo fields survive, no JWT-only enrichment).
//
// Return shape is (*ReissueResult, error). The local stub never produces
// a result.
//
// The audience + rawCasdoorJWT parameters are accepted for interface symmetry
// with RPCWriter. They're ignored (no signer to honor them).
func (s *UserService) ReissueToken(ctx context.Context, audience []string, rawCasdoorJWT string) (*ReissueResult, error) {
	_ = ctx
	_ = audience
	_ = rawCasdoorJWT
	return nil, ErrSelfSignUnavailable
}

// ParseIdentity is the local-backend stub. Server has no Casdoor JWKS client
// configured — verifying the raw JWT requires USER_SERVICE_BACKEND=rpc so the
// call routes through RPCWriter → cs-user. Returns (nil,
// ErrSelfSignUnavailable) unconditionally. Under local mode the bind identity
// callback fails outright (USER_SERVICE_BACKEND=local is DEPRECATED — see
// .env.example). DualWriter bypasses Primary entirely.
//
// The rawJWT parameter is accepted for interface symmetry with RPCWriter.
// It's ignored (no verifier to honour it).
func (s *UserService) ParseIdentity(ctx context.Context, rawJWT string) (*ParseIdentityResult, error) {
	_ = ctx
	_ = rawJWT
	return nil, ErrSelfSignUnavailable
}
