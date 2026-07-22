// Package gitsync resolver — Phase E3b.1.1.
//
// RPCResolver is the @server-side counterpart of cs-user's gitserver.Resolver.
// It hides the RPC client behind the same Resolve(ctx, tenantID) surface
// the gitsync.Service consumes, and adds a short-TTL cache to amortise the
// RPC cost across back-to-back SyncTeam calls.
//
// Cache semantics:
//
//   - TTL: 5 minutes (hardcoded; MVP — admin-triggered sync, not a hot path).
//   - Key: tenant_id (string).
//   - Hits return the cached *Config without doing the RPC.
//   - Misses do the RPC and cache the result on success.
//   - Errors are NOT cached — a transient RPC failure retries on the next
//     call instead of sticking for 5 minutes.
//   - No explicit invalidation; TTL is the only expiry. Operator rotation
//     of admin_token is rare, and 5 min is an acceptable staleness window.
//
// Concurrency: the cache map is guarded by an RWMutex. Concurrent misses for
// the same tenant_id may both do the RPC (single-flight is YAGNI for the
// admin-triggered workload).

package gitsync

import (
	"context"
	"errors"
	"sync"
	"time"
)

// GitServerConfig is the per-tenant Git server configuration the gitsync
// Service uses to construct a Client. Lives in gitsync to avoid an
// import cycle (user doesn't depend on gitsync; gitsync mustn't depend
// on user).
type GitServerConfig struct {
	ServerID   string
	Kind       string
	Endpoint   string
	AdminToken string
	// AdminUser / AdminPassword are OPTIONAL credentials used for Gitea
	// endpoints that reject admin PAT auth. Specifically, upstream Gitea's
	// POST /users/{name}/tokens is locked behind reqBasicOrRevProxyAuth,
	// which 401s any "Authorization: token ..." request. When these are
	// empty, the Client falls back to token auth (sufficient for every
	// other endpoint, including bot user creation under /admin/users).
	AdminUser     string
	AdminPassword string
}

// GitServerResolver is the abstract surface the Service consumes.
// RPCResolver is the production impl; tests inject a stub.
type GitServerResolver interface {
	Resolve(ctx context.Context, tenantID string) (*GitServerConfig, error)
}

// GitServerClient is the narrow RPC surface RPCResolver needs.
// *user.RPCClient satisfies this via a small adapter supplied by main.go
// (the adapter handles the type translation between user.TenantGitServerConfig
// and gitsync.GitServerConfig).
type GitServerClient interface {
	GetTenantGitServer(ctx context.Context, tenantID string) (*GitServerConfig, error)
}

// ResolveTimeout caps each RPC call. Generous because cs-user may be slow
// under load; we'd rather wait than have every sync fail.
const ResolveTimeout = 10 * time.Second

// CacheTTL is how long a successful resolve stays fresh. 5 min chosen
// because admin-triggered sync is bursty, not hot.
const CacheTTL = 5 * time.Minute

// RPCResolver resolves per-tenant Git server configs via the cs-user RPC.
type RPCResolver struct {
	client GitServerClient
	ttl    time.Duration

	mu    sync.RWMutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	cfg       *GitServerConfig
	expiresAt time.Time
}

// NewRPCResolver builds a resolver backed by the supplied client. The
// client is typically a small adapter wrapping *user.RPCClient. ttl<=0
// falls back to CacheTTL.
func NewRPCResolver(client GitServerClient, ttl time.Duration) *RPCResolver {
	if ttl <= 0 {
		ttl = CacheTTL
	}
	return &RPCResolver{
		client: client,
		ttl:    ttl,
		cache:  make(map[string]*cacheEntry),
	}
}

// Resolve returns the cached config if fresh, otherwise calls the RPC.
// Errors are never cached — a transient failure retries on the next call.
func (r *RPCResolver) Resolve(ctx context.Context, tenantID string) (*GitServerConfig, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("gitsync: nil RPCResolver or client")
	}
	if tenantID == "" {
		return nil, errors.New("gitsync: tenantID required")
	}

	if cached := r.lookup(tenantID); cached != nil {
		return cached, nil
	}

	ctx, cancel := context.WithTimeout(ctx, ResolveTimeout)
	defer cancel()
	cfg, err := r.client.GetTenantGitServer(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	r.store(tenantID, cfg)
	return cfg, nil
}

// lookup returns the cached entry if it's still fresh; nil otherwise.
// Caller holds neither lock — this function takes the read lock.
func (r *RPCResolver) lookup(tenantID string) *GitServerConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.cache[tenantID]
	if !ok {
		return nil
	}
	if time.Now().After(e.expiresAt) {
		return nil
	}
	return e.cfg
}

// store writes a fresh entry with the configured TTL.
func (r *RPCResolver) store(tenantID string, cfg *GitServerConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[tenantID] = &cacheEntry{
		cfg:       cfg,
		expiresAt: time.Now().Add(r.ttl),
	}
}
