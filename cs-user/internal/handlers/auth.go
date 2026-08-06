// cs-user OAuth-callback takeover endpoint.
//
// Casdoor is treated as a pure login source: it owns the login UI, OAuth
// dance, password reset, and MFA, and emits a short-lived access token.
// Server forwards that raw Casdoor JWT to this endpoint; cs-user verifies
// the signature itself against Casdoor's JWKS, then resolves the
// authoritative user — and from that user row, the authoritative tenant —
// entirely from cs-user's own data. Nothing the server supplies beyond the
// raw Casdoor JWT (no user_subject_id, no tenant_id, no parsed Identity)
// influences the issued token. The cs-user-signed JWT is the only token
// trusted at the platform layer.
//
// Required deployment posture: CS_USER_CASDOOR_JWKS_URL must be set.
// ReissueToken is self-sufficient on the OAuth callback identity path —
// it verifies the raw Casdoor JWT, resolves the user via external_key,
// and when no row exists yet provisions it inline via GetOrCreateUser
// (the same path @server's separate GetOrCreateUser RPC used to run first).
// The 200 response carries the canonical subject_id, external_key, and
// normalized profile so @server no longer needs to ParseJWTClaimsFromAccessToken
// on the happy path. @server's local GetOrCreateUser is
// still called afterwards against this response's profile to keep the local
// mirror row in sync — but it is no longer load-bearing for issuance.

package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/logger"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AuthAPI bundles the dependencies the reissue-token flow needs. Lives
// separately from UsersAPI because the orchestration spans three services
// (UserService for user/employment read, TenantReader for tenant slug
// lookup, Signer for JWT issuance) and the route shape
// (`/users/reissue-token`) sits inside the users group but isn't a pure
// user CRUD op.
type AuthAPI struct {
	Svc    EmploymentReader
	Signer *auth.Signer
	JWT    config.JWTConfig
	// CasdoorVerifier validates the raw Casdoor JWT forwarded in
	// reissueTokenRequest.CasdoorJWT. Must be non-nil for ReissueToken to
	// do anything useful — handler returns 503 if a caller hits a
	// verifier-less deployment.
	CasdoorVerifier *auth.CasdoorVerifier
	// TenantResolver looks up the tenants row (carrying slug) for the
	// resolved user's tenant_id. *tenant.Resolver satisfies this.
	TenantResolver TenantReader
	// Permissions optionally carries the permission readers. When
	// nil, no permission claims are populated (graceful — deployments that
	// haven't wired the permission readers yet still issue valid tokens).
	// When set, the handler queries both GetPlatformAdmin + ListActiveTenantRoles
	// and translates the result into the corresponding JWT claims.
	Permissions PermissionReader
}

// EmploymentReader is the subset of *user.Service the reissue flow needs.
// Declared as an interface for the same testability reasons as UserService
// — sqlite-backed fakes substitute without spinning a real Service.
//
// ApplyEnterpriseMapping is invoked BEFORE GetEmploymentIdentity so the
// fresh-login Casdoor JWT (ExternalClaims harvested directly from the raw
// verified JWT's `properties.*` + `signupApplication`) drives an upsert
// against the tenant's employment_providers.field_map. Errors are
// best-effort — see ReissueToken for the swallow-and-continue contract.
//
// GetOrCreateUser is the inline provisioning fallback when
// GetSubjectIDByExternalKey returns "". Previously ReissueToken returned
// 404 in that case and required @server to call its own GetOrCreateUser RPC
// first; now ReissueToken is self-sufficient — the OAuth callback only
// needs the single RPC.
type EmploymentReader interface {
	GetEmploymentIdentity(ctx context.Context, userSubjectID string) (*models.EmploymentIdentity, error)
	ApplyEnterpriseMapping(ctx context.Context, params user.EmploymentMappingParams) error
	// GetSubjectIDByExternalKey resolves cs-user's authoritative subject_id
	// from a Casdoor-style external_key. This is the PRIMARY resolution
	// path in the new flow — cs-user derives external_key from the verified
	// Casdoor claims and looks up its own user row. Returns ("", nil) when
	// no user matches; the handler then falls back to GetOrCreateUser.
	GetSubjectIDByExternalKey(ctx context.Context, externalKey string) (string, error)
	// GetUserByID loads the user row by subject_id. ReissueToken uses the
	// returned row to read tenant_id (the authoritative tenant binding for
	// the signed JWT) and to surface short_id / canonical profile claims.
	// Returns gorm.ErrRecordNotFound when no user matches.
	GetUserByID(ctx context.Context, subjectID string) (*models.User, error)
	// GetOrCreateUser provisions a new user row when GetSubjectIDByExternalKey
	// misses. Same shape as the older @server-facing users/get_or_create RPC;
	// ReissueToken uses it inline so the OAuth callback no longer needs the
	// separate GetOrCreateUser call before ReissueToken. Returns the
	// authoritative cs-user user row + isNew=true when a row was inserted.
	GetOrCreateUser(ctx context.Context, claims *models.JWTClaims) (*models.User, bool, error)
}

