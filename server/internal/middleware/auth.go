package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
)

const UserIDKey = "userId"
const UserNameKey = "userName"
const AuthClaimsKey = "authClaims"
const InternalSecretHeader = "X-Internal-Secret"
const SystemTokenHeader = "X-System-Token"
const AuthCookieName = "zgsmAdminToken"

type SubjectResolver func(claims AuthClaims) (subjectID string, preferredUsername string, err error)

type AuthClaims struct {
	ID                string
	Sub               string
	// Issuer mirrors CasdoorUserInfo.Issuer — the JWT `iss` claim.
	// Consumed by setAuthContext to skip the legacy subjectResolver for
	// cs-user-issued JWTs (their `sub` is already canonical). See
	// CasdoorUserInfo.Issuer for the full rationale.
	Issuer            string
	UniversalID       string
	Name              string
	PreferredUsername string
	Email             string
	Provider          string
	ProviderUserID    string
	Phone             string
	// TenantID (Phase B4): the canonical tenants.tenant_id PK extracted
	// from the JWT's `tenant_id` claim. cs-user-signed tokens (Phase A7)
	// always carry this — defaults to "default" at reissue time when the
	// request omits it. Empty for Casdoor-issued tokens (pre-cutover); the
	// TenantContext middleware falls back to tenant.DefaultTenantID before
	// storing in ctx so downstream query scoping never sees "".
	TenantID string
	// TenantSlug (Phase B): populated ONLY when the JWT carries the
	// `tenant_slug` claim — i.e. cs-user-signed tokens (Phase A7).
	// Empty for Casdoor-issued tokens (pre-cutover). The TenantMatch
	// middleware compares this against the runtime-resolved slug for
	// cross-tenant detection (B3b.2c). Empty claim → middleware skips
	// comparison (graceful pre-cutover behavior).
	TenantSlug string
	// PlatformAdmin (Phase C1): populated ONLY when the JWT carries the
	// `platform_admin` claim — i.e. cs-user-signed tokens issued after the
	// Phase C1 reissue-token wiring. False for Casdoor-issued tokens and
	// cs-user tokens for non-platform-admin users. When true, PlatformScope
	// carries the granularity (full / support / read_only). Consumed by
	// RequirePlatformAdmin middleware (§15.1 auth chain).
	PlatformAdmin bool
	// PlatformScope (Phase C1): granularity of the platform-admin grant.
	// Only meaningful when PlatformAdmin is true. Empty otherwise.
	PlatformScope string
	// TenantRoles (Phase C1): the user's active roles on AuthClaims.TenantID
	// (e.g. ["owner"]). Empty for regular tenant members. Consumed by
	// RequireTenantAdmin middleware. Sourced from the `tenant_roles` JWT
	// claim which cs-user populates from tenant_admins WHERE revoked_at IS NULL.
	TenantRoles []string
}

var subjectResolver SubjectResolver

func SetSubjectResolver(resolver SubjectResolver) {
	subjectResolver = resolver
}

func GetSubjectResolver() SubjectResolver {
	return subjectResolver
}

// StatusChecker resolves the account status for a resolved subject id. It is an
// optional, injected hook (mirroring SetSubjectResolver) so the middleware
// package needs no DB/gorm dependency. main.go wires the concrete implementation
// (backed by the users table) at startup.
//
// Contract / safety guarantees (account-status gate is a global, sensitive
// change, so the default is intentionally conservative):
//   - status: the literal account status ("active"/"disabled"/"banned").
//   - err:    a lookup error. The middleware FAILS OPEN on error (lets the
//     request through) so a transient DB hiccup can never lock out every user.
//   - When statusChecker is nil the middleware behaves exactly as before (no
//     status lookup at all). This keeps the default request path unchanged.
type StatusChecker func(subjectID string) (status string, err error)

var statusChecker StatusChecker

// statusCacheTTL bounds how long a resolved account status is reused before the
// underlying StatusChecker (a DB lookup) is consulted again. Short enough that a
// ban takes effect within seconds even without an explicit invalidate; long
// enough to keep the status gate off the per-request hot path.
const statusCacheTTL = 30 * time.Second

type statusCacheEntry struct {
	status    string
	expiresAt time.Time
}

var (
	statusCacheMu sync.RWMutex
	statusCache   = map[string]statusCacheEntry{}
)

