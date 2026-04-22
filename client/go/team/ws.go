package team

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsPingInterval = 30 * time.Second
	wsWriteWait    = 10 * time.Second
	wsPongWait     = 60 * time.Second
	wsSendCap      = 256
)

// wsConn manages a single WebSocket connection with automatic reconnect.
// It exposes two channels:
//   - inbound  chan CloudEvent  – events received from the server
//   - outbound chan []byte      – serialised events to be sent to the server
type wsConn struct {
	cfg Config

	inbound  chan CloudEvent
	outbound chan []byte

	mu      sync.Mutex
	rawConn *websocket.Conn

	closed chan struct{}
	once   sync.Once
}

func newWSConn(cfg Config) *wsConn {
	return &wsConn{
		cfg:      cfg,
		inbound:  make(chan CloudEvent, wsSendCap),
		outbound: make(chan []byte, wsSendCap),
		closed:   make(chan struct{}),
	}
}

// run connects (and reconnects on error) until ctx is cancelled.
// Must be called in a goroutine.
func (w *wsConn) run(ctx context.Context) {
	backoff := time.Duration(wsReconnectInitial) * time.Second
	maxBackoff := time.Duration(wsReconnectMax) * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.closed:
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.DialContext(ctx, w.buildURL(), nil)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-w.closed:
				return
			case <-time.After(backoff):
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
		}
		backoff = time.Duration(wsReconnectInitial) * time.Second

		w.mu.Lock()
		w.rawConn = conn
		w.mu.Unlock()

		writeDone := make(chan struct{})
		go w.writeLoop(ctx, conn, writeDone)
		w.readLoop(ctx, conn)
		close(writeDone)

		w.mu.Lock()
		w.rawConn = nil
		w.mu.Unlock()
	}
}

func (w *wsConn) readLoop(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadDeadline(time.Now().Add(wsPongWait)) //nolint:errcheck
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait)) //nolint:errcheck
		return nil
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var evt CloudEvent
		if json.Unmarshal(data, &evt) != nil {
			continue
		}
		select {
		case w.inbound <- evt:
		case <-ctx.Done():
			return
		}
	}
}

func (w *wsConn) writeLoop(ctx context.Context, conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case data := <-w.outbound:
			conn.SetWriteDeadline(time.Now().Add(wsWriteWait)) //nolint:errcheck
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(wsWriteWait)) //nolint:errcheck
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// send enqueues an event for sending. Non-blocking; drops if channel is full.
func (w *wsConn) send(evt CloudEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	select {
	case w.outbound <- data:
		return nil
	default:
		return fmt.Errorf("send channel full, event dropped (type=%s)", evt.Type)
	}
}

// close shuts down the connection cleanly.
func (w *wsConn) close() {
	w.once.Do(func() {
		close(w.closed)
		w.mu.Lock()
		if w.rawConn != nil {
			w.rawConn.Close()
		}
		w.mu.Unlock()
	})
}

// buildURL constructs the WebSocket URL from the Config:
//
//	wss://<ServerURL>/ws/sessions/<SessionID>?machineId=<MachineID>&token=<Token>
func (w *wsConn) buildURL() string {
	base := w.cfg.ServerURL
	base = strings.Replace(base, "https://", "wss://", 1)
	base = strings.Replace(base, "http://", "ws://", 1)
	base = strings.TrimRight(base, "/")

	q := url.Values{}
	q.Set("machineId", w.cfg.MachineID)
	if w.cfg.Token != "" {
		q.Set("token", w.cfg.Token)
	}
	return fmt.Sprintf("%s/ws/sessions/%s?%s", base, w.cfg.SessionID, q.Encode())
}
