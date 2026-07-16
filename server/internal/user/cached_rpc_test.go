package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/models"
)

// newRPCBackedCached builds a CachedUserService wired to an httptest cs-user
// backend. Returns the cache, the mock server (caller closes it), and a counter
// the handler increments on every call so tests can assert cache hits. The mock
// inspects the path so both /users/:id (bare object) and /users/by-ids (wrapped
// {"users": map}) shapes are served correctly.
func newRPCBackedCached(t *testing.T) (*CachedUserService, *httptest.Server, *int32) {
	t.Helper()
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/internal/users/by-ids":
			_, _ = w.Write([]byte(`{"users":{"usr_123":{"subject_id":"usr_123","username":"alice"}}}`))
		default:
			_ = json.NewEncoder(w).Encode(models.User{
				SubjectID: "usr_123", Username: "alice",
			})
		}
	}))
	t.Cleanup(srv.Close)

	rpc := NewRPCClient(config.UserServiceConfig{
		Backend:       "rpc",
		BaseURL:       srv.URL,
		InternalToken: "test-token",
		TimeoutSec:    2,
	})
	return NewCachedUserService(rpc), srv, &callCount
}

func TestCachedUserService_RPCBackendCachesByGetUserByID(t *testing.T) {
	cached, _, counter := newRPCBackedCached(t)

	// First call: cache miss → HTTP round-trip.
	got1, err := cached.GetUserByID(context.Background(), "usr_123")
	if err != nil {
		t.Fatalf("first GetUserByID: %v", err)
	}
	if got1.SubjectID != "usr_123" {
		t.Fatalf("unexpected user: %+v", got1)
	}
	if n := atomic.LoadInt32(counter); n != 1 {
		t.Fatalf("expected 1 RPC call after miss, got %d", n)
	}

	// Second call: cache hit → no HTTP.
	got2, err := cached.GetUserByID(context.Background(), "usr_123")
	if err != nil {
		t.Fatalf("second GetUserByID: %v", err)
	}
	if got2.SubjectID != "usr_123" {
		t.Fatalf("unexpected cached user: %+v", got2)
	}
	if n := atomic.LoadInt32(counter); n != 1 {
		t.Fatalf("expected still 1 RPC call (cache hit), got %d", n)
	}

	// Invalidate → next call hits RPC again.
	cached.InvalidateCache("usr_123")
	if _, err := cached.GetUserByID(context.Background(), "usr_123"); err != nil {
		t.Fatalf("post-invalidate GetUserByID: %v", err)
	}
	if n := atomic.LoadInt32(counter); n != 2 {
		t.Fatalf("expected 2 RPC calls after invalidate, got %d", n)
	}
}

func TestCachedUserService_RPCBackendCachesByGetUsersByIDs(t *testing.T) {
	cached, _, counter := newRPCBackedCached(t)

	// First batch: one miss.
	if _, err := cached.GetUsersByIDs(context.Background(), []string{"usr_123"}); err != nil {
		t.Fatalf("first GetUsersByIDs: %v", err)
	}
	if n := atomic.LoadInt32(counter); n != 1 {
		t.Fatalf("expected 1 RPC call, got %d", n)
	}

	// Repeat the same batch: cache hit, no new call.
	if _, err := cached.GetUsersByIDs(context.Background(), []string{"usr_123"}); err != nil {
		t.Fatalf("second GetUsersByIDs: %v", err)
	}
	if n := atomic.LoadInt32(counter); n != 1 {
		t.Fatalf("expected cache hit (still 1 RPC call), got %d", n)
	}
}