// SetStatusChecker installs the account-status hook, wrapped in a short-TTL
// in-memory cache so repeated authenticated requests from the same subject don't
// each hit the DB. Passing nil disables the gate (the historical, status-unaware
// behaviour) and clears the cache. The cache only stores successful lookups;
// errors are not cached and still fail open in enforceAccountStatus.
func SetStatusChecker(checker StatusChecker) {
	statusCacheMu.Lock()
	statusCache = map[string]statusCacheEntry{}
	statusCacheMu.Unlock()

	if checker == nil {
		statusChecker = nil
		return
	}

	statusChecker = func(subjectID string) (string, error) {
		now := time.Now()
		statusCacheMu.RLock()
		entry, ok := statusCache[subjectID]
		statusCacheMu.RUnlock()
		if ok && now.Before(entry.expiresAt) {
			return entry.status, nil
		}

		status, err := checker(subjectID)
		if err != nil {
			// Do not cache errors; caller fails open.
			return "", err
		}

		statusCacheMu.Lock()
		statusCache[subjectID] = statusCacheEntry{status: status, expiresAt: now.Add(statusCacheTTL)}
		statusCacheMu.Unlock()
		return status, nil
	}
}

// InvalidateStatusCache drops any cached account status for the given subject so
// a status change (ban/disable/restore) takes effect immediately rather than
// after the TTL elapses. Safe to call even when the gate is disabled.
func InvalidateStatusCache(subjectID string) {
	statusCacheMu.Lock()
	delete(statusCache, subjectID)
	statusCacheMu.Unlock()
}

// EnforceAccountStatus consults the injected StatusChecker for the resolved
// subject id (read from UserIDKey on the gin context) and aborts the request
// when the account is disabled/banned. It is a no-op when no checker is
// installed, when there is no resolved subject, or when the lookup errors
// (fail-open). Returns true when the request was aborted.
//
// Exported so that auth paths which set UserIDKey outside of RequireAuth — most
// importantly the device-token branch of requireUserOrDeviceAuth — can apply the
// same banned/disabled gate (otherwise a banned user could keep using a device
// token to bypass the status check). Callers must set UserIDKey first, then call
// this and return early if it reports true.
func EnforceAccountStatus(c *gin.Context) bool {
	return enforceAccountStatus(c)
}

// enforceAccountStatus consults the injected StatusChecker for the resolved
// subject id and aborts the request when the account is disabled/banned. It is a
// no-op when no checker is installed, when there is no resolved subject, or
// when the lookup errors (fail-open). Returns true when the request was
// aborted.
func enforceAccountStatus(c *gin.Context) bool {
	if statusChecker == nil {
		return false
	}
	subjectID := c.GetString(UserIDKey)
	if subjectID == "" {
		return false
	}
	status, err := statusChecker(subjectID)
	if err != nil {
		// Fail open: never let an audit/DB wobble lock out legitimate users.
		logger.Warn("[AccountStatus] status lookup failed for %s: %v (failing open)", subjectID, err)
		return false
	}
	switch status {
	case "banned":
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Account banned"})
		return true
	case "disabled":
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Account disabled"})
		return true
	default:
		return false
	}
}

// InternalAuth validates requests from internal services (gateway, etc.) using a shared secret.
// If secret is empty, all requests are rejected to prevent misconfiguration.
func InternalAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if secret == "" {
			logger.Error("[InternalAuth] INTERNAL_SECRET not configured, rejecting request")
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Internal API not available"})
			return
		}

		provided := c.GetHeader(InternalSecretHeader)
		if provided == "" || provided != secret {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Invalid internal secret"})
			return
		}

		c.Next()
	}
}

func SystemTokenAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			logger.Error("[SystemTokenAuth] SYSTEM_TOKEN not configured, rejecting request")
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "System API not available"})
			return
		}

		provided := c.GetHeader(SystemTokenHeader)
		if provided == "" || provided != token {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Invalid system token"})
			return
		}

		c.Set(UserIDKey, "system")
		c.Next()
	}
}

