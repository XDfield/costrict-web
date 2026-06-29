package clawagent

import "sync"

// EventType represents the type of internal event.
type EventType string

const (
	EventTaskProgress EventType = "task.progress"
	EventTaskComplete EventType = "task.complete"
	EventTaskFailed   EventType = "task.failed"
	EventTaskTimeout  EventType = "task.timeout"
)

// BusEvent is an event on the internal event bus.
type BusEvent struct {
	Type   EventType
	TaskID string
	Data   any
}

// EventBus is an internal pub/sub event bus for coordinating announcements and timeouts.
// It does NOT expose SSE to external clients.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string]map[string]chan<- BusEvent
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[string]map[string]chan<- BusEvent),
	}
}

// Subscribe subscribes to events for a specific task.
// Returns an unsubscribe function.
func (b *EventBus) Subscribe(taskID string, ch chan<- BusEvent) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subscribers[taskID] == nil {
		b.subscribers[taskID] = make(map[string]chan<- BusEvent)
	}

	// Use a unique key per subscriber
	key := taskID
	b.subscribers[taskID][key] = ch

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if subs, ok := b.subscribers[taskID]; ok {
			delete(subs, key)
			if len(subs) == 0 {
				delete(b.subscribers, taskID)
			}
		}
	}
}

// Publish publishes an event to all subscribers of a task.
func (b *EventBus) Publish(event BusEvent) {
	b.mu.RLock()
	subs := b.subscribers[event.TaskID]
	b.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- event:
		default:
		}
	}
}
