// Phase A7: cs-user OAuth-callback takeover endpoint.
//
// Strategy (b) re-sign: Casdoor still handles the login UI + OAuth dance +
// password reset + MFA. The server validates the Casdoor JWT it received,
// then forwards the parsed claims + user_subject_id to this endpoint. cs-user
// loads the employment_identities snapshot (Phase A4), builds enterprise
// claims (Phase A5), signs the token (Phase A3), and returns it. The server
// then sets the cookie + handles the dual-sign 灰度 window (Phase A8).
//
// Trust boundary: cs-user does NOT re-validate the Casdoor JWT signature.
// The X-Internal-Token middleware has already authenticated the caller as
// the server, and the server has already validated the original token; the
// Identity payload here is treated as data, not as a security primitive.

package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AuthAPI bundles the dependencies the reissue-token flow needs. Lives
// separately from UsersAPI because the orchestration spans two services
// (UserService for employment read, Signer for JWT issuance) and the route
// shape (`/users/reissue-token`) sits inside the users group but isn't a
// pure user CRUD op.
type AuthAPI struct {
	Svc    EmploymentReader
	Signer *auth.Signer
	JWT    config.JWTConfig
	// Permissions optionally carries the Phase C1 permission readers. When
	// nil, no permission claims are populated (graceful — used during the
	// Phase C1 灰度 rollout before middlewares enforce the new claims).
	// When set, the handler queries both GetPlatformAdmin + ListActiveTenantRoles
	// and translates the result into the corresponding JWT claims.
	Permissions PermissionReader
}

// EmploymentReader is the subset of *user.Service the reissue flow needs.
// Declared as an interface for the same testability reasons as UserService
// — sqlite-backed fakes substitute without spinning a real Service.
type EmploymentReader interface {
	GetEmploymentIdentity(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error)
}

// PermissionReader is the Phase C1 subset of *user.Service the reissue flow
// needs to populate the platform_admin / platform_scope / tenant_roles JWT
// claims. Same interface-for-testability rationale as EmploymentReader.
//
// Both methods use the graceful-degradation contract: missing data surfaces
// as (nil, nil) / empty slice — not an error — so a regular tenant member
// without admin roles still gets a valid token, just without the permission
// claims (TestReissueToken_NoPermissionRowStillIssuesToken locks this in).
type PermissionReader interface {
	GetPlatformAdmin(ctx context.Context, userSubjectID string) (*models.PlatformAdmin, error)
	ListActiveTenantRoles(ctx context.Context, userSubjectID, tenantID string) ([]string, error)
}

// reissueTokenRequest is the body shape for POST
// /api/internal/users/reissue-token. The server forwards the parsed Casdoor
// claims (Identity) plus the user_subject_id it resolved via
// GetOrCreateUser. TenantID is reserved for Phase B; Phase A callers pass
// "default" or leave empty.
//
// Audience overrides JWTConfig.DefaultAudience when the server knows a
// specific relying party is the target (e.g. csc CLI vs. costrict-web
// frontend). Empty array falls back to the default.
type reissueTokenRequest struct {
	// UserSubjectID is the cs-user user's stable subject_id. Required.
	UserSubjectID string `json:"user_subject_id" binding:"required"`

	// Identity carries the parsed Casdoor JWT claims. Optional — when nil,
	// only standard JWT claims + enterprise claims (if any) are emitted.
	// Typical Phase A7 callers always pass Identity; the nil path exists
	// for refresh-token flows (Phase B) where identity may be cached.
	Identity *models.JWTClaims `json:"identity,omitempty"`

	// TenantID is reserved for Phase B. Phase A callers pass "default" or
	// leave empty; the service falls back to "default".
	TenantID string `json:"tenant_id,omitempty"`

	// TenantSlug is the URL-friendly tenant key (Phase B). Forwarded by
	// server's RPCWriter from the request ctx (set by Try 1 subdomain or
	// Try 2 email-domain resolution in the OAuth callback). cs-user embeds
	// this verbatim in the signed JWT so server's TenantMatch middleware
	// can compare against the runtime-resolved slug without a per-request
	// slug→tenant_id lookup. Empty for Phase A callers.
	TenantSlug string `json:"tenant_slug,omitempty"`

	// Audience overrides the configured default. Empty slice falls back
	// to JWTConfig.DefaultAudience; populated slice replaces it.
	Audience []string `json:"audience,omitempty"`
}