// TenantReader is the subset of *tenant.Resolver the reissue flow needs to
// translate the resolved user's tenant_id into the matching tenants.slug.
// The slug is embedded in the signed JWT so server's TenantMatch middleware
// can compare without an extra lookup; the tenant_id stays the durable key.
//
// ResolveBySlug's existing contract accepts EITHER tenant_id OR slug and
// returns the matching active tenant — reissue-token passes user.TenantID.
type TenantReader interface {
	ResolveBySlug(ctx context.Context, idOrSlug string) (*models.Tenant, error)
}

// PermissionReader is the subset of *user.Service the reissue flow
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
// /api/internal/users/reissue-token. cs-user is the platform-layer identity
// authority: the ONLY field it accepts from the wire is the raw Casdoor JWT
// (and an optional audience override). Everything entering the issued token
// — subject_id, tenant_id, tenant_slug, ExternalClaims — is resolved by
// cs-user itself from the verified JWT + its own data. Legacy fields
// (user_subject_id / tenant_id / tenant_slug / identity) forwarded by an
// un-upgraded server are silently ignored at unmarshal time.
//
// Audience overrides JWTConfig.DefaultAudience when the server knows a
// specific relying party is the target (e.g. csc CLI vs. costrict-web
// frontend). Empty array falls back to the default.
type reissueTokenRequest struct {
	// CasdoorJWT is the raw Casdoor-issued access token. Required — cs-user
	// treats Casdoor as a pure login source and refuses to issue a token
	// without verifying the upstream JWT itself.
	CasdoorJWT string `json:"casdoor_jwt" binding:"required"`

	// Audience overrides the configured default. Empty slice falls back
	// to JWTConfig.DefaultAudience; populated slice replaces it.
	Audience []string `json:"audience,omitempty"`
}