// ExtractToken extracts the access token from the Authorization header, the
// zgsmAdminToken cookie, or (as a last resort, for browser-native WebSocket
// and EventSource handshakes only) the "token" query parameter.
//
// The query fallback exists because neither WebSocket nor EventSource lets
// the page set custom headers, so cross-origin handshakes that can't rely on
// cookies (SameSite=Lax blocks them) carry the session token as ?token=. It
// is gated on the Upgrade/Accept headers so an ordinary HTTP request can't
// authenticate via the URL (which would leak the token into access logs);
// those must keep using the Authorization header.
func ExtractToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if cookie, err := c.Cookie("zgsmAdminToken"); err == nil && cookie != "" {
		return cookie
	}
	if acceptsQueryToken(c) {
		if token := c.Query("token"); token != "" {
			return token
		}
	}
	return ""
}

// acceptsQueryToken reports whether the request is a browser-native WebSocket
// upgrade or EventSource handshake -- the only requests whose ?token= query
// parameter is honored. Ordinary HTTP fetches must use the Authorization
// header so the token never lands in a URL or access log.
func acceptsQueryToken(c *gin.Context) bool {
	if strings.Contains(strings.ToLower(c.GetHeader("Connection")), "upgrade") &&
		strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		return true
	}
	if strings.Contains(strings.ToLower(c.GetHeader("Accept")), "text/event-stream") {
		return true
	}
	return false
}

// tokenVerifierConfig holds the cs-user internal-verify endpoint wiring
// installed by SetTokenVerifier. Once set, every RequireAuth / OptionalAuth /
// ParseToken path delegates signature + expiration verification to cs-user's
// POST /api/internal/auth/verify (full JWKS / RSA-Public-Key check on its
// side). The middleware package stays free of crypto imports.
//
// Failing closed: when verifier is unconfigured OR the cs-user call returns
// 5xx / network error, RequireAuth returns 503 (service unavailable) and
// OptionalAuth silently treats the request as anonymous. We never fall back
// to local unverified decoding — that was the original SSRF/JWT vuln path.
type tokenVerifierConfig struct {
	baseURL       string
	internalToken string
	client        *http.Client
}

var (
	tokenVerifierMu sync.RWMutex
	tokenVerifier   tokenVerifierConfig
)

// tokenCacheTTL bounds how long a successful cs-user introspection is reused
// for the same bearer token. Long enough to keep the verify RPC off the
// per-request hot path on a busy page load; short enough that a token
// revoked server-side is re-checked within minutes. Hashed by SHA-256 so the
// raw token never lands as a map key in memory dumps.
const tokenCacheTTL = 5 * time.Minute

type tokenCacheEntry struct {
	info      *CasdoorUserInfo
	expiresAt time.Time
}

var (
	tokenCacheMu sync.RWMutex
	tokenCache   = map[string]tokenCacheEntry{}
)

// errInvalidToken is returned by introspectToken when cs-user explicitly
// rejects the token (401/400). Mapped to 401 in RequireAuth and silent
// pass-through in OptionalAuth.
var errInvalidToken = errors.New("invalid token")

// errVerifierUnavailable is returned by introspectToken when cs-user is
// unconfigured, unreachable, or returns 5xx. Mapped to 503 in RequireAuth
// (fail closed — no fallback to unverified decoding) and silent pass-through
// in OptionalAuth.
var errVerifierUnavailable = errors.New("token verifier unavailable")

// SetTokenVerifier wires the cs-user introspection endpoint. baseURL is the
// cs-user HTTP base (e.g. http://cs-user:8080), internalToken is the
// X-Internal-Token shared secret, timeout bounds each verify call. Passing
// an empty baseURL disables the verifier — RequireAuth then fails every
// request with 503 (fail closed); OptionalAuth silently degrades to
// anonymous. main.go fast-fails at boot when cfg.UserService.BaseURL is
// empty, so production never hits the disabled branch.
//
// Clears the token cache so a hot reload after config changes never serves
// stale introspection results.
func SetTokenVerifier(baseURL, internalToken string, timeout time.Duration) {
	tokenVerifierMu.Lock()
	tokenVerifier = tokenVerifierConfig{
		baseURL:       strings.TrimRight(baseURL, "/"),
		internalToken: internalToken,
		client: &http.Client{
			Timeout: timeout,
		},
	}
	tokenVerifierMu.Unlock()

	tokenCacheMu.Lock()
	tokenCache = map[string]tokenCacheEntry{}
	tokenCacheMu.Unlock()
}

