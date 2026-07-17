package user

import (
	"context"
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
)

// ErrSelfSignUnavailable is returned by UserService.ReissueToken on the local
// backend — server has no RSA signing key configured; JWT self-signing
// requires USER_SERVICE_BACKEND=rpc so the call routes through RPCWriter →
// cs-user. The OAuth callback treats this as a non-fatal fallback (Phase A8
// 灰度): when JWT_SIGN_MODE=dual|single and Backend=local, deployment config
// is contradictory — login falls back to the Casdoor token with a WARN log
// rather than refusing service.
//
// Added in Phase A7b; docstring updated in Phase A8.
var ErrSelfSignUnavailable = errors.New("jwt self-sign requires rpc backend (server has no local signer)")

// UserWriter is the write-side seam over user data, the write-path counterpart
// to UserReader. *UserService satisfies it directly (local DB); RPCWriter
// (rpc_writer.go) satisfies it over HTTP to the cs-user microservice.
// Module.NewWithConfig picks one (or a DualWriter wrapping both) based on the
// (Backend, WriteMode) combination — see user.go for the selection matrix.
//
// Signatures intentionally match *UserService's existing write methods
// verbatim. Phase B3b.2b added a leading context.Context parameter to every
// method — RPCWriter uses it to forward the tenant slug (and future tracing
// span) as X-Tenant-Id on the outbound cs-user RPC, and the local UserService
// threads it down to its GORM queries. RPCWriter still wraps the ctx with a
// per-request timeout internally (rpc_client.go defaultTimeout); cancellation
// is best-effort and mid-write aborts leave cs-user in whatever state the
// partial request produced.
type UserWriter interface {
	GetOrCreateUser(ctx context.Context, claims *JWTClaims) (*models.User, error)
	SyncUser(ctx context.Context, claims *JWTClaims) (*models.User, error)
	BindIdentityToUser(ctx context.Context, userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error
	TransferIdentityToUser(ctx context.Context, targetUserSubjectID string, externalKey string, sourceUserSubjectID string) error
	UnbindIdentityByProvider(ctx context.Context, userSubjectID string, provider string) error
	// ApplyEnterpriseMapping refreshes the user's employment_identities
	// snapshot on cs-user. Server has no local employment_identities table,
	// so the local UserService satisfies this with a no-op (see
	// service.go); only RPCWriter actually performs a write. Best-effort at
	// every caller — login must never block on this.
	// Added in Phase A4b.
	ApplyEnterpriseMapping(ctx context.Context, userSubjectID string, provider string) error
	// ReissueToken mints a cs-user-signed JWT carrying enterprise claims
	// (Phase A7). The local UserService has no RSA signing key and returns
	// ErrSelfSignUnavailable unconditionally; only RPCWriter (Backend=rpc)
	// can fulfill this. DualWriter routes to Secondary directly, bypassing
	// the no-op Primary.
	//
	// Returns (token, expiresAt, err). Callers (OAuth callback) treat errors
	// as best-effort: when ReissueToken fails the cookie keeps the Casdoor
	// token, when it succeeds the cookie gets the cs-user token.
	// Added in Phase A7b.
	ReissueToken(ctx context.Context, userSubjectID string, claims *JWTClaims, audience []string) (string, time.Time, error)
}

// DualWriter is the canary posture selected by USER_SERVICE_BACKEND=rpc with
// USER_SERVICE_WRITE_MODE=local (the P0-8 runbook's "step 3: dual-write
// belt-and-suspenders" combination). Every write hits the local UserService
// first (Primary); on success, it is best-effort replicated to cs-user via
// Secondary. Primary errors propagate to the caller; Secondary errors are
// logged but never fail the request — the canary's whole point is to expose
// RPC divergence under live traffic WITHOUT breaking user flows.
//
// Secondary is invoked synchronously so divergence is observable in the
// request path (a slow secondary will slow the request; if that turns out
// to be a problem, swap to a fire-and-forget goroutine with a bounded
// queue). The 10s per-request timeout (rpc_client.go:defaultTimeout) caps
// the worst case.
//
// GetOrCreateUser's post-login hook fires inside Primary (UserService), so
// the hook runs exactly once per login — Secondary (RPCWriter) does not
// re-run it (cs-user has no systemrole package; the hook is server-side).
type DualWriter struct {
	Primary   UserWriter // *UserService — authoritative during canary
	Secondary UserWriter // *RPCWriter — best-effort replication target
}

// GetOrCreateUser delegates to Primary (which fires the post-login hook) and
// best-effort replicates to Secondary. Returns Primary's user; Secondary
// divergence is logged only.
func (d *DualWriter) GetOrCreateUser(ctx context.Context, claims *JWTClaims) (*models.User, error) {
	u, err := d.Primary.GetOrCreateUser(ctx, claims)
	if err != nil {
		return nil, err
	}
	if d.Secondary != nil {
		if _, secErr := d.Secondary.GetOrCreateUser(ctx, claims); secErr != nil {
			logger.Warn("[user-dual-write] secondary GetOrCreateUser failed: %v", secErr)
		}
	}
	return u, nil
}

// SyncUser delegates to Primary and best-effort replicates to Secondary.
// SyncUser is the background-reconciliation path (no post-login hook), so
// divergences here surface stale search results rather than broken logins —
// still logged but lower urgency.
func (d *DualWriter) SyncUser(ctx context.Context, claims *JWTClaims) (*models.User, error) {
	u, err := d.Primary.SyncUser(ctx, claims)
	if err != nil {
		return nil, err
	}
	if d.Secondary != nil {
		if _, secErr := d.Secondary.SyncUser(ctx, claims); secErr != nil {
			logger.Warn("[user-dual-write] secondary SyncUser failed: %v", secErr)
		}
	}
	return u, nil
}

// BindIdentityToUser delegates to Primary and best-effort replicates to
// Secondary. Secondary's "identity already bound to another user" 409 is
// expected during canary (cs-user may already have the identity from a prior
// ETL tick) and is downgraded to a debug log — Primary's result is what the
// handler acts on.
func (d *DualWriter) BindIdentityToUser(ctx context.Context, userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error {
	if err := d.Primary.BindIdentityToUser(ctx, userSubjectID, claims, opts...); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.BindIdentityToUser(ctx, userSubjectID, claims, opts...); secErr != nil {
			logger.Warn("[user-dual-write] secondary BindIdentityToUser failed: %v", secErr)
		}
	}
	return nil
}

// TransferIdentityToUser delegates to Primary and best-effort replicates to
// Secondary. The third argument (sourceUserSubjectID) is accepted for
// interface symmetry; cs-user identifies the identity purely by external_key.
func (d *DualWriter) TransferIdentityToUser(ctx context.Context, targetUserSubjectID, externalKey, sourceUserSubjectID string) error {
	if err := d.Primary.TransferIdentityToUser(ctx, targetUserSubjectID, externalKey, sourceUserSubjectID); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.TransferIdentityToUser(ctx, targetUserSubjectID, externalKey, sourceUserSubjectID); secErr != nil {
			logger.Warn("[user-dual-write] secondary TransferIdentityToUser failed: %v", secErr)
		}
	}
	return nil
}

