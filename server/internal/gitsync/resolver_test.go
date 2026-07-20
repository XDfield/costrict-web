package gitsync

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubGitServerClient is a configurable GitServerClient for tests.
// calls counts every invocation; callsPerTenant tallies per-tenant.
type stubGitServerClient struct {
	mu             sync.Mutex
	calls          atomic.Int32
	callsPerTenant map[string]int
	cfg            *GitServerConfig
	err            error
	delay          time.Duration
	lastTenantID   string
}

func newStubGitServerClient() *stubGitServerClient {
	return &stubGitServerClient{callsPerTenant: map[string]int{}}
}

func (s *stubGitServerClient) GetTenantGitServer(ctx context.Context, tenantID string) (*GitServerConfig, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.callsPerTenant[tenantID]++
	s.lastTenantID = tenantID
	s.mu.Unlock()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.cfg != nil {
		return s.cfg, nil
	}
	return &GitServerConfig{
		ServerID:   "gs-" + tenantID,
		Kind:       "gitea",
		Endpoint:   "https://gitea." + tenantID + ".example.com",
		AdminToken: "tok-" + tenantID,
	}, nil
}

func (s *stubGitServerClient) callCount() int32 { return s.calls.Load() }

// TestRPCResolver_HappyPath verifies the RPC fires and the result is
// returned to the caller.
func TestRPCResolver_HappyPath(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	r := NewRPCResolver(client, time.Minute)

	got, err := r.Resolve(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServerID != "gs-t-acme" {
		t.Errorf("server_id: got %q", got.ServerID)
	}
	if got.Endpoint != "https://gitea.t-acme.example.com" {
		t.Errorf("endpoint: got %q", got.Endpoint)
	}
	if got.AdminToken != "tok-t-acme" {
		t.Errorf("admin_token: got %q", got.AdminToken)
	}
	if client.callCount() != 1 {
		t.Errorf("RPC call count: got %d, want 1", client.callCount())
	}
}

// TestRPCResolver_CacheHitSkipsRPC verifies the second Resolve for the
// same tenant_id does NOT fire the RPC.
func TestRPCResolver_CacheHitSkipsRPC(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	r := NewRPCResolver(client, time.Minute)

	if _, err := r.Resolve(context.Background(), "t-acme"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "t-acme"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if got := client.callCount(); got != 1 {
		t.Errorf("RPC call count after 2 resolves for same tenant: got %d, want 1", got)
	}
}

// TestRPCResolver_CacheMissOnDifferentTenants verifies distinct tenants
// each do their own RPC.
func TestRPCResolver_CacheMissOnDifferentTenants(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	r := NewRPCResolver(client, time.Minute)

	_, _ = r.Resolve(context.Background(), "t-a")
	_, _ = r.Resolve(context.Background(), "t-b")
	_, _ = r.Resolve(context.Background(), "t-c")

	if got := client.callCount(); got != 3 {
		t.Errorf("RPC call count after 3 distinct tenants: got %d, want 3", got)
	}
}

// TestRPCResolver_CacheExpiryTriggersRefresh verifies a stale entry
// triggers a fresh RPC.
func TestRPCResolver_CacheExpiryTriggersRefresh(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	r := NewRPCResolver(client, 30*time.Millisecond)

	_, _ = r.Resolve(context.Background(), "t-acme")
	time.Sleep(60 * time.Millisecond)
	_, _ = r.Resolve(context.Background(), "t-acme")

	if got := client.callCount(); got != 2 {
		t.Errorf("RPC call count after cache expiry: got %d, want 2", got)
	}
}

// TestRPCResolver_ErrorsAreNotCached verifies that a failed RPC retries
// on the next call (no negative caching).
func TestRPCResolver_ErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	client.err = errors.New("rpc unavailable")
	r := NewRPCResolver(client, time.Minute)

	_, err1 := r.Resolve(context.Background(), "t-acme")
	if err1 == nil {
		t.Fatal("first Resolve: got nil err, want error")
	}
	_, err2 := r.Resolve(context.Background(), "t-acme")
	if err2 == nil {
		t.Fatal("second Resolve: got nil err, want error (errors not cached)")
	}
	if got := client.callCount(); got != 2 {
		t.Errorf("RPC call count after 2 error resolves: got %d, want 2 (no negative caching)", got)
	}
}

// TestRPCResolver_ErrorThenSuccessRecovers verifies that after a
// transient error, a successful resolve lands in the cache.
func TestRPCResolver_ErrorThenSuccessRecovers(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	client.err = errors.New("rpc unavailable")
	r := NewRPCResolver(client, time.Minute)

	_, _ = r.Resolve(context.Background(), "t-acme")
	client.err = nil // clear the failure
	got, err := r.Resolve(context.Background(), "t-acme")
	if err != nil {
		t.Fatalf("post-recovery Resolve: %v", err)
	}
	if got == nil || got.ServerID != "gs-t-acme" {
		t.Errorf("post-recovery cfg: %+v", got)
	}
	// Third call — should hit cache now.
	_, _ = r.Resolve(context.Background(), "t-acme")
	if got := client.callCount(); got != 2 {
		t.Errorf("RPC call count after recovery + cache hit: got %d, want 2", got)
	}
}

// TestRPCResolver_NilClientSurfacesError covers the defensive guard.
func TestRPCResolver_NilClientSurfacesError(t *testing.T) {
	t.Parallel()
	r := NewRPCResolver(nil, time.Minute)
	_, err := r.Resolve(context.Background(), "t-acme")
	if err == nil {
		t.Fatal("Resolve: got nil err, want error")
	}
}

// TestRPCResolver_EmptyTenantIDSurfacesError covers the input-validation guard.
func TestRPCResolver_EmptyTenantIDSurfacesError(t *testing.T) {
	t.Parallel()
	r := NewRPCResolver(newStubGitServerClient(), time.Minute)
	_, err := r.Resolve(context.Background(), "")
	if err == nil {
		t.Fatal("Resolve: got nil err, want error")
	}
	if client := r.client.(*stubGitServerClient); client.callCount() != 0 {
		t.Errorf("RPC fired for empty tenant_id: %d", client.callCount())
	}
}

// TestRPCResolver_DefaultTTLWhenZeroOrNegative verifies the fallback.
func TestRPCResolver_DefaultTTLWhenZeroOrNegative(t *testing.T) {
	t.Parallel()
	for _, ttl := range []time.Duration{0, -1 * time.Second} {
		r := NewRPCResolver(newStubGitServerClient(), ttl)
		if r.ttl != CacheTTL {
			t.Errorf("ttl=%v: r.ttl got %v, want %v", ttl, r.ttl, CacheTTL)
		}
	}
}

// TestRPCResolver_ConcurrentResolvesAreSafe verifies no data race under
// concurrent access (run with -race to catch issues).
func TestRPCResolver_ConcurrentResolvesAreSafe(t *testing.T) {
	t.Parallel()
	client := newStubGitServerClient()
	r := NewRPCResolver(client, time.Minute)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = r.Resolve(context.Background(), "t-acme")
		}()
	}
	wg.Wait()
	// Without single-flight, some goroutines will race past the cache lookup
	// and all fire the RPC. We don't care about the exact count, only that
	// nothing panicked and at least one call fired.
	if client.callCount() < 1 {
		t.Errorf("expected at least 1 RPC call, got 0")
	}
}
