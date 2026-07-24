package middleware

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/authidentity"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
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
	UniversalID       string
	Name              string
	PreferredUsername string
	Email             string
	Provider          string
	ProviderUserID    string
	Phone             string
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
// no-op when no checker is installed, when there is no resolved subject, or when
// the lookup errors (fail-open). Returns true when the request was aborted.
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

func OptionalAuth(casdoorEndpoint string, jwks *JWKSProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)
		if token == "" {
			c.Next()
			return
		}

		userInfo, err := parseJWTToken(token, jwks)
		if err != nil {
			// Only fall back to Casdoor if the token looks like a JWT.
			// Device tokens (hex strings) and other non-JWT formats will
			// always fail Casdoor validation, so skip the network call.
			if looksLikeJWT(token) {
				userInfo, err = fetchUserInfo(casdoorEndpoint, token)
				if err != nil {
					logger.Warn("[OptionalAuth] token validation failed: %v, endpoint=%s", err, casdoorEndpoint)
					c.Next()
					return
				}
			} else {
				c.Next()
				return
			}
		}

		setAuthContext(c, userInfo)
		c.Set("accessToken", token)
		c.Next()
	}
}

func RequireAuth(casdoorEndpoint string, jwks *JWKSProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		userInfo, err := parseJWTToken(token, jwks)
		if err != nil {
			// Fallback to Casdoor API verification
			userInfo, err = fetchUserInfo(casdoorEndpoint, token)
			if err != nil {
				// Clear invalid cookie to prevent repeated failed requests
				ClearAuthCookie(c)
				logger.Warn("[RequireAuth] token validation failed: %v, endpoint=%s", err, casdoorEndpoint)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
				return
			}
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
	UniversalID       string `json:"universal_id"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	Provider          string `json:"provider"`
	ProviderUserID    string `json:"provider_user_id"`
	Phone             string `json:"phone"`
}

type casdoorUserinfoResponse struct {
	Status      string `json:"status"`
	Msg         string `json:"msg"`
	ID          string `json:"id"`
	Sub         string `json:"sub"`
	UniversalID string `json:"universal_id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
}

// parseJWTToken verifies and parses a Casdoor JWT token using JWKS public keys.
// If jwks is nil or key retrieval fails, returns an error so the caller can fall back.
func parseJWTToken(tokenString string, jwks *JWKSProvider) (*CasdoorUserInfo, error) {
	if jwks == nil {
		return nil, fmt.Errorf("JWKS provider not configured")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Ensure the signing method is RSA
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		kid, _ := token.Header["kid"].(string)
		return jwks.GetKey(kid)
	}, jwt.WithValidMethods([]string{"RS256"}))

	if err != nil {
		return nil, fmt.Errorf("JWT verification failed: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	normalized := authidentity.NormalizeClaimsMap(map[string]any(claims))
	sub := normalized.UniversalID
	if sub == "" {
		sub = normalized.Sub
	}
	if sub == "" {
		sub = normalized.ID
	}
	if sub == "" {
		return nil, fmt.Errorf("no id, sub or universal_id in token")
	}

	return &CasdoorUserInfo{
		ID:                normalized.ID,
		Sub:               sub,
		UniversalID:       normalized.UniversalID,
		Name:              normalized.Name,
		PreferredUsername: normalized.PreferredUsername,
		Email:             normalized.Email,
		Provider:          normalized.Provider,
		ProviderUserID:    normalized.ProviderUserID,
		Phone:             normalized.Phone,
	}, nil
}

func fetchUserInfo(endpoint, token string) (*CasdoorUserInfo, error) {
	url := endpoint + "/api/userinfo"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.Warn("[fetchUserInfo] casdoor returned %d: %s, url=%s", resp.StatusCode, string(body), url)
		return nil, fmt.Errorf("casdoor returned status %d", resp.StatusCode)
	}

	var casdoorResp casdoorUserinfoResponse
	if err := json.Unmarshal(body, &casdoorResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	// Check if Casdoor returned an error
	if casdoorResp.Status == "error" {
		return nil, fmt.Errorf("casdoor error: %s", casdoorResp.Msg)
	}

	if casdoorResp.Sub == "" {
		return nil, fmt.Errorf("no sub in response")
	}

	return &CasdoorUserInfo{
		ID:                casdoorResp.ID,
		Sub:               casdoorResp.Sub,
		UniversalID:       casdoorResp.UniversalID,
		Name:              casdoorResp.Name,
		PreferredUsername: casdoorResp.Name,
		Email:             casdoorResp.Email,
		Provider:          "",
		ProviderUserID:    "",
		Phone:             "",
	}, nil
}

// ParseToken verifies a token using JWKS first, falling back to Casdoor userinfo API.
func ParseToken(token string, casdoorEndpoint string, jwks *JWKSProvider) (*CasdoorUserInfo, error) {
	userInfo, err := parseJWTToken(token, jwks)
	if err != nil {
		userInfo, err = fetchUserInfo(casdoorEndpoint, token)
		if err != nil {
			return nil, err
		}
	}
	return userInfo, nil
}

func setAuthContext(c *gin.Context, userInfo *CasdoorUserInfo) {
	userID := userInfo.Sub
	userName := userInfo.PreferredUsername
	authClaims := AuthClaims{
		ID:                userInfo.ID,
		Sub:               userInfo.Sub,
		UniversalID:       userInfo.UniversalID,
		Name:              userInfo.Name,
		PreferredUsername: userInfo.PreferredUsername,
		Email:             userInfo.Email,
		Provider:          userInfo.Provider,
		ProviderUserID:    userInfo.ProviderUserID,
		Phone:             userInfo.Phone,
	}
	if subjectResolver != nil {
		resolvedID, resolvedName, err := subjectResolver(authClaims)
		if err == nil {
			if resolvedID != "" {
				userID = resolvedID
			}
			if resolvedName != "" {
				userName = resolvedName
			}
		}
	}
	c.Set(UserIDKey, userID)
	c.Set(UserNameKey, userName)
	c.Set(AuthClaimsKey, authClaims)
}

// looksLikeJWT returns true if the token has the standard JWT structure
// of three base64url-encoded segments separated by dots.
func looksLikeJWT(token string) bool {
	return strings.Count(token, ".") == 2
}

// ClearAuthCookie clears the authentication cookie to prevent the client
// from sending invalid tokens repeatedly.
func ClearAuthCookie(c *gin.Context) {
	// Set cookie with expired date to effectively delete it
	// Parameters must match the original cookie settings
	c.SetCookie(AuthCookieName, "", -1, "/", "", false, false)
}

func strClaim(claims jwt.MapClaims, key string) string {
	if v, ok := claims[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