// reissueTokenResponse returns the signed token plus its expiry so the
// caller (server) can set a cookie with the right MaxAge without re-parsing
// the JWT to read exp.
//
// ExternalKey / SubjectID / Profile let server stop calling
// ParseJWTClaimsFromAccessToken + MergeJWTClaims in its OAuth callback.
// IsNew signals whether the user row was provisioned inline on this call
// (true) or was already present (false) — server's local mirror writer
// keys off it to decide whether GetOrCreateUser is expected to insert
// vs. no-op. All non-essential fields are omitempty — old servers that
// only read Token / ExpiresAt keep working unchanged.
type reissueTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`

	// ExternalKey is the canonical `casdoor:<universal_id>` (or provider
	// variant) string cs-user resolved from the verified Casdoor JWT. Server
	// reads the value here instead of computing it itself via
	// user.BuildExternalKey (server's local copy has been deleted).
	ExternalKey string `json:"external_key,omitempty"`

	// SubjectID is the canonical cs-user users.subject_id (e.g. usr_<uuid>).
	// Echoing it back lets server short-circuit any follow-up resolver hop
	// and lets the local-mode mirror writer find the right row.
	SubjectID string `json:"subject_id,omitempty"`

	// IsNew is true when cs-user provisioned the user row inline on this
	// call (the external_key had no row, GetOrCreateUser ran inside
	// ReissueToken). False when the row already existed. Server keys off
	// this in its local mirror writer — when true, the local GetOrCreateUser
	// path is expected to insert; when false, it's a no-op find. Path (a).
	IsNew bool `json:"is_new,omitempty"`

	// Profile carries the verified, normalized IdP userinfo subset that
	// server needs to drive its own GetOrCreateUser (Phone / Picture /
	// PreferredUsername etc. — fields server cannot parse from the raw
	// JWT). Sourced from the verified Casdoor claims (post
	// NormalizeClaimsMap + employment display_name override), so the
	// values exactly match what cs-user used to mint the token.
	Profile *reissueProfile `json:"profile,omitempty"`
}

// reissueProfile mirrors the profile subset of server's user.JWTClaims. Field
// names match server's json tags 1:1 so server can unmarshal straight into
// user.JWTClaims without a translation layer.
type reissueProfile struct {
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

// ReissueToken godoc
//
//	@Summary		Reissue a cs-user-signed JWT (OAuth callback takeover)
//	@Description	cs-user is the platform-layer identity authority. Server forwards ONLY the raw Casdoor JWT; cs-user verifies it against Casdoor's JWKS, resolves the user via external_key (provisioning inline via GetOrCreateUser when no row exists yet), reads the authoritative tenant from the user row, builds enterprise + permission claims, signs via the configured RSA key, and returns the new token. Nothing server supplies beyond `casdoor_jwt` (no user_subject_id, no tenant_id, no parsed identity) influences the issued token. The OAuth callback only needs this single RPC — no separate GetOrCreateUser call is required beforehand. Returns 400 when `casdoor_jwt` is missing, 503 when the verifier is unconfigured, 401 when verification fails, 500 on tenant lookup failure, inline provisioning failure, or signing error.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		reissueTokenRequest	true	"Raw Casdoor JWT (required) + optional audience override"
//	@Success		200		{object}	reissueTokenResponse
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
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

	// Verifier must be configured — without it cs-user can't establish
	// the trust boundary. Treat nil verifier as operational misconfig (503)
	// rather than a caller bug.
	if a.CasdoorVerifier == nil {
		logger.Warn("[reissue-token] verifier not configured (CS_USER_CASDOOR_JWKS_URL missing)")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "casdoor verifier not configured"})
		return
	}
	// Verify the Casdoor JWT — the only thing server is trusted to forward
	// accurately. verified already carries ExternalClaims harvested from the
	// raw JWT payload (`properties.*` + `signupApplication`), so the entire
	// enterprise-claims chain stays inside the JWKS verification boundary.
	verified, verifyErr := a.CasdoorVerifier.Verify(c.Request.Context(), req.CasdoorJWT)
	if verifyErr != nil {
		if errors.Is(verifyErr, auth.ErrCasdoorVerifierDisabled) {
			// Verifier disappeared between the nil check above and now
			// (shouldn't happen but stay defensive). Treat as 503.
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "casdoor verifier not configured"})
			return
		}
		logger.Warn("[reissue-token] casdoor jwt verification failed: %v", verifyErr)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid casdoor jwt"})
		return
	}

	// Resolve the authoritative subject_id from the verified Casdoor claims.
	// cs-user is single-source-of-truth on its own users table — server's
	// user_subject_id is intentionally NOT accepted from the wire. When the
	// external_key has no row yet, ReissueToken provisions it inline via
	// GetOrCreateUser — the same path the older server-side GetOrCreateUser
	// RPC used to run separately. The OAuth callback no longer needs to
	// call GetOrCreateUser before ReissueToken.
	externalKey := user.BuildExternalKey(verified)
	if externalKey == "" {
		// Casdoor JWT verified but has no usable universal_id / sub / id —
		// malformed IdP response. Configurable IdPs are expected to emit at
		// least universal_id; absence is a configuration issue upstream.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid casdoor jwt"})
		return
	}
	subjectID, err := a.Svc.GetSubjectIDByExternalKey(c.Request.Context(), externalKey)
	if err != nil {
		logger.Warn("[reissue-token] subject_id lookup failed for external_key=%s: %v", externalKey, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	var isNewUser bool
	if subjectID == "" {
		// Path (a): inline provisioning. verified is already *models.JWTClaims
		// (the exact type GetOrCreateUser consumes), so no conversion layer.
		// Failures here are real DB / data-integrity issues — surface as 500
		// so callers don't retry a transient auth path.
		newUser, isNew, gocErr := a.Svc.GetOrCreateUser(c.Request.Context(), verified)
		if gocErr != nil {
			logger.Warn("[reissue-token] inline GetOrCreateUser failed for external_key=%s: %v", externalKey, gocErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		if newUser == nil || newUser.SubjectID == "" {
			logger.Warn("[reissue-token] inline GetOrCreateUser returned empty subject_id for external_key=%s", externalKey)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		subjectID = newUser.SubjectID
		isNewUser = isNew
		logger.Info("[reissue-token] provisioned user inline subject_id=%s external_key=%s is_new=%t", subjectID, externalKey, isNew)
	}

	// Load the authoritative user row — this is the source of truth for
	// tenant_id (server no longer forwards it) and for the canonical profile
	// fields NewEnterpriseClaims layers on top of Identity. When inline
	// provisioning just ran, this is a single redundant read; correctness
	// over micro-optimization.
	userRow, err := a.Svc.GetUserByID(c.Request.Context(), subjectID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// GetOrCreateUser just succeeded (or external_key pointed at an
			// existing user) but the row vanished between the two calls —
			// extremely unlikely (no cascade deletes); fail loud so the
			// operator notices rather than silently returning 200.
			logger.Warn("[reissue-token] GetUserByID returned not-found for subject_id=%s (vanished between lookups)", subjectID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		logger.Warn("[reissue-token] GetUserByID failed for subject_id=%s: %v", subjectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	// Resolve the tenant from the user row. userRow.TenantID is the
	// authoritative binding written at GetOrCreate time (produced by the
	// server via X-Tenant-Id context). The slug is joined here so server's
	// TenantMatch middleware can compare without an extra lookup; tenant_id
	// remains the durable key in the JWT.
	tenantID := userRow.TenantID
	if tenantID == "" {
		// GetOrCreateUser always writes a tenant_id (default 'default').
		// Empty here signals a backfill gap or schema drift — fail loud
		// so the operator notices rather than silently signing a token
		// with no tenant context.
		logger.Warn("[reissue-token] user %s has empty tenant_id — schema/backfill issue", subjectID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	tenantSlug := ""
	if a.TenantResolver != nil {
		tenantRow, tenantErr := a.TenantResolver.ResolveBySlug(c.Request.Context(), tenantID)
		if tenantErr != nil {
			logger.Warn("[reissue-token] tenant lookup failed for tenant_id=%s: %v", tenantID, tenantErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		tenantSlug = tenantRow.Slug
	}

	// Audience: request override wins; otherwise fall back to config default.
	audience := req.Audience
	if len(audience) == 0 {
		audience = a.JWT.DefaultAudience
	}

	// Refresh employment_identities from the freshly-verified Casdoor JWT
	// BEFORE reading it back. ExternalClaims (properties.* + signupApplication)
	// came straight out of the verified JWT — this is the entire enterprise
	// field pipeline, no server-parsed hint in the loop. Best-effort: every
	// error path is swallowed so a tenant with a malformed field_map can't
	// block login.
	if len(verified.ExternalClaims) > 0 {
		if err := a.Svc.ApplyEnterpriseMapping(c.Request.Context(), user.EmploymentMappingParams{
			TenantID:       tenantID,
			UserSubjectID:  subjectID,
			Provider:       verified.Provider,
			ExternalClaims: verified.ExternalClaims,
		}); err != nil {
			logger.Warn("[reissue-token] ApplyEnterpriseMapping returned error (login continues): %v", err)
		}
	}

	employment, err := a.Svc.GetEmploymentIdentity(c.Request.Context(), subjectID)
	if err != nil {
		logger.Warn("[reissue-token] GetEmploymentIdentity failed for subject_id=%s: %v", subjectID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	// employment == nil is success — user has no enterprise snapshot yet.

	// MULTI_TENANCY §9.6: when Casdoor's User.Name was populated from the
	// IdP's employee number (a common idtrust quirk), surface display_name
	// from the employment row instead. The field_map populated it from
	// oauth_Custom_displayName. Best-effort — fall back to verified.Name
	// when no employment row exists.
	if employment != nil && employment.DisplayName != nil {
		displayName := *employment.DisplayName
		if displayName != "" {
			uid := ""
			if employment.EnterpriseUID != nil {
				uid = *employment.EnterpriseUID
			}
			empNo := ""
			if employment.EmployeeNumber != nil {
				empNo = *employment.EmployeeNumber
			}
			if verified.Name == "" || verified.Name == uid || verified.Name == empNo {
				verified.Name = displayName
			}
		}
	}

	// Populate permission claims from tenant_admins + platform_admins.
	// Skipped entirely when Permissions is nil (graceful degradation).
	// Tenant scope is the resolved userRow.TenantID, not a server-supplied
	// value — cross-tenant synthesis via request injection is therefore
	// impossible at this layer.
	var platformAdmin bool
	var platformScope string
	var tenantRoles []string
	if a.Permissions != nil {
		pa, paErr := a.Permissions.GetPlatformAdmin(c.Request.Context(), subjectID)
		if paErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		if pa != nil {
			platformAdmin = true
			platformScope = pa.Scope
		}

		roles, rolesErr := a.Permissions.ListActiveTenantRoles(c.Request.Context(), subjectID, tenantID)
		if rolesErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		tenantRoles = roles
	}

	now := time.Now()
	claims, err := auth.NewEnterpriseClaims(auth.IssuanceParams{
		Issuer:        a.JWT.Issuer,
		Subject:       subjectID,
		User:          userRow,
		Audience:      audience,
		TTL:           a.JWT.TTL,
		JTI:           uuid.NewString(),
		Identity:      verified,
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
		Token:       signed,
		ExpiresAt:   claims.Expiry.Time,
		ExternalKey: externalKey,
		SubjectID:   subjectID,
		IsNew:       isNewUser,
		Profile: &reissueProfile{
			ID:                verified.ID,
			Sub:               verified.Sub,
			UniversalID:       verified.UniversalID,
			Name:              verified.Name,
			PreferredUsername: verified.PreferredUsername,
			Email:             verified.Email,
			Phone:             verified.Phone,
			Picture:           verified.Picture,
			Owner:             verified.Owner,
			Provider:          verified.Provider,
			ProviderUserID:    verified.ProviderUserID,
		},
	})
}

// verifyTokenRequest is the body shape for POST /api/internal/auth/verify.
// The gateway forwards whatever JWT the client carried — could be a cs-user
// token (new) or a Casdoor token (legacy). cs-user tries the new format
// first, falls back to Casdoor JWKS verification.
type verifyTokenRequest struct {
	// Token is the raw JWT carried by the client (typically in
	// Authorization: Bearer <token>). Required.
	Token string `json:"token" binding:"required"`
}

// verifyTokenResponse is the unified introspection shape returned to the
// gateway. Mirrors RFC 7662 (OAuth2 Token Introspection) loosely — `active`
// signals validity, the rest are normalized claims the gateway can route on
// without re-implementing two different JWT parsers.
//
// `token_source` is DEPRECATED — the Casdoor JWKS fallback path has been
// removed, so the only emitted value is now "cs-user" and `iss` (Issuer)
// is the canonical discriminator going forward. The field is retained for
// backward compatibility with deployed gateway code that may still switch
// on it, and will be removed in a subsequent release once all consumers
// migrate to `iss`.
type verifyTokenResponse struct {
	Active      bool      `json:"active"`
	TokenSource string    `json:"token_source,omitempty"` // Deprecated: use Issuer. "cs-user" | "casdoor"
	Subject     string    `json:"sub,omitempty"`
	UniversalID string    `json:"universal_id,omitempty"`
	ShortID     string    `json:"short_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	Email       string    `json:"email,omitempty"`
	Phone       string    `json:"phone,omitempty"`
	TenantID    string    `json:"tenant_id,omitempty"`
	TenantSlug  string    `json:"tenant_slug,omitempty"`
	ExpiresAt   time.Time `json:"exp,omitempty"`
	IssuedAt    time.Time `json:"iat,omitempty"`
	Issuer      string    `json:"iss,omitempty"`
}