// InvalidateTokenCache drops any cached introspection for the given bearer
// token. Safe to call when the verifier is unconfigured or the token was
// never cached.
func InvalidateTokenCache(token string) {
	if token == "" {
		return
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	tokenCacheMu.Lock()
	delete(tokenCache, key)
	tokenCacheMu.Unlock()
}

// introspectToken delegates JWT signature + expiration verification to
// cs-user's POST /api/internal/auth/verify. Returns the normalized user
// info on success. Cache hits (keyed by SHA-256 of the token) bypass the
// HTTP round-trip; only successful introspections are cached.
//
// Error mapping (consumed by RequireAuth / OptionalAuth):
//   - errInvalidToken:        cs-user returned 401/400 → token rejected.
//   - errVerifierUnavailable: cs-user unconfigured, network error, or 5xx.
//                             Caller must NOT fall back to local decoding.
func introspectToken(token string) (*CasdoorUserInfo, error) {
	if token == "" {
		return nil, errInvalidToken
	}

	sum := sha256.Sum256([]byte(token))
	cacheKey := hex.EncodeToString(sum[:])

	now := time.Now()
	tokenCacheMu.RLock()
	entry, ok := tokenCache[cacheKey]
	tokenCacheMu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.info, nil
	}

	tokenVerifierMu.RLock()
	cfg := tokenVerifier
	tokenVerifierMu.RUnlock()
	if cfg.baseURL == "" || cfg.internalToken == "" || cfg.client == nil {
		return nil, errVerifierUnavailable
	}

	info, err := introspectTokenViaCSUser(cfg, token)
	if err != nil {
		return nil, err
	}

	tokenCacheMu.Lock()
	tokenCache[cacheKey] = tokenCacheEntry{info: info, expiresAt: now.Add(tokenCacheTTL)}
	tokenCacheMu.Unlock()
	return info, nil
}

// introspectTokenViaCSUser performs the actual HTTP POST to cs-user. Split
// from introspectToken so the cache wrapper stays readable.
func introspectTokenViaCSUser(cfg tokenVerifierConfig, token string) (*CasdoorUserInfo, error) {
	body, _ := json.Marshal(map[string]string{"token": token})
	url := cfg.baseURL + "/api/internal/auth/verify"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// Malformed URL/config — treat as unavailable so boot-time
		// misconfig surfaces as 503 rather than a panic.
		logger.Warn("[introspectToken] request build failed: %v", err)
		return nil, errVerifierUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", cfg.internalToken)

	resp, err := cfg.client.Do(req)
	if err != nil {
		logger.Warn("[introspectToken] cs-user verify RPC failed: %v", err)
		return nil, errVerifierUnavailable
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK:
		// fall through to decode below
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusBadRequest:
		return nil, errInvalidToken
	case resp.StatusCode >= 500:
		logger.Warn("[introspectToken] cs-user verify returned %d: %s", resp.StatusCode, string(respBody))
		return nil, errVerifierUnavailable
	default:
		// Treat any other 4xx as an invalid token — cs-user is the
		// authority, and a 4xx means "I will not verify this".
		logger.Warn("[introspectToken] cs-user verify returned %d: %s", resp.StatusCode, string(respBody))
		return nil, errInvalidToken
	}

	var verified struct {
		Active      bool   `json:"active"`
		TokenSource string `json:"token_source,omitempty"`
		Subject     string `json:"sub,omitempty"`
		UniversalID string `json:"universal_id,omitempty"`
		ShortID     string `json:"short_id,omitempty"`
		Name        string `json:"name,omitempty"`
		Email       string `json:"email,omitempty"`
		Phone       string `json:"phone,omitempty"`
		TenantID    string `json:"tenant_id,omitempty"`
		TenantSlug  string `json:"tenant_slug,omitempty"`
		Issuer      string `json:"iss,omitempty"`
	}
	if err := json.Unmarshal(respBody, &verified); err != nil {
		logger.Warn("[introspectToken] decode cs-user response failed: %v", err)
		return nil, errVerifierUnavailable
	}
	if !verified.Active {
		return nil, errInvalidToken
	}
	if verified.Subject == "" {
		return nil, errInvalidToken
	}

	info := &CasdoorUserInfo{
		ID:                verified.Subject,
		Sub:               verified.Subject,
		UniversalID:       verified.UniversalID,
		Name:              verified.Name,
		PreferredUsername: verified.Name,
		Email:             verified.Email,
		Phone:             verified.Phone,
		TenantID:          verified.TenantID,
		TenantSlug:        verified.TenantSlug,
		Issuer:            verified.Issuer,
	}
	if info.Issuer == "" {
		// cs-user is the only issuer now; default so setAuthContext
		// always takes the "trust sub directly" branch.
		info.Issuer = csUserIssuer
	}
	return info, nil
}

