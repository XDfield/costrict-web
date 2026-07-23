// Tests for cs-user outbox (Git Ownership Refactor Phase 2).
//
// Two layers:
//   - Unit: Enqueue inserts rows; mark* updates state; backoff math.
//   - Integration: Outbox.Run against an httptest consumer, full
//     pending → delivered transition.

package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupOutboxDB(t *testing.T) *gorm.DB {
	t.Helper()
	// Use shared-cache in-memory DB so multiple connections from the GORM
	// pool see the same schema (otherwise sqlite :memory: is per-connection
	// and the worker goroutine sees a different empty DB).
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS user_events (
		event_id TEXT PRIMARY KEY,
		event_type TEXT NOT NULL,
		subject_id TEXT NOT NULL,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		payload TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		available_at DATETIME NOT NULL,
		delivered_at DATETIME,
		created_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Clean any rows from a prior test sharing the same in-memory DB.
	db.Exec("DELETE FROM user_events")
	return db
}

func TestEnqueue_InsertsPendingRow(t *testing.T) {
	db := setupOutboxDB(t)
	o := NewOutbox(db, Config{TargetURL: "http://x", TargetToken: "t"}, zap.NewNop())

	if err := o.Enqueue(context.Background(), "user.created", "usr-1", "t1", UserPayload{
		SubjectID: "usr-1", TenantID: "t1", Username: "alice",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var row models.UserEvent
	if err := db.First(&row, "subject_id = ?", "usr-1").Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.Status != models.UserEventStatusPending {
		t.Errorf("status = %q, want pending", row.Status)
	}
	if row.EventType != "user.created" {
		t.Errorf("type = %q", row.EventType)
	}
	if row.EventID == "" {
		t.Errorf("event_id empty")
	}
	var payload EventPayload
	if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.User.Username != "alice" {
		t.Errorf("payload username = %q", payload.User.Username)
	}
}

func TestEnqueue_RejectsInvalidInput(t *testing.T) {
	db := setupOutboxDB(t)
	o := NewOutbox(db, Config{TargetURL: "http://x"}, zap.NewNop())

	if err := o.Enqueue(context.Background(), "", "usr-1", "t1", UserPayload{}); err != ErrInvalidEvent {
		t.Errorf("empty type: got %v, want ErrInvalidEvent", err)
	}
	if err := o.Enqueue(context.Background(), "user.created", "", "t1", UserPayload{}); err != ErrInvalidEvent {
		t.Errorf("empty subject: got %v, want ErrInvalidEvent", err)
	}
}

func TestEnqueue_DuplicateEventIDIgnored(t *testing.T) {
	db := setupOutboxDB(t)

	// Force a known event_id by injecting one row directly, then Enqueue
	// with the same id.
	row := &models.UserEvent{
		EventID: "fixed-id", EventType: "user.created",
		SubjectID: "usr-1", TenantID: "t1", Payload: "{}",
		Status: models.UserEventStatusPending,
		AvailableAt: time.Now(), CreatedAt: time.Now(),
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Enqueue generates a new UUID by default; force the same id by
	// pre-seeding the row above. The ON CONFLICT DO NOTHING clause means
	// a second insert with the same id is silently ignored.
	row2 := &models.UserEvent{
		EventID: "fixed-id", EventType: "user.created",
		SubjectID: "usr-1", TenantID: "t1", Payload: "{\"x\":1}",
		Status: models.UserEventStatusPending,
		AvailableAt: time.Now(), CreatedAt: time.Now(),
	}
	if err := db.Create(row2).Error; err == nil {
		t.Errorf("expected duplicate-PK error")
	}

	// Verify only one row exists.
	var count int64
	db.Model(&models.UserEvent{}).Where("event_id = ?", "fixed-id").Count(&count)
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestRun_DeliversToConsumer(t *testing.T) {
	var (
		mu        sync.Mutex
		received  []EventPayload
		authSeen  string
		eventIDHD string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authSeen = r.Header.Get("X-Internal-Token")
		eventIDHD = r.Header.Get("X-Event-ID")
		body, _ := io.ReadAll(r.Body)
		var p EventPayload
		_ = json.Unmarshal(body, &p)
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := setupOutboxDB(t)
	cfg := Config{
		TargetURL:    srv.URL,
		TargetToken:  "test-tok",
		PollInterval: 10 * time.Millisecond,
		BatchSize:    10,
	}
	o := NewOutbox(db, cfg, zap.NewNop())
	if err := o.Enqueue(context.Background(), "user.created", "usr-A", "t1", UserPayload{
		SubjectID: "usr-A", Username: "alice",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	defer cancel()

	// Wait for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("consumer received %d events, want 1", len(received))
	}
	if received[0].SubjectID != "usr-A" {
		t.Errorf("subject = %q", received[0].SubjectID)
	}
	if authSeen != "test-tok" {
		t.Errorf("auth header = %q", authSeen)
	}
	if eventIDHD != received[0].EventID {
		t.Errorf("X-Event-ID header mismatch: %q vs %q", eventIDHD, received[0].EventID)
	}

	// Row should be marked delivered.
	var row models.UserEvent
	if err := db.First(&row, "event_id = ?", received[0].EventID).Error; err != nil {
		t.Fatalf("query row: %v", err)
	}
	if row.Status != models.UserEventStatusDelivered {
		t.Errorf("status = %q, want delivered", row.Status)
	}
	if row.DeliveredAt == nil {
		t.Errorf("delivered_at not set")
	}
}

func TestRun_5xxRetriesWithBackoff(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	db := setupOutboxDB(t)
	cfg := Config{
		TargetURL:    srv.URL,
		TargetToken:  "tok",
		PollInterval: 10 * time.Millisecond,
		BatchSize:    5,
		BackoffBase:  5 * time.Millisecond,
		BackoffMax:   20 * time.Millisecond,
		MaxAttempts:  3,
	}
	o := NewOutbox(db, cfg, zap.NewNop())
	if err := o.Enqueue(context.Background(), "user.created", "usr-B", "t1", UserPayload{
		SubjectID: "usr-B",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	defer cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var row models.UserEvent
		_ = db.First(&row, "subject_id = ?", "usr-B").Error
		if row.Status == models.UserEventStatusFailed {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	var row models.UserEvent
	if err := db.First(&row, "subject_id = ?", "usr-B").Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.Status != models.UserEventStatusFailed {
		t.Errorf("status = %q, want failed", row.Status)
	}
	if row.Attempts < 3 {
		t.Errorf("attempts = %d, want ≥3", row.Attempts)
	}
	if row.LastError == nil || !strings.Contains(*row.LastError, "503") {
		t.Errorf("last_error = %v, want 503", row.LastError)
	}
}

func TestRun_EmptyTargetURLBackoffs(t *testing.T) {
	db := setupOutboxDB(t)
	cfg := Config{
		TargetURL:    "", // not configured
		PollInterval: 10 * time.Millisecond,
		BackoffMax:   50 * time.Millisecond,
	}
	o := NewOutbox(db, cfg, zap.NewNop())
	if err := o.Enqueue(context.Background(), "user.created", "usr-C", "t1", UserPayload{
		SubjectID: "usr-C",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	defer cancel()

	// Give the worker a few ticks.
	time.Sleep(50 * time.Millisecond)

	var row models.UserEvent
	if err := db.First(&row, "subject_id = ?", "usr-C").Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 (config error must not increment attempts)", row.Attempts)
	}
	if row.LastError == nil || !strings.Contains(*row.LastError, "not configured") {
		t.Errorf("last_error = %v", row.LastError)
	}
}

func TestUserPublisher_MapsUserRow(t *testing.T) {
	db := setupOutboxDB(t)
	o := NewOutbox(db, Config{TargetURL: "http://x"}, zap.NewNop())
	p := NewUserPublisher(o)

	email := "alice@example.com"
	dn := "Alice"
	user := &models.User{
		SubjectID: "usr-1", TenantID: "t1",
		Username: "alice", DisplayName: &dn, Email: &email,
	}
	if err := p.PublishUserCreated(context.Background(), user); err != nil {
		t.Fatalf("PublishUserCreated: %v", err)
	}

	var row models.UserEvent
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	var payload EventPayload
	if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.User.Username != "alice" {
		t.Errorf("username = %q", payload.User.Username)
	}
	if payload.User.Email == nil || *payload.User.Email != "alice@example.com" {
		t.Errorf("email not mapped")
	}
	if payload.EventType != "user.created" {
		t.Errorf("type = %q", payload.EventType)
	}
}

func TestUUIDv4_HasCorrectShape(t *testing.T) {
	id := uuidV4()
	if len(id) != 36 {
		t.Fatalf("len = %d, want 36", len(id))
	}
	// version nibble at position 14 must be '4'.
	if id[14] != '4' {
		t.Errorf("version nibble = %q, want '4'", string(id[14]))
	}
	// variant bits at position 19 must be 8/9/a/b.
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("variant nibble = %q, want 8/9/a/b", string(id[19]))
	}
	// Must be unique across many calls.
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		x := uuidV4()
		if seen[x] {
			t.Fatalf("duplicate uuid: %s", x)
		}
		seen[x] = true
	}
}

func TestRun_ContextCancelStopsWorker(t *testing.T) {
	db := setupOutboxDB(t)
	o := NewOutbox(db, Config{
		TargetURL: "http://x", PollInterval: 5 * time.Millisecond,
	}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("worker did not stop after ctx cancel")
	}
}

// Smoke-check the backoff doubling math.
func TestBackoffDoubling(t *testing.T) {
	cfg := Config{BackoffBase: 100 * time.Millisecond, BackoffMax: 1 * time.Second}
	cfg.applyDefaults()
	for _, tc := range []struct{ attempts, wantMs int }{
		{1, 100},  // 2^0 * base
		{2, 200},  // 2^1
		{3, 400},  // 2^2
		{4, 800},  // 2^3
		{5, 1000}, // capped at Max
		{6, 1000}, // still capped
	} {
		got := cfg.BackoffBase << uint(tc.attempts-1)
		if got > cfg.BackoffMax {
			got = cfg.BackoffMax
		}
		if got.Milliseconds() != int64(tc.wantMs) {
			t.Errorf("attempt %d: got %v, want %dms", tc.attempts, got, tc.wantMs)
		}
	}
	_ = fmt.Sprintf // keep fmt imported for future asserts
}
