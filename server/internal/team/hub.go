package team

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Hub manages all active WebSocket connections for team sessions.
// It handles:
//   - Connection registration / removal
//   - Message routing (unicast, broadcast within a session)
//   - Redis-backed leader lock (distributed election via SET NX + TTL)
//   - Fencing token (monotonic INCR in Redis)
//   - Offline message backlog (Redis list, TTL = DefaultEventBacklogTTLMin)
//   - Synchronous explore channels (request/response pairing for remote explore)
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*WSConnection // connID → conn

	// sessionConns: sessionID → set of connIDs
	sessionConns map[string]map[string]struct{}

	// machineConns: machineID → connID (one WS per machine per session)
	machineConns map[string]string // "sessionID:machineID" → connID

	redis *redis.Client // nil → operate without Redis

	// exploreMu guards exploreChans
	exploreMu   sync.Mutex
	exploreChans map[string]chan CloudEvent // requestID → result channel

	// decomposeMu guards decomposeChans
	decomposeMu   sync.Mutex
	decomposeChans map[string]chan CloudEvent // requestID → result channel

	// leaderExpiryMu guards leaderExpiredSent
	leaderExpiryMu   sync.Mutex
	leaderExpiredSent map[string]bool // sessionID → already broadcast leader.expired

	stopCh chan struct{} // closed on Close() to stop background goroutines
}

func NewHub(rc *redis.Client) *Hub {
	h := &Hub{
		conns:             make(map[string]*WSConnection),
		sessionConns:      make(map[string]map[string]struct{}),
		machineConns:      make(map[string]string),
		redis:             rc,
		exploreChans:      make(map[string]chan CloudEvent),
		decomposeChans:    make(map[string]chan CloudEvent),
		leaderExpiredSent: make(map[string]bool),
		stopCh:            make(chan struct{}),
	}
	if rc != nil {
		go h.watchLeaderExpiry()
	}
	return h
}

// Close stops background goroutines. Call on server shutdown.
func (h *Hub) Close() {
	select {
	case <-h.stopCh:
		// already closed
	default:
		close(h.stopCh)
	}
}

// ─── Connection lifecycle ──────────────────────────────────────────────────

func (h *Hub) Register(conn *WSConnection) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.conns[conn.ID] = conn

	if h.sessionConns[conn.SessionID] == nil {
		h.sessionConns[conn.SessionID] = make(map[string]struct{})
	}
	h.sessionConns[conn.SessionID][conn.ID] = struct{}{}

	key := machineKey(conn.SessionID, conn.MachineID)
	h.machineConns[key] = conn.ID
}

func (h *Hub) Unregister(conn *WSConnection) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.conns, conn.ID)

	if subs, ok := h.sessionConns[conn.SessionID]; ok {
		delete(subs, conn.ID)
		if len(subs) == 0 {
			delete(h.sessionConns, conn.SessionID)
		}
	}

	key := machineKey(conn.SessionID, conn.MachineID)
	if h.machineConns[key] == conn.ID {
		delete(h.machineConns, key)
	}
}

// ─── Routing ───────────────────────────────────────────────────────────────

