// EnterpriseClaims is the JWT payload cs-user issues once it takes over the
// OAuth callback (A7). Three field groups:
//
//  1. Standard JWT (RFC 7519): iss/sub/aud/exp/nbf/iat/jti — wired through
//     jwt.Claims interface methods so jwt/v5's parser enforces exp/nbf.
//  2. OIDC identity (mirrors models.JWTClaims): universal_id / name / email /
//     picture / owner / provider / provider_user_id / phone. These overlap
//     1:1 with Casdoor's token shape, so a relying party switching from
//     Casdoor tokens to cs-user tokens sees no diff. Two relying-party-driven
//     claims ride in this group without a Casdoor counterpart: short_id (the
//     Gitea username) and user_id (the Gitea binding-lookup key, same value
//     as sub) — see the field comments before changing either.
//  3. Enterprise context (populated from employment_identities):
//     employee_number / job_title / job_level / employment_type /
//     cost_center / org_path / work_location. Plus tenant_id (single-tenant
//     for now).
//
// Wire compatibility: server's existing JWTClaims parser
// (server/internal/user/service.go) already handles group 2 — group 3 fields
// are added there in A7 when cs-user lights up as token issuer.

package auth

import (
	"errors"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/golang-jwt/jwt/v5"
)

// EnterpriseClaims is the cs-user-issued JWT payload shape.
type EnterpriseClaims struct {
	// --- Standard JWT (RFC 7519) ---
	// iat/nbf/exp MUST be *jwt.NumericDate, not *time.Time — RFC 7519
	// §2 requires NumericDate (JSON number = Unix seconds). *time.Time
	// marshals to an RFC 3339 string, which jwt/v5 MapClaims / verifiers
	// on the relying-party side reject ("exp claim is invalid"), causing
	// every cs-user-signed token to fail verification.
	Issuer    string           `json:"iss,omitempty"`
	Subject   string           `json:"sub,omitempty"`
	IssuedAt  *jwt.NumericDate `json:"iat,omitempty"`
	NotBefore *jwt.NumericDate `json:"nbf,omitempty"`
	Expiry    *jwt.NumericDate `json:"exp,omitempty"`
	Audience  []string         `json:"aud,omitempty"`
	JTI       string           `json:"jti,omitempty"`

	// --- OIDC identity (mirrors models.JWTClaims) ---
	// UserID intentionally carries the SAME value as Subject (users.subject_id).
	// Do NOT "de-duplicate" it: the claim NAME is fixed by the Gitea fork, which
	// reads `user_id` — and only `user_id` — as the key for its binding-status
	// lookup (gitea services/auth/jwt.go: checkBinding(claims.UserID); an empty
	// value errors out and every user is rejected with 503). Our side answers
	// that lookup from user_git_binding, whose primary key is
	// (user_subject_id, tenant_id), so subject_id is the value that hits the PK
	// directly. Distinct in purpose from ShortID: `short_id` is consumed as the
	// Gitea *username*, `user_id` as the *binding lookup key* — two different
	// claims serving two different lookups in the same middleware.
	UserID            string `json:"user_id,omitempty"`
	UniversalID       string `json:"universal_id,omitempty"`
	Name              string `json:"name,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	// ShortID is the platform-wide compact handle (e.g. "u-khGu8ddt") derived
	// from SubjectID. Consumed verbatim by relying parties that need a stable,
	// collision-resistant, charset-safe identifier — notably the Gitea fork's
	// CoStrictJWT middleware, which uses it directly as the Gitea username
	// (no further transformation). Distinct from PreferredUsername (display
	// name) which carries no uniqueness / charset guarantees.
	ShortID        string `json:"short_id,omitempty"`
	Email          string `json:"email,omitempty"`
	Picture        string `json:"picture,omitempty"`
	Owner          string `json:"owner,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ProviderUserID string `json:"provider_user_id,omitempty"`
	Phone          string `json:"phone,omitempty"`

	// --- Enterprise context (from employment_identities) ---
	// EnterpriseUID is the user's stable identifier at the enterprise IdP
	// (e.g. idtrust id). DisplayName is the per-provider 姓名 (display name).
	// Both are immutable from the user's perspective — every login
	// overwrites them via ApplyEnterpriseMapping. Required pair per the
	// two-layer design: basic user info is mutable, enterprise identity is
	// IdP-synced and not user-editable.
	EnterpriseUID  string `json:"enterprise_uid,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	EmployeeNumber string `json:"employee_number,omitempty"`
	JobTitle       string `json:"job_title,omitempty"`
	JobLevel       string `json:"job_level,omitempty"`
	EmploymentType string `json:"employment_type,omitempty"`
	CostCenter     string `json:"cost_center,omitempty"`
	OrgPath        string `json:"org_path,omitempty"`
	WorkLocation   string `json:"work_location,omitempty"`

	// --- Tenant ---
	TenantID string `json:"tenant_id,omitempty"`
	// TenantSlug is the URL-friendly tenant key. Server's auth
	// middleware reads this claim to compare against the runtime-resolved
	// slug (cookie / subdomain) for cross-tenant detection (B3b.2c). Empty
	// for Casdoor-issued tokens (pre-cutover) — comparison must skip.
	TenantSlug string `json:"tenant_slug,omitempty"`

	// --- Permission (populated from tenant_admins + platform_admins) ---
	// TenantRoles lists the user's active roles on their current tenant
	// (TenantID). Sourced from tenant_admins rows WHERE revoked_at IS NULL.
	// Empty for users who are not tenant admins (regular tenant members).
	// Format: []string e.g. ["tenant_admin"] or ["owner","admin"].
	TenantRoles []string `json:"tenant_roles,omitempty"`
	// PlatformAdmin marks the user as a platform-level admin (cross-tenant
	// authority). When true, PlatformScope carries the granularity
	// (full / support / read_only). When false, PlatformScope is ignored.
	PlatformAdmin bool   `json:"platform_admin,omitempty"`
	PlatformScope string `json:"platform_scope,omitempty"`
}

// jwt.Claims interface — implementation wires standard fields into jwt/v5's
// parser so a relying party's `jwt.Parse` enforces exp/nbf and surfaces
// iss/sub/aud. Custom fields (OIDC + enterprise) ride along in JSON.

func (c *EnterpriseClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	if c == nil || c.Expiry == nil {
		return nil, nil
	}
	return c.Expiry, nil
}

func (c *EnterpriseClaims) GetNotBefore() (*jwt.NumericDate, error) {
	if c == nil || c.NotBefore == nil {
		return nil, nil
	}
	return c.NotBefore, nil
}

func (c *EnterpriseClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	if c == nil || c.IssuedAt == nil {
		return nil, nil
	}
	return c.IssuedAt, nil
}

func (c *EnterpriseClaims) GetIssuer() (string, error) {
	if c == nil {
		return "", nil
	}
	return c.Issuer, nil
}

func (c *EnterpriseClaims) GetSubject() (string, error) {
	if c == nil {
		return "", nil
	}
	return c.Subject, nil
}

func (c *EnterpriseClaims) GetAudience() (jwt.ClaimStrings, error) {
	if c == nil || len(c.Audience) == 0 {
		return nil, nil
	}
	return jwt.ClaimStrings(c.Audience), nil
}

// IssuanceParams bundles the inputs to NewEnterpriseClaims. Identity and
// Employment are optional — when nil, the corresponding claim groups are
// omitted (omitempty drops them at marshal time).
type IssuanceParams struct {
	// Issuer is the iss claim — typically cs-user's public base URL.
	Issuer string
	// Subject is the user's stable subject_id (users.subject_id). Required.
	Subject string
	// ShortID is the platform-wide compact handle (users.short_id, e.g.
	// "u-khGu8ddt"). Emitted verbatim as the `short_id` JWT claim for
	// relying parties (Gitea fork auth) that key on it directly. Empty for
	// users created before the short_id migration was backfilled.
	ShortID string
	// User is cs-user's canonical user record. When non-nil, profile-shaped
	// claims (name, preferred_username, email, phone, picture, short_id) are
	// sourced from this row with Identity as fallback. cs-user is the single
	// source of truth for who the user IS — Identity fields (sourced from
	// Casdoor via server's reissue-token call) describe how the user
	// authenticated at the IdP, not their canonical profile, and Casdoor's
	// `name` field in particular drifts (e.g. phone-registered users have
	// name=phone-number, not their display name). Nil skips the override
	// (legacy callers / synthetic issuance paths).
	User *models.User
	// Audience is the aud claim — relying parties that should accept the
	// token. Empty slice means "no aud claim" (some validators reject this;
	// populate when in doubt).
	Audience []string
	// TTL is the time from issuance to expiry. Required (Phase A default
	// is set by the caller, typically A7 — 1h is a sensible starting point).
	TTL time.Duration
	// JTI is the unique token id. Optional; caller can generate via uuid.
	JTI string
	// Identity carries the OIDC identity fields. May be nil if signing for
	// a user without an active identity context (rare).
	Identity *models.JWTClaims
	// Employment carries the enterprise snapshot. May be nil if the user
	// has no employment_identities row — enterprise fields are then omitted.
	Employment *models.EmploymentIdentity
	// TenantID is reserved for Phase B multi-tenancy. Phase A callers
	// should pass "default" or leave empty.
	TenantID string
	// TenantSlug is the URL-friendly tenant key (Phase B). Server's
	// TenantMatch middleware compares this against the runtime-resolved
	// slug — empty means "leave the JWT claim unset" (e.g. Casdoor-token
	// re-sign under pre-cutover conditions).
	TenantSlug string
	// TenantRoles lists the user's active roles on their current tenant
	// (Phase C1 — sourced from tenant_admins WHERE revoked_at IS NULL).
	// Empty / nil for users with no admin role on the tenant.
	TenantRoles []string
	// PlatformAdmin + PlatformScope mark the user as a platform-level
	// admin (Phase C1 — sourced from platform_admins). When PlatformAdmin
	// is false, PlatformScope is ignored.
	PlatformAdmin bool
	PlatformScope string
}

// ErrEmptySubject is returned by NewEnterpriseClaims when Subject is empty.
// Issuing a token without a subject is always a caller bug.
var ErrEmptySubject = errors.New("auth: empty subject")

// ErrZeroTTL is returned by NewEnterpriseClaims when TTL is zero. Forces
// the caller to make an explicit expiry decision; prevents accidental
// forever-tokens.
var ErrZeroTTL = errors.New("auth: zero TTL")

// NewEnterpriseClaims builds the claim set ready for Signer.SignJWT. now is
// injected as a parameter (not read from time.Now internally) so tests can
// pin issuance time.
//
// nbf is set to now (token valid immediately) — callers wanting a deferred
// activation can mutate the returned claims before signing.
func NewEnterpriseClaims(params IssuanceParams, now time.Time) (*EnterpriseClaims, error) {
	if params.Subject == "" {
		return nil, ErrEmptySubject
	}
	if params.TTL == 0 {
		return nil, ErrZeroTTL
	}

	// NumericDate is a value type — &jwt.NewNumericDate(t) returns a pointer
	// to a fresh copy, so no shared aliasing risk between fields. Each call
	// below is independent.
	c := &EnterpriseClaims{
		Issuer:  params.Issuer,
		Subject: params.Subject,
		// Deliberate duplicate of Subject — see the UserID field comment. The
		// Gitea fork keys its binding check on `user_id`, so dropping this
		// assignment silently locks every user out of Gitea (503), with no
		// failure visible on cs-user's own side.
		UserID:        params.Subject,
		ShortID:       params.ShortID,
		IssuedAt:      jwt.NewNumericDate(now),
		NotBefore:     jwt.NewNumericDate(now),
		Expiry:        jwt.NewNumericDate(now.Add(params.TTL)),
		Audience:      params.Audience,
		JTI:           params.JTI,
		TenantID:      params.TenantID,
		TenantSlug:    params.TenantSlug,
		TenantRoles:   params.TenantRoles,
		PlatformAdmin: params.PlatformAdmin,
		PlatformScope: params.PlatformScope,
	}

	if params.Identity != nil {
		c.UniversalID = params.Identity.UniversalID
		c.Name = params.Identity.Name
		c.PreferredUsername = params.Identity.PreferredUsername
		c.Email = params.Identity.Email
		c.Picture = params.Identity.Picture
		// Owner intentionally NOT carried into the cs-user JWT — it's a
		// Casdoor-only concept (built-in organization, always "user-group"
		// or similar) that the new identity architecture replaces with
		// tenant_id. The struct field stays for vocab-lock / parsing
		// compatibility, but emission is suppressed so relying parties
		// don't see Casdoor's legacy org noise. See MULTI_TENANCY_DESIGN
		// §12.1 — `owner` is absent from the canonical claim set.
		c.Provider = params.Identity.Provider
		c.ProviderUserID = params.Identity.ProviderUserID
		c.Phone = params.Identity.Phone
	}

	// cs-user's own user record is the canonical source for profile-shaped
	// claims. Identity (sourced from Casdoor via server) carries IdP-stale
	// values — phone-registered users in particular get name=phone-number,
	// which is not a display name. When User is supplied, override name /
	// preferred_username / email / phone / picture / short_id with cs-user's
	// truth, keeping Identity only as fallback for fields cs-user doesn't
	// store (universal_id stays sourced from Identity/Casdoor above since it
	// is the cross-IdP join key, not a profile field).
	if params.User != nil {
		if params.User.ShortID != "" {
			c.ShortID = params.User.ShortID
		}
		// name prefers display_name (admin-editable profile field) and falls
		// back to username (unique login handle) so the claim is never empty.
		if params.User.DisplayName != nil && *params.User.DisplayName != "" {
			c.Name = *params.User.DisplayName
		} else if params.User.Username != "" {
			c.Name = params.User.Username
		}
		if params.User.Username != "" {
			c.PreferredUsername = params.User.Username
		}
		if params.User.Email != nil && *params.User.Email != "" {
			c.Email = *params.User.Email
		}
		if params.User.Phone != nil && *params.User.Phone != "" {
			c.Phone = *params.User.Phone
		}
		if params.User.AvatarURL != nil && *params.User.AvatarURL != "" {
			c.Picture = *params.User.AvatarURL
		}
	}

	// universal_id is sourced from the Casdoor JWT (preferred — server's
	// MergeJWTClaims lets the signed JWT value win over /api/getUserInfo
	// for OAuth-brokered users like idtrust). When that source is also
	// empty (rare: Casdoor JWT itself lacks universal_id), fall back to
	// cs-user's internal Subject so quota-manager / cs-cloud never see a
	// blank universal_id (they treat it as a hard dependency).
	if c.UniversalID == "" {
		c.UniversalID = params.Subject
	}

	if params.Employment != nil {
		c.EnterpriseUID = derefStr(params.Employment.EnterpriseUID)
		c.DisplayName = derefStr(params.Employment.DisplayName)
		c.EmployeeNumber = derefStr(params.Employment.EmployeeNumber)
		c.JobTitle = derefStr(params.Employment.JobTitle)
		c.JobLevel = derefStr(params.Employment.JobLevel)
		c.EmploymentType = derefStr(params.Employment.EmploymentType)
		c.CostCenter = derefStr(params.Employment.CostCenter)
		c.OrgPath = derefStr(params.Employment.OrgPath)
		c.WorkLocation = derefStr(params.Employment.WorkLocation)
	}

	return c, nil
}

// derefStr safely unwraps a *string to "" when nil. Used to flatten the
// nullable enterprise fields into omitempty-able string claims.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
