package user

import (
	"context"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/patrickmn/go-cache"
)

// CachedUserService wraps a UserReader with an in-memory TTL cache for the
// point-lookup methods (GetUserByID, GetUsersByIDs). List/search methods are
// passed through without caching — their result sets are typically large and
// change frequently, so caching them is more likely to mislead than to speed up.
type CachedUserService struct {
	reader UserReader
	cache  *cache.Cache
}

// NewCachedUserService creates a CachedUserService backed by the given reader.
// The reader may be the local *UserService (default) or an *RPCClient that
// proxies reads to cs-user.
func NewCachedUserService(reader UserReader) *CachedUserService {
	return &CachedUserService{
		reader: reader,
		cache:  cache.New(10*time.Minute, 30*time.Minute),
	}
}

// GetUserByID returns a user by subject id, serving from cache on hits and
// filling the cache on misses.
func (s *CachedUserService) GetUserByID(ctx context.Context, userID string) (*models.User, error) {
	if cached, found := s.cache.Get(userID); found {
		if user, ok := cached.(*models.User); ok {
			return user, nil
		}
	}

	user, err := s.reader.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	s.cache.Set(userID, user, cache.DefaultExpiration)
	return user, nil
}

// GetUsersByIDs batch loads users, filling the cache for misses.
func (s *CachedUserService) GetUsersByIDs(ctx context.Context, userIDs []string) (map[string]*models.User, error) {
	result := make(map[string]*models.User, len(userIDs))
	missing := make([]string, 0, len(userIDs))

	for _, userID := range userIDs {
		if cached, found := s.cache.Get(userID); found {
			if user, ok := cached.(*models.User); ok {
				result[userID] = user
				continue
			}
		}
		missing = append(missing, userID)
	}

	if len(missing) == 0 {
		return result, nil
	}

	fetched, err := s.reader.GetUsersByIDs(ctx, missing)
	if err != nil {
		return nil, err
	}

	for _, user := range fetched {
		result[user.SubjectID] = user
		s.cache.Set(user.SubjectID, user, cache.DefaultExpiration)
	}

	return result, nil
}

// SearchUsers is a non-cached pass-through to the underlying reader.
func (s *CachedUserService) SearchUsers(ctx context.Context, keyword string, limit int) ([]*models.User, error) {
	return s.reader.SearchUsers(ctx, keyword, limit)
}

// ListUserIdentities is a non-cached pass-through to the underlying reader.
func (s *CachedUserService) ListUserIdentities(ctx context.Context, userSubjectID string) ([]*models.UserAuthIdentity, error) {
	return s.reader.ListUserIdentities(ctx, userSubjectID)
}

// InvalidateCache removes one user's cache entry. This is the hook target for
// UserService.SetOnUserUpdated so that writes invalidate the read cache
// regardless of which backend (local or RPC) populated it.
func (s *CachedUserService) InvalidateCache(userID string) {
	s.cache.Delete(userID)
}