// Send delivers an event to a single connection by connID.
func (h *Hub) Send(connID string, evt CloudEvent) {
	h.mu.RLock()
	conn, ok := h.conns[connID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	h.deliver(conn, evt)
}

// SendToMachine routes an event to the machine's active WS connection.
// If offline, the event is pushed to the Redis backlog.
func (h *Hub) SendToMachine(sessionID, machineID string, evt CloudEvent) {
	h.mu.RLock()
	connID, ok := h.machineConns[machineKey(sessionID, machineID)]
	var conn *WSConnection
	if ok {
		conn = h.conns[connID]
	}
	h.mu.RUnlock()

	if conn != nil {
		h.deliver(conn, evt)
		return
	}
	// Machine offline — persist to backlog
	h.appendBacklog(sessionID, machineID, evt)
}

// Broadcast sends an event to every connection in the session.
func (h *Hub) Broadcast(sessionID string, evt CloudEvent) {
	h.mu.RLock()
	subs := h.sessionConns[sessionID]
	targets := make([]*WSConnection, 0, len(subs))
	for cid := range subs {
		if c, ok := h.conns[cid]; ok {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		h.deliver(c, evt)
	}
}

// DrainBacklog pushes all queued events to a reconnected machine.
func (h *Hub) DrainBacklog(sessionID, machineID string) []CloudEvent {
	if h.redis == nil {
		return nil
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyBacklog, sessionID, machineID)

	data, err := h.redis.LRange(ctx, key, 0, -1).Result()
	if err != nil || len(data) == 0 {
		return nil
	}
	h.redis.Del(ctx, key)

	events := make([]CloudEvent, 0, len(data))
	for _, raw := range data {
		var evt CloudEvent
		if json.Unmarshal([]byte(raw), &evt) == nil {
			events = append(events, evt)
		}
	}
	return events
}

// ─── Leader election (Redis) ───────────────────────────────────────────────

// TryAcquireLeader atomically tries to set the leader lock for the session.
// Returns (fencingToken, true) on success, (0, false) if another leader holds it.
func (h *Hub) TryAcquireLeader(sessionID, machineID string) (int64, bool) {
	if h.redis == nil {
		// No Redis: first caller always wins (single-node mode)
		token, _ := h.incrFencingToken(sessionID)
		h.ResetLeaderExpiredSent(sessionID)
		return token, true
	}
	ctx := context.Background()
	lockKey := fmt.Sprintf(redisKeyLeaderLock, sessionID)
	ttl := time.Duration(DefaultLeaderLockTTLSec) * time.Second

	res, err := h.redis.SetArgs(ctx, lockKey, machineID, redis.SetArgs{TTL: ttl, Mode: "NX"}).Result()
	if err != nil || res != "OK" {
		return 0, false
	}
	token, err := h.incrFencingToken(sessionID)
	if err != nil {
		return 0, false
	}
	h.ResetLeaderExpiredSent(sessionID)
	return token, true
}

// RenewLeader refreshes the leader lock TTL. Returns false if the lock is gone.
func (h *Hub) RenewLeader(sessionID, machineID string) bool {
	if h.redis == nil {
		return true
	}
	ctx := context.Background()
	lockKey := fmt.Sprintf(redisKeyLeaderLock, sessionID)

	current, err := h.redis.Get(ctx, lockKey).Result()
	if err != nil || current != machineID {
		return false
	}
	ttl := time.Duration(DefaultLeaderLockTTLSec) * time.Second
	h.redis.Expire(ctx, lockKey, ttl)
	return true
}

// ReleaseLeader deletes the leader lock if this machine still owns it.
func (h *Hub) ReleaseLeader(sessionID, machineID string) {
	if h.redis == nil {
		return
	}
	ctx := context.Background()
	lockKey := fmt.Sprintf(redisKeyLeaderLock, sessionID)

	current, err := h.redis.Get(ctx, lockKey).Result()
	if err == nil && current == machineID {
		h.redis.Del(ctx, lockKey)
	}
}

// GetLeaderMachineID returns the current leader's machineID, or "" if none.
func (h *Hub) GetLeaderMachineID(sessionID string) string {
	if h.redis == nil {
		return ""
	}
	ctx := context.Background()
	lockKey := fmt.Sprintf(redisKeyLeaderLock, sessionID)
	val, err := h.redis.Get(ctx, lockKey).Result()
	if err != nil {
		return ""
	}
	return val
}

// ValidateFencingToken checks that the provided token matches the current
// session token (rejects stale leader writes).
func (h *Hub) ValidateFencingToken(sessionID string, token int64) bool {
	if h.redis == nil {
		return true
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyFencingToken, sessionID)
	current, err := h.redis.Get(ctx, key).Int64()
	if err != nil {
		return true // no token stored yet; allow
	}
	return token >= current
}

// IsMachineOnline returns true if the machine has an active WS connection
// in the session. More reliable than DB status which may lag.
func (h *Hub) IsMachineOnline(sessionID, machineID string) bool {
	h.mu.RLock()
	_, ok := h.machineConns[machineKey(sessionID, machineID)]
	h.mu.RUnlock()
	return ok
}

// ─── Presence ──────────────────────────────────────────────────────────────

// MarkPresence records a machine's last-seen timestamp in Redis.
func (h *Hub) MarkPresence(sessionID, machineID string) {
	if h.redis == nil {
		return
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyPresence, sessionID, machineID)
	h.redis.Set(ctx, key, time.Now().UnixMilli(), time.Duration(DefaultLeaderLockTTLSec*3)*time.Second)
}

// SessionConnCount returns the number of live WS connections for a session.
func (h *Hub) SessionConnCount(sessionID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessionConns[sessionID])
}

// ─── Internal helpers ──────────────────────────────────────────────────────

func (h *Hub) deliver(conn *WSConnection, evt CloudEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	select {
	case conn.Send <- data:
	default:
		// Drop if channel full; caller may handle reconnect
	}

	// Append to the session event log for lastEventId replay
	h.appendEventLog(conn.SessionID, evt)
}

func (h *Hub) appendBacklog(sessionID, machineID string, evt CloudEvent) {
	if h.redis == nil {
		return
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyBacklog, sessionID, machineID)
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	pipe := h.redis.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, time.Duration(DefaultEventBacklogTTLMin)*time.Minute)
	pipe.Exec(ctx) //nolint:errcheck
}

