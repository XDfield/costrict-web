package middleware

import (
	"context"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/redis/go-redis/v9"
)

// behaviorRateLimitScript atomically increments the window counter and, on the
// first hit, sets its expiry — so a crash between INCR and EXPIRE can never
// leave a key without a TTL (which would permanently block the caller).
var behaviorRateLimitScript = redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
if c == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return c
`)

// AllowBehavior reports whether a behavior write keyed by `key` is within the
// rate limit for the current window, using a Redis fixed window.
//
// SRC-2026-4791 P1-1: requiring auth stops anonymous forgery, but an
// authenticated caller can still bulk-spam trust writes; the caller applies this
// only to trust actions (install/feedback/success/fail/…) so ordinary browsing
// (view/click) is never throttled and can't starve a legitimate install.
//
// It fails OPEN: with no Redis configured, a non-positive limit, or a Redis
// error, it returns true — a rate-limiter outage must never block legit traffic.
func AllowBehavior(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) bool {
	if rdb == nil || limit <= 0 {
		return true
	}
	count, err := behaviorRateLimitScript.Run(ctx, rdb, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		logger.Warn("[ratelimit] behavior limiter failing open: %v", err)
		return true
	}
	return count <= int64(limit)
}