// UnbindIdentityByProvider delegates to Primary and best-effort replicates to
// Secondary.
func (d *DualWriter) UnbindIdentityByProvider(ctx context.Context, userSubjectID, provider string) error {
	if err := d.Primary.UnbindIdentityByProvider(ctx, userSubjectID, provider); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.UnbindIdentityByProvider(ctx, userSubjectID, provider); secErr != nil {
			logger.Warn("[user-dual-write] secondary UnbindIdentityByProvider failed: %v", secErr)
		}
	}
	return nil
}

// ApplyEnterpriseMapping delegates to Primary (which is the local UserService —
// a no-op, since the server has no employment_identities table) and then to
// Secondary (the RPCWriter, which forwards the actual write to cs-user). The
// Primary call is preserved for interface symmetry and so a future local
// implementation could be slotted in without touching DualWriter.
//
// Errors from Secondary are logged but never returned: this method is fired
// from the OAuth callback's post-GetOrCreateUser hook, and employment mapping
// is a bonus feature that must never block login.
func (d *DualWriter) ApplyEnterpriseMapping(ctx context.Context, userSubjectID, provider string) error {
	if err := d.Primary.ApplyEnterpriseMapping(ctx, userSubjectID, provider); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.ApplyEnterpriseMapping(ctx, userSubjectID, provider); secErr != nil {
			logger.Warn("[user-dual-write] secondary ApplyEnterpriseMapping failed: %v", secErr)
		}
	}
	return nil
}

// ReissueToken routes to Secondary (RPCWriter → cs-user) and skips Primary
// entirely. Unlike the other DualWriter methods, Primary cannot fulfill this:
// the local UserService has no RSA signing key. Routing through Primary would
// always return ErrSelfSignUnavailable and mask Secondary's result. Secondary
// is authoritative for token issuance.
//
// When Secondary is nil (e.g. a future single-primary config), returns
// ErrSelfSignUnavailable so the OAuth callback surfaces the misconfiguration.
// Phase A7b.
func (d *DualWriter) ReissueToken(ctx context.Context, userSubjectID string, claims *JWTClaims, audience []string) (string, time.Time, error) {
	if d.Secondary == nil {
		return "", time.Time{}, ErrSelfSignUnavailable
	}
	token, exp, err := d.Secondary.ReissueToken(ctx, userSubjectID, claims, audience)
	if err != nil {
		// Log + propagate. Unlike ApplyEnterpriseMapping (pure best-effort),
		// ReissueToken errors must reach the OAuth callback so it can decide
		// whether to fall back to the Casdoor token or fail the request.
		logger.Warn("[user-dual-write] secondary ReissueToken failed: %v", err)
		return "", time.Time{}, err
	}
	return token, exp, nil
}
