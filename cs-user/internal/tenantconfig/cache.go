// cache.go — ProviderMapping cache layer (E1.4).
//
// Implements an in-memory cache using patrickmn/go-cache with 5-minute TTL,
// matching MULTI_TENANCY_DESIGN.md §17.2. The cache key format is
// "provider_mapping:tenant:<tenant_id>".
//
// Future evolution:
//   - Replace with Redis-backed cache for multi-instance deployments.
//   - Cache interface allows switching implementations without changing
//     the Service contract.
//
// Cache invalidation:
//   - TTL-based: 5 minutes (configurable via constant)
//   - Active invalidation: tenant_configs update triggers Invalidate(tenantID)
//   - E1.5 implements active invalidation wiring

package tenantconfig

import (
	"context"
	"fmt"
	"time"

	"github.com/patrickmn/go-cache"
)

const (
	// cacheTTL is the time-to-live for cached provider mappings.
	// 5 minutes matches MULTI_TENANCY_DESIGN.md §17.2.
	cacheTTL = 5 * time.Minute

	// cacheCleanupInterval is how often the cache scanner runs to evict
	// expired items. 10 minutes is 2x TTL, a common pattern.
	cacheCleanupInterval = 10 * time.Minute

	// cacheKeyPrefix is the prefix for all provider mapping cache keys.
	cacheKeyPrefix = "provider_mapping:tenant:"
)

// Cacher is the cache interface for provider mappings.
// Implemented by in-memory cache today; can be replaced with Redis
// without changing Service contract.
type Cacher interface {
	Get(tenantID string) (*ProviderMapping, bool)
	Set(tenantID string, mapping *ProviderMapping)
	Invalidate(tenantID string)
	InvalidateAll()
}

// memoryCache is the in-memory implementation using go-cache.
type memoryCache struct {
	cache *cache.Cache
}

// NewMemoryCache creates a new in-memory cache with the configured TTL.
func NewMemoryCache() Cacher {
	return &memoryCache{
		cache: cache.New(cacheTTL, cacheCleanupInterval),
	}
}

// Get retrieves a cached provider mapping for the tenant.
// Returns (mapping, true) on hit, (nil, false) on miss.
func (c *memoryCache) Get(tenantID string) (*ProviderMapping, bool) {
	key := cacheKey(tenantID)
	if val, found := c.cache.Get(key); found {
		if mapping, ok := val.(*ProviderMapping); ok {
			return mapping, true
		}
	}
	return nil, false
}

// Set stores a provider mapping in the cache for the tenant.
func (c *memoryCache) Set(tenantID string, mapping *ProviderMapping) {
	key := cacheKey(tenantID)
	c.cache.Set(key, mapping, cache.DefaultExpiration)
}

// Invalidate removes the cached entry for a specific tenant.
// Called when tenant_configs are updated (E1.5).
func (c *memoryCache) Invalidate(tenantID string) {
	key := cacheKey(tenantID)
	c.cache.Delete(key)
}

// InvalidateAll clears all cached entries.
// Useful for testing or global config changes.
func (c *memoryCache) InvalidateAll() {
	c.cache.Flush()
}

// cacheKey builds the cache key for a tenant.
func cacheKey(tenantID string) string {
	return fmt.Sprintf("%s%s", cacheKeyPrefix, tenantID)
}

// CachedService wraps a tenantconfig.Service with caching.
// It implements the same methods but checks the cache before hitting the DB.
type CachedService struct {
	inner  *Service
	cacher Cacher
}

// NewCachedService creates a new cached service.
// If cacher is nil, a new memory cache is created.
func NewCachedService(inner *Service, cacher Cacher) *CachedService {
	if cacher == nil {
		cacher = NewMemoryCache()
	}
	// Wire the cache invalidation callback into the inner service
	inner.SetCacher(cacher)
	return &CachedService{
		inner:  inner,
		cacher: cacher,
	}
}

// LoadProviderMapping returns the cached provider mapping if available,
// otherwise loads from the inner service and caches the result.
// This is the cached version of Service.LoadProviderMapping.
func (s *CachedService) LoadProviderMapping(ctx context.Context, tenantID string) (*ProviderMapping, error) {
	// Try cache first
	if mapping, found := s.cacher.Get(tenantID); found {
		return mapping, nil
	}

	// Cache miss — load from DB (with global+tenant merge)
	mapping, err := s.inner.LoadProviderMapping(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	// Cache the result
	s.cacher.Set(tenantID, mapping)
	return mapping, nil
}

// InvalidateProviderMappingCache invalidates the cached entry for a tenant.
// This should be called when tenant_configs are updated (E1.5).
func (s *CachedService) InvalidateProviderMappingCache(tenantID string) {
	s.cacher.Invalidate(tenantID)
}

// InvalidateAllCaches clears all cached entries.
// Useful for testing or global config changes.
func (s *CachedService) InvalidateAllCaches() {
	s.cacher.InvalidateAll()
}

// GetEnabledProviders returns the enabled providers for a tenant.
// This method benefits from caching: the provider mapping is cached
// from LoadProviderMapping, then filtered to enabled entries.
func (s *CachedService) GetEnabledProviders(ctx context.Context, tenantID string) ([]string, error) {
	return s.inner.GetEnabledProviders(ctx, tenantID)
}
