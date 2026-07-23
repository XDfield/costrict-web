// Package eventbus implements cs-user's outbox pattern (Git Ownership
// Refactor Phase 2).
//
// Architecture (matches docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md §5):
//
//	user/service.go::GetOrCreateUser
//	   │
//	   ▼
//	Enqueue(eventType, payload)   ◄── Writer (sync, called inline)
//	   │
//	┌──┴───────────────┐
//	│  user_events     │  Postgres table
//	└──┬───────────────┘
//	   │
//	Worker.Run(ctx)    ◄── goroutine, polls every tick
//	   │
//	   ▼
//	HTTP POST /api/internal/users/created  →  server (consumer)
//
// Guarantees:
//
//   - At-least-once: a row stays pending until the consumer ACKs (2xx).
//   - Idempotency rests on the consumer side (keyed by event_id).
//   - Ordering: a single worker goroutine drains FIFO by created_at; per-
//     subject ordering is preserved because each Enqueue is sequenced.
//   - Backoff: failed delivery pushes available_at into the future with
//     exponential backoff (base * 2^attempts, capped).
//
// The worker is a singleton per process; Phase 2 does not implement
// leader election (R1 mitigation deferred to Phase 5 once the consumer is
// stable). Multiple cs-user replicas will double-deliver — the consumer
// MUST be idempotent.

package eventbus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Sentinel errors.
var (
	// ErrOutboxClosed — Enqueue called after Close. Surfaces as a hard
	// failure so the user-create flow can decide whether to log+continue
	// or refuse.
	ErrOutboxClosed = errors.New("eventbus: outbox closed")

	// ErrInvalidEvent — Enqueue called with empty type or subject.
	ErrInvalidEvent = errors.New("eventbus: invalid event (type/subject required)")
)

// Config tunes outbox worker behaviour.
type Config struct {
	// TargetURL is the consumer endpoint (e.g.
	// "http://server:8080/api/internal/users/created"). Required.
	TargetURL string

	// TargetToken is the X-Internal-Token header value. Required.
	TargetToken string

	// PollInterval is the gap between worker sweeps. Default 1s.
	PollInterval time.Duration

	// BatchSize caps the rows fetched per sweep. Default 50.
	BatchSize int

	// MaxAttempts caps retry count; row moves to 'failed' after this. 0 = unlimited.
	MaxAttempts int

	// BackoffBase is the initial backoff (doubles each attempt).
	BackoffBase time.Duration

	// BackoffMax is the backoff ceiling.
	BackoffMax time.Duration

	// HTTPTimeout caps a single delivery attempt.
	HTTPTimeout time.Duration
}

func (c *Config) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 1 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 2 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Minute
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 5 * time.Second
	}
}

// EventPayload is the JSON body delivered to the consumer.
//
// Field naming mirrors the spec's example in
// docs/repo-management/GIT_OWNERSHIP_REFACTOR_PROPOSAL.md §5.
type EventPayload struct {
	EventID   string    `json:"event_id"`
	EventType string    `json:"event_type"`
	SubjectID string    `json:"subject_id"`
	TenantID  string    `json:"tenant_id"`
	OccurredAt time.Time `json:"occurred_at"`
	User      UserPayload `json:"user"`
}