func (h *Hub) incrFencingToken(sessionID string) (int64, error) {
	if h.redis == nil {
		return time.Now().UnixMilli(), nil
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyFencingToken, sessionID)
	return h.redis.Incr(ctx, key).Result()
}

// appendEventLog stores the event in a Redis list for lastEventId-based replay.
// The list is capped to DefaultEventLogMaxLen entries using LTRIM.
func (h *Hub) appendEventLog(sessionID string, evt CloudEvent) {
	if h.redis == nil {
		return
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyEventLog, sessionID)
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	pipe := h.redis.Pipeline()
	pipe.RPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, int64(DefaultEventLogMaxLen-1))
	pipe.Expire(ctx, key, time.Duration(DefaultEventBacklogTTLMin)*time.Minute)
	pipe.Exec(ctx) //nolint:errcheck
}

// ReplayEvents returns all events after the given lastEventId from the session
// event log. Returns nil if no Redis or no events to replay.
func (h *Hub) ReplayEvents(sessionID, lastEventID string) []CloudEvent {
	if h.redis == nil || lastEventID == "" {
		return nil
	}
	ctx := context.Background()
	key := fmt.Sprintf(redisKeyEventLog, sessionID)

	data, err := h.redis.LRange(ctx, key, 0, -1).Result()
	if err != nil || len(data) == 0 {
		return nil
	}

	// Find the position of lastEventID and return everything after it
	startIdx := -1
	for i, raw := range data {
		var evt CloudEvent
		if json.Unmarshal([]byte(raw), &evt) == nil && evt.EventID == lastEventID {
			startIdx = i
			break
		}
	}

	if startIdx < 0 {
		// Event ID not found in log — replay everything (conservative)
		var events []CloudEvent
		for _, raw := range data {
			var evt CloudEvent
			if json.Unmarshal([]byte(raw), &evt) == nil {
				events = append(events, evt)
			}
		}
		return events
	}

	// Return events after the found position
	var events []CloudEvent
	for _, raw := range data[startIdx+1:] {
		var evt CloudEvent
		if json.Unmarshal([]byte(raw), &evt) == nil {
			events = append(events, evt)
		}
	}
	return events
}

// ─── Leader expiry watcher ──────────────────────────────────────────────

