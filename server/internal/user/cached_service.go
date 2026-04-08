package user

import (
	"context"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/patrickmn/go-cache"
	"gorm.io/gorm"
)

// CachedUserService provides cached user lookup operations.
type CachedUserService struct {
	db    *gorm.DB
	cache *cache.Cache
}

// NewCachedUserService creates a CachedUserService with 10min TTL and 30min cleanup.
func NewCachedUserService(db *gorm.DB) *CachedUserService {
	return &CachedUserService{
		db:    db,
		cache: cache.New(10*time.Minute, 30*time.Minute),
	}
}

// GetUserByID gets a user by id, using in-memory cache first.
func (s *CachedUserService) GetUserByID(ctx context.Context, userID string) (*models.User, error) {
	if cached, found := s.cache.Get(userID); found {
		if user, ok := cached.(*models.User); ok {
			return user, nil
		}
	}

	var user models.User
	if err := s.db.WithContext(ctx).Where("subject_id = ?", userID).First(&user).Error; err != nil {
		return nil, err
	}

	s.cache.Set(userID, &user, cache.DefaultExpiration)
	return &user, nil
}

// GetUsersByIDs batch loads users, filling cache for misses.
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

	var users []*models.User
	if err := s.db.WithContext(ctx).Where("subject_id IN ?", missing).Find(&users).Error; err != nil {
		return nil, err
	}

	for _, user := range users {
		result[user.SubjectID] = user
		s.cache.Set(user.SubjectID, user, cache.DefaultExpiration)
	}

	return result, nil
}

// InvalidateCache removes one user's cache entry.
func (s *CachedUserService) InvalidateCache(userID string) {
	s.cache.Delete(userID)
}

// WarmupCache preloads active users into cache.
func (s *CachedUserService) WarmupCache(ctx context.Context) error {
	var users []*models.User
	if err := s.db.WithContext(ctx).Where("is_active = ?", true).Find(&users).Error; err != nil {
		return err
	}

	for _, user := range users {
		s.cache.Set(user.SubjectID, user, cache.DefaultExpiration)
	}

	return nil
}
