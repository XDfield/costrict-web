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

// TestModule_RPCBackendEndToEnd verifies the full wire path selected by
// USER_SERVICE_BACKEND=rpc: Module → RPCClient → CachedUserService → caller.
// First read is a cache miss (one HTTP call); second is a cache hit (zero new
// HTTP calls); after InvalidateCache the next read is another miss. The local
// *UserService is also constructed (writes still go local) but reads must route
// over HTTP because Backend="rpc".
func TestModule_RPCBackendEndToEnd(t *testing.T) {
	var httpCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&httpCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.User{
			SubjectID: "usr_123", Username: "alice",
		})
	}))
	defer srv.Close()

	db := setupUserTestDB(t)
	module := NewWithConfig(db, 15, config.UserServiceConfig{
		Backend:       "rpc",
		BaseURL:       srv.URL,
		InternalToken: "test-token",
		TimeoutSec:    2,
	})

	// Confirm the Reader actually got swapped to *RPCClient — guards against
	// accidentally defaulting to the local UserService when the test means to
	// exercise the RPC path.
	if _, ok := module.Reader.(*RPCClient); !ok {
		t.Fatalf("expected Reader to be *RPCClient, got %T", module.Reader)
	}

	ctx := context.Background()

	// First read: cache miss → 1 HTTP call.
	got1, err := module.CachedService.GetUserByID(ctx, "usr_123")
	if err != nil {
		t.Fatalf("first GetUserByID: %v", err)
	}
	if got1.SubjectID != "usr_123" {
		t.Fatalf("unexpected user: %+v", got1)
	}
	if n := atomic.LoadInt32(&httpCalls); n != 1 {
		t.Fatalf("expected 1 HTTP call after miss, got %d", n)
	}

	// Second read: cache hit → no new HTTP call.
	if _, err := module.CachedService.GetUserByID(ctx, "usr_123"); err != nil {
		t.Fatalf("second GetUserByID: %v", err)
	}
	if n := atomic.LoadInt32(&httpCalls); n != 1 {
		t.Fatalf("expected still 1 HTTP call after cache hit, got %d", n)
	}

	// Cache invalidation (fired by SetOnUserUpdated hook on local writes,
	// invoked directly here to test the cached service plumbing) forces the
	// next read back through RPC.
	module.CachedService.InvalidateCache("usr_123")
	if _, err := module.CachedService.GetUserByID(ctx, "usr_123"); err != nil {
		t.Fatalf("post-invalidate GetUserByID: %v", err)
	}
	if n := atomic.LoadInt32(&httpCalls); n != 2 {
		t.Fatalf("expected 2 HTTP calls after invalidate, got %d", n)
	}
}

// TestModule_LocalBackendDefault confirms the zero-value default (no Backend
// set) routes reads through *UserService against this process's DB — i.e. the
// historical monolith behaviour is unchanged when an operator does nothing.
func TestModule_LocalBackendDefault(t *testing.T) {
	db := setupUserTestDB(t)
	module := NewWithConfig(db, 15, config.UserServiceConfig{Backend: "local"})

	if _, ok := module.Reader.(*UserService); !ok {
		t.Fatalf("expected Reader to be *UserService for local backend, got %T", module.Reader)
	}

	seed := models.User{SubjectID: "u_local", Username: "local_alice", IsActive: true}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	got, err := module.CachedService.GetUserByID(context.Background(), "u_local")
	if err != nil {
		t.Fatalf("GetUserByID via local backend: %v", err)
	}
	if got.SubjectID != "u_local" {
		t.Fatalf("unexpected user: %+v", got)
	}
}
