package user

import (
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
)

// UserWriter is the write-side seam over user data, the write-path counterpart
// to UserReader. *UserService satisfies it directly (local DB); RPCWriter
// (rpc_writer.go) satisfies it over HTTP to the cs-user microservice.
// Module.NewWithConfig picks one (or a DualWriter wrapping both) based on the
// (Backend, WriteMode) combination — see user.go for the selection matrix.
//
// Signatures intentionally match *UserService's existing write methods
// verbatim — including the lack of a context.Context parameter — so the local
// backend satisfies the interface without modification. RPCWriter uses
// context.Background() internally with the configured per-request timeout as
// the bound; cancellation is not exposed because writes are infrequent
// (login + admin actions) and aborting mid-write would leave cs-user in an
// inconsistent state.
type UserWriter interface {
	GetOrCreateUser(claims *JWTClaims) (*models.User, error)
	SyncUser(claims *JWTClaims) (*models.User, error)
	BindIdentityToUser(userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error
	TransferIdentityToUser(targetUserSubjectID string, externalKey string, sourceUserSubjectID string) error
	UnbindIdentityByProvider(userSubjectID string, provider string) error
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
func (d *DualWriter) GetOrCreateUser(claims *JWTClaims) (*models.User, error) {
	u, err := d.Primary.GetOrCreateUser(claims)
	if err != nil {
		return nil, err
	}
	if d.Secondary != nil {
		if _, secErr := d.Secondary.GetOrCreateUser(claims); secErr != nil {
			logger.Warn("[user-dual-write] secondary GetOrCreateUser failed: %v", secErr)
		}
	}
	return u, nil
}

// SyncUser delegates to Primary and best-effort replicates to Secondary.
// SyncUser is the background-reconciliation path (no post-login hook), so
// divergences here surface stale search results rather than broken logins —
// still logged but lower urgency.
func (d *DualWriter) SyncUser(claims *JWTClaims) (*models.User, error) {
	u, err := d.Primary.SyncUser(claims)
	if err != nil {
		return nil, err
	}
	if d.Secondary != nil {
		if _, secErr := d.Secondary.SyncUser(claims); secErr != nil {
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
func (d *DualWriter) BindIdentityToUser(userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error {
	if err := d.Primary.BindIdentityToUser(userSubjectID, claims, opts...); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.BindIdentityToUser(userSubjectID, claims, opts...); secErr != nil {
			logger.Warn("[user-dual-write] secondary BindIdentityToUser failed: %v", secErr)
		}
	}
	return nil
}

// TransferIdentityToUser delegates to Primary and best-effort replicates to
// Secondary. The third argument (sourceUserSubjectID) is accepted for
// interface symmetry; cs-user identifies the identity purely by external_key.
func (d *DualWriter) TransferIdentityToUser(targetUserSubjectID, externalKey, sourceUserSubjectID string) error {
	if err := d.Primary.TransferIdentityToUser(targetUserSubjectID, externalKey, sourceUserSubjectID); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.TransferIdentityToUser(targetUserSubjectID, externalKey, sourceUserSubjectID); secErr != nil {
			logger.Warn("[user-dual-write] secondary TransferIdentityToUser failed: %v", secErr)
		}
	}
	return nil
}

// UnbindIdentityByProvider delegates to Primary and best-effort replicates to
// Secondary.
func (d *DualWriter) UnbindIdentityByProvider(userSubjectID, provider string) error {
	if err := d.Primary.UnbindIdentityByProvider(userSubjectID, provider); err != nil {
		return err
	}
	if d.Secondary != nil {
		if secErr := d.Secondary.UnbindIdentityByProvider(userSubjectID, provider); secErr != nil {
			logger.Warn("[user-dual-write] secondary UnbindIdentityByProvider failed: %v", secErr)
		}
	}
	return nil
}
