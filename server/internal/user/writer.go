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
// cs-user. The OAuth callback treats this as a non-fatal fallback: with
// Backend=local the deployment can't mint cs-user JWTs, so login falls back
// to the Casdoor token with a WARN log rather than refusing service.
var ErrSelfSignUnavailable = errors.New("jwt self-sign requires rpc backend (server has no local signer)")

// ReissueProfile is the verified, normalized IdP userinfo subset cs-user
// returns in ReissueResult. Field names mirror cs-user's reissueProfile wire
// type 1:1. AsJWTClaims() projects it into the user.JWTClaims shape that
// UserWriter.GetOrCreateUser consumes, so the OAuth callback can drive the
// local mirror write without re-parsing the raw Casdoor JWT.
type ReissueProfile struct {
	ID                string `json:"id,omitempty"`
	Sub               string `json:"sub,omitempty"`
	UniversalID       string `json:"universal_id,omitempty"`
	Name              string `json:"name,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	Email             string `json:"email,omitempty"`
	Phone             string `json:"phone,omitempty"`
	Picture           string `json:"picture,omitempty"`
	Owner             string `json:"owner,omitempty"`
	Provider          string `json:"provider,omitempty"`
	ProviderUserID    string `json:"provider_user_id,omitempty"`
}

// AsJWTClaims projects the profile into a *JWTClaims that UserWriter.
// GetOrCreateUser consumes. Returns nil when p is nil so callers can chain
// without a nil-check.
func (p *ReissueProfile) AsJWTClaims() *JWTClaims {
	if p == nil {
		return nil
	}
	return &JWTClaims{
		ID:                p.ID,
		Sub:               p.Sub,
		UniversalID:       p.UniversalID,
		Name:              p.Name,
		PreferredUsername: p.PreferredUsername,
		Email:             p.Email,
		Phone:             p.Phone,
		Picture:           p.Picture,
		Owner:             p.Owner,
		Provider:          p.Provider,
		ProviderUserID:    p.ProviderUserID,
	}
}

// ReissueResult carries the cs-user reissue-token response. Token + ExpiresAt
// are the legacy fields (always populated on success). ExternalKey /
// SubjectID / IsNew / Profile are populated by RPCWriter roundtrip;
// zero-valued on the local-backend stub (which returns
// ErrSelfSignUnavailable anyway). When Profile is non-nil, the OAuth
// callback can drive its local mirror GetOrCreateUser directly from this
// struct without parsing the raw JWT.
type ReissueResult struct {
	Token       string          `json:"token"`
	ExpiresAt   time.Time       `json:"expires_at"`
	ExternalKey string          `json:"external_key,omitempty"`
	SubjectID   string          `json:"subject_id,omitempty"`
	IsNew       bool            `json:"is_new,omitempty"`
	Profile     *ReissueProfile `json:"profile,omitempty"`
}

// ParseIdentityResult carries the cs-user parse-identity response.
// ExternalKey is the canonical casdoor[:<provider>]:<universal_id>
// cs-user computed from the verified JWT — server uses it for merge_token
// state on the identity_already_bound branch. Profile is the verified,
// normalized IdP userinfo subset; Profile.AsJWTClaims() projects it into a
// *JWTClaims that BindIdentityToUser consumes. RPCWriter populates both
// fields on success; the local-backend stub returns
// (nil, ErrSelfSignUnavailable) under USER_SERVICE_BACKEND=local.
type ParseIdentityResult struct {
	ExternalKey string          `json:"external_key,omitempty"`
	Profile     *ReissueProfile `json:"profile,omitempty"`
}

// UserWriter is the write-side seam over user data, the write-path counterpart
// to UserReader. *UserService satisfies it directly (local DB); RPCWriter
// (rpc_writer.go) satisfies it over HTTP to the cs-user microservice.
// Module.NewWithConfig picks one (or a DualWriter wrapping both) based on the
// (Backend, WriteMode) combination — see user.go for the selection matrix.
//
// Signatures intentionally match *UserService's existing write methods
// verbatim. A leading context.Context parameter is carried on every method —
// RPCWriter uses it to forward the tenant slug (and future tracing
// span) as X-Tenant-Id on the outbound cs-user RPC, and the local UserService
// threads it down to its GORM queries. RPCWriter still wraps the ctx with a
// per-request timeout internally (rpc_client.go defaultTimeout); cancellation
// is best-effort and mid-write aborts leave cs-user in whatever state the
// partial request produced.
type UserWriter interface {
	GetOrCreateUser(ctx context.Context, claims *JWTClaims) (*models.User, bool, error)
	SyncUser(ctx context.Context, claims *JWTClaims) (*models.User, error)
	BindIdentityToUser(ctx context.Context, userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error
	TransferIdentityToUser(ctx context.Context, targetUserSubjectID string, externalKey string, sourceUserSubjectID string) error
	UnbindIdentityByProvider(ctx context.Context, userSubjectID string, provider string) error
	// ApplyEnterpriseMapping refreshes the user's employment_identities
	// snapshot on cs-user. Server has no local employment_identities table,
	// so the local UserService satisfies this with a no-op (see
	// service.go); only RPCWriter actually performs a write. Best-effort at
	// every caller — login must never block on this.
	ApplyEnterpriseMapping(ctx context.Context, userSubjectID string, provider string) error
	// ReissueToken mints a cs-user-signed JWT carrying enterprise claims.
	// The local UserService has no RSA signing key and returns
	// (nil, ErrSelfSignUnavailable) unconditionally; only RPCWriter
	// (Backend=rpc) can fulfill this. DualWriter routes to Secondary
	// directly, bypassing the no-op Primary.
	//
	// RPCWriter's response carries SubjectID / IsNew / Profile alongside the
	// token. The OAuth callback uses Profile to drive its local mirror
	// GetOrCreateUser without re-parsing the raw JWT; IsNew lets it
	// short-circuit the insert-vs-find expectation. The local stub returns
	// (nil, ErrSelfSignUnavailable); under USER_SERVICE_BACKEND=local the
	// OAuth callback's fallback path proceeds with /api/userinfo fields
	// only (deprecated posture — see .env.example).
	// Callers (OAuth callback) treat errors as best-effort: when
	// ReissueToken fails the cookie keeps the Casdoor token, when it
	// succeeds the cookie gets the cs-user token.
	ReissueToken(ctx context.Context, audience []string, rawCasdoorJWT string) (*ReissueResult, error)
	// R2 (REGISTRATION_PROFILE_DESIGN): user-side registration + profile
	// self-edit. Username uniqueness is tenant-scoped under the rpc backend
	// (cs-user) and global under the local backend (server has no tenant_id
	// column on users). complete-registration also stamps profile_completed_at.
	CompleteRegistration(ctx context.Context, userSubjectID, username, displayName string) (*models.User, error)
	UpdateMyProfile(ctx context.Context, userSubjectID, displayName string) (*models.User, error)
	IsUsernameAvailable(ctx context.Context, username, excludeSubjectID string) (bool, error)
	// SuggestProfile is a pure provider → {username, display_name}
	// hint. Local backend has no provider-mapping logic — it returns an
	// empty suggestion; only RPCWriter (Backend=rpc) actually consults
	// cs-user's MapProviderToProfile.
	SuggestProfile(ctx context.Context, claims *JWTClaims) (username, displayName string, err error)
	// ParseIdentity forwards a raw Casdoor JWT to cs-user and gets back the
	// verified profile + computed external_key. The local UserService has
	// no Casdoor JWKS client and returns (nil, ErrSelfSignUnavailable)
	// unconditionally; only RPCWriter (Backend=rpc) can fulfill this.
	// DualWriter routes to Secondary directly, bypassing the no-op Primary.
	// Callers (bindAuthCallback) treat ErrSelfSignUnavailable as failure
	// under USER_SERVICE_BACKEND=local (deprecated — see .env.example).
	// Added in Phase 3.
	ParseIdentity(ctx context.Context, rawJWT string) (*ParseIdentityResult, error)
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
//
// Returned identity authority: GetOrCreateUser is asymmetric — when
// Secondary succeeds, its *models.User (cs-user's authoritative record) is
// returned, not Primary's. cs-user's subject_id is the durable handle for
// all identity-side concerns (employment, permissions, JWT sub). The other
// methods (SyncUser, BindIdentityToUser, etc.) preserve Primary-authoritative
// return semantics because their return values do not flow into JWT signing.
type DualWriter struct {
	Primary   UserWriter // *UserService — authoritative during canary
	Secondary UserWriter // *RPCWriter — best-effort replication target
}

// GetOrCreateUser delegates to Primary (which fires the post-login hook and
// writes the local mirror row) and then best-effort replicates to Secondary.
//
// Return value: when Secondary succeeds, its user (cs-user's authoritative
// record) is returned — NOT Primary's. cs-user generates its own stable
// subject_id (usr_<uuid>) per external_key, distinct from server's
// locally-generated subject_id. Downstream callers — most notably the OAuth
// callback's ReissueToken call — must use cs-user's subject_id, because
// cs-user keys employment_identities / tenant_roles / platform_admins to
// that ID and embeds it as the JWT `sub`. Returning Primary's ID here would
// mint a JWT whose `sub` cs-user cannot resolve, dropping enterprise +
// permission claims on every dual-write-canary login.
//
// The post-login hook still fires inside Primary on Primary's user; the local
// users table still receives Primary's write. Canary divergence is observable
// by comparing the two databases, not by inspecting this method's return
// value. When Secondary fails or is nil, Primary's user is returned as a
// degraded fallback — login proceeds, JWT signing will surface the
// misalignment via missing enterprise claims (caught by cs-user's
// external_key fallback at the cs-user side).
func (d *DualWriter) GetOrCreateUser(ctx context.Context, claims *JWTClaims) (*models.User, bool, error) {
	u, isNew, err := d.Primary.GetOrCreateUser(ctx, claims)
	if err != nil {
		return nil, false, err
	}
	if d.Secondary != nil {
		secUser, secIsNew, secErr := d.Secondary.GetOrCreateUser(ctx, claims)
		if secErr != nil {
			logger.Warn("[user-dual-write] secondary GetOrCreateUser failed (returning primary user as fallback): %v", secErr)
		} else if secUser != nil {
			return secUser, secIsNew, nil
		}
	}
	return u, isNew, nil
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
// (nil, ErrSelfSignUnavailable) so the OAuth callback surfaces the
// misconfiguration. The return shape is *ReissueResult so the OAuth
// callback can drive its local mirror GetOrCreateUser from the response
// Profile.
func (d *DualWriter) ReissueToken(ctx context.Context, audience []string, rawCasdoorJWT string) (*ReissueResult, error) {
	if d.Secondary == nil {
		return nil, ErrSelfSignUnavailable
	}
	result, err := d.Secondary.ReissueToken(ctx, audience, rawCasdoorJWT)
	if err != nil {
		// Log + propagate. Unlike ApplyEnterpriseMapping (pure best-effort),
		// ReissueToken errors must reach the OAuth callback so it can decide
		// whether to fall back to the Casdoor token or fail the request.
		logger.Warn("[user-dual-write] secondary ReissueToken failed: %v", err)
		return nil, err
	}
	return result, nil
}

// CompleteRegistration is dual-write: cs-user is the eventual authority,
// but server's local mirror must stay consistent so handlers see the new
// username immediately. Primary-first for atomic read-back; Secondary failure
// is logged but does NOT unwind Primary — the caller has already shown the
// user a success page, and a retry path will reconcile cs-user.
func (d *DualWriter) CompleteRegistration(ctx context.Context, userSubjectID, username, displayName string) (*models.User, error) {
	u, err := d.Primary.CompleteRegistration(ctx, userSubjectID, username, displayName)
	if err != nil {
		return nil, err
	}
	if d.Secondary != nil {
		if _, secErr := d.Secondary.CompleteRegistration(ctx, userSubjectID, username, displayName); secErr != nil {
			logger.Warn("[user-dual-write] secondary CompleteRegistration failed: %v", secErr)
		}
	}
	return u, nil
}

// UpdateMyProfile dual-writes the display_name change. username is not
// in scope for user-self edits (admin override uses a separate admin RPC).
func (d *DualWriter) UpdateMyProfile(ctx context.Context, userSubjectID, displayName string) (*models.User, error) {
	u, err := d.Primary.UpdateMyProfile(ctx, userSubjectID, displayName)
	if err != nil {
		return nil, err
	}
	if d.Secondary != nil {
		if _, secErr := d.Secondary.UpdateMyProfile(ctx, userSubjectID, displayName); secErr != nil {
			logger.Warn("[user-dual-write] secondary UpdateMyProfile failed: %v", secErr)
		}
	}
	return u, nil
}

// IsUsernameAvailable consults Primary (the local mirror). The local
// table has no tenant_id, so uniqueness is global under the local backend;
// under rpc-only deployments the call is served by RPCWriter and respects
// cs-user's tenant scope via X-Tenant-Id.
func (d *DualWriter) IsUsernameAvailable(ctx context.Context, username, excludeSubjectID string) (bool, error) {
	return d.Primary.IsUsernameAvailable(ctx, username, excludeSubjectID)
}

// SuggestProfile routes to Secondary (cs-user's pure generator). The
// local Primary has no provider-mapping logic; forwarding to it would
// always return an empty suggestion and mask Secondary's result. Mirrors
// ReissueToken's pattern. Errors propagate so the handler can fall back
// to an empty suggestion on RPC failure.
func (d *DualWriter) SuggestProfile(ctx context.Context, claims *JWTClaims) (string, string, error) {
	if d.Secondary == nil {
		return "", "", nil
	}
	return d.Secondary.SuggestProfile(ctx, claims)
}

// ParseIdentity routes to Secondary (RPCWriter → cs-user) and skips Primary
// entirely. Mirrors ReissueToken: the local Primary has no Casdoor JWKS
// client, so routing through it would always return ErrSelfSignUnavailable
// and mask Secondary's result. When Secondary is nil (e.g. a single-primary
// config), returns (nil, ErrSelfSignUnavailable) — under
// USER_SERVICE_BACKEND=local the bind identity callback fails outright.
func (d *DualWriter) ParseIdentity(ctx context.Context, rawJWT string) (*ParseIdentityResult, error) {
	if d.Secondary == nil {
		return nil, ErrSelfSignUnavailable
	}
	result, err := d.Secondary.ParseIdentity(ctx, rawJWT)
	if err != nil {
		// Log + propagate. Unlike ApplyEnterpriseMapping (pure best-effort),
		// ParseIdentity errors must reach the bind callback so it can decide
		// whether to fall back to the local unverified parse or fail the
		// request.
		logger.Warn("[user-dual-write] secondary ParseIdentity failed: %v", err)
		return nil, err
	}
	return result, nil
}