// OptionalAuth delegates token verification to cs-user via introspectToken.
// Tokens that fail verification are silently ignored (no auth context
// populated) so unauthenticated clients can still hit optional routes.
// Failures from cs-user (5xx / network) are also silently ignored — the
// request degrades to anonymous rather than failing closed, because
// optional routes (public reads, swagger, etc.) must stay reachable even
// during a cs-user outage.
func OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)
		if token == "" {
			c.Next()
			return
		}

		userInfo, err := introspectToken(token)
		if err != nil {
			logger.Warn("[OptionalAuth] token verify failed: %v", err)
			c.Next()
			return
		}

		setAuthContext(c, userInfo)
		c.Set("accessToken", token)
		c.Next()
	}
}

// RequireAuth delegates token verification to cs-user via introspectToken.
// A rejected token (errInvalidToken) → 401 and cookie clear. A cs-user
// outage (errVerifierUnavailable) → 503 (fail closed — no fallback to
// unverified decoding, since that was the original SSRF/JWT bypass).
// Account-status gate (banned/disabled) runs after the verify succeeds.
func RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		userInfo, err := introspectToken(token)
		if err != nil {
			if errors.Is(err, errVerifierUnavailable) {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Token service unavailable"})
				return
			}
			ClearAuthCookie(c)
			logger.Warn("[RequireAuth] token verify failed: %v", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		setAuthContext(c, userInfo)
		c.Set("accessToken", token)

		// Account-status gate (banned/disabled). No-op when no checker is
		// installed; fails open on lookup error. Runs only for required-auth
		// requests so it rejects new authenticated requests from a banned user
		// without touching the public/optional-auth paths.
		if enforceAccountStatus(c) {
			return
		}

		c.Next()
	}
}

