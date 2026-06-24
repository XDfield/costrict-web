package dedup

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

type entry struct {
	SeenAt time.Time
}

type Store struct {
	mu   sync.RWMutex
	cache *lru.Cache[string, entry]
	ttl   time.Duration
}

func NewStore(maxEntries int, ttl time.Duration) (*Store, error) {
	cache, err := lru.New[string, entry](maxEntries)
	if err != nil {
		return nil, err
	}
	return &Store{
		cache: cache,
		ttl:   ttl,
	}, nil
}

// Check returns true if the message ID was already seen (duplicate).
// If not seen, it records the ID and returns false.
func (s *Store) Check(msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.cache.Get(msgID); ok {
		if time.Since(existing.SeenAt) < s.ttl {
			return true // duplicate
		}
		s.cache.Remove(msgID)
	}

	s.cache.Add(msgID, entry{SeenAt: time.Now()})
	return false
}

// Size returns the current number of tracked messages.
func (s *Store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache.Len()
}

// PurgeExpired removes expired entries.
func (s *Store) PurgeExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := s.cache.Keys()
	now := time.Now()
	for _, key := range keys {
		if e, ok := s.cache.Get(key); ok && now.Sub(e.SeenAt) > s.ttl {
			s.cache.Remove(key)
		}
	}
}
