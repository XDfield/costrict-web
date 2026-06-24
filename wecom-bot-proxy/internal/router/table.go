package router

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

type RouteEntry struct {
	Backend   string
	ExpiresAt time.Time
}

type Table struct {
	mu sync.RWMutex

	taskRoutes      *lru.Cache[string, RouteEntry]
	defaultBackend  string
	taskRouteTTL    time.Duration
}

func NewTable(defaultBackend string, maxSize int, ttl time.Duration) (*Table, error) {
	cache, err := lru.New[string, RouteEntry](maxSize)
	if err != nil {
		return nil, err
	}
	return &Table{
		taskRoutes:     cache,
		defaultBackend: defaultBackend,
		taskRouteTTL:   ttl,
	}, nil
}

// Register adds a task_id → backend mapping with the configured TTL.
func (t *Table) Register(taskID, backend string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.taskRoutes.Add(taskID, RouteEntry{
		Backend:   backend,
		ExpiresAt: time.Now().Add(t.taskRouteTTL),
	})
}

// Lookup finds the backend for a task_id. Returns ("", false) if not found or expired.
func (t *Table) Lookup(taskID string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	entry, ok := t.taskRoutes.Get(taskID)
	if !ok {
		return "", false
	}
	if time.Now().After(entry.ExpiresAt) {
		t.taskRoutes.Remove(taskID)
		return "", false
	}
	return entry.Backend, true
}

// DefaultBackend returns the configured default backend.
func (t *Table) DefaultBackend() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.defaultBackend
}

// RouteForCardEvent resolves the backend for a card event.
// Falls back to default if task_id is not found.
func (t *Table) RouteForCardEvent(taskID string) string {
	if backend, ok := t.Lookup(taskID); ok {
		return backend
	}
	return t.DefaultBackend()
}

// Size returns the current number of task route entries.
func (t *Table) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.taskRoutes.Len()
}

// PurgeExpired removes all expired entries.
func (t *Table) PurgeExpired() {
	t.mu.Lock()
	defer t.mu.Unlock()

	keys := t.taskRoutes.Keys()
	now := time.Now()
	for _, key := range keys {
		if entry, ok := t.taskRoutes.Get(key); ok && now.After(entry.ExpiresAt) {
			t.taskRoutes.Remove(key)
		}
	}
}
