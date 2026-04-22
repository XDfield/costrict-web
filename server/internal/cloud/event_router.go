package cloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/gateway"
)

type EventRouter struct {
	manager         *ConnectionManager
	gatewayRegistry *gateway.GatewayRegistry
	gatewayClient   *gateway.Client
	mu              sync.Mutex
	batchQueue      map[string][]Event
	staleDeltas     map[string]struct{}
}

func NewEventRouter(manager *ConnectionManager, gatewayRegistry *gateway.GatewayRegistry, gatewayClient *gateway.Client) *EventRouter {
	r := &EventRouter{
		manager:         manager,
		gatewayRegistry: gatewayRegistry,
		gatewayClient:   gatewayClient,
		batchQueue:      make(map[string][]Event),
		staleDeltas:     make(map[string]struct{}),
	}
	go r.startBatchFlush()
	return r
}

func (r *EventRouter) RouteDeviceEvent(deviceID, sessionID string, event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if event.Type == EventMessagePartUpdated {
		if props := event.Properties; props != nil {
			msgID, _ := props["messageID"].(string)
			partID, _ := props["partID"].(string)
			if msgID != "" && partID != "" {
				key := sessionID + ":" + msgID + ":" + partID
				r.staleDeltas[key] = struct{}{}
			}
		}
	}

	var targetConnIDs []string
	switch {
	case strings.HasPrefix(event.Type, "session.") || strings.HasPrefix(event.Type, "message."):
		targetConnIDs = r.manager.FindUserConnsBySession(sessionID)
	}

	for _, connID := range targetConnIDs {
		r.batchQueue[connID] = append(r.batchQueue[connID], event)
	}
}

func (r *EventRouter) RouteUserCommand(deviceID string, event Event) error {
	gw, err := r.gatewayRegistry.GetDeviceGateway(deviceID)
	if err != nil {
		return fmt.Errorf("device not connected")
	}

	path := eventTypeToPath(event.Type, event.Properties)
	if path == "" {
		return fmt.Errorf("unsupported event type: %s", event.Type)
	}

	body, err := json.Marshal(event.Properties)
	if err != nil {
		return fmt.Errorf("marshal failed: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://placeholder"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	rw := &discardResponseWriter{header: make(http.Header)}
	return r.gatewayClient.ProxyRequest(gw.InternalURL, deviceID, req, rw)
}

func eventTypeToPath(eventType string, props map[string]any) string {
	sessionID, _ := props["sessionID"].(string)
	switch eventType {
	case EventSessionAbort:
		if sessionID != "" {
			return "/api/session/" + sessionID + "/abort"
		}
	case EventSessionMessage:
		if sessionID != "" {
			return "/api/session/" + sessionID + "/message"
		}
	case EventTeamTaskDispatch:
		return "/api/v1/teamworker/tasks/dispatch"
	}
	return ""
}

type discardResponseWriter struct {
	header     http.Header
	statusCode int
}

func (w *discardResponseWriter) Header() http.Header         { return w.header }
func (w *discardResponseWriter) WriteHeader(statusCode int)  { w.statusCode = statusCode }
func (w *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

func (r *EventRouter) startBatchFlush() {
	ticker := time.NewTicker(BatchFlushIntervalMs * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		r.flush()
	}
}

func (r *EventRouter) flush() {
	r.mu.Lock()
	queue := r.batchQueue
	stale := r.staleDeltas
	r.batchQueue = make(map[string][]Event)
	r.staleDeltas = make(map[string]struct{})
	r.mu.Unlock()

	for connID, events := range queue {
		filtered := events[:0]
		for _, e := range events {
			if e.Type == EventMessagePartDelta {
				if props := e.Properties; props != nil {
					sessionID, _ := props["sessionID"].(string)
					msgID, _ := props["messageID"].(string)
					partID, _ := props["partID"].(string)
					key := sessionID + ":" + msgID + ":" + partID
					if _, isStale := stale[key]; isStale {
						continue
					}
				}
			}
			filtered = append(filtered, e)
		}

		if len(filtered) == 0 {
			continue
		}

		var batch Event
		if len(filtered) == 1 {
			batch = filtered[0]
		} else {
			batch = Event{
				Type:       EventBatch,
				Properties: map[string]any{"events": filtered},
			}
		}

		r.manager.RouteEvent(batch, []string{connID})
	}
}
