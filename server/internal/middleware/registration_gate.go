// Package middleware — RequireProfileComplete gate (R3 of
// REGISTRATION_PROFILE_DESIGN). Wraps the authenticated request path so
// first-time users without profile_completed_at get a 403
// profile_incomplete and the frontend can route them to /register/complete.
//
// The gate is opt-in via PROFILE_GATE_ENABLED (default off) so the rollout
// can be staged: dev → canary → prod. When disabled, RequireProfileComplete
// is a literal no-op (returns c.Next()).
//
// Whitelist: even with the gate on, the registration endpoints themselves
// (username-available / complete-registration / profile), plus /me (so the
// frontend can read current state), /auth/logout, /health, and swagger
// docs must remain reachable. The whitelist is path-prefix based and
// intentionally short.
package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
)

// ProfileChecker reports whether the user has finished first-time
// registration (profile_completed_at IS NOT NULL). Injected by main.go so
// this package has no DB dependency. Mirrors StatusChecker's shape and
// safety contract:
//   - complete: true when profile_completed_at is non-null.
//   - err:      lookup error. The gate FAILS OPEN on error (lets the
//     request through) so a transient DB hiccup can never lock out every
//     user. Errors are not cached.
//   - When ProfileChecker is nil the gate is a no-op even if
//     PROFILE_GATE_ENABLED=true (defensive: a misconfigured rollout must
//     not produce a 403 storm).
type ProfileChecker func(subjectID string) (complete bool, err error)

var profileGateEnabled bool
var profileChecker ProfileChecker

// profileCacheTTL bounds the cached profile-completion result. Same
// rationale as statusCacheTTL: short enough that a fresh
// complete-registration takes effect within seconds; long enough to keep
// the gate off the per-request hot path.
const profileCacheTTL = 30 * time.Second

type profileCacheEntry struct {
	complete  bool
	expiresAt time.Time
}

var (
	profileCacheMu sync.RWMutex
	profileCache   = map[string]profileCacheEntry{}
)

// SetProfileGateEnabled toggles the gate at runtime. Wired to the
// PROFILE_GATE_ENABLED env var in main.go. When false, RequireProfileComplete
// short-circuits before any DB lookup.
func SetProfileGateEnabled(enabled bool) {
	profileGateEnabled = enabled
}

// IsProfileGateEnabled reports the current flag state (useful for tests
// and for /debug endpoints that want to surface the rollout posture).
func IsProfileGateEnabled() bool {
	return profileGateEnabled
}

// SetProfileChecker installs the lookup hook, wrapped in a short-TTL cache
// mirroring SetStatusChecker. Passing nil disables the gate's lookup
// (RequireProfileComplete becomes a no-op even if the flag is on) and
// clears the cache.
func SetProfileChecker(checker ProfileChecker) {
	profileCacheMu.Lock()
	profileCache = map[string]profileCacheEntry{}
	profileCacheMu.Unlock()

	if checker == nil {
		profileChecker = nil
		return
	}

	profileChecker = func(subjectID string) (bool, error) {
		now := time.Now()
		profileCacheMu.RLock()
		entry, ok := profileCache[subjectID]
		profileCacheMu.RUnlock()
		if ok && now.Before(entry.expiresAt) {
			return entry.complete, nil
		}

		complete, err := checker(subjectID)
		if err != nil {
			return false, err
		}

		profileCacheMu.Lock()
		profileCache[subjectID] = profileCacheEntry{complete: complete, expiresAt: now.Add(profileCacheTTL)}
		profileCacheMu.Unlock()
		return complete, nil
	}
}

// InvalidateProfileCache drops the cached profile-completion result for a
// subject. Called by handlers.CompleteRegistration on success so the user
// doesn't have to wait out the TTL before the gate stops 403ing them.
func InvalidateProfileCache(subjectID string) {
	profileCacheMu.Lock()
	delete(profileCache, subjectID)
	profileCacheMu.Unlock()
}

// profileGateWhitelist lists path prefixes that bypass the gate even when
// it's enabled. Kept as raw strings (not regex) for predictable match
// semantics and easy auditing. Each entry must include the leading /api/.
//
// Rationale per entry:
//   - /api/users/me/username-available, complete-registration, profile:
//     the registration flow itself must be reachable, else the user is
//     stuck 403→register→403.
//   - /api/users/me, /api/auth/me: frontend reads the current user's
//     profile_completed_at to decide which screen to show.
//   - /api/auth/logout: a gated user must still be able to log out.
//   - /api/health, /api/version, /swagger, /docs: infrastructure probes.
var profileGateWhitelist = []string{
	"/api/users/me/username-available",
	"/api/users/me/complete-registration",
	"/api/users/me/profile",
	"/api/users/me",
	"/api/auth/me",
	"/api/auth/logout",
	"/api/health",
	"/api/version",
	"/swagger",
	"/docs",
}

// isProfileGateWhitelisted returns true when path falls under any whitelist
// prefix. Prefix match is intentional so sub-paths (e.g. /api/users/me/...)
// inherit the bypass.
func isProfileGateWhitelisted(path string) bool {
	for _, prefix := range profileGateWhitelist {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// RequireProfileComplete is the gate middleware. Order matters: it must
// run AFTER RequireAuth so UserIDKey is populated. Short-circuits on:
//   - gate disabled (PROFILE_GATE_ENABLED=false)
//   - no checker installed (defensive)
//   - no subject (unauthenticated — RequireAuth should already have 401'd)
//   - whitelisted path
//   - lookup error (fail open)
//
// On a complete=false result, aborts with 403 profile_incomplete so the
// frontend can route the user to /register/complete.
func RequireProfileComplete(c *gin.Context) {
	if !profileGateEnabled || profileChecker == nil {
		c.Next()
		return
	}
	if isProfileGateWhitelisted(c.Request.URL.Path) {
		c.Next()
		return
	}
	subjectID := c.GetString(UserIDKey)
	if subjectID == "" {
		// RequireAuth is upstream; reaching here without a subject means
		// the route is intentionally public — let it through.
		c.Next()
		return
	}
	complete, err := profileChecker(subjectID)
	if err != nil {
		logger.Warn("[ProfileGate] lookup failed for %s: %v (failing open)", subjectID, err)
		c.Next()
		return
	}
	if !complete {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":  "profile_incomplete",
			"reason": "complete_registration_required",
		})
		return
	}
	c.Next()
}