type CasdoorUserInfo struct {
	ID                string `json:"id"`
	Sub               string `json:"sub"`
	// Issuer carries the JWT's `iss` claim. Used by setAuthContext to
	// decide whether the local users-table resolver should run: cs-user
	// JWTs (iss="cs-user") already carry the canonical subject_id in
	// `sub`, so resolver lookup is unnecessary AND can return a stale
	// local subject_id from the pre-cs-user era (when users.subject_id
	// was set to Casdoor's universal_id). For Casdoor JWTs (iss=anything
	// else, typically the Casdoor issuer URL), the resolver is the only
	// way to bridge universal_id → local subject_id.
	Issuer            string `json:"iss,omitempty"`
	UniversalID       string `json:"universal_id"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	Provider          string `json:"provider"`
	ProviderUserID    string `json:"provider_user_id"`
	Phone             string `json:"phone"`
	// TenantID (Phase B4): canonical tenants.tenant_id PK. Read directly
	// from MapClaims ("tenant_id") — NormalizeClaimsMap only handles
	// standard Casdoor fields. cs-user-signed tokens (Phase A7) always
	// carry this (defaults to "default" at reissue). Empty for Casdoor
	// tokens — the TenantContext middleware falls back to "default".
	TenantID string `json:"tenant_id,omitempty"`
	// TenantSlug (Phase B / A7): populated ONLY when the JWT carries the
	// custom `tenant_slug` claim — i.e. tokens signed by cs-user's
	// /api/internal/users/reissue-token (Phase A7). Empty for Casdoor-issued
	// tokens (pre-cutover). Read directly from the MapClaims map because
	// authidentity.NormalizeClaimsMap only handles standard Casdoor fields.
	TenantSlug string `json:"tenant_slug,omitempty"`
	// PlatformAdmin (Phase C1): true when the JWT carries
	// `platform_admin:true` — only emitted by cs-user for users with a row
	// in platform_admins. Read straight from the map (NormalizeClaimsMap
	// doesn't handle Phase C1 fields). Consumed by RequirePlatformAdmin.
	PlatformAdmin bool `json:"platform_admin,omitempty"`
	// PlatformScope (Phase C1): the granularity string (full / support /
	// read_only). Only meaningful when PlatformAdmin is true.
	PlatformScope string `json:"platform_scope,omitempty"`
	// TenantRoles (Phase C1): user's active roles on TenantID. Sourced from
	// the `tenant_roles` JWT array claim emitted by cs-user. nil/empty for
	// regular tenant members.
	TenantRoles []string `json:"tenant_roles,omitempty"`
}

// csUserIssuer is the iss claim value cs-user stamps on its JWTs. Sourced
// here as a string rather than imported from the cs-user module (separate
// go.mod) to keep middleware's dependency surface minimal.
const csUserIssuer = "cs-user"

// ParseToken delegates token verification to cs-user via introspectToken.
// Used by authz.Service.VerifyTokenWithUser (the internal /auth/verify
// handler). Mirrors RequireAuth's failure contract: errInvalidToken on
// explicit rejection, errVerifierUnavailable on cs-user outage — the caller
// decides how to surface those to its caller.
func ParseToken(token string) (*CasdoorUserInfo, error) {
	return introspectToken(token)
}

func setAuthContext(c *gin.Context, userInfo *CasdoorUserInfo) {
	userID := userInfo.Sub
	userName := userInfo.PreferredUsername
	authClaims := AuthClaims{
		ID:                userInfo.ID,
		Sub:               userInfo.Sub,
		Issuer:            userInfo.Issuer,
		UniversalID:       userInfo.UniversalID,
		Name:              userInfo.Name,
		PreferredUsername: userInfo.PreferredUsername,
		Email:             userInfo.Email,
		Provider:          userInfo.Provider,
		ProviderUserID:    userInfo.ProviderUserID,
		Phone:             userInfo.Phone,
		TenantID:          userInfo.TenantID,
		TenantSlug:        userInfo.TenantSlug,
		PlatformAdmin:     userInfo.PlatformAdmin,
		PlatformScope:     userInfo.PlatformScope,
		TenantRoles:       userInfo.TenantRoles,
	}
	// cs-user JWTs already carry the canonical subject_id in `sub` —
	// skip the local users-table resolver entirely. The resolver exists
	// to bridge Casdoor JWTs (which lack a local subject_id) to the
	// local table; running it on cs-user JWTs can return stale local
	// rows whose subject_id predates cs-user (e.g. legacy rows where
	// users.subject_id was set to Casdoor's universal_id), which then
	// 404 against cs-user's GET /api/internal/users/:subject_id.
	if userInfo.Issuer == csUserIssuer {
		logger.Info("[auth-debug] setAuthContext cs-user JWT: trusting sub=%q directly (resolver skipped)", userID)
	} else if subjectResolver != nil {
		resolvedID, resolvedName, err := subjectResolver(authClaims)
		logger.Info("[auth-debug] setAuthContext resolver in: id=%q sub=%q universal_id=%q provider=%q",
			authClaims.ID, authClaims.Sub, authClaims.UniversalID, authClaims.Provider)
		if err == nil {
			if resolvedID != "" {
				userID = resolvedID
			}
			if resolvedName != "" {
				userName = resolvedName
			}
		} else {
			logger.Warn("[auth-debug] setAuthContext resolver err=%v", err)
		}
		logger.Info("[auth-debug] setAuthContext resolver out: resolved_id=%q resolved_name=%q final_userID=%q", resolvedID, resolvedName, userID)
	}
	c.Set(UserIDKey, userID)
	c.Set(UserNameKey, userName)
	c.Set(AuthClaimsKey, authClaims)
}

// cookieDomain mirrors handlers.cookieDomain — Domain attribute used when
// setting auth cookies, applied here on ClearAuthCookie so the expired
// Set-Cookie matches the original cookie's scope. Empty (default) = host-only.
var cookieDomain string

// SetCookieDomain configures the Domain attribute used by ClearAuthCookie.
// Call once at startup from main, after config load.
func SetCookieDomain(d string) {
	cookieDomain = d
}

// ClearAuthCookie clears the authentication cookie to prevent the client
// from sending invalid tokens repeatedly.
func ClearAuthCookie(c *gin.Context) {
	// Set cookie with expired date to effectively delete it
	// Parameters must match the original cookie settings
	c.SetCookie(AuthCookieName, "", -1, "/", cookieDomain, false, false)
}
