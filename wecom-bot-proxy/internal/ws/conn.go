package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/costrict/costrict-web/wecom-bot-proxy/internal/config"
	"github.com/gorilla/websocket"
)

type ConnectionState int

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
)

func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	default:
		return "unknown"
	}
}

// InboundHandler processes parsed WS frames received from WeCom.
type InboundHandler func(frame *WSFrame)

// Conn manages the WebSocket connection lifecycle.
type Conn struct {
	cfg     config.BotConfig
	logger  *slog.Logger

	mu      sync.RWMutex
	state   ConnectionState
	conn    *websocket.Conn
	sendCh  chan *WSFrame

	handler InboundHandler

	connectedAt time.Time
	lastPong    time.Time

	cancelFn context.CancelFunc
	done     chan struct{}
}

func NewConn(cfg config.BotConfig, logger *slog.Logger, handler InboundHandler) *Conn {
	return &Conn{
		cfg:     cfg,
		logger:  logger,
		state:   StateDisconnected,
		sendCh:  make(chan *WSFrame, 256),
		handler: handler,
		done:    make(chan struct{}),
	}
}

// Start connects and runs the connection loop with reconnect.
func (c *Conn) Start(ctx context.Context) {
	go c.run(ctx)
}

func (c *Conn) run(ctx context.Context) {
	backoff := c.cfg.ReconnectInitialBackoff

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := c.connectAndRun(ctx)
		if err != nil {
			c.logger.Error("ws connection failed", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = backoff * 2
		if backoff > c.cfg.ReconnectMaxBackoff {
			backoff = c.cfg.ReconnectMaxBackoff
		}
	}
}

func (c *Conn) connectAndRun(ctx context.Context) error {
	c.setState(StateConnecting)
	c.logger.Info("connecting to wecom ws", "url", c.cfg.WSURL)

	connCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Use gorilla/websocket DefaultDialer.DialContext instead of nhooyr.io/websocket.Dial
	conn, resp, err := websocket.DefaultDialer.DialContext(connCtx, c.cfg.WSURL, nil)
	if err != nil {
		// Print response details if available
		if resp != nil {
			c.logger.Error("ws dial failed", "status", resp.Status, "statusCode", resp.StatusCode, "headers", resp.Header)
			// Try to read response body if present
			if resp.Body != nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if len(body) > 0 {
					c.logger.Error("ws dial response body", "body", string(body))
				}
			}
		}
		return fmt.Errorf("dial: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	defer func() {
		// Use gorilla/websocket close method
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down"))
		conn.Close()
		c.setState(StateDisconnected)
	}()

	// Subscribe
	if err := c.subscribe(ctx); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	c.setState(StateConnected)
	c.connectedAt = time.Now()
	c.logger.Info("ws connected and subscribed", "bot_id", c.cfg.BotID)

	// Reset backoff on successful connect
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	c.mu.Lock()
	c.cancelFn = runCancel
	c.mu.Unlock()

	// Read loop
	go c.readLoop(runCtx, conn)

	// Write loop (serializes all WS writes)
	go c.writeLoop(runCtx, conn)

	// Heartbeat
	go c.heartbeatLoop(runCtx, conn)

	// Wait for disconnect
	<-runCtx.Done()
	return nil
}

func (c *Conn) subscribe(ctx context.Context) error {
	frame, err := NewCommand(CmdSubscribe, generateReqID(), &SubscribeRequest{
		BotID:  c.cfg.BotID,
		Secret: c.cfg.Secret,
	})
	if err != nil {
		return err
	}

	resp, err := c.sendAndWait(ctx, frame)
	if err != nil {
		return err
	}
	if resp.ErrCode != 0 {
		return fmt.Errorf("subscribe failed: %d %s", resp.ErrCode, resp.ErrMsg)
	}
	return nil
}

func (c *Conn) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Use gorilla/websocket ReadMessage instead of nhooyr.io/websocket.Read
		_, data, err := conn.ReadMessage()
		if err != nil {
			c.logger.Error("ws read error", "error", err)
			c.cancel()
			return
		}

		frame, err := ParseFrame(data)
		if err != nil {
			c.logger.Warn("failed to parse ws frame", "error", err, "data", string(data))
			continue
		}

		// Handle pong internally
		if frame.Cmd == "pong" || (frame.Cmd == CmdPing && frame.ErrCode == 0) {
			c.mu.Lock()
			c.lastPong = time.Now()
			c.mu.Unlock()
			continue
		}

		// Handle disconnected_event
		if frame.Cmd == CmdEventCallback {
			var body EventCallbackBody
			if err := json.Unmarshal(frame.Body, &body); err == nil {
				if body.Event.EventType == EventTypeDisconnected {
					c.logger.Warn("received disconnected_event from wecom")
					c.cancel()
					return
				}
			}
		}

		// Dispatch to handler
		if c.handler != nil {
			c.handler(frame)
		}
	}
}

func (c *Conn) writeLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-c.sendCh:
			data, err := json.Marshal(frame)
			if err != nil {
				c.logger.Error("failed to marshal ws frame", "error", err)
				continue
			}
			// Use gorilla/websocket WriteMessage instead of nhooyr.io/websocket.Write
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				c.logger.Error("ws write error", "error", err)
				c.cancel()
				return
			}
		}
	}
}

func (c *Conn) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()

	missedPongs := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame, err := NewCommand(CmdPing, generateReqID(), nil)
			if err != nil {
				c.logger.Error("failed to create ping frame", "error", err)
				continue
			}

			c.mu.RLock()
			lastPong := c.lastPong
			c.mu.RUnlock()

			if !lastPong.IsZero() && time.Since(lastPong) > c.cfg.HeartbeatInterval*3 {
				missedPongs++
				if missedPongs >= 3 {
					c.logger.Error("heartbeat timeout: 3 consecutive pongs missed, reconnecting")
					c.cancel()
					return
				}
			} else {
				missedPongs = 0
			}

			select {
			case c.sendCh <- frame:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Send enqueues a WSFrame for sending via the write loop.
func (c *Conn) Send(frame *WSFrame) error {
	c.mu.RLock()
	state := c.state
	c.mu.RUnlock()

	if state != StateConnected {
		return fmt.Errorf("not connected (state: %s)", state)
	}

	select {
	case c.sendCh <- frame:
		return nil
	default:
		return fmt.Errorf("send channel full")
	}
}

func (c *Conn) sendAndWait(ctx context.Context, frame *WSFrame) (*WSFrame, error) {
	// For subscribe, we send directly and read one response
	data, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("no connection")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Use gorilla/websocket WriteMessage instead of nhooyr.io/websocket.Write
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	// Use gorilla/websocket ReadMessage instead of nhooyr.io/websocket.Read
	_, respData, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	resp, err := ParseFrame(respData)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return resp, nil
}

func (c *Conn) setState(state ConnectionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

func (c *Conn) State() ConnectionState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *Conn) ConnectedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connectedAt
}

func (c *Conn) LastPong() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastPong
}

func (c *Conn) cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelFn != nil {
		c.cancelFn()
	}
}

func generateReqID() string {
	return fmt.Sprintf("proxy_%d", time.Now().UnixNano())
}