// VerifyToken godoc
//
//	@Summary		Verify a client JWT (gateway introspection)
//	@Description	Gateway-facing token verification. Accepts only cs-user-signed JWTs (cs-user is the sole identity authority). The legacy Casdoor JWKS fallback has been removed — clients carrying pre-cutover Casdoor cookies now receive 401 and must re-authenticate. Returns normalized claims on success, 401 on any verification failure. Requires the X-Internal-Token shared secret — this endpoint is consumed by the gateway only, never exposed publicly. Returns 400 on missing token, 503 when the signer is not configured.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		verifyTokenRequest	true	"Raw JWT to verify"
//	@Success		200		{object}	verifyTokenResponse
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{active=boolean,error=string}
//	@Failure		503		{object}	object{error=string}
//	@Router			/api/internal/auth/verify [post]
func (a *AuthAPI) VerifyToken(c *gin.Context) {
	var req verifyTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	// cs-user is the sole identity authority — VerifyToken only accepts
	// cs-user-signed JWTs. The CasdoorVerifier field remains in use by
	// ReissueToken and ParseIdentity (which verify freshly-exchanged
	// Casdoor JWTs during the OAuth/login flow), but NOT by gateway
	// introspection.
	if a.Signer == nil {
		// Operator misconfig — 503 to match reissue-token handler's stance.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no token verifier configured"})
		return
	}

	claims, err := a.Signer.VerifyJWT(req.Token, a.JWT.Issuer)
	if err != nil {
		if !errors.Is(err, auth.ErrSignerDisabled) {
			logger.Warn("[verify-token] cs-user verification failed: %v", err)
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token", "active": false})
		return
	}
	if claims == nil {
		// Defensive — Signer contract says err==nil implies claims!=nil, but
		// guard against silent regressions in the underlying jwt library.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token", "active": false})
		return
	}

	c.JSON(http.StatusOK, verifyTokenResponse{
		Active:      true,
		TokenSource: "cs-user",
		Subject:     claims.Subject,
		UniversalID: claims.UniversalID,
		ShortID:     claims.ShortID,
		Name:        claims.Name,
		Email:       claims.Email,
		Phone:       claims.Phone,
		TenantID:    claims.TenantID,
		TenantSlug:  claims.TenantSlug,
		ExpiresAt:   claims.Expiry.Time,
		IssuedAt:    claims.IssuedAt.Time,
		Issuer:      claims.Issuer,
	})
}

// parseIdentityRequest is the body shape for POST
// /api/internal/auth/parse-identity. The endpoint replaces server's
// bind-identity-callback unverified ParseJWTClaimsFromAccessToken:
// server forwards the raw Casdoor JWT, cs-user verifies it against JWKS
// and returns the verified + normalized profile fields plus the computed
// external_key. Server no longer needs to parse the JWT itself nor compute
// external_key locally — the identity-trust boundary moves entirely
// inside cs-user's verifier.
//
// Shape mirrors reissueTokenRequest's CasdoorJWT field (single string) so the
// two endpoints stay consistent.
type parseIdentityRequest struct {
	// Token is the raw Casdoor-issued JWT (typically an access token from
	// OAuth code exchange, or a session token from an existing cookie).
	// Required.
	Token string `json:"token" binding:"required"`
}

// parseIdentityResponse is the wire shape for the parse-identity RPC.
// It carries the verified profile (reissueProfile — same fields server's
// user.JWTClaims consumes) plus the canonical external_key cs-user computed
// from the same verified claims. Server uses the profile fields to drive
// BindIdentityToUser and the external_key to populate the merge_token
// redirect state when bind hits identity_already_bound.
//
// Reusing reissueProfile (rather than declaring a new struct) keeps the
// field-name contract identical to /users/reissue-token's Profile, so @server
// can decode into its existing user.ReissueProfile type and call
// AsJWTClaims() for the BindIdentityToUser path.
type parseIdentityResponse struct {
	// ExternalKey is the canonical casdoor[:<provider>]:<universal_id>
	// string cs-user computed from the verified Casdoor claims. Server
	// uses this to populate merge_token.ExternalKey on the
	// identity_already_bound branch — replacing server's local
	// user.BuildExternalKey.
	ExternalKey string `json:"external_key,omitempty"`
	// Profile is the verified, normalized IdP userinfo subset. Sourced
	// straight from the verified CasdoorVerifier output (post
	// NormalizeClaimsMap), so the values exactly match what the
	// verification boundary produced.
	Profile *reissueProfile `json:"profile,omitempty"`
}

// ParseIdentity godoc
//
//	@Summary		Parse a Casdoor JWT and return verified identity fields
//	@Description	Replacement for server's bind-identity callback unverified parse. Server forwards the raw Casdoor JWT (the newly-bound identity token, OR the current session token); cs-user verifies it against Casdoor's JWKS, runs NormalizeClaimsMap, computes the canonical external_key, and returns both. Server uses the response to drive BindIdentityToUser and the merge_token redirect — without ever parsing the JWT payload itself. Returns 400 when `token` is missing, 503 when the verifier is unconfigured, 401 on verification failure.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		parseIdentityRequest	true	"Raw Casdoor JWT to verify + extract identity fields from"
//	@Success		200		{object}	parseIdentityResponse
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		503		{object}	object{error=string}
//	@Router			/api/internal/auth/parse-identity [post]
func (a *AuthAPI) ParseIdentity(c *gin.Context) {
	// Verifier must be configured — without it cs-user can't establish the
	// trust boundary. Same 503 contract as ReissueToken in the same state.
	if a.CasdoorVerifier == nil {
		logger.Warn("[parse-identity] verifier not configured (CS_USER_CASDOOR_JWKS_URL missing)")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "casdoor verifier not configured"})
		return
	}

	var req parseIdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	// Verify the Casdoor JWT — the only thing server is trusted to forward
	// accurately. verified carries the post-NormalizeClaimsMap profile +
	// ExternalClaims harvested from properties.* + signupApplication, so
	// the entire identity chain stays inside the JWKS verification boundary.
	verified, verifyErr := a.CasdoorVerifier.Verify(c.Request.Context(), req.Token)
	if verifyErr != nil {
		if errors.Is(verifyErr, auth.ErrCasdoorVerifierDisabled) {
			// Verifier disappeared between the nil check above and now —
			// surface as 503, mirroring ReissueToken.
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "casdoor verifier not configured"})
			return
		}
		logger.Warn("[parse-identity] casdoor jwt verification failed: %v", verifyErr)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid casdoor jwt"})
		return
	}

	// Compute external_key from the SAME verified claims — server uses this
	// to populate merge_token.ExternalKey on the identity_already_bound
	// branch. Returns "" when the JWT carries no universal_id/sub/id (a
	// malformed IdP response); server treats empty as a parse failure.
	externalKey := user.BuildExternalKey(verified)

	c.JSON(http.StatusOK, parseIdentityResponse{
		ExternalKey: externalKey,
		Profile: &reissueProfile{
			ID:                verified.ID,
			Sub:               verified.Sub,
			UniversalID:       verified.UniversalID,
			Name:              verified.Name,
			PreferredUsername: verified.PreferredUsername,
			Email:             verified.Email,
			Phone:             verified.Phone,
			Picture:           verified.Picture,
			Owner:             verified.Owner,
			Provider:          verified.Provider,
			ProviderUserID:    verified.ProviderUserID,
		},
	})
}

