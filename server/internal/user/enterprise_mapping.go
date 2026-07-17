// Phase A4b: local UserService stub for ApplyEnterpriseMapping. The server's
// local DB has no employment_identities table (that table is cs-user's
// exclusive ownership — see cs-user/migrations/20260716150000). When the
// server runs in Backend=local (default) there is nothing to write; the call
// must be a no-op so the OAuth callback's hook fires harmlessly on every
// deployment posture:
//
//   - Backend=local, WriteMode=local  → this no-op (local has no table)
//   - Backend=rpc,   WriteMode=local  → DualWriter: this no-op (Primary) +
//                                       RPCWriter (Secondary)
//   - Backend=rpc,   WriteMode=readonly → RPCWriter only (writer.go skips svc)
//
// When the writer selection (user.go:NewWithConfig) leaves the writer as
// *UserService, employment mapping silently degrades to "off" — that's the
// correct behaviour for a deployment that hasn't cutover to cs-user yet.
//
// Phase A7b: ReissueToken stub added. Server has no RSA signing key locally;
// the call always returns ErrSelfSignUnavailable. Callers (OAuth callback)
// must gate on USER_SERVICE_BACKEND=rpc before invoking.

package user

import (
	"context"
	"time"
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

// ReissueToken is the local-backend stub for Phase A7b. Server has no RSA
// signing key configured — JWT self-signing requires USER_SERVICE_BACKEND=rpc
// so the call routes through RPCWriter → cs-user. Returns
// ErrSelfSignUnavailable unconditionally so callers can detect this path
// (DualWriter bypasses Primary entirely; OAuth callback must check that the
// writer isn't a bare *UserService when JWT_SELF_SIGN_ENABLED=true).
//
// The audience parameter is accepted for interface symmetry with RPCWriter.
// It's ignored (no signer to honor it).
func (s *UserService) ReissueToken(ctx context.Context, userSubjectID string, claims *JWTClaims, audience []string) (string, time.Time, error) {
	_ = ctx
	_ = userSubjectID
	_ = claims
	_ = audience
	return "", time.Time{}, ErrSelfSignUnavailable
}