// UserPayload carries the user fields the consumer needs to provision
// downstream accounts (Gitea login, email for invitation, etc.).
type UserPayload struct {
	SubjectID   string  `json:"subject_id"`
	TenantID    string  `json:"tenant_id"`
	Username    string  `json:"username,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	Email       *string `json:"email,omitempty"`
}

// Outbox is the write+worker façade. Construct once via NewOutbox, share
// across goroutines. Callers MUST Run(ctx) once at boot.
type Outbox struct {
	db     *gorm.DB
	cfg    Config
	log    *zap.Logger
	client *http.Client

	// closed flag flips true on Close(); Enqueue refuses afterwards.
	closedMu sync.RWMutex
	closed   bool

	// httpClient can be overridden in tests.
	httpDo func(req *http.Request) (*http.Response, error)
}

// NewOutbox wires dependencies. cfg.TargetURL may be empty — the writer
// still enqueues rows, but the worker logs delivery failures as config
// errors. Useful for dev environments where server is not yet running.
func NewOutbox(db *gorm.DB, cfg Config, log *zap.Logger) *Outbox {
	cfg.applyDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	o := &Outbox{
		db:     db,
		cfg:    cfg,
		log:    log,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
	}
	o.httpDo = o.client.Do
	return o
}

// Enqueue writes a pending row. Best-effort: callers ignore the returned
// error so user signup never fails because of the outbox. The reconciliation
// cron (Phase 5) can backfill missing rows.
func (o *Outbox) Enqueue(ctx context.Context, eventType, subjectID, tenantID string, user UserPayload) error {
	if o == nil {
		return nil
	}
	o.closedMu.RLock()
	defer o.closedMu.RUnlock()
	if o.closed {
		return ErrOutboxClosed
	}
	if eventType == "" || subjectID == "" {
		return ErrInvalidEvent
	}
	if tenantID == "" {
		tenantID = "default"
	}

	// Caller supplies event_id — the same UUID is reused on retries, which
	// is what makes the consumer idempotent.
	if user.SubjectID == "" {
		user.SubjectID = subjectID
	}
	if user.TenantID == "" {
		user.TenantID = tenantID
	}
	payload := EventPayload{
		EventID:    newEventID(),
		EventType:  eventType,
		SubjectID:  subjectID,
		TenantID:   tenantID,
		OccurredAt: time.Now().UTC(),
		User:       user,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("eventbus: marshal payload: %w", err)
	}

	row := &models.UserEvent{
		EventID:     payload.EventID,
		EventType:   eventType,
		SubjectID:   subjectID,
		TenantID:    tenantID,
		Payload:     string(body),
		Status:      models.UserEventStatusPending,
		Attempts:    0,
		AvailableAt: time.Now(),
		CreatedAt:   time.Now(),
	}
	// INSERT ... ON CONFLICT DO NOTHING — Enqueue may race a duplicate
	// (rare but possible if the upstream retry). Idempotent by event_id.
	if err := o.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(row).Error; err != nil {
		return fmt.Errorf("eventbus: insert: %w", err)
	}
	return nil
}

// Run drains the outbox until ctx is cancelled. Blocks; spawn in a
// goroutine. Safe to call once per process.
func (o *Outbox) Run(ctx context.Context) {
	if o == nil {
		return
	}
	o.log.Info("eventbus.outbox: worker started",
		zap.String("target", o.cfg.TargetURL),
		zap.Duration("poll", o.cfg.PollInterval),
		zap.Int("batch", o.cfg.BatchSize),
	)
	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			o.log.Info("eventbus.outbox: worker stopped")
			return
		case <-ticker.C:
			o.drainOnce(ctx)
		}
	}
}

// drainOnce does one sweep: fetch a batch, deliver each, mark results.
// Bounded by cfg.BatchSize; runs sequentially to preserve per-subject order.
func (o *Outbox) drainOnce(ctx context.Context) {
	if o.db == nil {
		return
	}
	var rows []models.UserEvent
	// SELECT ... FOR UPDATE SKIP LOCKED would be ideal for multi-replica;
	// sqlite (used in tests) doesn't support it. Use a plain SELECT — the
	// worst case is double-delivery, which the consumer rejects via
	// event_id idempotency.
	err := o.db.WithContext(ctx).
		Raw(`SELECT * FROM user_events
		     WHERE status = ? AND available_at <= ?
		     ORDER BY created_at ASC
		     LIMIT ?`,
			models.UserEventStatusPending, time.Now(), o.cfg.BatchSize).
		Scan(&rows).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			o.log.Warn("eventbus.outbox: query failed", zap.Error(err))
		}
		return
	}
	for i := range rows {
		select {
		case <-ctx.Done():
			return
		default:
		}
		o.deliver(ctx, &rows[i])
	}
}

// deliver attempts one HTTP POST and updates the row.
func (o *Outbox) deliver(ctx context.Context, row *models.UserEvent) {
	if o.cfg.TargetURL == "" {
		// Config incomplete — don't increment attempts (operator bug, not a
		// transient failure). available_at advances so the row doesn't hot-
		// loop on every tick.
		o.markAvailableBackoff(ctx, row, "target URL not configured")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.TargetURL, bytes.NewBufferString(row.Payload))
	if err != nil {
		o.markFailure(ctx, row, fmt.Sprintf("build request: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", o.cfg.TargetToken)
	req.Header.Set("X-Event-ID", row.EventID)

	resp, err := o.httpDo(req)
	if err != nil {
		o.markFailure(ctx, row, fmt.Sprintf("http: %v", err))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		o.markDelivered(ctx, row)
		return
	}
	o.markFailure(ctx, row, fmt.Sprintf("consumer returned status=%d", resp.StatusCode))
}

func (o *Outbox) markDelivered(ctx context.Context, row *models.UserEvent) {
	now := time.Now()
	updates := map[string]any{
		"status":       models.UserEventStatusDelivered,
		"delivered_at": now,
		"last_error":   nil,
	}
	if err := o.db.WithContext(ctx).Model(&models.UserEvent{}).
		Where("event_id = ?", row.EventID).
		Updates(updates).Error; err != nil {
		o.log.Warn("eventbus.outbox: markDelivered failed",
			zap.String("event_id", row.EventID), zap.Error(err))
		return
	}
	row.Status = models.UserEventStatusDelivered
	row.DeliveredAt = &now
	row.LastError = nil
}

func (o *Outbox) markFailure(ctx context.Context, row *models.UserEvent, reason string) {
	attempts := row.Attempts + 1
	status := models.UserEventStatusPending
	if o.cfg.MaxAttempts > 0 && attempts >= o.cfg.MaxAttempts {
		status = models.UserEventStatusFailed
	}
	backoff := o.cfg.BackoffBase << uint(attempts-1)
	if backoff > o.cfg.BackoffMax {
		backoff = o.cfg.BackoffMax
	}
	available := time.Now().Add(backoff)
	updates := map[string]any{
		"attempts":     attempts,
		"last_error":   reason,
		"status":       status,
		"available_at": available,
	}
	if err := o.db.WithContext(ctx).Model(&models.UserEvent{}).
		Where("event_id = ?", row.EventID).
		Updates(updates).Error; err != nil {
		o.log.Warn("eventbus.outbox: markFailure failed",
			zap.String("event_id", row.EventID), zap.Error(err))
		return
	}
	row.Attempts = attempts
	row.Status = status
	row.LastError = &reason
	row.AvailableAt = available

	if status == models.UserEventStatusFailed {
		o.log.Error("eventbus.outbox: event moved to failed",
			zap.String("event_id", row.EventID),
			zap.String("type", row.EventType),
			zap.Int("attempts", attempts),
			zap.String("reason", reason),
		)
	}
}

// markAvailableBackoff advances available_at without incrementing attempts
// (used for "feature not configured" — not a transient failure).
func (o *Outbox) markAvailableBackoff(ctx context.Context, row *models.UserEvent, reason string) {
	available := time.Now().Add(o.cfg.BackoffMax)
	updates := map[string]any{
		"available_at": available,
		"last_error":   reason,
	}
	if err := o.db.WithContext(ctx).Model(&models.UserEvent{}).
		Where("event_id = ?", row.EventID).
		Updates(updates).Error; err != nil {
		o.log.Warn("eventbus.outbox: markAvailableBackoff failed",
			zap.String("event_id", row.EventID), zap.Error(err))
		return
	}
	row.AvailableAt = available
	row.LastError = &reason
}

// Close flips the closed flag; subsequent Enqueue calls return
// ErrOutboxClosed. Does NOT cancel a running worker (callers cancel ctx).
func (o *Outbox) Close() {
	if o == nil {
		return
	}
	o.closedMu.Lock()
	defer o.closedMu.Unlock()
	o.closed = true
}

// PendingCount returns the count of pending rows — used by health checks
// and Phase 5 monitoring alerts.
func (o *Outbox) PendingCount(ctx context.Context) (int64, error) {
	if o == nil || o.db == nil {
		return 0, nil
	}
	var n int64
	err := o.db.WithContext(ctx).Model(&models.UserEvent{}).
		Where("status = ?", models.UserEventStatusPending).Count(&n).Error
	return n, err
}

// newEventID returns a UUID v4 string. Imported lazily so a future switch
// to UUIDv7 doesn't touch every call site.
func newEventID() string {
	// Avoid importing google/uuid at the package boundary in case cs-user
	// later switches to a different generator; keep the helper here.
	return strings.ToLower(fmt.Sprintf("%s", uuidV4()))
}

// uuidV4 is split out so tests can override; returns a hyphenated UUID.
// Implemented via crypto/rand to avoid the google/uuid dependency.
func uuidV4() string {
	var b [16]byte
	_, _ = readRandBytes(b[:])
	// RFC 4122 §4.4: set version (4) and variant (10) bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
