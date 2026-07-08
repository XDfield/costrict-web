package middleware

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// With no Redis client the limiter fails open — AllowBehavior always permits.
func TestAllowBehavior_FailsOpenWithoutRedis(t *testing.T) {
	for i := 0; i < 10; i++ {
		if !AllowBehavior(context.Background(), nil, "k", 3, time.Minute) {
			t.Fatalf("request #%d: expected allow (fail open) with nil redis", i)
		}
	}
}

// A non-positive limit disables limiting.
func TestAllowBehavior_ZeroLimitDisabled(t *testing.T) {
	if !AllowBehavior(context.Background(), nil, "k", 0, time.Minute) {
		t.Fatal("expected allow when limit disabled")
	}
}

// Integration: with a real Redis, the Nth+1 call within the window is denied.
// Skips when REDIS_URL is unset (mirrors leader_test.go's DATABASE_URL gate).
func TestAllowBehavior_LimitsWithRedis(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skipping rate-limit integration test")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("bad REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	ctx := context.Background()
	key := "ratelimit:behavior:test:" + t.Name()
	_ = rdb.Del(ctx, key).Err()
	t.Cleanup(func() { _ = rdb.Del(ctx, key).Err() })

	for i := 1; i <= 3; i++ {
		if !AllowBehavior(ctx, rdb, key, 3, time.Minute) {
			t.Fatalf("call #%d: expected allow", i)
		}
	}
	if AllowBehavior(ctx, rdb, key, 3, time.Minute) {
		t.Fatal("call #4: expected deny (over limit)")
	}
}