// reissueTokenResponse returns the signed token plus its expiry so the
// caller (server) can set a cookie with the right MaxAge without re-parsing
// the JWT to read exp.
type reissueTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReissueToken godoc
//
//	@Summary		Reissue a cs-user-signed JWT (OAuth callback takeover)
//	@Description	Strategy (b) re-sign: server validates the Casdoor JWT, then calls this endpoint with the parsed claims + user_subject_id. cs-user loads the user's employment_identities snapshot (Phase A4), builds enterprise claims (Phase A5), signs via the configured RSA key (Phase A3), and returns the new token. cs-user does NOT re-validate the Casdoor JWT — the X-Internal-Token middleware authenticates the caller as the server, and the server has already validated the original. Returns 503 when no signing key is configured.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		reissueTokenRequest	true	"Parsed Casdoor claims + user_subject_id + optional tenant_id / audience override"
//	@Success		200		{object}	reissueTokenResponse
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Failure		503		{object}	object{error=string}
//	@Router			/api/internal/users/reissue-token [post]
func (a *AuthAPI) ReissueToken(c *gin.Context) {
	if a.Signer == nil {
		// JWKS also returns 503 in this state — operator hasn't set
		// CS_USER_JWT_SIGNING_KEY_PATH yet. We surface it as 503 (not 500)
		// so health probes can distinguish config-missing from bug.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "JWT signing not configured"})
		return
	}

	var req reissueTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	// Audience: request override wins; otherwise fall back to config default.
	audience := req.Audience
	if len(audience) == 0 {
		audience = a.JWT.DefaultAudience
	}

	// TenantID: Phase A always uses "default" when caller omits.
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = "default"
	}

	// TenantSlug (Phase B): pass through verbatim. Server's auth middleware
	// reads this from the signed JWT to compare against runtime-resolved slug
	// (cookie/subdomain) for cross-tenant detection (B3b.2c). Empty for
	// pre-cutover / Phase A callers — server's TenantMatch middleware skips
	// comparison when the JWT has no tenant_slug claim.
	tenantSlug := req.TenantSlug

	employment, err := a.Svc.GetEmploymentIdentity(c.Request.Context(), req.UserSubjectID)
	if err != nil {
		switch {
		case errors.Is(err, user.ErrEmptySubjectID):
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_subject_id is required"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	// employment == nil is success — user has no enterprise snapshot yet.

	// Phase C1: populate permission claims from tenant_admins +
	// platform_admins. Skipped entirely when Permissions is nil (灰度
	// rollout: callers that haven't wired the new readers yet). Errors here
	// surface as 500 — both methods only return errors on real DB faults;
	// missing data is (nil,nil) / empty slice, not error.
	var platformAdmin bool
	var platformScope string
	var tenantRoles []string
	if a.Permissions != nil {
		pa, err := a.Permissions.GetPlatformAdmin(c.Request.Context(), req.UserSubjectID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		if pa != nil {
			platformAdmin = true
			platformScope = pa.Scope
		}

		roles, err := a.Permissions.ListActiveTenantRoles(c.Request.Context(), req.UserSubjectID, tenantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		tenantRoles = roles
	}

	now := time.Now()
	claims, err := auth.NewEnterpriseClaims(auth.IssuanceParams{
		Issuer:        a.JWT.Issuer,
		Subject:       req.UserSubjectID,
		Audience:      audience,
		TTL:           a.JWT.TTL,
		JTI:           uuid.NewString(),
		Identity:      req.Identity,
		Employment:    employment,
		TenantID:      tenantID,
		TenantSlug:    tenantSlug,
		TenantRoles:   tenantRoles,
		PlatformAdmin: platformAdmin,
		PlatformScope: platformScope,
	}, now)
	if err != nil {
		// NewEnterpriseClaims only fails on empty Subject (caught above by
		// binding) or zero TTL (config bug, not caller bug). Either way
		// surface as 500 — the caller did nothing wrong.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	signed, err := a.Signer.SignJWT(claims, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, reissueTokenResponse{
		Token:     signed,
		ExpiresAt: *claims.Expiry,
	})
}