// watchLeaderExpiry subscribes to Redis keyspace notifications for leader
// lock key expiry events. When a leader lock expires, it broadcasts
// leader.expired to the session so clients can trigger re-election.
func (h *Hub) watchLeaderExpiry() {
	ctx := context.Background()

	// Enable expiry notifications on the connection
	h.redis.ConfigSet(ctx, "notify-keyspace-events", "Ex")

	sub := h.redis.PSubscribe(ctx, "__keyevent@0__:expired")
	defer sub.Close()

	ch := sub.Channel()
	for {
		select {
		case <-h.stopCh:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Key pattern: team:session:<sessionID>:leader_lock
			sessionID := extractSessionFromLockKey(msg.Payload)
			if sessionID == "" {
				continue
			}
			// Deduplicate: only broadcast once per expiry until a new leader is elected
			h.leaderExpiryMu.Lock()
			if h.leaderExpiredSent[sessionID] {
				h.leaderExpiryMu.Unlock()
				continue
			}
			h.leaderExpiredSent[sessionID] = true
			h.leaderExpiryMu.Unlock()

			h.Broadcast(sessionID, CloudEvent{
				EventID:   fmt.Sprintf("le-exp-%d", time.Now().UnixMilli()),
				Type:      EventLeaderExpired,
				SessionID: sessionID,
				Timestamp: time.Now().UnixMilli(),
				Payload:   map[string]any{"reason": "ttl_expired"},
			})
		}
	}
}

// ResetLeaderExpiredSent clears the dedup flag when a new leader is elected,
// so future expirations can be broadcast.
func (h *Hub) ResetLeaderExpiredSent(sessionID string) {
	h.leaderExpiryMu.Lock()
	delete(h.leaderExpiredSent, sessionID)
	h.leaderExpiryMu.Unlock()
}

// extractSessionFromLockKey parses "team:session:<id>:leader_lock" → "<id>".
func extractSessionFromLockKey(key string) string {
	// Expected: team:session:UUID:leader_lock
	const prefix = "team:session:"
	const suffix = ":leader_lock"
	if len(key) <= len(prefix)+len(suffix) {
		return ""
	}
	if key[:len(prefix)] != prefix || key[len(key)-len(suffix):] != suffix {
		return ""
	}
	return key[len(prefix) : len(key)-len(suffix)]
}

func machineKey(sessionID, machineID string) string {
	return sessionID + ":" + machineID
}

// ─── Synchronous explore channels ─────────────────────────────────────────
// These are used to pair an explore.request HTTP call with its explore.result
// WebSocket response from the target machine.

// RegisterExplore creates a buffered channel for an outgoing explore request.
// The caller is responsible for calling CancelExplore when done.
func (h *Hub) RegisterExplore(requestID string) chan CloudEvent {
	ch := make(chan CloudEvent, 1)
	h.exploreMu.Lock()
	h.exploreChans[requestID] = ch
	h.exploreMu.Unlock()
	return ch
}

// DeliverExplore sends the result event to a waiting RegisterExplore channel.
// It is a no-op if no channel is registered for the requestID.
func (h *Hub) DeliverExplore(requestID string, evt CloudEvent) {
	h.exploreMu.Lock()
	ch, ok := h.exploreChans[requestID]
	h.exploreMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- evt:
	default:
	}
}

// CancelExplore removes the channel for the given requestID.
func (h *Hub) CancelExplore(requestID string) {
	h.exploreMu.Lock()
	delete(h.exploreChans, requestID)
	h.exploreMu.Unlock()
}

// ─── Synchronous decompose channels ──────────────────────────────────────
// Used to pair a decompose HTTP call with its decompose.result WebSocket
// response from the target teammate machine.

// RegisterDecompose creates a buffered channel for an outgoing decompose request.
func (h *Hub) RegisterDecompose(requestID string) chan CloudEvent {
	ch := make(chan CloudEvent, 1)
	h.decomposeMu.Lock()
	h.decomposeChans[requestID] = ch
	h.decomposeMu.Unlock()
	return ch
}

// DeliverDecompose sends the result event to a waiting RegisterDecompose channel.
func (h *Hub) DeliverDecompose(requestID string, evt CloudEvent) {
	h.decomposeMu.Lock()
	ch, ok := h.decomposeChans[requestID]
	h.decomposeMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- evt:
	default:
	}
}

// CancelDecompose removes the channel for the given requestID.
func (h *Hub) CancelDecompose(requestID string) {
	h.decomposeMu.Lock()
	delete(h.decomposeChans, requestID)
	h.decomposeMu.Unlock()
}
